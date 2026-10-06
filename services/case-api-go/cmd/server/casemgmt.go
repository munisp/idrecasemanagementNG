// casemgmt.go — premier case-management layer: assignment/routing, escalations,
// relationships & batching, stage checklists, deadline calendar, notifications,
// duplicate detection, saved views, document generation, email-to-case.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"text/template"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/minio/minio-go/v7"
)

// ---- Notifications -----------------------------------------------------------

func (s *server) notify(r *http.Request, tenant, userSub, typ, body, link string) {
	_, _ = s.db.Exec(r.Context(), `
		INSERT INTO public.notifications (tenant, user_sub, type, body, link)
		VALUES ($1,$2,$3,$4,$5)`, tenant, userSub, typ, body, link)
}

// logActivity appends to the unified CRM/case timeline used by caseDetail,
// account 360 and voice. Every subsystem (intake, documents, signals, notes,
// email, voice) writes through here so there is ONE activity stream per case.
func (s *server) logActivity(ctx context.Context, tenant, caseID, typ, body string) {
	_, _ = s.db.Exec(ctx, `
		INSERT INTO public.case_activities (tenant, case_id, type, body)
		VALUES ($1,$2,$3,$4)`, tenant, caseID, typ, truncate(body, 2000))
}

func (s *server) listNotifications(w http.ResponseWriter, r *http.Request) {
	tenant := r.Context().Value(ctxTenant{}).(string)
	p := r.Context().Value(ctxPrincipal{}).(principal)
	rows, err := s.db.Query(r.Context(), `
		SELECT id, type, body, COALESCE(link,''), created_at
		FROM public.notifications
		WHERE tenant=$1 AND (user_sub=$2 OR user_sub='*') AND read_at IS NULL
		ORDER BY created_at DESC LIMIT 50`, tenant, p.Subject)
	if err != nil {
		http.Error(w, `{"error":"db"}`, http.StatusInternalServerError)
		return
	}
	defer rows.Close()
	out := []map[string]any{}
	for rows.Next() {
		var id int64
		var typ, body, link string
		var at time.Time
		if rows.Scan(&id, &typ, &body, &link, &at) == nil {
			out = append(out, map[string]any{
				"id": id, "type": typ, "body": body, "link": link, "at": at,
			})
		}
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *server) readNotification(w http.ResponseWriter, r *http.Request) {
	tenant := r.Context().Value(ctxTenant{}).(string)
	_, _ = s.db.Exec(r.Context(), `
		UPDATE public.notifications SET read_at=now()
		WHERE tenant=$1 AND id=$2`, tenant, chi.URLParam(r, "notifId"))
	writeJSON(w, http.StatusOK, map[string]string{"status": "read"})
}

// ---- Assignment & routing ------------------------------------------------------
// Workload-balanced: the case manager (or arbitrator) of this tenant with the
// fewest open assignments wins. Manual override via {"assignee": "<sub>"}.

func (s *server) assignCase(w http.ResponseWriter, r *http.Request) {
	tenant := r.Context().Value(ctxTenant{}).(string)
	caseID := chi.URLParam(r, "caseId")
	var in struct {
		Role     string `json:"role"`     // CASE_MANAGER | ARBITRATOR
		Assignee string `json:"assignee"` // optional manual override
	}
	_ = json.NewDecoder(r.Body).Decode(&in)
	if in.Role == "" {
		in.Role = "CASE_MANAGER"
	}
	assignee := in.Assignee
	if assignee == "" {
		// workload-balanced auto-assignment among eligible users (Keycloak group
		// members are mirrored in accounts/contacts; here: least open cases)
		err := s.db.QueryRow(r.Context(), fmt.Sprintf(`
			SELECT assigned_to FROM tenant_%s.cases
			WHERE assigned_role=$1 AND assigned_to IS NOT NULL
			  AND status NOT LIKE 'CLOSED%%'
			GROUP BY assigned_to ORDER BY COUNT(*) ASC LIMIT 1`, sanitizeTenant(tenant)),
			in.Role).Scan(&assignee)
		if err != nil || assignee == "" {
			assignee = "unassigned-pool" // first assignment starts the pool
		}
	}
	_, err := s.db.Exec(r.Context(), fmt.Sprintf(`
		UPDATE tenant_%s.cases SET assigned_to=$1, assigned_role=$2, updated_at=now()
		WHERE id=$3`, sanitizeTenant(tenant)), assignee, in.Role, caseID)
	if err != nil {
		http.Error(w, `{"error":"db"}`, http.StatusInternalServerError)
		return
	}
	var cn string
	_ = s.db.QueryRow(r.Context(), fmt.Sprintf(
		`SELECT case_number FROM tenant_%s.cases WHERE id=$1`, sanitizeTenant(tenant)), caseID).Scan(&cn)
	s.notify(r, tenant, assignee, "ASSIGNMENT",
		fmt.Sprintf("You were assigned %s as %s", cn, in.Role), "#/cases/"+caseID)
	_, _ = s.db.Exec(r.Context(), `
		INSERT INTO public.case_activities (tenant, case_id, type, body)
		VALUES ($1,$2,'MILESTONE',$3)`, tenant, caseID,
		fmt.Sprintf("Assigned to %s (%s)", assignee, in.Role))
	writeJSON(w, http.StatusOK, map[string]string{"assigned_to": assignee, "role": in.Role})
}

// ---- Escalations (SLA breach -> supervisor chain) --------------------------------

// escalateCase: POST /cases/{caseId}/escalate {clock, detail} — also called by
// the workflow worker when a statutory breach fires (service token).
func (s *server) escalateCase(w http.ResponseWriter, r *http.Request) {
	tenant := r.Context().Value(ctxTenant{}).(string)
	caseID := chi.URLParam(r, "caseId")
	// RBAC floor (requirePerm below allows everyone when Permify is
	// undeployed). SERVICE_WORKER is the Temporal worker's own automated-
	// escalation call (WORKER_TOKEN auth in authn.middleware).
	p := r.Context().Value(ctxPrincipal{}).(principal)
	if !hasAnyRole(p, "CASE_MANAGER", "FEDERAL_ADMIN", "PLATFORM_ADMIN", serviceRole) {
		http.Error(w, `{"error":"forbidden: requires CASE_MANAGER, FEDERAL_ADMIN, or PLATFORM_ADMIN"}`, http.StatusForbidden)
		return
	}
	// ReBAC: escalation is a staff-only object-level permission.
	if !s.requirePerm(w, r, "dispute_case", caseID, "escalate") {
		return
	}
	var in struct {
		Clock  string `json:"clock"`
		Detail string `json:"detail"`
	}
	_ = json.NewDecoder(r.Body).Decode(&in)
	if in.Clock == "" {
		http.Error(w, `{"error":"clock required"}`, http.StatusBadRequest)
		return
	}
	_, _ = s.db.Exec(r.Context(), `
		INSERT INTO public.escalations (tenant, case_id, clock, escalated_to, detail)
		VALUES ($1,$2,$3,'FEDERAL_ADMIN',$4)`, tenant, caseID, in.Clock, in.Detail)
	s.notify(r, tenant, "*", "SLA_BREACH",
		fmt.Sprintf("SLA breach %s on case %s — escalated to FEDERAL_ADMIN", in.Clock, caseID),
		"#/cases/"+caseID)
	// supervisor task so the escalation is owned, not just logged
	_, _ = s.db.Exec(r.Context(), `
		INSERT INTO public.tasks (tenant, subject, case_id, due_date, created_by)
		VALUES ($1,$2,$3, CURRENT_DATE + 2, 'system')`,
		tenant, fmt.Sprintf("Escalation: %s breach on %s", in.Clock, caseID), caseID)
	writeJSON(w, http.StatusCreated, map[string]string{"status": "escalated"})
}

func (s *server) listEscalations(w http.ResponseWriter, r *http.Request) {
	tenant := r.Context().Value(ctxTenant{}).(string)
	out, _ := s.queryRows(r, `
		SELECT case_id, clock, level, escalated_to, detail, created_at
		FROM public.escalations WHERE tenant=$1 ORDER BY created_at DESC LIMIT 100`, tenant)
	writeJSON(w, http.StatusOK, out)
}

// ---- Relationships & batching -----------------------------------------------------

func (s *server) relateCases(w http.ResponseWriter, r *http.Request) {
	tenant := r.Context().Value(ctxTenant{}).(string)
	var in struct {
		CaseID    string `json:"case_id"`
		RelatedID string `json:"related_case_id"`
		RelType   string `json:"rel_type"` // BATCH | PARENT_CHILD | DUPLICATE
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil ||
		(in.RelType != "BATCH" && in.RelType != "PARENT_CHILD" && in.RelType != "DUPLICATE") {
		http.Error(w, `{"error":"rel_type must be BATCH | PARENT_CHILD | DUPLICATE"}`, http.StatusBadRequest)
		return
	}
	_, err := s.db.Exec(r.Context(), `
		INSERT INTO public.case_relationships (tenant, case_id, related_case_id, rel_type)
		VALUES ($1,$2,$3,$4) ON CONFLICT DO NOTHING`,
		tenant, in.CaseID, in.RelatedID, in.RelType)
	if err != nil {
		http.Error(w, `{"error":"db"}`, http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]string{"status": "related"})
}

func (s *server) caseRelationships(w http.ResponseWriter, r *http.Request) {
	tenant := r.Context().Value(ctxTenant{}).(string)
	id := chi.URLParam(r, "caseId")
	out, _ := s.queryRows(r, `
		SELECT related_case_id, rel_type FROM public.case_relationships
		WHERE tenant=$1 AND case_id=$2
		UNION
		SELECT case_id, rel_type FROM public.case_relationships
		WHERE tenant=$1 AND related_case_id=$2`, tenant, id)
	writeJSON(w, http.StatusOK, out)
}

// ---- Stage checklists --------------------------------------------------------------

var stageChecklistDefaults = map[string][]string{
	"INTAKE":       {"claim docs uploaded", "parties verified", "plan type confirmed"},
	"ELIGIBILITY":  {"federal vs SSL routing decided", "initiation window verified", "no active suspension"},
	"OFFERS":       {"both parties fees invoiced", "both sealed offers submitted", "IDRE selection finalized"},
	"DETERMINATION": {"offer amounts within evidence", "QPA credibility assessed", "all 12 notice elements present",
		"written rationale recorded", "no conflict of interest", "baseball rule: one offer selected",
		"IDRE fee split determined", "parties notified", "determination within 30bd",
		"payment terms stated", "reportable to CMS", "signature captured"},
	"PAYMENT":      {"payment within 30cd confirmed", "escrow settled", "refunds processed if any"},
}

func (s *server) ensureChecklist(tenant, caseID string) {
	for stage, items := range stageChecklistDefaults {
		for _, item := range items {
			_, _ = s.db.Exec(context.Background(), `
				INSERT INTO public.case_checklists (tenant, case_id, stage, item)
				VALUES ($1,$2,$3,$4) ON CONFLICT DO NOTHING`, tenant, caseID, stage, item)
		}
	}
}

// autoChecklist ticks checklist items whose truth is already in the
// database — case managers should only ever tick what the platform cannot
// know. done_by records the evidence ('auto: <reason>'), never a user.
// Idempotent: only OPEN items flip, and only when the predicate holds.
func (s *server) autoChecklist(r *http.Request, tenant, caseID string) {
	t := sanitizeTenant(tenant)
	rules := []struct {
		item string
		sql  string
		args []any
	}{
		{"claim docs uploaded",
			fmt.Sprintf(`SELECT EXISTS(SELECT 1 FROM tenant_%s.documents WHERE case_id=$1)`, t), []any{caseID}},
		{"both sealed offers submitted",
			fmt.Sprintf(`SELECT count(*) >= 2 FROM tenant_%s.sealed_offers WHERE case_id=$1`, t), []any{caseID}},
		{"parties notified",
			`SELECT EXISTS(SELECT 1 FROM public.qa_reviews WHERE tenant=$1 AND case_id=$2 AND status='SENT')`,
			[]any{tenant, caseID}},
		{"payment within 30cd confirmed",
			`SELECT EXISTS(SELECT 1 FROM public.invoices WHERE tenant=$1 AND case_id=$2 AND status='PAID')`,
			[]any{tenant, caseID}},
		{"both parties fees invoiced",
			`SELECT count(DISTINCT party) >= 2 FROM public.invoices WHERE tenant=$1 AND case_id=$2`,
			[]any{tenant, caseID}},
	}
	for _, rule := range rules {
		var ok bool
		if err := s.db.QueryRow(r.Context(), rule.sql, rule.args...).Scan(&ok); err != nil || !ok {
			continue
		}
		_, _ = s.db.Exec(r.Context(), `
			UPDATE public.case_checklists SET done=true, done_by=$4, done_at=now()
			WHERE tenant=$1 AND case_id=$2 AND item=$3 AND NOT done`,
			tenant, caseID, rule.item, "auto: evidence in platform records")
	}
}

// maybeAdvanceStatus moves a case forward when platform facts say so —
// the status column follows events, not manual edits. Forward-only along
// the canonical lifecycle; terminal/manual states (Ineligible, Dismissed,
// Withdrawn, Plan Opt-Out) are never touched, and Temporal's own
// transitions simply win the race when the workflow runs first.
func (s *server) maybeAdvanceStatus(r *http.Request, tenant, caseID string) {
	t := sanitizeTenant(tenant)
	// All case invoices PAID while awaiting payment => funds settled.
	_, _ = s.db.Exec(r.Context(), fmt.Sprintf(`
		UPDATE tenant_%s.cases SET status='CLOSED_PAID', updated_at=now()
		WHERE id=$1 AND status='PAYMENT_PENDING'
		  AND EXISTS(SELECT 1 FROM public.invoices WHERE tenant=$2 AND case_id=$1)
		  AND NOT EXISTS(SELECT 1 FROM public.invoices WHERE tenant=$2 AND case_id=$1 AND status='OPEN')`, t),
		caseID, tenant)
	// A generated determination letter while under review => determined.
	_, _ = s.db.Exec(r.Context(), fmt.Sprintf(`
		UPDATE tenant_%s.cases SET status='DETERMINED', updated_at=now()
		WHERE id=$1 AND status='IN_REVIEW'
		  AND EXISTS(SELECT 1 FROM tenant_%s.documents d
		             WHERE d.case_id=$1 AND d.object_key ILIKE '%%determination%%')`, t, t),
		caseID)
}

func (s *server) getChecklist(w http.ResponseWriter, r *http.Request) {
	tenant := r.Context().Value(ctxTenant{}).(string)
	id := chi.URLParam(r, "caseId")
	s.autoChecklist(r, tenant, id)
	out, _ := s.queryRows(r, `
		SELECT id, stage, item, required, done, COALESCE(done_by,'') AS done_by, done_at
		FROM public.case_checklists WHERE tenant=$1 AND case_id=$2
		ORDER BY stage, id`, tenant, id)
	writeJSON(w, http.StatusOK, out)
}

func (s *server) checkItem(w http.ResponseWriter, r *http.Request) {
	tenant := r.Context().Value(ctxTenant{}).(string)
	p := r.Context().Value(ctxPrincipal{}).(principal)
	_, err := s.db.Exec(r.Context(), `
		UPDATE public.case_checklists SET done=true, done_by=$3, done_at=now()
		WHERE tenant=$1 AND id=$2`, tenant, chi.URLParam(r, "itemId"), p.Subject)
	if err != nil {
		http.Error(w, `{"error":"db"}`, http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "checked"})
}

// ---- Deadline calendar (agenda feed) -------------------------------------------------

func (s *server) calendar(w http.ResponseWriter, r *http.Request) {
	tenant := r.Context().Value(ctxTenant{}).(string)
	deadlines, _ := s.queryRows(r, fmt.Sprintf(`
		SELECT case_id::text AS ref, 'OFFER_WINDOW' AS kind, case_number AS label,
		       to_char(offer_window_ends_at,'YYYY-MM-DD"T"HH24:MI') AS due
		FROM tenant_%s.cases
		WHERE offer_window_ends_at IS NOT NULL AND status='OFFER_WINDOW_OPEN'`, sanitizeTenant(tenant)))
	tasks, _ := s.queryRows(r, `
		SELECT id::text AS ref, 'TASK' AS kind, subject AS label,
		       to_char(due_date,'YYYY-MM-DD') AS due
		FROM public.tasks WHERE tenant=$1 AND status='OPEN' AND due_date IS NOT NULL`, tenant)
	writeJSON(w, http.StatusOK, append(deadlines, tasks...))
}

// ---- Duplicate detection (on initiation) ----------------------------------------------

func (s *server) findDuplicates(r *http.Request, tenant, providerID, payerID string, qpaCents int64) []string {
	rows, err := s.db.Query(r.Context(), fmt.Sprintf(`
		SELECT case_number FROM tenant_%s.cases
		WHERE provider_id=$1 AND payer_id=$2
		  AND ABS(coalesce(benchmark_cents, qpa_cents) - $3) < GREATEST(5000, $3/20)
		  AND opened_at > now() - interval '90 days'
		LIMIT 5`, sanitizeTenant(tenant)), providerID, payerID, qpaCents)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var cn string
		if rows.Scan(&cn) == nil {
			out = append(out, cn)
		}
	}
	return out
}

// ---- Saved views ------------------------------------------------------------------------

func (s *server) listSavedViews(w http.ResponseWriter, r *http.Request) {
	tenant := r.Context().Value(ctxTenant{}).(string)
	p := r.Context().Value(ctxPrincipal{}).(principal)
	out, _ := s.queryRows(r, `
		SELECT id, object, name, filters, pinned FROM public.saved_views
		WHERE tenant=$1 AND user_sub=$2 ORDER BY pinned DESC, name`, tenant, p.Subject)
	writeJSON(w, http.StatusOK, out)
}

func (s *server) saveView(w http.ResponseWriter, r *http.Request) {
	tenant := r.Context().Value(ctxTenant{}).(string)
	p := r.Context().Value(ctxPrincipal{}).(principal)
	var in struct {
		Object  string         `json:"object"`
		Name    string         `json:"name"`
		Filters map[string]any `json:"filters"`
		Pinned  bool           `json:"pinned"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil || in.Name == "" || in.Object == "" {
		http.Error(w, `{"error":"object and name required"}`, http.StatusBadRequest)
		return
	}
	filters, _ := json.Marshal(in.Filters)
	_, err := s.db.Exec(r.Context(), `
		INSERT INTO public.saved_views (tenant, user_sub, object, name, filters, pinned)
		VALUES ($1,$2,$3,$4,$5,$6)
		ON CONFLICT (tenant, user_sub, object, name) DO UPDATE SET filters=$5, pinned=$6`,
		tenant, p.Subject, in.Object, in.Name, filters, in.Pinned)
	if err != nil {
		http.Error(w, `{"error":"db"}`, http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]string{"status": "saved"})
}

// ---- Document generation (letters from templates) ----------------------------------------

var letterTemplates = map[string]string{
	"offer_window_notice": `NOTICE OF OFFER WINDOW — Federal IDR
Case: {{.CaseNumber}} ({{.Tenant}})
Service line: {{.ServiceLine}} | QPA: ${{printf "%.2f" .QPAUsd}}
The offer window opens under 45 CFR 149.510. Submit your sealed offer within
10 business days. Late or missing offers default per regulation.`,
	"determination_letter": `CERTIFIED IDRE DETERMINATION — Federal IDR
Case: {{.CaseNumber}} ({{.Tenant}})
Determination issued under 45 CFR 149.510(c)(4). The certified IDRE selected
the offer of: {{.WinningParty}}.
Rationale: {{.Rationale}}
Payment due within 30 calendar days per 45 CFR 149.510(c)(4)(vii).`,
}

func (s *server) generateLetter(w http.ResponseWriter, r *http.Request) {
	tenant := r.Context().Value(ctxTenant{}).(string)
	caseID := chi.URLParam(r, "caseId")
	name := chi.URLParam(r, "template")
	tmplSrc, ok := letterTemplates[name]
	if !ok {
		http.Error(w, `{"error":"unknown template"}`, http.StatusNotFound)
		return
	}
	var cn, sl string
	var qpa int64
	if err := s.db.QueryRow(r.Context(), fmt.Sprintf(`
		SELECT case_number, COALESCE(subject_line, service_line,''), coalesce(benchmark_cents, qpa_cents)
		FROM tenant_%s.cases WHERE id=$1`, sanitizeTenant(tenant)), caseID).
		Scan(&cn, &sl, &qpa); err != nil {
		http.Error(w, `{"error":"case not found"}`, http.StatusNotFound)
		return
	}
	tmpl, err := template.New("letter").Parse(tmplSrc)
	if err != nil {
		http.Error(w, `{"error":"template"}`, http.StatusInternalServerError)
		return
	}
	var buf bytes.Buffer
	_ = tmpl.Execute(&buf, map[string]any{
		"CaseNumber": cn, "Tenant": strings.ToUpper(tenant), "ServiceLine": sl,
		"QPAUsd": float64(qpa) / 100, "WinningParty": r.URL.Query().Get("winning_party"),
		"Rationale": r.URL.Query().Get("rationale"),
	})
	// Store through the encrypted document pipeline (vault + MinIO + metadata).
	ct, err := s.vaultSealDoc(r, tenant, fmt.Sprintf("%s/cases/%s/gen-%s.txt", tenant, caseID, name), buf.Bytes())
	if err != nil {
		http.Error(w, `{"error":"seal failed"}`, http.StatusBadGateway)
		return
	}
	key := fmt.Sprintf("%s/cases/%s/gen-%s.enc", tenant, caseID, name)
	_, err = s.docs.mc.PutObject(r.Context(), docBucket, key, bytes.NewReader(ct), int64(len(ct)),
		minio.PutObjectOptions{ContentType: "application/octet-stream"})
	if err != nil {
		http.Error(w, `{"error":"store failed"}`, http.StatusBadGateway)
		return
	}
	docID := newUUID()
	_, _ = s.db.Exec(r.Context(), fmt.Sprintf(`
		INSERT INTO tenant_%s.documents (id, case_id, object_key, size_bytes, content_type, sealed, uploaded_by, version)
		VALUES ($1,$2,$3,$4,'text/plain',false,'system',1)`, sanitizeTenant(tenant)),
		docID, caseID, key, buf.Len())
	writeJSON(w, http.StatusCreated, map[string]string{"doc_id": docID, "object_key": key})
}

// ---- Email channel: inbound email -> lead/activity (email-to-case) ------------------------

// emailInbound receives provider webhooks (Mailgun/SES-style), token-authenticated.
func (s *server) emailInbound(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("X-Email-Token") == "" {
		http.Error(w, `{"error":"unauthorized"}`, http.StatusUnauthorized)
		return
	}
	var in struct {
		Tenant  string `json:"tenant"`
		From    string `json:"from"`
		Subject string `json:"subject"`
		Body    string `json:"body"`
		CaseRef string `json:"case_number"` // optional: "Re: CMS-TX-2026-00001"
	}
	_ = json.NewDecoder(r.Body).Decode(&in)
	tenant := strings.ToLower(in.Tenant)
	if len(tenant) != 2 {
		http.Error(w, `{"error":"tenant required"}`, http.StatusBadRequest)
		return
	}
	if in.CaseRef != "" {
		// email-to-case: append to the case timeline
		var caseID string
		if err := s.db.QueryRow(r.Context(), fmt.Sprintf(
			`SELECT id FROM tenant_%s.cases WHERE case_number=$1`, sanitizeTenant(tenant)),
			in.CaseRef).Scan(&caseID); err == nil {
			_, _ = s.db.Exec(r.Context(), `
				INSERT INTO public.case_activities (tenant, case_id, type, body)
				VALUES ($1,$2,'EMAIL',$3)`, tenant, caseID,
				fmt.Sprintf("From %s — %s\n%s", in.From, in.Subject, truncate(in.Body, 400)))
			writeJSON(w, http.StatusAccepted, map[string]string{"status": "attached", "case_id": caseID})
			return
		}
	}
	// otherwise: lead capture (email-to-lead)
	_, _ = s.db.Exec(r.Context(), `
		INSERT INTO public.leads (tenant, source, name, email, summary)
		VALUES ($1,'EMAIL',$2,$3,$4)`, tenant, in.From, in.From,
		fmt.Sprintf("%s\n%s", in.Subject, truncate(in.Body, 400)))
	writeJSON(w, http.StatusAccepted, map[string]string{"status": "lead_created"})
}

func truncate(s string, n int) string {
	if len(s) > n {
		return s[:n] + "…"
	}
	return s
}
