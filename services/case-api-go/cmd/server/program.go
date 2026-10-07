package main

// Program rules engine — per-tenant configuration that lets each state run its
// own dispute program (clocks, statuses, eligibility thresholds, numbering,
// fees, escalation thresholds, correspondence templates, deliverables) without
// code changes. Federal NSA remains the built-in default when a tenant has no
// program_rules row; FL AHCA CDR is seeded in scripts/program-rules.sql.
//
// Gaps closed here: G1 (clock packs), G2 (eligibility), G5 (dual status),
// G6 (escalation auto-trigger), G11 (numbering).

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
)

type FollowUp struct {
	Day    int    `json:"day"`
	Action string `json:"action"`
}

type ClockRule struct {
	Name     string     `json:"name"`
	Label    string     `json:"label"`
	Basis    string     `json:"basis"` // key into program_dates (received_at, plan_notified_at, …)
	Days     int        `json:"days"`
	DayType  string     `json:"day_type"` // calendar|business
	Cite     string     `json:"cite"`
	Breach   string     `json:"breach"`
	FollowUp []FollowUp `json:"follow_ups"`
}

type ThresholdRule struct {
	ProviderType string `json:"provider_type"`
	Contracted   *bool  `json:"contracted,omitempty"`
	MinCents     int64  `json:"min_cents"`
}

type ProgramConfig struct {
	CaseNumber struct {
		Pattern string `json:"pattern"` // e.g. "FL{yy}-{seq}"
		SeqPad  int    `json:"seq_pad"`
	} `json:"case_number"`
	Statuses struct {
		Internal []string `json:"internal"`
		Agency   []string `json:"agency"`
	} `json:"statuses"`
	Clocks      []ClockRule `json:"clocks"`
	Eligibility struct {
		Thresholds         []ThresholdRule `json:"thresholds"`
		FilingWindowMonths int             `json:"filing_window_months"`
		ProofOfTimeliness  []string        `json:"proof_of_timeliness"`
		Reasons            []string        `json:"ineligibility_reasons"`
	} `json:"eligibility"`
	Fees struct {
		InitialFeeCents int64 `json:"initial_fee_cents"`
		RefundWindowDays int  `json:"refund_window_days"`
		InvoiceDueDays   *int `json:"invoice_due_days"`
	} `json:"fees"`
	Escalation struct {
		AmountTriggerCents int64    `json:"amount_trigger_cents"`
		RouteRole          string   `json:"route_role"`
		Reasons            []string `json:"reasons"`
	} `json:"escalation"`
	NotesStreams []string            `json:"notes_streams"`
	FieldSchema  map[string][]string `json:"field_schema"`
}

// loadProgram returns nil when the tenant runs the built-in federal NSA program.
func (s *server) loadProgram(r *http.Request, tenant string) *ProgramConfig {
	var raw []byte
	err := s.db.QueryRow(r.Context(),
		`SELECT config FROM public.program_rules WHERE tenant=$1`, tenant).Scan(&raw)
	if err != nil {
		return nil
	}
	var cfg ProgramConfig
	if json.Unmarshal(raw, &cfg) != nil {
		return nil
	}
	return &cfg
}

// getProgram exposes the tenant's program config so the portal can render
// program-specific labels, statuses, and rules without hard-coding a state.
func (s *server) getProgram(w http.ResponseWriter, r *http.Request) {
	tenant := r.Context().Value(ctxTenant{}).(string)
	cfg := s.loadProgram(r, tenant)
	if cfg == nil {
		writeJSON(w, http.StatusOK, map[string]any{"program": "FEDERAL_NSA", "config": nil})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"program": "custom", "config": cfg})
}

// programDates reads the case's program-specific date bag.
func (s *server) programDates(r *http.Request, tenant, caseID string) map[string]string {
	var raw []byte
	err := s.db.QueryRow(r.Context(),
		fmt.Sprintf(`SELECT program_dates FROM tenant_%s.cases WHERE id=$1`, sanitizeTenant(tenant)),
		caseID).Scan(&raw)
	if err != nil {
		return map[string]string{}
	}
	out := map[string]string{}
	_ = json.Unmarshal(raw, &out)
	return out
}

// projectProgramClocks runs the generic date-driven clock engine over a case's
// program_dates. Calendar-day and business-day rules both supported.
func projectProgramClocks(rules []ClockRule, dates map[string]string, today time.Time) []ClockView {
	out := []ClockView{}
	for _, rl := range rules {
		raw, ok := dates[rl.Basis]
		if !ok || raw == "" {
			continue // clock not started — basis event hasn't happened
		}
		basis, err := time.Parse("2006-01-02", raw[:minI(10, len(raw))])
		if err != nil {
			continue
		}
		var due time.Time
		var remaining int
		if rl.DayType == "business" {
			due = addBusinessDays(basis, rl.Days, nil)
			remaining = businessDaysBetween(today, due, nil)
		} else {
			due = basis.AddDate(0, 0, rl.Days)
			remaining = int(due.Sub(today).Hours() / 24)
		}
		cv := ClockView{
			Clock: rl.Name, Label: rl.Label, Basis: rl.DayType,
			TotalDays: rl.Days, Remaining: remaining,
			Due: due.Format("2006-01-02"), State: clockState(remaining, rl.Days),
			Cite: rl.Cite,
			BasisNote: fmt.Sprintf("from %s %s", rl.Basis, raw[:minI(10, len(raw))]),
		}
		out = append(out, cv)
	}
	return out
}

// caseProgramClocks: generic program clock projection (used when the tenant
// has a program_rules row; otherwise the federal NSA engine in engines.go runs).
func (s *server) caseProgramClocks(w http.ResponseWriter, r *http.Request, tenant, caseID string, cfg *ProgramConfig) bool {
	dates := s.programDates(r, tenant, caseID)
	// received_at falls back to opened_at (federal field) when unset
	if _, ok := dates["received_at"]; !ok {
		var opened time.Time
		if err := s.db.QueryRow(r.Context(),
			fmt.Sprintf(`SELECT opened_at FROM tenant_%s.cases WHERE id=$1`, sanitizeTenant(tenant)), caseID).
			Scan(&opened); err == nil {
			dates["received_at"] = opened.Format("2006-01-02")
		}
	}
	writeJSON(w, http.StatusOK, projectProgramClocks(cfg.Clocks, dates, time.Now()))
	return true
}

// setProgramDate records a program-specific date (starts/restarts a clock).
func (s *server) setProgramDate(w http.ResponseWriter, r *http.Request) {
	tenant := r.Context().Value(ctxTenant{}).(string)
	id := chi.URLParam(r, "caseId")
	var in struct {
		Key   string `json:"key"`   // e.g. plan_notified_at, estimate_sent_at
		Value string `json:"value"` // YYYY-MM-DD
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil || in.Key == "" || in.Value == "" {
		http.Error(w, `{"error":"key and value required"}`, http.StatusBadRequest)
		return
	}
	if _, err := time.Parse("2006-01-02", in.Value); err != nil {
		http.Error(w, `{"error":"value must be YYYY-MM-DD"}`, http.StatusBadRequest)
		return
	}
	_, err := s.db.Exec(r.Context(),
		fmt.Sprintf(`UPDATE tenant_%s.cases SET program_dates = program_dates || jsonb_build_object($2, $3), updated_at=now() WHERE id=$1`,
			sanitizeTenant(tenant)), id, in.Key, in.Value)
	if err != nil {
		http.Error(w, `{"error":"db"}`, http.StatusInternalServerError)
		return
	}
	s.logActivity(r.Context(), tenant, id, "PROGRAM_DATE", fmt.Sprintf("%s recorded as %s", in.Key, in.Value))
	writeJSON(w, http.StatusOK, map[string]string{"status": "recorded"})
}

// setDualStatus updates internal and/or agency status (G5).
func (s *server) setDualStatus(w http.ResponseWriter, r *http.Request) {
	tenant := r.Context().Value(ctxTenant{}).(string)
	id := chi.URLParam(r, "caseId")
	var in struct {
		Internal string `json:"internal_status"`
		Agency   string `json:"agency_status"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil || (in.Internal == "" && in.Agency == "") {
		http.Error(w, `{"error":"internal_status and/or agency_status required"}`, http.StatusBadRequest)
		return
	}
	if cfg := s.loadProgram(r, tenant); cfg != nil {
		if in.Internal != "" && !contains(cfg.Statuses.Internal, in.Internal) {
			http.Error(w, `{"error":"unknown internal status for this program"}`, http.StatusBadRequest)
			return
		}
		if in.Agency != "" && !contains(cfg.Statuses.Agency, in.Agency) {
			http.Error(w, `{"error":"unknown agency status for this program"}`, http.StatusBadRequest)
			return
		}
	}
	if in.Internal != "" {
		_, _ = s.db.Exec(r.Context(),
			fmt.Sprintf(`UPDATE tenant_%s.cases SET internal_status=$2, updated_at=now() WHERE id=$1`, sanitizeTenant(tenant)), id, in.Internal)
	}
	if in.Agency != "" {
		_, _ = s.db.Exec(r.Context(),
			fmt.Sprintf(`UPDATE tenant_%s.cases SET agency_status=$2, updated_at=now() WHERE id=$1`, sanitizeTenant(tenant)), id, in.Agency)
	}
	p := r.Context().Value(ctxPrincipal{}).(principal)
	s.logActivity(r.Context(), tenant, id, "STATUS_CHANGE",
		fmt.Sprintf("Status updated by %s — internal: %s, agency: %s", p.Subject, orDash(in.Internal), orDash(in.Agency)))
	writeJSON(w, http.StatusOK, map[string]string{"status": "updated"})
}

// checkEligibility (G2): computes the eligibility result from program rules —
// threshold matrix, 12-month filing window, explicit ineligibility flags —
// and stores the review with its evidence. The decision logic itself lives
// in triage.go:evalEligibility, shared with the auto-adjudication path so
// the two can never drift apart.
func (s *server) checkEligibility(w http.ResponseWriter, r *http.Request) {
	tenant := r.Context().Value(ctxTenant{}).(string)
	id := chi.URLParam(r, "caseId")
	cfg := s.loadProgram(r, tenant)
	if cfg == nil {
		http.Error(w, `{"error":"no program rules for tenant"}`, http.StatusBadRequest)
		return
	}
	var in eligibilityInput
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		http.Error(w, `{"error":"bad request"}`, http.StatusBadRequest)
		return
	}

	result, reason, evidence := evalEligibility(cfg, in)
	p := r.Context().Value(ctxPrincipal{}).(principal)
	reviewID := s.persistEligibilityOutcome(r, tenant, id, p.Subject, result, reason, evidence)
	writeJSON(w, http.StatusOK, map[string]any{"review_id": reviewID, "result": result, "reason": reason, "evidence": evidence})
}

// eligibilityHistory lists past reviews for a case — the UI shows how the
// current eligibility state was reached instead of leaving users guessing.
func (s *server) eligibilityHistory(w http.ResponseWriter, r *http.Request) {
	tenant := r.Context().Value(ctxTenant{}).(string)
	id := chi.URLParam(r, "caseId")
	rows, err := s.queryRows(r, `
		SELECT id, result, COALESCE(reason,'') AS reason, evidence, decided_by, created_at
		FROM public.eligibility_reviews WHERE tenant=$1 AND case_id=$2
		ORDER BY created_at DESC LIMIT 10`, tenant, id)
	if err != nil {
		http.Error(w, `{"error":"db"}`, http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"reviews": rows})
}

// nextCaseNumber generates per-program case numbers (G11): "FL26-042" etc.
func (s *server) nextCaseNumber(r *http.Request, tenant string, cfg *ProgramConfig) string {
	var n int
	_ = s.db.QueryRow(r.Context(),
		fmt.Sprintf(`SELECT count(*) FROM tenant_%s.cases`, sanitizeTenant(tenant))).Scan(&n)
	seq := fmt.Sprintf("%0*d", cfg.CaseNumber.SeqPad, n+1)
	yy := time.Now().Format("06")
	out := strings.ReplaceAll(cfg.CaseNumber.Pattern, "{yy}", yy)
	out = strings.ReplaceAll(out, "{seq}", seq)
	return out
}

// escalationTrigger (G6): auto-route large/fraud-suspect cases to the PM.
func (s *server) escalationTrigger(r *http.Request, tenant, caseID string, disputedCents int64) {
	cfg := s.loadProgram(r, tenant)
	if cfg == nil || cfg.Escalation.AmountTriggerCents <= 0 || disputedCents < cfg.Escalation.AmountTriggerCents {
		return
	}
	detail := fmt.Sprintf("Disputed amount $%d.%02d meets the program escalation trigger — routed to %s for independent medical review (F&WA screening)",
		disputedCents/100, disputedCents%100, cfg.Escalation.RouteRole)
	_, _ = s.db.Exec(r.Context(), `
		INSERT INTO public.escalations (tenant, case_id, clock, level, escalated_to, detail)
		VALUES ($1,$2,'PROGRAM_TRIGGER',2,$3,$4)`, tenant, caseID, cfg.Escalation.RouteRole, detail)
	s.logActivity(r.Context(), tenant, caseID, "ESCALATION_AUTO", detail)
	s.notify(r, tenant, "*", "ESCALATION", detail, "#/cases/"+caseID)
}

func contains(xs []string, v string) bool {
	for _, x := range xs {
		if x == v {
			return true
		}
	}
	return false
}

func orDash(s string) string {
	if s == "" {
		return "—"
	}
	return s
}

func minI(a, b int) int {
	if a < b {
		return a
	}
	return b
}
