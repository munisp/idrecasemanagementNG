package main

// queues_summary.go — the queues-at-a-glance aggregate. One endpoint that
// composes the signals the productivity arc surfaces individually (attention
// ranking, QA queue pressure, documents awaiting findings review, intake
// pipeline next steps, statutory clocks) into a single "start here" summary
// for the dashboard. Every number maps to a queue the user can click through
// to — nothing here computes new judgments, it aggregates the existing
// rule-based ones so the morning scan is one call, not five.
//
// Distinct from /assistant/briefing (briefing.go): that one is the
// worker-scoped LLM-narrated digest; this one is the structured counts-only
// rollup for the dashboard strip — no model involved, so it can never be an
// error toast when Ollama is down.

import (
	"fmt"
	"net/http"
	"time"
)

// stalledCaseDays: an open case with no logged activity for this long is
// stalled. 7 days is quiet-but-not-alarming for federal IDRE work (open
// negotiation runs 30 business days); it's long enough that a healthy case
// would normally see SOME touch — a note, a doc, a status read.
const stalledCaseDays = 7

// queueCounts are the headline numbers for the dashboard strip. Each maps to
// a queue the user can open: attention -> #/dashboard strip links,
// qa_pending -> #/qa, docs_findings_review -> #/doc-review,
// intake_steps_due -> #/intake, statutory_* -> #/cases.
type queueCounts struct {
	AttentionCases     int `json:"attention_cases"`
	QAPending          int `json:"qa_pending"`
	QAPendingAging     int `json:"qa_pending_aging"` // subset PENDING past qaAgingHours
	DocsFindingsReview int `json:"docs_findings_review"`
	DocsFindingsAging  int `json:"docs_findings_aging"` // subset unacknowledged past docFindingsAgingHours
	IntakeStepsDue     int `json:"intake_steps_due"`
	IntakeOverdue      int `json:"intake_overdue"`
	StatutoryOverdue   int `json:"statutory_overdue"`
	StatutoryDue72h    int `json:"statutory_due_72h"`
	StalledCases       int `json:"stalled_cases"` // open, no activity past stalledCaseDays
}

// queuesSummary: GET /queues/summary — aggregated day-start counts for the
// signed-in actor. Visibility follows the same rule as the attention
// ranking: reviewers see the tenant-wide picture, other roles see their own
// docket.
func (s *server) queuesSummary(w http.ResponseWriter, r *http.Request) {
	tenant := r.Context().Value(ctxTenant{}).(string)
	p := r.Context().Value(ctxPrincipal{}).(principal)
	now := time.Now().UTC()

	items, err := s.computeAttention(r.Context(), tenant, p)
	if err != nil {
		http.Error(w, `{"error":"db"}`, http.StatusInternalServerError)
		return
	}

	var c queueCounts
	c.AttentionCases = len(items)

	// QA queue pressure (tenant-wide: a reviewer picking up the queue needs
	// the whole backlog, not just their own drafts), with the aging subset
	// counted separately — a 3-day-old PENDING draft is stuck, not queued.
	_ = s.db.QueryRow(r.Context(), `
		SELECT count(*),
		       count(*) FILTER (WHERE created_at < now() - make_interval(hours => $2))
		FROM public.qa_reviews WHERE tenant=$1 AND status='PENDING'`,
		tenant, qaAgingHours).Scan(&c.QAPending, &c.QAPendingAging)

	// Documents whose analysis surfaced findings nobody has acknowledged yet,
	// with the aging subset (past docFindingsAgingHours) counted separately so
	// a silent backlog shows on the dashboard, not only inside the inbox.
	_ = s.db.QueryRow(r.Context(), `
		SELECT count(*),
		       count(*) FILTER (WHERE analyzed_at < now() - make_interval(hours => $2))
		FROM public.doc_analysis WHERE tenant=$1 AND status='ANALYZED_WITH_FINDINGS'`,
		tenant, docFindingsAgingHours).Scan(&c.DocsFindingsReview, &c.DocsFindingsAging)

	// Intake pipeline: run the rule-based next-step recommender over the open
	// requests; "due" = a step exists, "overdue" = the step's clock expired.
	rows, err := s.db.Query(r.Context(), `
		SELECT status, created_at, packet_complete_at
		FROM public.intake_requests
		WHERE tenant=$1 AND status NOT IN ('CONVERTED','CLOSED_REFUNDED','INELIGIBLE')
		LIMIT 500`, tenant)
	if err == nil {
		for rows.Next() {
			var status string
			var createdAt time.Time
			var packetAt *time.Time
			if rows.Scan(&status, &createdAt, &packetAt) != nil {
				continue
			}
			step := recommendIntakeStep(status, createdAt, packetAt, now)
			if step == nil {
				continue
			}
			c.IntakeStepsDue++
			if step.Overdue {
				c.IntakeOverdue++
			}
		}
		rows.Close()
	}

	// Stalled cases: open cases whose newest activity (or opened_at, if none)
	// is older than stalledCaseDays. Every other count in this rollup is a
	// SIGNAL — pending QA, unread findings, a clock expiring. A stalled case
	// is the opposite: the absence of any signal, which no queue catches.
	// The day count is a rolling window over recent activity, not a breach
	// flag — a case mid-offer-window is legitimately quiet, which is why
	// cases with a live statutory clock are excluded: their deadline already
	// surfaces them elsewhere in this rollup.
	_ = s.db.QueryRow(r.Context(), fmt.Sprintf(`
		SELECT count(*) FROM tenant_%s.cases c
		WHERE c.status NOT LIKE 'CLOSED%%' AND c.status <> 'SETTLED_IN_NEGOTIATION'
		  AND c.offer_window_ends_at IS NULL AND c.open_negotiation_end IS NULL
		  AND coalesce((SELECT max(a.created_at) FROM public.case_activities a
		                WHERE a.tenant=$1 AND a.case_id=c.id::text), c.opened_at)
		      < now() - make_interval(days => $2)`,
		sanitizeTenant(tenant)), tenant, stalledCaseDays).Scan(&c.StalledCases)

	// Statutory clocks across the open docket: nearest of offer window /
	// negotiation end, bucketed overdue vs due inside 72h.
	_ = s.db.QueryRow(r.Context(), fmt.Sprintf(`
		SELECT count(*) FILTER (WHERE d < now()),
		       count(*) FILTER (WHERE d >= now() AND d < now() + interval '72 hours')
		FROM (
			SELECT LEAST(c.offer_window_ends_at, c.open_negotiation_end::timestamptz) AS d
			FROM tenant_%s.cases c
			WHERE c.status NOT LIKE 'CLOSED%%' AND c.status <> 'SETTLED_IN_NEGOTIATION'
		) t WHERE d IS NOT NULL`, sanitizeTenant(tenant)), // no user input — tenant is identifier-sanitized
	).Scan(&c.StatutoryOverdue, &c.StatutoryDue72h)

	top := items
	if len(top) > 3 {
		top = top[:3]
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"counts":      c,
		"attention":   top,
		"computed_at": now,
	})
}
