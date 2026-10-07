package main

// Worker morning briefing — conversation-first migration, step 2 (NG).
//
// The Assistant home surface opens with "where does my day stand" instead of
// a case picker. This endpoint gathers a WORKER-SCOPED digest from the same
// tables the screens use (my assigned cases by triage lane + SLA risk, fresh
// document analyses, checks needing review, the QA gate, tasks due) and lets
// the local model narrate it in one bounded call.
//
// Invariants carried over from the copilot work:
//  1. GROUNDED OR SILENT — the digest is assembled in SQL/Go, never by the
//     model. The model only narrates the JSON it is given; when the model is
//     unreachable the structured digest renders as plain text (fallback
//     narration), so the briefing NEVER fails because Ollama is down.
//  2. ONE BOUNDED CALL, temperature 0, no tools, no loops.
//  3. READ-ONLY — the briefing touches nothing; every actionable item links
//     to the same screens/gates a screen-first worker would use.
//
// Delivery is on-demand (computed when the worker opens the Assistant) rather
// than a scheduled push: zero new moving parts, and the digest is always
// fresh. A Temporal cron emitting the same payload as a notification is a
// later option — the digest gather is deliberately a pure function of tenant
// + worker so a scheduler can reuse it unchanged.
//
// NG divergence from the original repo: SLA is EXACT business days — computed
// Go-side via slaDaysRemaining(openedAt, tenant holidays), never the SQL
// calendar-day approximation.

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"
)

// briefingSLARiskDays bounds the "at risk" slice: anything whose SLA clock
// has this many business days or fewer left is called out by name.
const briefingSLARiskDays = 5

// briefingMaxCases / briefingMaxTasks bound prompt size; the full lists are
// on the screens, the briefing surfaces what needs eyes first.
const (
	briefingMaxCases = 8
	briefingMaxTasks = 5
)

type briefingCase struct {
	CaseID     string `json:"case_id"`
	CaseNumber string `json:"case_number"`
	Status     string `json:"status"`
	Lane       string `json:"lane"`
	SLADays    int    `json:"sla_days_remaining"` // exact business days (NG)
}

type briefingTask struct {
	Subject string `json:"subject"`
	CaseID  string `json:"case_id,omitempty"`
	Due     string `json:"due"`
}

type briefingDigest struct {
	Worker       string         `json:"worker"`
	GeneratedAt  time.Time      `json:"generated_at"`
	MyOpenCases  int            `json:"my_open_cases"`
	Cases        []briefingCase `json:"cases"`       // most SLA-pressured first
	AtRiskSLA    []briefingCase `json:"at_risk_sla"` // subset of Cases with SLA <= briefingSLARiskDays
	NewDocs24h   int            `json:"new_docs_24h"`
	ChecksReview int            `json:"checks_in_review"`
	PendingQA    int            `json:"pending_qa"`
	TasksDue     []briefingTask `json:"tasks_due"` // overdue or due within 2 days
}

// briefingNarrationSystem is the system half of the one bounded narration
// call; the digest JSON travels as the user message. The model sees ONLY the
// digest — same grounded-or-silent contract as the per-case chat.
const briefingNarrationSystem = `You are the morning briefing voice inside a federal No Surprises Act IDRE case platform.
A case worker just opened their assistant. Narrate their day from the digest JSON in the user message.

HARD RULES:
- Use ONLY the JSON. Never invent cases, amounts, deadlines, or counts.
- Under 120 words. Lead with what is most urgent (SLA risk, then overdue tasks, then the QA gate).
- Mention case numbers when calling out specific cases. Speak in plain sentences, no markdown, no bullet lists — this is read in a chat bubble.
- If everything is quiet, say so plainly and briefly.
- You are advisory: you cannot act on any of this; direct the worker to the relevant screen or action chip only by naming it.`

func briefingNarrationUser(d briefingDigest) string {
	b, _ := json.Marshal(d)
	return "DIGEST (JSON, platform-verified):\n" + string(b)
}

// fallbackNarration renders the digest as plain sentences when the model is
// unreachable or unconfigured. The briefing must never be an error toast.
func fallbackNarration(d briefingDigest) string {
	var b strings.Builder
	if d.MyOpenCases == 0 && d.PendingQA == 0 && d.ChecksReview == 0 && len(d.TasksDue) == 0 {
		return "Your queue is clear — no assigned open cases, nothing at the QA gate, no tasks due."
	}
	fmt.Fprintf(&b, "You have %d open case(s) assigned to you.", d.MyOpenCases)
	if len(d.AtRiskSLA) > 0 {
		nums := make([]string, 0, len(d.AtRiskSLA))
		for _, c := range d.AtRiskSLA {
			nums = append(nums, fmt.Sprintf("%s (%d business day(s) left)", c.CaseNumber, c.SLADays))
		}
		fmt.Fprintf(&b, " SLA risk: %s.", strings.Join(nums, ", "))
	}
	if len(d.TasksDue) > 0 {
		fmt.Fprintf(&b, " %d task(s) overdue or due within 2 days.", len(d.TasksDue))
	}
	if d.PendingQA > 0 {
		fmt.Fprintf(&b, " %d item(s) waiting at the QA gate.", d.PendingQA)
	}
	if d.ChecksReview > 0 {
		fmt.Fprintf(&b, " %d check(s) in REVIEW on the finance screen.", d.ChecksReview)
	}
	if d.NewDocs24h > 0 {
		fmt.Fprintf(&b, " %d document(s) analyzed in the last 24 hours.", d.NewDocs24h)
	}
	return b.String()
}

// gatherBriefingDigest is a pure read of tenant + worker scope. Kept separate
// from the handler so a future scheduled delivery reuses it unchanged.
func (s *server) gatherBriefingDigest(ctx context.Context, tenant, worker string) (briefingDigest, error) {
	d := briefingDigest{
		Worker:      worker,
		GeneratedAt: time.Now().UTC(),
		Cases:       []briefingCase{},
		AtRiskSLA:   []briefingCase{},
		TasksDue:    []briefingTask{},
	}
	tbl := sanitizeTenant(tenant)
	holidays := s.holidaysFor(ctx, tenant)

	// My assigned open cases. SLA is exact business days, computed Go-side
	// (NG); sort by it after the scan. Lane is the manifest lane SQL — same
	// expression as the case list, so the briefing can never disagree with
	// the screen.
	rows, err := s.db.Query(ctx, fmt.Sprintf(`
		SELECT c.id::text, c.case_number, c.status, c.opened_at, %s AS lane
		FROM tenant_%s.cases c
		LEFT JOIN (SELECT DISTINCT case_id FROM public.escalations WHERE tenant=$1) esc
		  ON esc.case_id = c.id
		WHERE c.assigned_to=$2
		  AND c.status NOT IN ('CLOSED','DETERMINED','PAID','WITHDRAWN')
		ORDER BY c.opened_at ASC LIMIT %d`, triageLaneSQL, tbl, briefingMaxCases*4), tenant, worker)
	if err != nil {
		return d, err
	}
	defer rows.Close()
	var all []briefingCase
	for rows.Next() {
		var c briefingCase
		var opened time.Time
		if rows.Scan(&c.CaseID, &c.CaseNumber, &c.Status, &opened, &c.Lane) != nil {
			continue
		}
		c.SLADays = slaDaysRemaining(opened, holidays)
		all = append(all, c)
	}
	rows.Close()
	for i := 0; i < len(all); i++ { // insertion sort by SLA asc — tiny list
		for j := i + 1; j < len(all); j++ {
			if all[j].SLADays < all[i].SLADays {
				all[i], all[j] = all[j], all[i]
			}
		}
	}
	d.MyOpenCases = len(all)
	for _, c := range all {
		if len(d.Cases) >= briefingMaxCases {
			break
		}
		d.Cases = append(d.Cases, c)
		if c.SLADays <= briefingSLARiskDays {
			d.AtRiskSLA = append(d.AtRiskSLA, c)
		}
	}

	one := func(dst *int, q string, args ...any) {
		if s.db.QueryRow(ctx, q, args...).Scan(dst) != nil {
			*dst = 0
		}
	}
	one(&d.NewDocs24h, `SELECT count(*) FROM public.doc_analysis
		WHERE tenant=$1 AND case_id IS NOT NULL AND analyzed_at > now() - interval '24 hours'`, tenant)
	one(&d.ChecksReview, `SELECT count(*) FROM public.checks WHERE tenant=$1 AND status='REVIEW'`, tenant)
	one(&d.PendingQA, `SELECT count(*) FROM public.qa_reviews WHERE tenant=$1 AND status='PENDING'`, tenant)

	// Tasks overdue or due within 2 days — the worker's own, or unassigned
	// (an unassigned overdue task is everyone's problem in a small shop).
	trows, err := s.db.Query(ctx, `
		SELECT subject, coalesce(case_id,''), to_char(due_date,'YYYY-MM-DD')
		FROM public.tasks
		WHERE tenant=$1 AND status='OPEN' AND due_date IS NOT NULL
		  AND due_date <= CURRENT_DATE + 2
		  AND (assignee=$2 OR created_by=$2 OR coalesce(assignee,'')='')
		ORDER BY due_date ASC LIMIT $3`, tenant, worker, briefingMaxTasks)
	if err == nil {
		defer trows.Close()
		for trows.Next() {
			var t briefingTask
			if trows.Scan(&t.Subject, &t.CaseID, &t.Due) == nil {
				d.TasksDue = append(d.TasksDue, t)
			}
		}
	}
	return d, nil
}

// briefing handles GET /assistant/briefing — the worker-scoped digest plus a
// one-call narration (with a structured fallback when the model is down).
func (s *server) briefing(w http.ResponseWriter, r *http.Request) {
	tenant := r.Context().Value(ctxTenant{}).(string)
	p := r.Context().Value(ctxPrincipal{}).(principal)
	if !hasAnyRole(p, copilotRoles...) {
		http.Error(w, `{"error":"forbidden: requires case staff role"}`, http.StatusForbidden)
		return
	}
	d, err := s.gatherBriefingDigest(r.Context(), tenant, p.Subject)
	if err != nil {
		http.Error(w, `{"error":"db (tenant migrated?)"}`, http.StatusInternalServerError)
		return
	}
	narration, model := "", ""
	if s.cfg.CopilotEndpoint != "" {
		narration, err = ollamaChat(r.Context(), s.cfg.CopilotEndpoint, s.cfg.CopilotModel,
			briefingNarrationSystem, briefingNarrationUser(d), 400)
		if err != nil {
			narration = "" // fall through to the structured fallback
		} else {
			model = s.cfg.CopilotModel
		}
	}
	if narration == "" {
		narration = fallbackNarration(d)
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"digest":    d,
		"narration": narration,
		"model":     model, // empty = fallback narration (client shows a small badge)
	})
}
