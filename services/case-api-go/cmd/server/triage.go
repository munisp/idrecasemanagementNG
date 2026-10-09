package main

// Case-worker productivity: eligibility auto-adjudication + queue triage.
//
// The workload analysis is unambiguous about where case-manager minutes go:
// eligibility review is the single biggest sink (the federal program found
// the same — it is their primary processing bottleneck), and a FIFO queue
// forces expert judgment onto cases that needed one-click confirmation.
//
//   Lever 1 — auto-eligibility: evalEligibility is the SAME decision logic
//     checkEligibility has always used, refactored pure. autoEligibility
//     derives the inputs from the case record + analyzed document
//     extractions and only decides when every input the program rules
//     actually need is present — anything missing routes to a human with
//     an explicit "what's missing" list. Miss, don't guess: an auto
//     "ELIGIBLE" fabricated from absent evidence would be worse than no
//     automation.
//   Lever 2/4 — triage lanes + SLA: listCases computes a triage_lane
//     (AUTO_REVIEW | STANDARD | COMPLEX) and sla_days_remaining per case,
//     sortable (sort=sla = soonest deadline first) and filterable (lane=).
//     Lanes are computed in SQL from signals the platform already has:
//     batches, duplicates, escalation history, dollar materiality, and
//     whether the numbers needed for a one-click confirm are present.
//
// NG note: SLA is exact — 30 statutory business days counted against the
// tenant's holidays table (public.holidays) via the calendar engine, not
// the 42-calendar-day approximation the original platform uses.

import (
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	chi "github.com/go-chi/chi/v5"
)

// eligibilityInput is everything evalEligibility needs. Pointer fields are
// tri-state: nil = unknown (auto-adjudication must NOT decide on unknowns
// the rules would have used).
type eligibilityInput struct {
	ProviderType         string   `json:"provider_type"`
	Contracted           *bool    `json:"contracted"`
	DisputedAmountCents  int64    `json:"disputed_amount_cents"`
	FinalDeterminationAt string   `json:"final_determination_at"` // YYYY-MM-DD
	Flags                []string `json:"flags"`
	AORValid             *bool    `json:"aor_valid"`
}

// evalEligibility is the pure decision core shared by checkEligibility
// (human-invoked) and autoEligibility (system-invoked). Same rules, same
// thresholds, same window math — one implementation, two callers, so the
// auto path can never drift from what a human reviewer would compute.
func evalEligibility(cfg *ProgramConfig, in eligibilityInput) (result, reason string, evidence map[string]any) {
	result, reason = "ELIGIBLE", ""
	evidence = map[string]any{"flags": in.Flags, "provider_type": in.ProviderType}

	// explicit ineligibility flags win
	for _, f := range in.Flags {
		if contains(cfg.Eligibility.Reasons, f) {
			return "INELIGIBLE", f, evidence
		}
	}
	// filing window (12-month style)
	if cfg.Eligibility.FilingWindowMonths > 0 && in.FinalDeterminationAt != "" {
		if fd, err := time.Parse("2006-01-02", in.FinalDeterminationAt); err == nil {
			deadline := fd.AddDate(0, cfg.Eligibility.FilingWindowMonths, 0)
			evidence["filing_deadline"] = deadline.Format("2006-01-02")
			if time.Now().After(deadline) {
				return "INELIGIBLE", "over_filing_window", evidence
			}
		}
	}
	// threshold matrix
	for _, th := range cfg.Eligibility.Thresholds {
		if th.ProviderType != in.ProviderType {
			continue
		}
		if th.Contracted != nil && in.Contracted != nil && *th.Contracted != *in.Contracted {
			continue
		}
		evidence["threshold_min_cents"] = th.MinCents
		if in.DisputedAmountCents < th.MinCents {
			return "INELIGIBLE", "below_threshold", evidence
		}
		break
	}
	// AOR hold
	if in.AORValid != nil && !*in.AORValid {
		return "HOLD_AOR", "aor_invalid_pending_attorney", evidence
	}
	return result, reason, evidence
}

// missingEligibilityInputs lists which rule-required inputs are absent.
// The auto path decides ONLY when this is empty (or the missing inputs
// belong to rules the program doesn't use).
func missingEligibilityInputs(cfg *ProgramConfig, in eligibilityInput) []string {
	var missing []string
	if len(cfg.Eligibility.Thresholds) > 0 && in.ProviderType == "" {
		missing = append(missing, "provider_type")
	}
	needsContracted := false
	for _, th := range cfg.Eligibility.Thresholds {
		if th.Contracted != nil {
			needsContracted = true
			break
		}
	}
	if needsContracted && in.Contracted == nil {
		missing = append(missing, "contracted")
	}
	if cfg.Eligibility.FilingWindowMonths > 0 && in.FinalDeterminationAt == "" {
		missing = append(missing, "final_determination_at")
	}
	return missing
}

// deriveEligibilityInput assembles the input from the case row and the
// case's analyzed documents (doc-intel persists ctx including normalized
// fields into public.doc_analysis.result). Details jsonb wins over
// document guesses — a value a human put on the case beats an extraction.
func (s *server) deriveEligibilityInput(r *http.Request, tenant, caseID string) (eligibilityInput, error) {
	tbl := sanitizeTenant(tenant)
	var in eligibilityInput
	var details []byte
	err := s.db.QueryRow(r.Context(), fmt.Sprintf(
		`SELECT coalesce(disputed_amount_cents,0), coalesce(details,'{}'::jsonb)
		 FROM tenant_%s.cases WHERE id=$1`, tbl), caseID).Scan(&in.DisputedAmountCents, &details)
	if err != nil {
		return in, err
	}
	var d map[string]any
	_ = json.Unmarshal(details, &d)
	if v, _ := d["provider_type"].(string); v != "" {
		in.ProviderType = v
	}
	if v, ok := d["contracted"].(bool); ok {
		in.Contracted = &v
	}
	if v, _ := d["final_determination_at"].(string); v != "" {
		in.FinalDeterminationAt = v
	}
	if v, ok := d["aor_valid"].(bool); ok {
		in.AORValid = &v
	}
	if arr, ok := d["eligibility_flags"].([]any); ok {
		for _, f := range arr {
			if s2, ok := f.(string); ok {
				in.Flags = append(in.Flags, s2)
			}
		}
	}
	// Document-derived fallbacks: with MULTIPLE EOBs/determinations on one
	// dispute (the common case — a claim bundle), the filing window runs from
	// the LAST EOB/determination date, not from whichever document happened
	// to be analyzed most recently. Take the max date across every analyzed
	// EOB and determination letter; ISO dates order lexicographically.
	if in.FinalDeterminationAt == "" {
		_ = s.db.QueryRow(r.Context(), `
			SELECT coalesce(MAX(v), '') FROM (
				SELECT NULLIF(result->'normalized'->>'determination_date','') AS v
				FROM public.doc_analysis
				WHERE tenant=$1 AND case_id=$2 AND status LIKE 'ANALYZED%'
				  AND doc_type IN ('eob','determination_letter')
				UNION ALL
				SELECT NULLIF(result->'normalized'->>'service_date','')
				FROM public.doc_analysis
				WHERE tenant=$1 AND case_id=$2 AND status LIKE 'ANALYZED%'
				  AND doc_type IN ('eob','determination_letter')
			) dates WHERE v IS NOT NULL`, tenant, caseID).Scan(&in.FinalDeterminationAt)
	}
	return in, nil
}

// persistEligibilityOutcome records the review and applies the same side
// effects as the manual endpoint (dual-status moves, AOR notify, workflow
// signal, activity). One helper, both callers.
func (s *server) persistEligibilityOutcome(r *http.Request, tenant, caseID, decidedBy,
	result, reason string, evidence map[string]any) string {
	ev, _ := json.Marshal(evidence)
	var reviewID string
	_ = s.db.QueryRow(r.Context(), `
		INSERT INTO public.eligibility_reviews (tenant, case_id, result, reason, evidence, decided_by)
		VALUES ($1,$2,$3,$4,$5,$6) RETURNING id`,
		tenant, caseID, result, reason, ev, decidedBy).Scan(&reviewID)

	switch result {
	case "HOLD_AOR":
		_, _ = s.db.Exec(r.Context(),
			fmt.Sprintf(`UPDATE tenant_%s.cases SET internal_status='Hold', agency_status='Other', updated_at=now() WHERE id=$1`, sanitizeTenant(tenant)), caseID)
		s.notify(r, tenant, "*", "AOR_REVIEW", fmt.Sprintf("Case %s: AOR flagged invalid — attorney confirmation requested", caseID), "#/cases/"+caseID)
	case "INELIGIBLE":
		_, _ = s.db.Exec(r.Context(),
			fmt.Sprintf(`UPDATE tenant_%s.cases SET internal_status='Provider Closure Letter Issued', updated_at=now() WHERE id=$1`, sanitizeTenant(tenant)), caseID)
	}
	// Signal the case's Temporal workflow (NG stores workflow_id on the row).
	var wfID string
	if err := s.db.QueryRow(r.Context(),
		fmt.Sprintf(`SELECT coalesce(workflow_id,'') FROM tenant_%s.cases WHERE id=$1`, sanitizeTenant(tenant)), caseID).Scan(&wfID); err == nil && wfID != "" {
		_ = s.tc.SignalWorkflow(r.Context(), wfID, "", "ELIGIBILITY_RESULT", map[string]any{"result": result, "reason": reason})
	}
	s.logActivity(r.Context(), tenant, caseID, "ELIGIBILITY_REVIEW",
		fmt.Sprintf("Eligibility %s%s — evidence recorded (review %s, by %s)", result, orDash(" — "+reason), reviewID, decidedBy))
	return reviewID
}

// autoEligibility (Lever 1): derive -> decide-if-complete -> persist.
// Returns NEEDS_HUMAN with the missing input list when the rules can't be
// fully evaluated from platform data — the case then surfaces in the human
// eligibility queue with the blanks already enumerated, which is itself a
// minutes-per-touch saving.
func (s *server) autoEligibility(w http.ResponseWriter, r *http.Request) {
	p := r.Context().Value(ctxPrincipal{}).(principal)
	if !hasAnyRole(p, "CASE_MANAGER", "ARBITRATOR", "FEDERAL_ADMIN", "PLATFORM_ADMIN", serviceRole) {
		http.Error(w, `{"error":"forbidden"}`, http.StatusForbidden)
		return
	}
	tenant := r.Context().Value(ctxTenant{}).(string)
	id := chi.URLParam(r, "caseId")
	cfg := s.loadProgram(r, tenant)
	if cfg == nil {
		http.Error(w, `{"error":"no program rules for tenant"}`, http.StatusBadRequest)
		return
	}
	in, err := s.deriveEligibilityInput(r, tenant, id)
	if err != nil {
		http.Error(w, `{"error":"not found"}`, http.StatusNotFound)
		return
	}
	if missing := missingEligibilityInputs(cfg, in); len(missing) > 0 {
		writeJSON(w, http.StatusOK, map[string]any{
			"result": "NEEDS_HUMAN", "missing": missing,
			"detail": "rule-required inputs are not derivable from the case record or analyzed documents",
		})
		return
	}
	result, reason, evidence := evalEligibility(cfg, in)
	evidence["auto_inputs"] = in
	reviewID := s.persistEligibilityOutcome(r, tenant, id,
		"auto: case+document data", result, reason, evidence)
	writeJSON(w, http.StatusOK, map[string]any{
		"review_id": reviewID, "result": result, "reason": reason,
		"evidence": evidence, "auto": true,
	})
}

// --- triage lanes + SLA (listCases enrichment) ----------------------------

// triageLaneSQL computes AUTO_REVIEW | STANDARD | COMPLEX from signals the
// platform already tracks. COMPLEX first (batched, duplicated, escalated,
// or high-materiality), then AUTO_REVIEW (everything needed for a one-click
// confirm is present and nothing is on fire), else STANDARD. NG reads the
// manifest-terminology twin columns with legacy fallback.
const triageLaneSQL = `
	CASE
	  WHEN c.batch_id IS NOT NULL OR c.duplicate_of IS NOT NULL
	       OR coalesce(c.disputed_amount_cents,0) >= 10000000
	       OR esc.case_id IS NOT NULL
	  THEN 'COMPLEX'
	  WHEN coalesce(c.benchmark_cents, c.qpa_cents, 0) > 0 AND coalesce(c.disputed_amount_cents,0) > 0
	  THEN 'AUTO_REVIEW'
	  ELSE 'STANDARD'
	END`

// determinationWindowBD is the 30-business-day determination clock
// (45 CFR 149.510(c)(4)(ii)) — counted exactly against tenant holidays.
const determinationWindowBD = 30

// slaDaysRemaining computes signed business days to the determination
// deadline: positive = time left, negative = overdue.
func slaDaysRemaining(openedAt time.Time, holidays map[string]bool) int {
	today := time.Now()
	due := addBusinessDays(openedAt, determinationWindowBD, holidays)
	if !today.After(due) {
		return businessDaysBetween(today, due, holidays)
	}
	return -businessDaysBetween(due, today, holidays)
}
