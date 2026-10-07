package main

// Agentic copilot — Phase 1: grounded, READ-ONLY case briefs.
//
// Value model (from the workload analysis): a case manager's expensive
// minutes go to RECONSTRUCTING context — eligibility posture, what the
// documents actually say, which numbers conflict. The copilot eliminates
// the reconstruction: on demand it assembles the facts the platform has
// ALREADY VERIFIED (auto-eligibility core, trusted doc-intel extractions,
// triage lane, SLA) and has the local ollama render them as a decision-
// ready brief. The worker confirms instead of assembling.
//
// Hard boundaries (deliberate, non-negotiable):
//   - ADVISORY ONLY. The brief never touches case state, never decides,
//   	is labeled DRAFT in the activity stream, and its generation is
//   	audit-logged. Payment determinations stay with the human reviewer.
//   - GROUNDED OR SILENT. The prompt carries only platform-verified facts
//   	and instructs the model to write "not in record" for anything absent.
//   	Low-confidence doc-intel fields are explicitly flagged as such.
//   - ONE BOUNDED CALL. The endpoint is the shared LOCAL ollama (known OOM
//   	history) — a single temperature-0 call with a token cap, never an
//   	agent loop. LangGraph-style multi-step loops were evaluated and
//   	rejected: Temporal already owns durable orchestration on this
//   	platform, and per-step LLM loops are the wrong economics against a
//   	single shared 7B endpoint.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	chi "github.com/go-chi/chi/v5"
)

// copilotDocFact is one analyzed document's trusted view for the brief.
type copilotDocFact struct {
	DocType       string         `json:"doc_type"`
	Status        string         `json:"status"`
	TrustedFields map[string]any `json:"trusted_fields,omitempty"`
	LowConfidence []string       `json:"low_confidence_fields,omitempty"`
	FindingIssues []string       `json:"findings,omitempty"`
}

// copilotFacts is everything the brief is allowed to know. Every element is
// platform-derived; nothing is model-generated upstream of the prompt.
type copilotFacts struct {
	CaseNumber          string           `json:"case_number"`
	InternalStatus      string           `json:"internal_status"`
	AgencyStatus        string           `json:"agency_status"`
	DisputedAmountCents int64            `json:"disputed_amount_cents"`
	QPACents            int64            `json:"qpa_cents"`
	OpenedAt            string           `json:"opened_at"`
	TriageLane          string           `json:"triage_lane"`
	SLADaysRemaining    int              `json:"sla_days_remaining"`
	EligibilityResult   string           `json:"eligibility_result,omitempty"`
	EligibilityReason   string           `json:"eligibility_reason,omitempty"`
	EligibilityMissing  []string         `json:"eligibility_missing_inputs,omitempty"`
	Documents           []copilotDocFact `json:"documents,omitempty"`
	RecentActivity      []string         `json:"recent_activity,omitempty"`
}

// copilotPrompt renders the system+user pair. The grounding contract is
// stated as instructions AND enforced structurally: the user message is a
// JSON fact sheet, so the model has no reason (and no permission) to reach
// outside it.
func copilotPrompt(f copilotFacts) (system, user string) {
	system = `You are a case-review assistant inside a No Surprises Act IDRE case-management platform.
Write a decision-preparation brief for the human case worker, in exactly these sections:
1. ELIGIBILITY BRIEF — posture, rule basis, and (if listed) exactly which inputs are missing for auto-adjudication.
2. EVIDENCE COMPARISON — what the analyzed documents establish, QPA vs disputed amount when both present, and any conflicts between documents.
3. UNCERTAINTIES — low-confidence fields, findings, and anything a human must verify before relying on this brief.
Rules: use ONLY the JSON facts below. If a fact is absent write "not in record". NEVER invent identifiers, amounts, dates, or parties. Cite the source field for every number (e.g. "per doc eob trusted field allowed_amount_usd"). Close with one line: "DRAFT — advisory only; not a determination."`
	facts, _ := json.MarshalIndent(f, "", "  ")
	user = "CASE FACT SHEET (platform-verified):\n" + string(facts)
	return system, user
}

// ollamaChat is one bounded OpenAI-compatible chat call against the LOCAL
// ollama. Extracted pure for testing (httptest fake).
func ollamaChat(ctx context.Context, endpoint, model, system, user string, maxTokens int) (string, error) {
	body, _ := json.Marshal(map[string]any{
		"model": model,
		"messages": []map[string]string{
			{"role": "system", "content": system},
			{"role": "user", "content": user},
		},
		"temperature": 0,
		"max_tokens":  maxTokens,
		"stream":      false,
	})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		strings.TrimRight(endpoint, "/")+"/chat/completions", bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	client := &http.Client{Timeout: 180 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	var out struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
		Error *struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return "", fmt.Errorf("ollama response not parseable: %w", err)
	}
	if out.Error != nil {
		return "", fmt.Errorf("ollama: %s", out.Error.Message)
	}
	if len(out.Choices) == 0 || strings.TrimSpace(out.Choices[0].Message.Content) == "" {
		return "", fmt.Errorf("ollama returned no content (HTTP %d)", resp.StatusCode)
	}
	return strings.TrimSpace(out.Choices[0].Message.Content), nil
}

// gatherCopilotFacts assembles the fact sheet from platform records.
func (s *server) gatherCopilotFacts(r *http.Request, tenant, caseID string) (copilotFacts, error) {
	tbl := sanitizeTenant(tenant)
	var f copilotFacts
	var batchID, dupOf *string
	var opened time.Time
	err := s.db.QueryRow(r.Context(), fmt.Sprintf(
		`SELECT case_number, coalesce(internal_status,''), coalesce(agency_status,''),
		        coalesce(disputed_amount_cents,0), coalesce(qpa_cents,0),
		        opened_at, batch_id::text, duplicate_of::text
		 FROM tenant_%s.cases WHERE id=$1`, tbl), caseID,
	).Scan(&f.CaseNumber, &f.InternalStatus, &f.AgencyStatus,
		&f.DisputedAmountCents, &f.QPACents, &opened, &batchID, &dupOf)
	if err != nil {
		return f, err
	}
	f.OpenedAt = opened.Format("2006-01-02")

	// Lane: same signal logic as listCases' triageLaneSQL, computed Go-side
	// for one case. Escalation history escalates to COMPLEX.
	hasEscalation := false
	_ = s.db.QueryRow(r.Context(), fmt.Sprintf(
		`SELECT EXISTS(SELECT 1 FROM tenant_%s.escalations WHERE case_id=$1)`, tbl), caseID,
	).Scan(&hasEscalation)
	switch {
	case batchID != nil || dupOf != nil || f.DisputedAmountCents >= 10000000 || hasEscalation:
		f.TriageLane = "COMPLEX"
	case f.QPACents > 0 && f.DisputedAmountCents > 0:
		f.TriageLane = "AUTO_REVIEW"
	default:
		f.TriageLane = "STANDARD"
	}
	// SLA is exact here: 30 statutory business days against the tenant's
	// holiday calendar (calendar.go), matching listCases — the original
	// platform uses its slaDaysSQL calendar approximation instead.
	f.SLADaysRemaining = slaDaysRemaining(opened, s.holidaysFor(r.Context(), tenant))

	// Eligibility posture from the SAME pure core as auto/manual review.
	if cfg := s.loadProgram(r, tenant); cfg != nil {
		if in, derr := s.deriveEligibilityInput(r, tenant, caseID); derr == nil {
			if missing := missingEligibilityInputs(cfg, in); len(missing) > 0 {
				f.EligibilityResult = "NEEDS_HUMAN"
				f.EligibilityMissing = missing
			} else {
				f.EligibilityResult, f.EligibilityReason, _ = evalEligibility(cfg, in)
			}
		}
	}

	// Trusted document views: latest analysis per doc_type. Only the
	// verified normalized fields are offered as fact; low-confidence fields
	// are named so the brief can flag them instead of quoting them.
	rows, err := s.db.Query(r.Context(), `
		SELECT DISTINCT ON (coalesce(result->>'doc_type','unknown'))
		       coalesce(result->>'doc_type','unknown'), coalesce(status,''), result
		FROM public.doc_analysis
		WHERE tenant=$1 AND case_id=$2
		ORDER BY coalesce(result->>'doc_type','unknown'), created_at DESC
		LIMIT 12`, tenant, caseID)
	if err == nil {
		defer rows.Close()
		for rows.Next() {
			var d copilotDocFact
			var raw []byte
			if rows.Scan(&d.DocType, &d.Status, &raw) != nil {
				continue
			}
			var res map[string]any
			if json.Unmarshal(raw, &res) == nil {
				if norm, ok := res["normalized"].(map[string]any); ok && len(norm) > 0 {
					d.TrustedFields = norm
				}
				if conf, ok := res["field_confidence"].(map[string]any); ok {
					for k, v := range conf {
						if v == "low" {
							d.LowConfidence = append(d.LowConfidence, k)
						}
					}
				}
				if findings, ok := res["findings"].([]any); ok {
					for _, fnd := range findings {
						if m, ok := fnd.(map[string]any); ok {
							if iss, _ := m["issue"].(string); iss != "" {
								d.FindingIssues = append(d.FindingIssues, iss)
							}
						}
					}
				}
			}
			f.Documents = append(f.Documents, d)
		}
	}

	actRows, err := s.db.Query(r.Context(), `
		SELECT type || ' — ' || left(body, 300)
		FROM public.case_activities
		WHERE tenant=$1 AND case_id=$2 AND type <> 'COPILOT_BRIEF'
		ORDER BY created_at DESC LIMIT 8`, tenant, caseID)
	if err == nil {
		defer actRows.Close()
		for actRows.Next() {
			var line string
			if actRows.Scan(&line) == nil {
				f.RecentActivity = append(f.RecentActivity, line)
			}
		}
	}
	return f, nil
}

var copilotRoles = []string{"CASE_MANAGER", "ATTORNEY", "FEDERAL_ADMIN", "PLATFORM_ADMIN", serviceRole}

// copilotBrief handles POST /cases/{caseId}/copilot/brief — generate and
// persist a grounded advisory brief. Generation is audit-logged; the brief
// itself lands in case_activities labeled DRAFT, visible in the stream.
func (s *server) copilotBrief(w http.ResponseWriter, r *http.Request) {
	p := r.Context().Value(ctxPrincipal{}).(principal)
	if !hasAnyRole(p, copilotRoles...) {
		http.Error(w, `{"error":"forbidden"}`, http.StatusForbidden)
		return
	}
	if s.cfg.CopilotEndpoint == "" {
		http.Error(w, `{"error":"copilot disabled (COPILOT_ENDPOINT unset)"}`, http.StatusServiceUnavailable)
		return
	}
	tenant := r.Context().Value(ctxTenant{}).(string)
	id := chi.URLParam(r, "caseId")
	facts, err := s.gatherCopilotFacts(r, tenant, id)
	if err != nil {
		http.Error(w, `{"error":"not found"}`, http.StatusNotFound)
		return
	}
	system, user := copilotPrompt(facts)
	brief, err := ollamaChat(r.Context(), s.cfg.CopilotEndpoint, s.cfg.CopilotModel, system, user, 1200)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]any{
			"error":  "copilot model unreachable — the brief is advisory; the facts below are still authoritative",
			"detail": err.Error(), "facts": facts,
		})
		return
	}
	attribution := fmt.Sprintf(
		"%s\n\n———\nDRAFT — advisory only; not a determination. model=%s generated=%s facts=platform-verified",
		brief, s.cfg.CopilotModel, time.Now().UTC().Format(time.RFC3339))
	s.logActivity(r.Context(), tenant, id, "COPILOT_BRIEF", truncate(attribution, 8000))
	// NG has no public.audit_log (rule_changes is its audit trail); the
	// persisted COPILOT_BRIEF activity above is the durable attribution record.
	writeJSON(w, http.StatusOK, map[string]any{
		"brief": attribution, "facts": facts, "model": s.cfg.CopilotModel,
		"advisory": true,
	})
}

// copilotBriefLatest handles GET /cases/{caseId}/copilot/brief — the most
// recent persisted brief, 404 when none has been generated yet.
func (s *server) copilotBriefLatest(w http.ResponseWriter, r *http.Request) {
	p := r.Context().Value(ctxPrincipal{}).(principal)
	if !hasAnyRole(p, copilotRoles...) {
		http.Error(w, `{"error":"forbidden"}`, http.StatusForbidden)
		return
	}
	tenant := r.Context().Value(ctxTenant{}).(string)
	id := chi.URLParam(r, "caseId")
	var body string
	var at time.Time
	err := s.db.QueryRow(r.Context(), `
		SELECT body, created_at FROM public.case_activities
		WHERE tenant=$1 AND case_id=$2 AND type='COPILOT_BRIEF'
		ORDER BY created_at DESC LIMIT 1`, tenant, id).Scan(&body, &at)
	if err != nil {
		http.Error(w, `{"error":"no brief generated yet"}`, http.StatusNotFound)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"brief": body, "generated_at": at.UTC().Format(time.RFC3339), "advisory": true,
	})
}
