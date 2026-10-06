// case-api — transactional control plane for the NSA Federal IDRE platform.
//
// Responsibilities:
//   - REST API for cases, parties, offer metadata, fees, documents metadata
//   - Keycloak JWT validation (JWKS) + fail-closed tenant resolution (50 state tenants)
//   - Starts/signals Temporal workflows for the statutory lifecycle
//   - Posts double-entry escrow/fee transfers to TigerBeetle (ledger-per-tenant)
//   - Publishes domain events through the Dapr sidecar pub/sub (Kafka backing)
//   - Voice-AI integration surface: agent "tools" endpoints + HMAC event webhooks
package main

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/big"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/go-chi/chi/v5"
	chimw "github.com/go-chi/chi/v5/middleware"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/lestrrat-go/jwx/v2/jwk"
	"github.com/lestrrat-go/jwx/v2/jwt"
	tb "github.com/tigerbeetle/tigerbeetle-go"
	tb_types "github.com/tigerbeetle/tigerbeetle-go/pkg/types"
	temporalclient "go.temporal.io/sdk/client"
)

// ---------------------------------------------------------------------------
// Config
// ---------------------------------------------------------------------------

type Config struct {
	Addr           string // :8080
	DatabaseURL    string // postgres://...
	KeycloakJWKS   string // https://keycloak/realms/idre/protocol/openid-connect/certs
	KeycloakIssuer string // https://keycloak/realms/idre
	TemporalHost   string // temporal-frontend:7233
	TemporalNS     string // idre
	TBAddresses    string // tigerbeetle-0:3000,tigerbeetle-1:3000,...
	TBClusterID    string // decimal cluster ID this TigerBeetle deployment was formatted with
	DaprHTTP       string // http://localhost:3500
	VaultURL       string // http://vault:8081 (mTLS via Dapr in k8s)
	GraphIntelURL  string // http://graph-intel:8082 ("" = graph features disabled)
	StripeSecret   string // sk_live_… / sk_test_… ("" = card payments disabled)
	StripeWebhook  string // whsec_… signing secret for /api/webhooks/stripe
	MojaloopAdapter string // SDK scheme-adapter base URL ("" = mojaloop provider disabled)
	MojaloopSecret  string // HMAC secret shared with the scheme adapter
	PortalBaseURL  string // https://portal.example.gov — Stripe success/cancel return
	ClamdAddr      string // clamd:3310 — ClamAV INSTREAM target (uploads fail-closed if down)
	SMTPHost       string // outbound mail relay (state SMTP / SES / Mailgun); empty = delivery skipped
	SMTPPort       int
	SMTPUser       string
	SMTPPass       string
	SMTPFrom       string // e.g. flcdr@example.org
	WorkerToken    string // shared secret the Temporal worker (idre-workflows) authenticates service-to-service calls with
}

func configFromEnv() Config {
	get := func(k, d string) string {
		if v := os.Getenv(k); v != "" {
			return v
		}
		return d
	}
	getInt := func(k string, d int) int {
		if v := os.Getenv(k); v != "" {
			if n, err := strconv.Atoi(v); err == nil {
				return n
			}
		}
		return d
	}
	return Config{
		Addr:           get("ADDR", ":8080"),
		DatabaseURL:    get("DATABASE_URL", "postgres://idre:idre@localhost:5432/idre"),
		KeycloakJWKS:   get("KEYCLOAK_JWKS_URL", "http://localhost:8085/realms/idre/protocol/openid-connect/certs"),
		KeycloakIssuer: get("KEYCLOAK_ISSUER", "http://localhost:8085/realms/idre"),
		TemporalHost:   get("TEMPORAL_HOST", "localhost:7233"),
		TemporalNS:     get("TEMPORAL_NAMESPACE", "idre"),
		TBAddresses:    get("TIGERBEETLE_ADDRESSES", "localhost:3000"),
		// No safe default: this value is fixed by whichever `tigerbeetle format
		// --cluster=N` created the replicas' on-disk state, and a wrong ID
		// doesn't error -- the client just hangs forever with every connection
		// silently rejected ("invalid header_cluster" on the replica side).
		TBClusterID:    get("TIGERBEETLE_CLUSTER_ID", ""),
		DaprHTTP:       get("DAPR_HTTP_ENDPOINT", "http://localhost:3500"),
		VaultURL:       get("VAULT_URL", "http://localhost:8081"),
		GraphIntelURL:  get("GRAPH_INTEL_URL", "http://localhost:8082"),
		StripeSecret:   get("STRIPE_SECRET_KEY", ""),
		StripeWebhook:  get("STRIPE_WEBHOOK_SECRET", ""),
		MojaloopAdapter: get("MOJALOOP_ADAPTER_URL", ""),
		MojaloopSecret:  get("MOJALOOP_WEBHOOK_SECRET", ""),
		PortalBaseURL:  get("PORTAL_BASE_URL", "http://localhost:8080"),
		ClamdAddr:      get("CLAMD_ADDR", "localhost:3310"),
		SMTPHost:       get("SMTP_HOST", ""),
		SMTPPort:       getInt("SMTP_PORT", 587),
		SMTPUser:       get("SMTP_USER", ""),
		SMTPPass:       get("SMTP_PASS", ""),
		SMTPFrom:       get("SMTP_FROM", "idre@localhost"),
		// No default: an empty WorkerToken disables the service-auth path
		// entirely rather than accepting a guessable default as a credential.
		WorkerToken: get("WORKER_TOKEN", ""),
	}
}

// ---------------------------------------------------------------------------
// Domain types (contracts mirror contracts/idr.ts from the spec)
// ---------------------------------------------------------------------------

type Case struct {
	ID              string    `json:"id"`
	CaseNumber      string    `json:"case_number"`
	Tenant          string    `json:"tenant"`
	Status          string    `json:"status"`
	ServiceLine     string    `json:"service_line"`
	QPA             int64     `json:"qpa_cents"`
	OpenedAt        time.Time `json:"opened_at"`
	OfferWindowEnds time.Time `json:"offer_window_ends_at"`
}

type InitiateRequest struct {
	CaseNumber       string `json:"case_number"`           // optional when the tenant program defines a numbering pattern
	ServiceLine      string `json:"service_line"`
	PlanType         string `json:"plan_type"` // FULLY_INSURED | SELF_FUNDED
	QPACents         int64  `json:"qpa_cents"`
	ProviderID       string `json:"provider_id"`
	PayerID          string `json:"payer_id"`
	OpenNegotiationEnd string `json:"open_negotiation_end"` // YYYY-MM-DD
	DisputedAmountCents int64 `json:"disputed_amount_cents"` // program disputes: drives thresholds + escalation
	NumClaims        int    `json:"num_claims"`
}

type FeeTransfer struct {
	CaseID   string `json:"case_id"`
	Kind     string `json:"kind"` // ADMIN_FEE | IDRE_FEE_RESERVE | SETTLEMENT | REFUND
	PartyID  string `json:"party_id"`
	Amount   uint64 `json:"amount_cents"`
	PostKind string `json:"post_kind"` // PENDING | POST | VOID
}

// ---------------------------------------------------------------------------
// Auth: Keycloak JWT + tenant extraction (fail-closed)
// ---------------------------------------------------------------------------

type principal struct {
	Subject string
	Roles   []string
	Tenants []string // from Keycloak group claim, e.g. /tenant/tx
}

type authn struct {
	keys        jwk.Set
	issuer      string
	workerToken string // shared secret for service-to-service calls; "" disables this path
}

// serviceRole is the synthetic role a valid WORKER_TOKEN caller gets. Never a
// real Keycloak realm role, so nothing issued by Keycloak can collide with
// it. idre-workflows sends `Authorization: Bearer $WORKER_TOKEN` on its own
// service-to-service calls (e.g. the automated SLA-breach escalate call) --
// nothing here ever validated that token, so that path always 401'd.
const serviceRole = "SERVICE_WORKER"

func (a *authn) middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		if raw == "" || raw == r.Header.Get("Authorization") {
			http.Error(w, `{"error":"missing bearer token"}`, http.StatusUnauthorized)
			return
		}
		if a.workerToken != "" && subtle.ConstantTimeCompare([]byte(raw), []byte(a.workerToken)) == 1 {
			p := principal{Subject: "service:idre-workflows", Roles: []string{serviceRole}}
			next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), ctxPrincipal{}, p)))
			return
		}
		tok, err := jwt.ParseString(raw,
			jwt.WithKeySet(a.keys),
			jwt.WithIssuer(a.issuer),
			jwt.WithValidate(true),
		)
		if err != nil {
			http.Error(w, `{"error":"invalid token"}`, http.StatusUnauthorized)
			return
		}
		p := principal{Subject: tok.Subject()}
		if g, ok := tok.Get("groups"); ok {
			if arr, ok := g.([]any); ok {
				for _, it := range arr {
					s := fmt.Sprint(it)
					if strings.HasPrefix(s, "/tenant/") {
						p.Tenants = append(p.Tenants, strings.TrimPrefix(s, "/tenant/"))
					}
				}
			}
		}
		if rr, ok := tok.Get("realm_access"); ok {
			if m, ok := rr.(map[string]any); ok {
				if arr, ok := m["roles"].([]any); ok {
					for _, it := range arr {
						p.Roles = append(p.Roles, fmt.Sprint(it))
					}
				}
			}
		}
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), ctxPrincipal{}, p)))
	})
}

type ctxPrincipal struct{}
type ctxTenant struct{}

// tenancy resolves the state tenant from the path and enforces it against the
// token's tenant group claim. Access matrix:
//   - PLATFORM_ADMIN / FEDERAL_ADMIN: read+write in every state tenant.
//   - STATE_AUDITOR: read-only (GET/HEAD/OPTIONS) in every state tenant;
//     writes are rejected even in the auditor's home tenant — audit is
//     observation, not operation. An auditor who also holds an operational
//     role (e.g. CASE_MANAGER) can still write in tenants where they are a
//     group member.
//   - everyone else: only tenants present in the /tenant/<st> group claim.
func tenancy(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := r.Context().Value(ctxPrincipal{}).(principal)
		tenant := strings.ToLower(chi.URLParam(r, "tenant"))
		if len(tenant) != 2 {
			http.Error(w, `{"error":"tenant required"}`, http.StatusBadRequest)
			return
		}
		readOnly := r.Method == http.MethodGet || r.Method == http.MethodHead || r.Method == http.MethodOptions
		allowed := false
		for _, role := range p.Roles {
			if role == "PLATFORM_ADMIN" || role == "FEDERAL_ADMIN" || role == serviceRole {
				allowed = true
			}
			if role == "STATE_AUDITOR" && readOnly {
				allowed = true
			}
		}
		for _, t := range p.Tenants {
			if t == tenant && (readOnly || !isAuditorOnly(p)) {
				allowed = true
			}
		}
		if !allowed {
			http.Error(w, `{"error":"tenant access denied"}`, http.StatusForbidden)
			return
		}
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), ctxTenant{}, tenant)))
	})
}

// isAuditorOnly reports whether the principal's only elevated role is
// STATE_AUDITOR (pure auditors never write, anywhere).
func isAuditorOnly(p principal) bool {
	for _, r := range p.Roles {
		switch r {
		case "CASE_MANAGER", "ARBITRATOR", "FINANCE", "PARTY", "FEDERAL_ADMIN", "PLATFORM_ADMIN":
			return false
		}
	}
	for _, r := range p.Roles {
		if r == "STATE_AUDITOR" {
			return true
		}
	}
	return false
}

// ---------------------------------------------------------------------------
// Server
// ---------------------------------------------------------------------------

type server struct {
	cfg     Config
	db      *pgxpool.Pool
	tc      temporalclient.Client
	tb      tb.Client
	docs    *docStore
	rds     *redisClient // cache + idempotency (fail-open)
	permify string       // Permify base URL ("" = ReBAC check disabled, dev mode)
}

func envOr(k, d string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return d
}

// resolveTBAddresses turns host:port entries into ip:port before the
// TigerBeetle client is constructed. No TigerBeetle client binding resolves
// DNS internally -- it needs raw IPs -- so entries that are StatefulSet
// per-pod headless-service hostnames (stable across a pod reschedule, unlike
// the pod's own IP) are resolved here instead of being frozen as IPs at
// deploy time.
func resolveTBAddresses(addrs []string) ([]string, error) {
	out := make([]string, len(addrs))
	for i, a := range addrs {
		host, port, err := net.SplitHostPort(a)
		if err != nil {
			return nil, fmt.Errorf("bad tigerbeetle address %q: %w", a, err)
		}
		if net.ParseIP(host) != nil {
			out[i] = a
			continue
		}
		ips, err := net.LookupIP(host)
		if err != nil {
			return nil, fmt.Errorf("resolving tigerbeetle address %q: %w", a, err)
		}
		var ip net.IP
		for _, c := range ips {
			if v4 := c.To4(); v4 != nil {
				ip = v4
				break
			}
		}
		if ip == nil {
			return nil, fmt.Errorf("tigerbeetle address %q has no IPv4 record", a)
		}
		out[i] = net.JoinHostPort(ip.String(), port)
	}
	return out, nil
}

func main() {
	cfg := configFromEnv()
	ctx := context.Background()

	// Connection pool sized for throughput: 50 in-flight queries per replica
	// (PgBouncer txn-pooling fronts Postgres in-cluster, so 50/replica is safe).
	pcfg, err := pgxpool.ParseConfig(cfg.DatabaseURL)
	must(err)
	if v := os.Getenv("DB_POOL_MAX_CONNS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			pcfg.MaxConns = int32(n)
		}
	} else {
		pcfg.MaxConns = 50
	}
	pcfg.MinConns = 10
	pcfg.MaxConnLifetime = 30 * time.Minute
	pcfg.MaxConnIdleTime = 5 * time.Minute
	pcfg.HealthCheckPeriod = 30 * time.Second
	pool, err := pgxpool.NewWithConfig(ctx, pcfg)
	must(err)
	defer pool.Close()

	tc, err := temporalclient.Dial(temporalclient.Options{
		HostPort:  cfg.TemporalHost,
		Namespace: cfg.TemporalNS,
	})
	must(err)
	defer tc.Close()

	if cfg.TBClusterID == "" {
		panic("TIGERBEETLE_CLUSTER_ID is required -- must match the decimal cluster ID " +
			"the TigerBeetle replicas were formatted with (tigerbeetle format --cluster=N); " +
			"a wrong value does not error, it hangs every ledger connection forever")
	}
	clusterID, ok := new(big.Int).SetString(cfg.TBClusterID, 10)
	if !ok {
		panic("TIGERBEETLE_CLUSTER_ID is not a valid decimal integer: " + cfg.TBClusterID)
	}
	tbAddrs, err := resolveTBAddresses(strings.Split(cfg.TBAddresses, ","))
	must(err)
	tbc, err := tb.NewClient(tb_types.BigIntToUint128(*clusterID), tbAddrs)
	must(err)
	defer tbc.Close()

	rds := newRedis(envOr("REDIS_ADDR", ""), envOr("REDIS_PASSWORD", ""))

	// JWKS via Redis: all replicas share one cache; Keycloak key rotation
	// propagates within the TTL instead of hammering the certs endpoint.
	var keys jwk.Set
	if raw := rds.get("idre:jwks"); raw != "" {
		keys, err = jwk.ParseString(raw)
	}
	if keys == nil {
		keys, err = jwk.Fetch(ctx, cfg.KeycloakJWKS)
		must(err)
		if buf, merr := json.Marshal(keys); merr == nil {
			rds.setex("idre:jwks", 300, string(buf))
		}
	}
	a := &authn{keys: keys, issuer: cfg.KeycloakIssuer, workerToken: cfg.WorkerToken}

	docs, err := newDocStore(
		envOr("MINIO_ENDPOINT", "localhost:9000"),
		envOr("MINIO_USER", "idre"), envOr("MINIO_PASSWORD", "idre-secret"))
	must(err)

	s := &server{cfg: cfg, db: pool, tc: tc, tb: tbc, docs: docs, rds: rds,
		permify: envOr("PERMIFY_URL", "")}

	r := chi.NewRouter()
	r.Use(chimw.RequestID, chimw.RealIP, chimw.Logger, chimw.Recoverer)

	r.Get("/healthz", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })

	// Authenticated, tenant-scoped API.
	r.Route("/v1/tenants/{tenant}", func(r chi.Router) {
		r.Use(a.middleware, tenancy)
		r.Post("/cases/initiate", s.initiateCase)
		r.Get("/cases", s.listCases)
		r.Get("/cases/{caseId}", s.getCase)
		r.Post("/fees/transfer", s.postFeeTransfer)      // escrow/admin/IDRE fee double-entry
		r.Post("/cases/{caseId}/signal", s.signalCase)   // e.g. response filed, fees paid

		// Documents: encrypted upload, authorized download, analysis status.
		r.Post("/cases/{caseId}/documents", s.uploadDocument)
		r.Get("/cases/{caseId}/documents", s.listDocuments)
		r.Get("/cases/{caseId}/documents/{docId}/download", s.downloadDocument)
		r.Patch("/cases/{caseId}/documents/{docId}", s.moveDocument) // re-file to folder (staff only)
		r.Get("/cases/{caseId}/documents/{docId}/analysis", s.documentAnalysis)

		// Stakeholder onboarding: applications, decisions, status.
		r.Patch("/cases/{caseId}/details", s.setCaseDetails)
		r.Post("/cases/{caseId}/lettergen/{templateKey}", s.requestLetterGen)
		r.Get("/cases/{caseId}/documents.zip", s.zipCaseDocuments)
		r.Post("/deliverables/request", s.requestAdhocDeliverable)
		r.Post("/onboarding/applications", s.submitApplication)
		r.Get("/onboarding/applications", s.listApplications)
		r.Post("/onboarding/applications/{appId}/decision", s.decideApplication)
		r.Post("/onboarding/applications/{appId}/documents", s.uploadApplicationDocument)
		r.Post("/onboarding/applications/{appId}/signal", s.signalApplication) // doc-intel -> StakeholderOnboardingWorkflow, SERVICE_WORKER only

		// Voice console + compliance reports (JWT-authenticated reads).
		r.Get("/voice/intake", s.listVoiceIntake)
		r.Get("/voice/logs", s.listVoiceLogs)
		r.Post("/voice/outbound", s.outboundCall)          // trigger outbound calls
		r.Get("/cases/{caseId}/activities", s.listActivities) // CRM record timeline

		// CRM core: accounts, contacts, leads, tasks, notes, search.
		r.Get("/accounts", s.listAccounts)
		r.Post("/accounts", s.createAccount)
		r.Get("/accounts/{accountId}/360", s.account360)
		r.Post("/contacts", s.createContact)
		r.Get("/leads", s.listLeads)
		r.Post("/leads/{leadId}/convert", s.convertLead)
		r.Get("/tasks", s.listTasks)
		r.Post("/tasks", s.createTask)
		r.Post("/tasks/{taskId}/complete", s.completeTask)
		r.Post("/notes", s.addNote)
		r.Get("/search", s.globalSearch)

		// Case management: assignment, escalation, relationships, checklists,
		// calendar, notifications, saved views, letters.
		r.Post("/cases/{caseId}/assign", s.assignCase)
		r.Post("/cases/{caseId}/escalate", s.escalateCase)
		r.Get("/internal/ledger/balances", s.ledgerBalances) // worker-token: reconciliation job
		r.Post("/checks", s.uploadCheck)                        // physical check photo/scan intake
		r.Get("/checks", s.listChecks)                          // review queue
		r.Post("/checks/{checkId}/clear", s.clearCheck)         // funds-cleared settlement
		r.Post("/internal/checks/{checkId}/result", s.checkResult) // worker-token: doc-intel OCR
		r.Post("/cases/relate", s.relateCases)
		r.Get("/cases/{caseId}/relationships", s.caseRelationships)
		r.Get("/cases/{caseId}/checklist", s.getChecklist)
		r.Post("/checklists/{itemId}/check", s.checkItem)
		r.Get("/calendar", s.calendar)
		r.Get("/notifications", s.listNotifications)
		r.Post("/notifications/{notifId}/read", s.readNotification)
		r.Get("/views", s.listSavedViews)
		r.Post("/views", s.saveView)
		r.Get("/prefs", s.getPrefs)   // server-side user preferences (source of truth)
		r.Put("/prefs", s.putPref)
		r.Get("/cases/clocks", s.casesClocks)          // batch statutory-clock projection (grids)
		r.Get("/cases/{caseId}/clocks", s.caseClocks)  // per-case projection (workspace header)
		r.Post("/cases/bulk", s.bulkCases)             // bulk assign / status with per-item results
		r.Post("/queues/grab-next", s.grabNext)        // atomic queue claim (triage fast lane)

		// Graph intelligence (proxied to graph-intel: FalkorDB + GraphSAGE + EPR-KGQA).
		r.Post("/graph/ask", s.graphAsk)                      // EPR-KGQA natural-language query
		r.Post("/graph/feedback", s.graphFeedback)            // thumbs up/down -> ART-ready log
		r.Post("/graph/sync", s.graphSyncNow)                 // Postgres -> FalkorDB -> lakehouse
		r.Post("/graph/to-lakehouse", s.graphToLakehouse)     // FalkorDB -> gold-zone export
		r.Post("/graph/train", s.graphTrain)                  // GraphSAGE training round
		r.Get("/cases/{caseId}/related", s.caseRelated)       // GNN link predictions
		r.Get("/cases/{caseId}/graph-neighbors", s.caseGraphNeighbors)
		r.Post("/cases/{caseId}/letters/{template}", s.generateLetter)
		r.Get("/reports/sla", s.slaReport)
		r.Get("/reports/summary", s.summaryReport)

		// Operations dashboard: live presence heartbeat + manager KPI board.
		r.Post("/presence/ping", s.presencePing)     // any authenticated user
		r.Get("/ops/dashboard", s.opsDashboard)      // staff roles only

		// Program rules (per-state customization; federal NSA is the no-config default).
		r.Get("/program", s.getProgram)
		r.Post("/cases/{caseId}/program-date", s.setProgramDate)      // record clock-basis events
		r.Post("/cases/{caseId}/status", s.setDualStatus)             // dual internal/agency status (G5)
		r.Post("/cases/{caseId}/eligibility", s.checkEligibility)     // threshold matrix + filing window (G2)
		r.Post("/cases/{caseId}/correspondence", s.draftCorrespondence) // template draft / send (G3)
		r.Get("/cases/{caseId}/correspondence", s.listCorrespondence)
		r.Post("/cases/{caseId}/share-links", s.createShareLink)      // tokenized upload/download (G9)
		r.Get("/qa", s.qaQueue)                                       // QA gate queue (G4)
		r.Get("/qa/{qaId}", s.qaGet)
		r.Post("/qa/{qaId}/decision", s.qaDecision)
		r.Post("/cases/{caseId}/invoices", s.issueInvoice)            // dual-party receivables (G8)
		r.Get("/cases/{caseId}/invoices", s.listInvoices)
		r.Get("/invoices", s.listInvoices)
		r.Post("/invoices/{invId}/settle", s.settleInvoice)           // PAY|REFUND|VOID
		r.Get("/reports/receivables", s.receivablesReport)
		r.Post("/invoices/{invId}/checkout", s.createCheckout) // Stripe Checkout session
		r.Get("/payments", s.listPayments)                     // payment history (tenant)
		r.Get("/cases/{caseId}/payments", s.listPayments)      // payment history (case)
		r.Get("/reports/financial", s.financialReport)         // finance dashboard aggregate
		r.Post("/cases/{caseId}/claims", s.importClaims)              // bulk claim lines (G10)
		r.Get("/cases/{caseId}/claims", s.listClaims)
		r.Post("/intake", s.createIntake)                             // pre-case intake (G12)
		r.Get("/intake", s.listIntake)
		r.Post("/intake/{intakeId}/advance", s.advanceIntake)         // refund window enforced

		// Rule engine administration (admin roles only; every write audited).
		r.Get("/rules", s.listRules)
		r.Put("/rules", s.putRules)
		r.Get("/manifest", s.getManifest)
		r.Get("/cases/{caseId}/valuation", s.getValuation)
		r.Put("/manifest", s.putManifest)
		r.Get("/rules/audit", s.rulesAudit)
		r.Get("/deliverables", s.listDeliverables)                    // contract schedule (G7)
		r.Post("/deliverables", s.submitDeliverable)
		r.Post("/cases/{caseId}/opt-out", s.recordOptOut)             // plan opt-out adjudication (G14)
	})

	// Voice-AI surface (API-key auth, not OIDC).
	r.Route("/api/voice", func(r chi.Router) {
		r.Post("/tools/case-status", s.voiceCaseStatus)
		r.Post("/tools/deadlines", s.voiceDeadlines)
		r.Post("/tools/intake", s.voiceIntake)
		r.Post("/events", s.voiceEvents) // HMAC-signed platform → us webhooks
	})

	// Inbound email (Mailgun/SES-style provider webhook, token-authenticated).
	r.Post("/api/email/inbound", s.emailInbound)

	// ShareBox (no OIDC; token + expiry + use-count is the auth):
	// HTML landing page + real no-login upload/download endpoints.
	r.Get("/api/share/{token}", s.shareLanding)
	r.Head("/api/share/{token}", s.resolveShareLink)
	r.Get("/api/share/{token}/meta", s.resolveShareLink)
	r.Post("/api/share/{token}/upload", s.shareUpload)                        // one-shot, small files
	r.Post("/api/share/{token}/uploads", s.shareCreateUpload)                 // resumable: create
	r.Head("/api/share/{token}/uploads/{uploadId}", s.shareUploadOffset)      // resume probe
	r.Patch("/api/share/{token}/uploads/{uploadId}", s.shareUploadChunk)      // next chunk
	r.Post("/api/share/{token}/uploads/{uploadId}/complete", s.shareCompleteUpload)
	r.Get("/api/share/{token}/download", s.shareDownload)                     // Range/resume supported

	// Stripe webhook (no OIDC; HMAC-SHA256 signature against STRIPE_WEBHOOK_SECRET is the auth).
	r.Post("/api/webhooks/stripe", s.stripeWebhook)
	r.Post("/api/webhooks/mojaloop", s.mojaloopWebhook)

	// Public stakeholder application (no OIDC; per-IP throttled, tenant + type validated).
	r.Post("/api/public/apply", s.publicApply)

	// Day-13 intake completeness gate (AHCA 2026): hourly sweep flips stale
	// intakes to INELIGIBLE and raises staff notifications for the letters.
	// Idempotent (status-transition guarded); safe across replicas.
	sweepStop := make(chan struct{})
	go func() {
		t := time.NewTicker(time.Hour)
		defer t.Stop()
		for {
			select {
			case <-sweepStop:
				return
			default:
			}
			if n := s.sweepIntakeDay13(); n > 0 {
				slog.Info("day-13 sweep", "ineligible", n)
			}
			select {
			case <-sweepStop:
				return
			case <-t.C:
			}
		}
	}()

	srv := &http.Server{Addr: cfg.Addr, Handler: r,
		ReadHeaderTimeout: 15 * time.Second, IdleTimeout: 120 * time.Second}
	// Graceful shutdown: drain in-flight requests on SIGTERM/SIGINT so a
	// rolling deploy never truncates an upload or a payment webhook.
	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGTERM, syscall.SIGINT)
	go func() {
		<-quit
		close(sweepStop)
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		_ = srv.Shutdown(ctx)
	}()

	slog.Info("case-api listening", "addr", cfg.Addr)
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		must(err)
	}
}

func must(err error) {
	if err != nil {
		slog.Error("fatal", "err", err)
		os.Exit(1)
	}
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

// ---------------------------------------------------------------------------
// Case initiation: DB row (tenant schema) + Temporal workflow + outbox event
// ---------------------------------------------------------------------------

// businessDaysBetween counts business days (Mon–Fri, minus tenant holidays when
// provided — NG phase 4 calendar engine) from a (exclusive) to b (inclusive).
func businessDaysBetween(a, b time.Time, holidays map[string]bool) int {
	days := 0
	for d := a.AddDate(0, 0, 1); !d.After(b); d = d.AddDate(0, 0, 1) {
		if isBusinessDay(d, holidays) {
			days++
		}
	}
	return days
}

// initiationWindowBD is the federal 4-business-day window to initiate IDR after
// the open negotiation period ends (45 CFR 149.510(b)(2)).
const initiationWindowBD = 4

func (s *server) initiateCase(w http.ResponseWriter, r *http.Request) {
	tenant := r.Context().Value(ctxTenant{}).(string)

	// Idempotency (Redis): mobile/PWA retries replay the same key and get the
	// original case back instead of a duplicate dispute.
	if key := r.Header.Get("Idempotency-Key"); key != "" {
		rkey := fmt.Sprintf("idre:%s:idem:%s", tenant, key)
		if existing := s.rds.get(rkey); existing != "" {
			writeJSON(w, http.StatusOK, map[string]any{"case_id": existing, "idempotent_replay": true})
			return
		}
		if !s.rds.setnx(rkey, 86400, "PENDING") {
			// Another replica is mid-create with this key; ask the client to retry.
			w.Header().Set("Retry-After", "2")
			http.Error(w, `{"error":"request in progress — retry"}`, http.StatusConflict)
			return
		}
	}

	var req InitiateRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, `{"error":"bad request"}`, http.StatusBadRequest)
		return
	}

	// Fail-closed schema pin: one state tenant == one Postgres schema.
	tx, err := s.db.Begin(r.Context())
	if err != nil {
		http.Error(w, `{"error":"db"}`, http.StatusInternalServerError)
		return
	}
	defer tx.Rollback(r.Context())
	if _, err := tx.Exec(r.Context(), fmt.Sprintf(`SET LOCAL search_path TO tenant_%s, public`, sanitizeTenant(tenant))); err != nil {
		http.Error(w, `{"error":"db"}`, http.StatusInternalServerError)
		return
	}

	// Negotiation window: if the 30bd open negotiation period is still running,
	// the case is tracked now and the workflow auto-opens the dispute when it ends.
	today := time.Now().UTC().Truncate(24 * time.Hour)
	negEnd, _ := time.Parse("2006-01-02", req.OpenNegotiationEnd)
	initialStatus := "INITIATED"
	if negEnd.After(today) {
		initialStatus = "NEGOTIATION_TRACKED"
	}

	// Program numbering (G11): when the tenant runs a custom program with a
	// numbering pattern and the caller leaves case_number blank, generate it
	// (e.g. FL26-042). Explicit case numbers are still honored.
	prog := s.loadProgram(r, tenant)
	if req.CaseNumber == "" && prog != nil && prog.CaseNumber.Pattern != "" {
		req.CaseNumber = s.nextCaseNumber(r, tenant, prog)
	}

	var caseID string
	err = tx.QueryRow(r.Context(), `
		INSERT INTO cases (case_number, status, service_line, plan_type, qpa_cents,
		                   provider_id, payer_id, open_negotiation_end,
		                   disputed_amount_cents, num_claims, subject_line, benchmark_cents)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$3,$5) RETURNING id`,
		req.CaseNumber, initialStatus, req.ServiceLine, req.PlanType, req.QPACents,
		req.ProviderID, req.PayerID, req.OpenNegotiationEnd,
		req.DisputedAmountCents, req.NumClaims).Scan(&caseID)
	if err != nil {
		// A duplicate case_number under concurrent requests correctly hits
		// the unique constraint (verified elsewhere: exactly 1 row survives
		// a 10-way race) -- but that's a client error (retry won't help with
		// the same number), not a server fault, so it gets its own status
		// instead of an indistinguishable-from-a-real-bug 500.
		if isUniqueViolation(err) {
			http.Error(w, `{"error":"case_number already exists"}`, http.StatusConflict)
			return
		}
		http.Error(w, `{"error":"insert"}`, http.StatusInternalServerError)
		return
	}

	// Transactional outbox — published to Kafka by the outbox relay.
	payload, _ := json.Marshal(map[string]any{
		"type": "case.initiated", "tenant": tenant,
		"case_id": caseID, "case_number": req.CaseNumber, "at": time.Now().UTC(),
	})
	if _, err := tx.Exec(r.Context(),
		`INSERT INTO outbox (topic, key, payload) VALUES ($1,$2,$3)`,
		fmt.Sprintf("idre.%s.cases", tenant), caseID, payload); err != nil {
		http.Error(w, `{"error":"outbox"}`, http.StatusInternalServerError)
		return
	}
	if err := tx.Commit(r.Context()); err != nil {
		http.Error(w, `{"error":"commit"}`, http.StatusInternalServerError)
		return
	}

	// Start the statutory lifecycle workflow (durable timers for every deadline).
	wfID := fmt.Sprintf("IDR-%s-%s", strings.ToUpper(tenant), req.CaseNumber)
	_, err = s.tc.ExecuteWorkflow(r.Context(), temporalclient.StartWorkflowOptions{
		ID:        wfID,
		TaskQueue: "idre-cases",
	}, "IdrCaseWorkflow", map[string]any{
		"tenant": tenant, "case_id": caseID, "case_number": req.CaseNumber,
		"open_negotiation_end": req.OpenNegotiationEnd, "plan_type": req.PlanType,
	})
	if err != nil {
		http.Error(w, `{"error":"workflow start failed"}`, http.StatusBadGateway)
		return
	}
	// Case-management enrichment: stage checklists + duplicate detection + timeline.
	s.ensureChecklist(tenant, caseID)
	s.logActivity(r.Context(), tenant, caseID, "CASE_INITIATED",
		fmt.Sprintf("Dispute %s initiated (%s) — %s / %s, QPA $%d.%02d, workflow %s",
			req.CaseNumber, initialStatus, req.ServiceLine, req.PlanType, req.QPACents/100, req.QPACents%100, wfID))

	// Federal 4-business-day initiation window (45 CFR 149.510(b)(2)):
	// late filings are allowed but flagged for compliance review.
	var lateInitiation bool
	if !negEnd.After(today) {
		if bd := businessDaysBetween(negEnd, today, nil); bd > initiationWindowBD {
			lateInitiation = true
			detail := fmt.Sprintf("initiated %d business days after open negotiation ended %s (statutory window: %d bd)",
				bd, req.OpenNegotiationEnd, initiationWindowBD)
			_, _ = s.db.Exec(r.Context(), `
				INSERT INTO public.sla_breaches (tenant, case_id, clock, detail)
				VALUES ($1,$2,'INITIATION_4BD',$3)`, tenant, caseID, detail)
			s.logActivity(r.Context(), tenant, caseID, "LATE_INITIATION_FLAGGED", detail)
			s.notify(r, tenant, "*", "LATE_INITIATION",
				fmt.Sprintf("Case %s %s — flagged for compliance review", req.CaseNumber, detail),
				fmt.Sprintf("#/cases/%s", caseID))
		}
	}

	// Program escalation trigger (G6): auto-route over-threshold disputes.
	s.escalationTrigger(r, tenant, caseID, req.DisputedAmountCents)

	dups := s.findDuplicates(r, tenant, req.ProviderID, req.PayerID, req.QPACents)
	if key := r.Header.Get("Idempotency-Key"); key != "" {
		s.rds.setex(fmt.Sprintf("idre:%s:idem:%s", tenant, key), 86400, caseID)
	}
	s.graphSync(tenant) // nudge FalkorDB + lakehouse silver (best-effort)
	writeJSON(w, http.StatusCreated, map[string]any{
		"case_id": caseID, "workflow_id": wfID, "status": initialStatus,
		"late_initiation_flagged": lateInitiation,
		"possible_duplicates":    dups, // non-blocking warning, triage via /cases/relate
	})
}

// listCases: keyset-paginated dispute list. Enterprise tenants hold thousands
// of disputes (FL alone projects ~3k/yr), so a hard LIMIT 200 silently hid
// cases once a tenant outgrew it. Params:
//   ?limit=N      page size, default 50, max 200
//   ?cursor=ts|id keyset position from a previous page's next_cursor
//   ?status=S     exact status filter (saved views push filtering server-side
//                 so a filter never applies to a partial page)
//   ?q=text       case_number ILIKE search
// Response: {"cases": [...], "next_cursor": "...", "total": N}
func (s *server) listCases(w http.ResponseWriter, r *http.Request) {
	tenant := r.Context().Value(ctxTenant{}).(string)
	q := r.URL.Query()
	limit := 50
	if n, err := strconv.Atoi(q.Get("limit")); err == nil && n > 0 {
		limit = min(n, 200)
	}
	where, args := "", []any{}
	add := func(clause string, v any) {
		args = append(args, v)
		where += fmt.Sprintf(" AND "+clause, len(args))
	}
	if st := q.Get("status"); st != "" {
		add("status = $%d", st)
	}
	if qs := q.Get("q"); qs != "" {
		add("case_number ILIKE '%%' || $%d || '%%'", qs)
	}
	// Sort: default opened_at DESC uses keyset pagination; any explicit column
	// sort switches to offset mode (keyset over arbitrary columns isn't stable).
	sortCol, sortDir := "opened_at", "DESC"
	if s := q.Get("sort"); s != "" {
		whitelist := map[string]string{"opened_at": "opened_at", "case_number": "case_number",
			"status": "status", "qpa_cents": "qpa_cents", "service_line": "service_line"}
		col, dir := s, "ASC"
		if strings.HasPrefix(s, "-") {
			col, dir = strings.TrimPrefix(s, "-"), "DESC"
		}
		if c, ok := whitelist[col]; ok {
			sortCol, sortDir = c, dir
		}
	}
	offsetMode := sortCol != "opened_at" || sortDir != "DESC"
	offset := 0
	if offsetMode {
		if n, err := strconv.Atoi(q.Get("offset")); err == nil && n >= 0 {
			offset = n
		}
	} else if cur := q.Get("cursor"); cur != "" {
		parts := strings.SplitN(cur, "|", 2)
		if len(parts) == 2 {
			if ts, err := time.Parse(time.RFC3339Nano, parts[0]); err == nil {
				args = append(args, ts, parts[1])
				where += fmt.Sprintf(" AND (opened_at, id) < ($%d, $%d::uuid)", len(args)-1, len(args))
			}
		}
	}
	tbl := sanitizeTenant(tenant)
	var total int
	if err := s.db.QueryRow(r.Context(),
		fmt.Sprintf(`SELECT count(*) FROM tenant_%s.cases WHERE true%s`, tbl, where), args...).Scan(&total); err != nil {
		http.Error(w, `{"error":"db"}`, http.StatusInternalServerError)
		return
	}
	query := fmt.Sprintf(`SELECT id, case_number, status, coalesce(subject_line, service_line) AS service_line, coalesce(benchmark_cents, qpa_cents) AS qpa_cents, opened_at
		             FROM tenant_%s.cases WHERE true%s
		             ORDER BY %s %s, id DESC LIMIT %d`, tbl, where, sortCol, sortDir, limit+1)
	if offsetMode {
		query += fmt.Sprintf(" OFFSET %d", offset)
	}
	rows, err := s.db.Query(r.Context(), query, args...)
	if err != nil {
		http.Error(w, `{"error":"db"}`, http.StatusInternalServerError)
		return
	}
	defer rows.Close()
	out := []Case{}
	for rows.Next() {
		var c Case
		if err := rows.Scan(&c.ID, &c.CaseNumber, &c.Status, &c.ServiceLine, &c.QPA, &c.OpenedAt); err == nil {
			c.Tenant = tenant
			out = append(out, c)
		}
	}
	next := ""
	if len(out) > limit {
		out = out[:limit]
		if offsetMode {
			next = fmt.Sprintf("offset:%d", offset+limit)
		} else {
			last := out[len(out)-1]
			next = last.OpenedAt.UTC().Format(time.RFC3339Nano) + "|" + last.ID
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"cases": out, "next_cursor": next, "total": total})
}

func (s *server) getCase(w http.ResponseWriter, r *http.Request) {
	tenant := r.Context().Value(ctxTenant{}).(string)
	id := chi.URLParam(r, "caseId")
	var c Case
	var details []byte
	var internal, agency *string
	err := s.db.QueryRow(r.Context(),
		fmt.Sprintf(`SELECT id, case_number, status, coalesce(subject_line, service_line) AS service_line, coalesce(benchmark_cents, qpa_cents) AS qpa_cents, opened_at,
		                    internal_status, agency_status, details
		             FROM tenant_%s.cases WHERE id=$1`, sanitizeTenant(tenant)), id).
		Scan(&c.ID, &c.CaseNumber, &c.Status, &c.ServiceLine, &c.QPA, &c.OpenedAt, &internal, &agency, &details)
	if err != nil {
		http.Error(w, `{"error":"not found"}`, http.StatusNotFound)
		return
	}
	c.Tenant = tenant
	var dj map[string]any
	_ = json.Unmarshal(details, &dj)
	writeJSON(w, http.StatusOK, map[string]any{
		"id": c.ID, "case_number": c.CaseNumber, "tenant": c.Tenant, "status": c.Status,
		"service_line": c.ServiceLine, "qpa_cents": c.QPA, "opened_at": c.OpenedAt,
		"internal_status": internal, "agency_status": agency, "details": dj,
	})
}

// signalCase forwards business signals into the running workflow.
func (s *server) signalCase(w http.ResponseWriter, r *http.Request) {
	tenant := r.Context().Value(ctxTenant{}).(string)
	id := chi.URLParam(r, "caseId")
	var body struct {
		Signal string         `json:"signal"` // RESPONSE_FILED | OFFER_SUBMITTED | FEES_PAID | ...
		Data   map[string]any `json:"data"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.Signal == "" {
		http.Error(w, `{"error":"bad request"}`, http.StatusBadRequest)
		return
	}
	var wfID, status string
	if err := s.db.QueryRow(r.Context(),
		fmt.Sprintf(`SELECT workflow_id, status FROM tenant_%s.cases WHERE id=$1`, sanitizeTenant(tenant)), id).Scan(&wfID, &status); err != nil {
		http.Error(w, `{"error":"not found"}`, http.StatusNotFound)
		return
	}
	// Statutory offer window (45 CFR 149.520(b)(2)): sealed offers are only
	// accepted while the 10-business-day window is open — no late offers.
	if body.Signal == "OFFER_SUBMITTED" && status != "OFFER_WINDOW_OPEN" {
		s.logActivity(r.Context(), tenant, id, "LATE_OFFER_REJECTED",
			fmt.Sprintf("Offer rejected: case status is %s, offer window is not open", status))
		http.Error(w, `{"error":"offer window is not open — late offers are not accepted"}`, http.StatusConflict)
		return
	}
	if err := s.tc.SignalWorkflow(r.Context(), wfID, "", body.Signal, body.Data); err != nil {
		http.Error(w, `{"error":"signal failed"}`, http.StatusBadGateway)
		return
	}
	detail, _ := json.Marshal(body.Data)
	s.logActivity(r.Context(), tenant, id, body.Signal,
		fmt.Sprintf("Workflow signal %s delivered to %s — %s", body.Signal, wfID, truncate(string(detail), 500)))
	s.graphSync(tenant) // graph reflects status transitions (best-effort)
	writeJSON(w, http.StatusAccepted, map[string]string{"status": "signaled"})
}

// ---------------------------------------------------------------------------
// TigerBeetle: double-entry escrow/fee transfers (ledger-per-tenant)
// ---------------------------------------------------------------------------

// Account codes (per tenant ledger):
const (
	acctEscrowTrustHeld  uint32 = 1000
	acctAdminRemittance  uint32 = 3000
	acctIdreCompensation uint32 = 4000
	acctRefundPayable    uint32 = 5000
	ledgerCodeIDRE       uint16 = 700 // platform code; ledger id = tenantLedgerID(tenant)
)

func tenantLedgerID(tenant string) uint32 {
	// Deterministic FIPS-style map: 'tx' -> 48, etc. (kept in public.state_config.tb_ledger_id).
	// Fallback: stable hash into [100..199].
	h := sha256.Sum256([]byte(tenant))
	return 100 + uint32(h[0])%100
}

func acctID(tenant string, code uint32, party string) tb_types.Uint128 {
	h := sha256.Sum256([]byte(fmt.Sprintf("%s:%d:%s", tenant, code, party)))
	var b [16]byte
	copy(b[:], h[:16])
	return tb_types.BytesToUint128(b)
}

// hex128 renders a TigerBeetle Uint128 ID as hex (Bytes() returns an
// unaddressable array — copy first).
func hex128(v tb_types.Uint128) string {
	b := v.Bytes()
	return hex.EncodeToString(b[:])
}

// u128lo returns the low 64 bits (balances are well within uint64).
func u128lo(v tb_types.Uint128) uint64 {
	b := v.Bytes()
	return binary.LittleEndian.Uint64(b[0:8])
}

func hash16(parts ...string) tb_types.Uint128 {
	h := sha256.Sum256([]byte(strings.Join(parts, ":")))
	var b [16]byte
	copy(b[:], h[:16])
	return tb_types.BytesToUint128(b)
}

// transferIDs returns (id, pendingID) for a fee-transfer request. id is this
// transfer's own TigerBeetle ID; pendingID is what to set on the Transfer's
// PendingID field (zero for a PENDING transfer, which doesn't reference one).
//
// The original code here used ONE id, hashed without postKind, for all three
// postKind values, and never set PendingID. A POST or VOID call therefore
// tried to create a second transfer under the SAME id as the original
// pending one, with PendingID left at zero -- TigerBeetle requires PendingID
// on a post/void transfer (zero -> immediate TransferPendingIDMustNotBeZero)
// and separately rejects a second transfer at an id that already exists
// under different flags (TransferExistsWithDifferentFlags, confirmed live
// on the sibling deployment of this same endpoint on the main branch) --
// every two-phase transfer was permanently stuck pending. Fix: a PENDING
// transfer's id IS the reference other calls hash back to (unchanged);
// POST/VOID hash postKind in too for their own distinct id and set
// PendingID to the pending transfer's id so TigerBeetle can find it.
func transferIDs(caseID, kind, partyID string, amount uint64, postKind string) (id, pendingID tb_types.Uint128) {
	pendingID = hash16(caseID, kind, partyID, fmt.Sprint(amount))
	if postKind == "POST" || postKind == "VOID" {
		return hash16(caseID, kind, partyID, fmt.Sprint(amount), postKind), pendingID
	}
	return pendingID, tb_types.Uint128{}
}

func (s *server) postFeeTransfer(w http.ResponseWriter, r *http.Request) {
	tenant := r.Context().Value(ctxTenant{}).(string)
	// RBAC floor: requirePerm (below) is the real fine-grained gate, but it
	// unconditionally allows everyone when Permify isn't deployed (permify.go:
	// "disabled (allow) in dev when unset"). This role check is the floor
	// that holds even with Permify absent -- confirmed on this same endpoint
	// in the sibling deployment (main branch) that an ARBITRATOR reached
	// TigerBeetle and posted a transfer with no role check at all in front of it.
	p := r.Context().Value(ctxPrincipal{}).(principal)
	if !hasAnyRole(p, "FINANCE", "CASE_MANAGER", "FEDERAL_ADMIN", "PLATFORM_ADMIN") {
		http.Error(w, `{"error":"forbidden: requires FINANCE, CASE_MANAGER, FEDERAL_ADMIN, or PLATFORM_ADMIN"}`, http.StatusForbidden)
		return
	}
	// ReBAC: object-level permit on this tenant's ledger (fail-closed).
	if !s.requirePerm(w, r, "ledger", tenant, "transact") {
		return
	}
	var req FeeTransfer
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Amount == 0 {
		http.Error(w, `{"error":"bad request"}`, http.StatusBadRequest)
		return
	}
	var debit, credit tb_types.Uint128
	switch req.Kind {
	case "ADMIN_FEE": // party pays admin fee into remittance account via escrow
		debit, credit = acctID(tenant, acctEscrowTrustHeld, req.PartyID), acctID(tenant, acctAdminRemittance, "")
	case "IDRE_FEE_RESERVE":
		debit, credit = acctID(tenant, acctEscrowTrustHeld, req.PartyID), acctID(tenant, acctIdreCompensation, "")
	case "REFUND":
		debit, credit = acctID(tenant, acctRefundPayable, ""), acctID(tenant, acctEscrowTrustHeld, req.PartyID)
	default: // SETTLEMENT
		debit, credit = acctID(tenant, acctEscrowTrustHeld, req.PartyID), acctID(tenant, acctRefundPayable, "")
	}
	id, transferPendingID := transferIDs(req.CaseID, req.Kind, req.PartyID, req.Amount, req.PostKind)
	var flags uint16
	switch req.PostKind {
	case "PENDING":
		flags = tb_types.TransferFlags{Pending: true}.ToUint16()
	case "POST":
		flags = tb_types.TransferFlags{PostPendingTransfer: true}.ToUint16()
	case "VOID":
		flags = tb_types.TransferFlags{VoidPendingTransfer: true}.ToUint16()
	}
	res, err := s.tb.CreateTransfers([]tb_types.Transfer{{
		ID:              id,
		DebitAccountID:  debit,
		CreditAccountID: credit,
		Amount:          tb_types.ToUint128(req.Amount),
		PendingID:       transferPendingID,
		Ledger:          tenantLedgerID(tenant),
		Code:            ledgerCodeIDRE,
		Flags:           flags,
	}})
	if err != nil {
		http.Error(w, `{"error":"ledger unavailable"}`, http.StatusBadGateway)
		return
	}
	for _, r := range res {
		if r.Result != tb_types.TransferOK && r.Result != tb_types.TransferExists {
			http.Error(w, fmt.Sprintf(`{"error":"ledger rejected: %s"}`, r.Result), http.StatusUnprocessableEntity)
			return
		}
	}
	s.publish(r.Context(), tenant, "fees", map[string]any{
		"type": "fee.transfer", "case_id": req.CaseID, "kind": req.Kind,
		"party": req.PartyID, "amount_cents": req.Amount, "post": req.PostKind,
	})
	writeJSON(w, http.StatusAccepted, map[string]string{"status": "recorded"})
}

// ---------------------------------------------------------------------------
// Dapr pub/sub publish (Kafka-backed component "idre-pubsub")
// ---------------------------------------------------------------------------

func (s *server) publish(ctx context.Context, tenant, domain string, event map[string]any) {
	topic := fmt.Sprintf("idre.%s.%s", tenant, domain)
	url := fmt.Sprintf("%s/v1.0/publish/idre-pubsub/%s", s.cfg.DaprHTTP, topic)
	body, _ := json.Marshal(event)
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, url, strings.NewReader(string(body)))
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		slog.Error("dapr publish failed", "topic", topic, "err", err)
		return
	}
	resp.Body.Close()
}

// ---------------------------------------------------------------------------
// Voice-AI integration (getline.ai-style): tools + signed event webhooks
// ---------------------------------------------------------------------------

// voiceKeyAuth validates per-tenant API keys (sha256 hash stored in Postgres).
func (s *server) voiceKeyAuth(r *http.Request) (string, error) {
	key := r.Header.Get("X-Voice-Api-Key")
	if len(key) < 16 {
		return "", errors.New("missing key")
	}
	sum := sha256.Sum256([]byte(key))
	var tenant string
	err := s.db.QueryRow(r.Context(),
		`SELECT tenant FROM public.voice_api_keys WHERE key_hash=$1 AND revoked_at IS NULL`,
		hex.EncodeToString(sum[:])).Scan(&tenant)
	return tenant, err
}

func (s *server) voiceCaseStatus(w http.ResponseWriter, r *http.Request) {
	tenant, err := s.voiceKeyAuth(r)
	if err != nil {
		http.Error(w, `{"error":"unauthorized"}`, http.StatusUnauthorized)
		return
	}
	var in struct {
		CaseNumber string `json:"case_number"`
	}
	_ = json.NewDecoder(r.Body).Decode(&in)
	var status, nextDeadline string
	err = s.db.QueryRow(r.Context(), fmt.Sprintf(
		`SELECT status, COALESCE(to_char(offer_window_ends_at,'YYYY-MM-DD'),'') FROM tenant_%s.cases WHERE case_number=$1`,
		sanitizeTenant(tenant)), in.CaseNumber).Scan(&status, &nextDeadline)
	if err != nil {
		writeJSON(w, http.StatusOK, map[string]any{"found": false})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"found": true, "case_number": in.CaseNumber, "status": status,
		"next_deadline": nextDeadline,
	})
}

func (s *server) voiceDeadlines(w http.ResponseWriter, r *http.Request) {
	tenant, err := s.voiceKeyAuth(r)
	if err != nil {
		http.Error(w, `{"error":"unauthorized"}`, http.StatusUnauthorized)
		return
	}
	// Computed by the workflow engine; exposed as a compact list for the agent to read.
	writeJSON(w, http.StatusOK, map[string]any{
		"tenant": tenant, "statutory_defaults": map[string]any{
			"open_negotiation_bd": 30, "initiation_bd": 4, "response_bd": 3,
			"offer_window_bd": 10, "determination_bd": 30, "payment_cd": 30,
			"admin_fee_usd": 15,
		},
	})
}

func (s *server) voiceIntake(w http.ResponseWriter, r *http.Request) {
	tenant, err := s.voiceKeyAuth(r)
	if err != nil {
		http.Error(w, `{"error":"unauthorized"}`, http.StatusUnauthorized)
		return
	}
	var in struct {
		CallerPhone  string `json:"caller_phone"`
		CallerName   string `json:"caller_name"`
		Organization string `json:"organization"`
		Summary      string `json:"summary"`
	}
	_ = json.NewDecoder(r.Body).Decode(&in)
	var id string
	if err := s.db.QueryRow(r.Context(), `
		INSERT INTO public.voice_intake_requests (tenant, caller_phone, caller_name, organization, summary)
		VALUES ($1,$2,$3,$4,$5) RETURNING id`,
		tenant, in.CallerPhone, in.CallerName, in.Organization, in.Summary).Scan(&id); err != nil {
		http.Error(w, `{"error":"db"}`, http.StatusInternalServerError)
		return
	}
	// CRM lead capture: every voice intake becomes a lead automatically.
	_, _ = s.db.Exec(r.Context(), `
		INSERT INTO public.leads (tenant, source, name, organization, phone, summary, voice_intake_id)
		VALUES ($1,'VOICE',$2,$3,$4,$5,$6)`,
		tenant, in.CallerName, in.Organization, in.CallerPhone, in.Summary, id)
	s.publish(r.Context(), tenant, "voice", map[string]any{
		"type": "voice.intake", "intake_id": id, "caller": in.CallerName, "org": in.Organization,
	})
	writeJSON(w, http.StatusCreated, map[string]string{"intake_id": id})
}

// voiceEvents receives platform → us webhooks (call.completed etc.), HMAC-verified.
func (s *server) voiceEvents(w http.ResponseWriter, r *http.Request) {
	// A single net.Conn Read() is not guaranteed to return the whole body
	// (plain io.Reader semantics, not io.ReadAll). Confirmed on the sibling
	// deployment of this same endpoint: a genuinely, correctly HMAC-signed
	// ~300KB webhook (a realistic call transcript) got silently truncated
	// here, hashed as a partial body, and rejected as "bad signature" -- a
	// small test payload passed, which is exactly how this went unnoticed.
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 1<<20))
	if err != nil {
		http.Error(w, `{"error":"body too large or unreadable"}`, http.StatusRequestEntityTooLarge)
		return
	}
	sig := r.Header.Get("X-Signature")

	// Resolve tenant by looking up the secret per known header hint, then verify.
	tenant := r.Header.Get("X-Tenant-Code")
	var secret string
	if err := s.db.QueryRow(r.Context(),
		`SELECT webhook_secret FROM public.voice_configs WHERE tenant=$1`, tenant).Scan(&secret); err != nil {
		http.Error(w, `{"error":"unknown tenant"}`, http.StatusUnauthorized)
		return
	}
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	if !hmac.Equal([]byte("sha256="+hex.EncodeToString(mac.Sum(nil))), []byte(sig)) {
		http.Error(w, `{"error":"bad signature"}`, http.StatusUnauthorized)
		return
	}
	var evt struct {
		Type             string         `json:"type"`
		DynamicVariables map[string]any `json:"dynamic_variables"`
		Transcript       string         `json:"transcript"`
	}
	_ = json.Unmarshal(body, &evt)
	_, _ = s.db.Exec(r.Context(), `
		INSERT INTO public.voice_call_logs (tenant, direction, tool, caller_phone, case_number, summary)
		VALUES ($1,'INBOUND_EVENT',$2,$3,$4,$5)`,
		tenant, evt.Type,
		fmt.Sprint(evt.DynamicVariables["caller_phone"]),
		fmt.Sprint(evt.DynamicVariables["case_number"]),
		evt.Transcript)
	// CRM auto-update: attach the call to the case timeline + link intake.
	s.recordVoiceActivity(r, tenant,
		fmt.Sprint(evt.DynamicVariables["case_number"]), evt.Transcript)
	s.publish(r.Context(), tenant, "voice", map[string]any{
		"type": "voice.event", "event": evt.Type, "case_number": evt.DynamicVariables["case_number"],
	})
	writeJSON(w, http.StatusAccepted, map[string]string{"status": "accepted"})
}

// sanitizeTenant guards the one place we build SQL identifiers from input.
func sanitizeTenant(t string) string {
	for _, c := range t {
		if (c < 'a' || c > 'z') && (c < '0' || c > '9') {
			return "xx" // unreachable: tenancy() already validated 2-letter codes
		}
	}
	return t
}

// isUniqueViolation reports whether err is Postgres error code 23505
// (unique_violation) -- a client-error condition (the row already exists),
// distinct from an actual server fault.
func isUniqueViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505"
}
