package main

// nextaction.go — Phase 2: the one-button case page. A rule-based recommender
// picks the single most likely next action for this case and actor; the
// portal renders it as the primary button and emits next_action_accept /
// next_action_override telemetry. Those events are the training data for the
// learned successor (per-role transition statistics → contextual bandit):
// until accept-rate data accumulates, the rules below are the priors, each
// carrying its reason so the UI can show "why this".

import (
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
)

type nextActionInput struct {
	Status          string
	Lock            string
	PendingQA       int
	UnreadDocs      int  // docs analyzed with findings nobody reviewed
	ChecklistDue    int  // open checklist items
	HasOverlaps     bool // clinical-overlap duplicate candidates exist
	OfferWindowOpen bool
	AssignedToMe    bool
	IsReviewer      bool // role can act on QA
	CanDecide       bool // role can issue determinations/offers
}

type nextAction struct {
	ActionID   string  `json:"action_id"`
	Label      string  `json:"label"`
	Href       string  `json:"href"`
	Reason     string  `json:"reason"`
	Confidence float64 `json:"confidence"`
}

// recommendNextAction: pure, priority-ordered. First match wins; later rules
// are progressively weaker priors.
func recommendNextAction(in nextActionInput, caseID string) nextAction {
	qa := func() nextAction {
		return nextAction{"qa_review", "Review pending QA draft", "#/qa",
			"A draft is waiting in the QA gate — approving it unblocks the case clock", 0.9}
	}
	switch {
	case in.Lock == "AWAITING_PAYMENT":
		return nextAction{"send_payment_reminder", "Send payment reminder", "#/cases/" + caseID + "#correspondence",
			"Case is frozen until the initial fee is paid — a reminder is the only forward action", 0.85}
	case in.Lock == "INELIGIBLE":
		return nextAction{"review_eligibility", "Review eligibility determination", "#/cases/" + caseID,
			"Case is locked as ineligible — confirm or appeal before the record finalizes", 0.8}
	case in.PendingQA > 0 && in.IsReviewer:
		return qa()
	case in.UnreadDocs > 0:
		return nextAction{"review_documents", "Review analyzed documents", "#/cases/" + caseID + "#documents",
			"Analysis finished with findings — extracted values need a human check", 0.8}
	case in.HasOverlaps:
		return nextAction{"review_duplicates", "Review possible duplicates", "#/cases/" + caseID + "#relationships",
			"Other cases share this case's clinical fingerprint — link or dismiss before negotiating", 0.75}
	case in.OfferWindowOpen && in.CanDecide:
		return nextAction{"submit_offer", "Submit sealed offer", "#/cases/" + caseID + "/offers",
			"The offer window is open — the deadline is statutory", 0.85}
	case in.ChecklistDue > 0:
		return nextAction{"clear_checklist", "Complete checklist items", "#/cases/" + caseID + "#checklist",
			"Open checklist items gate the next status transition", 0.65}
	case strings.Contains(in.Status, "OPEN_NEGOTIATION"):
		return nextAction{"log_negotiation", "Log negotiation activity", "#/cases/" + caseID + "#correspondence",
			"Open negotiation is running — record outreach so the clock and the record agree", 0.6}
	case in.Status == "INITIATED":
		return nextAction{"triage", "Triage and assign", "#/cases/" + caseID,
			"New dispute — assign an owner and verify eligibility before clocks advance", 0.7}
	default:
		return nextAction{"review_case", "Review case status", "#/cases/" + caseID,
			"No blocking signal detected — review the timeline", 0.4}
	}
}

// caseNextAction: GET /cases/{caseId}/next-action.
func (s *server) caseNextAction(w http.ResponseWriter, r *http.Request) {
	tenant := r.Context().Value(ctxTenant{}).(string)
	p := r.Context().Value(ctxPrincipal{}).(principal)
	caseID := chi.URLParam(r, "caseId")
	tcase := "tenant_" + sanitizeTenant(tenant) + ".cases"
	var status string
	var assignedTo *string
	err := s.db.QueryRow(r.Context(),
		`SELECT status, assigned_to FROM `+tcase+` WHERE id=$1`, caseID).Scan(&status, &assignedTo)
	if err != nil {
		http.Error(w, `{"error":"case not found"}`, http.StatusNotFound)
		return
	}
	in := nextActionInput{Status: status}
	if assignedTo != nil && *assignedTo == p.Subject {
		in.AssignedToMe = true
	}
	in.IsReviewer = hasAnyRole(p, "CASE_MANAGER", "FEDERAL_ADMIN", "PLATFORM_ADMIN", "DOCTOR", "NURSE")
	in.CanDecide = in.IsReviewer
	// Lock mirrors the case-page projection (caselock.go).
	if row, err := s.queryRowMap(r.Context(), `SELECT coalesce(details->>'lock','') AS lock FROM `+tcase+` WHERE id=$1`, caseID); err == nil && row != nil {
		if v, ok := row["lock"].(string); ok {
			in.Lock = v
		}
	}
	_ = s.db.QueryRow(r.Context(),
		`SELECT count(*) FROM public.qa_reviews WHERE tenant=$1 AND case_id=$2 AND status='PENDING'`,
		tenant, caseID).Scan(&in.PendingQA)
	_ = s.db.QueryRow(r.Context(),
		`SELECT count(*) FROM public.doc_analysis WHERE tenant=$1 AND case_id=$2 AND status='ANALYZED_WITH_FINDINGS'`,
		tenant, caseID).Scan(&in.UnreadDocs)
	_ = s.db.QueryRow(r.Context(),
		`SELECT count(*) FROM public.case_checklists WHERE tenant=$1 AND case_id=$2 AND done=false`,
		tenant, caseID).Scan(&in.ChecklistDue)
	_ = s.db.QueryRow(r.Context(), `
		SELECT EXISTS(
			SELECT 1 FROM public.document_clinical d, jsonb_array_elements(d.entities) e
			WHERE d.tenant=$1 AND d.case_id=$2 AND e->>'type' IN ('DRUG','CONDITION','PROCEDURE'))`,
		tenant, caseID).Scan(&in.HasOverlaps)
	if in.HasOverlaps {
		// Only counts as a signal when ANOTHER case shares the fingerprint.
		var any2 bool
		_ = s.db.QueryRow(r.Context(), `
			WITH mine AS (
				SELECT DISTINCT lower(e->>'text') AS term
				FROM public.document_clinical d, jsonb_array_elements(d.entities) e
				WHERE d.tenant=$1 AND d.case_id=$2 AND e->>'type' IN ('DRUG','CONDITION','PROCEDURE'))
			SELECT EXISTS(
				SELECT 1 FROM public.document_clinical d, jsonb_array_elements(d.entities) e
				JOIN mine m ON m.term=lower(e->>'text')
				WHERE d.tenant=$1 AND d.case_id<>$2 AND d.case_id IS NOT NULL)`,
			tenant, caseID).Scan(&any2)
		in.HasOverlaps = any2
	}
	if strings.Contains(status, "OFFER_WINDOW_OPEN") {
		in.OfferWindowOpen = true
	}
	rec := recommendNextAction(in, caseID)
	// Confidence calibration: hand-set priors until this action has been
	// SHOWN enough times in this tenant to trust the observed accept rate.
	// The portal emits next_action_shown on render and next_action_accept
	// on click; below the sample threshold the prior stands and the basis
	// says so — the UI never shows an unearned calibrated number.
	var shown, accepts int
	_ = s.db.QueryRow(r.Context(), `
		SELECT count(*) FILTER (WHERE action='next_action_shown' AND target=$2),
		       count(*) FILTER (WHERE action='next_action_accept' AND target=$2)
		FROM public.interaction_events
		WHERE tenant=$1 AND created_at > now() - interval '90 days'`,
		tenant, rec.ActionID).Scan(&shown, &accepts)
	conf, basis := calibrateConfidence(rec.Confidence, shown, accepts)
	rec.Confidence = conf
	writeJSON(w, http.StatusOK, map[string]any{
		"next_action": rec, "confidence_basis": basis, "computed_at": time.Now().UTC()})
}

// calibrateConfidence: below minShown the prior is returned untouched with
// basis "prior"; at or above it, the Laplace-smoothed observed accept rate
// replaces it with basis "observed". Smoothing keeps a 30/30 streak at 0.97
// rather than a literal 1.0 — one future override shouldn't contradict a
// number the UI already showed.
func calibrateConfidence(prior float64, shown, accepts int) (float64, string) {
	const minShown = 30
	if shown < minShown {
		return prior, "prior"
	}
	return float64(accepts+1) / float64(shown+2), "observed"
}
