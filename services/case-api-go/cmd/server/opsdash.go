// opsdash.go — operations dashboard: live presence, task throughput, workload
// distribution, and the outstanding-work + financial KPIs a manager or
// administrator needs for internal/external stakeholder reporting.
//
// Presence: the portal pings POST /presence/ping every 45s while a user is
// active; a row in public.presence is upserted per (tenant, user_sub).
// "Online" = seen within the last 3 minutes. Presence lives in Postgres (not
// Redis) so it survives cache flushes and is queryable in one round trip with
// the rest of the dashboard — the write rate (one upsert per active user per
// 45s) is trivially within Postgres' envelope.
package main

import (
	"encoding/json"
	"fmt"
	"net/http"
)

var opsRoles = []string{"CASE_MANAGER", "ARBITRATOR", "FINANCE", "FEDERAL_ADMIN", "PLATFORM_ADMIN", "STATE_AUDITOR"}

func (s *server) presencePing(w http.ResponseWriter, r *http.Request) {
	p := r.Context().Value(ctxPrincipal{}).(principal)
	tenant := r.Context().Value(ctxTenant{}).(string)
	var in struct {
		Name string `json:"name"`
	}
	_ = json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&in)
	name := in.Name
	if name == "" {
		name = p.Subject
	}
	rolesJSON, _ := json.Marshal(p.Roles)
	_, err := s.db.Exec(r.Context(), `
		INSERT INTO public.presence (tenant, user_sub, display_name, roles, last_seen)
		VALUES ($1,$2,$3,$4,now())
		ON CONFLICT (tenant, user_sub) DO UPDATE
		  SET display_name=EXCLUDED.display_name, roles=EXCLUDED.roles, last_seen=now()`,
		tenant, p.Subject, truncate(name, 120), rolesJSON)
	if err != nil {
		http.Error(w, `{"error":"db"}`, http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "seen"})
}

func (s *server) opsDashboard(w http.ResponseWriter, r *http.Request) {
	p := r.Context().Value(ctxPrincipal{}).(principal)
	tenant := r.Context().Value(ctxTenant{}).(string)
	elevated := false
	for _, ro := range opsRoles {
		if hasRole(p, ro) {
			elevated = true
			break
		}
	}
	if !elevated {
		http.Error(w, `{"error":"ops dashboard requires a staff role"}`, http.StatusForbidden)
		return
	}
	st := sanitizeTenant(tenant)
	out := map[string]any{"tenant": tenant}

	// Each panel fails independently: one bad query never blanks the board.
	panels := map[string]func() (any, error){
		// Who is online right now (seen in the last 3 minutes).
		"online": func() (any, error) {
			return s.queryRows(r, `
				SELECT user_sub, display_name, roles, last_seen
				FROM public.presence
				WHERE tenant=$1 AND last_seen > now() - interval '3 minutes'
				ORDER BY last_seen DESC`, tenant)
		},
		// Workload distribution: open/overdone tasks per assignee.
		"tasks_by_assignee": func() (any, error) {
			return s.queryRows(r, `
				SELECT COALESCE(assignee,'(unassigned)') AS assignee,
				       count(*) FILTER (WHERE status='OPEN') AS open,
				       count(*) FILTER (WHERE status='OPEN' AND due_date < CURRENT_DATE) AS overdue,
				       count(*) FILTER (WHERE status='DONE' AND created_at > now() - interval '30 days') AS done_30d
				FROM public.tasks WHERE tenant=$1
				GROUP BY assignee ORDER BY open DESC`, tenant)
		},
		// Task throughput + completion score: completed share of all tasks
		// touched in the last 30 days.
		"task_kpis": func() (any, error) {
			return s.queryRows(r, `
				SELECT count(*) FILTER (WHERE status='OPEN') AS open,
				       count(*) FILTER (WHERE status='OPEN' AND due_date < CURRENT_DATE) AS overdue,
				       count(*) FILTER (WHERE status='DONE' AND created_at > now() - interval '30 days') AS done_30d,
				       count(*) FILTER (WHERE created_at > now() - interval '30 days') AS created_30d,
				       round(100.0 * count(*) FILTER (WHERE status='DONE' AND created_at > now() - interval '30 days')
				             / NULLIF(count(*) FILTER (WHERE created_at > now() - interval '30 days'),0), 1) AS completion_pct_30d
				FROM public.tasks WHERE tenant=$1`, tenant)
		},
		// Case pipeline: counts by status + actionable-unassigned + intake pace.
		"cases": func() (any, error) {
			return s.queryRows(r, fmt.Sprintf(`
				SELECT status, count(*) AS n FROM tenant_%s.cases GROUP BY status ORDER BY n DESC`, st))
		},
		"case_kpis": func() (any, error) {
			return s.queryRows(r, fmt.Sprintf(`
				SELECT count(*) FILTER (WHERE assigned_to IS NULL
				          AND status NOT LIKE 'CLOSED%%' AND status <> 'SETTLED_IN_NEGOTIATION') AS unassigned_open,
				       count(*) FILTER (WHERE opened_at > now() - interval '7 days') AS opened_7d,
				       count(*) FILTER (WHERE opened_at > now() - interval '30 days') AS opened_30d,
				       round(AVG(EXTRACT(EPOCH FROM (now() - opened_at))/86400.0)
				             FILTER (WHERE status NOT LIKE 'CLOSED%%'), 1) AS avg_open_age_days
				FROM tenant_%s.cases`, st))
		},
		// Compliance: SLA breaches (7d / all time).
		"sla": func() (any, error) {
			return s.queryRows(r, `
				SELECT count(*) FILTER (WHERE created_at > now() - interval '7 days') AS breaches_7d,
				       count(*) AS breaches_total,
				       count(*) FILTER (WHERE clock='DETERMINATION_30BD') AS determination_breaches,
				       count(*) FILTER (WHERE clock='PAYMENT_30CD') AS payment_breaches
				FROM public.sla_breaches WHERE tenant=$1`, tenant)
		},
		// Outstanding receivables by party.
		"outstanding": func() (any, error) {
			return s.queryRows(r, `
				SELECT party, count(*) AS open_invoices, sum(amount_cents) AS open_cents,
				       sum(amount_cents) FILTER (WHERE due_date < CURRENT_DATE) AS overdue_cents
				FROM public.invoices WHERE tenant=$1 AND status='OPEN'
				GROUP BY party ORDER BY open_cents DESC`, tenant)
		},
		// Money in (30d / all time) from the unified event stream.
		"financial": func() (any, error) {
			return s.queryRows(r, `
				SELECT sum(amount_cents) FILTER (WHERE kind='PAYMENT_PAID') AS collected_cents,
				       sum(amount_cents) FILTER (WHERE kind='PAYMENT_PAID' AND created_at > now() - interval '30 days') AS collected_30d_cents,
				       sum(amount_cents) FILTER (WHERE kind='REFUND_ISSUED') AS refunded_cents
				FROM public.financial_events WHERE tenant=$1`, tenant)
		},
		// Queues needing a human: checks in REVIEW, MATCHED awaiting clearing,
		// QA gate, pre-case intake backlog.
		"queues": func() (any, error) {
			return s.queryRows(r, `
				SELECT
				  (SELECT count(*) FROM public.checks WHERE tenant=$1 AND status='REVIEW') AS checks_review,
				  (SELECT count(*) FROM public.checks WHERE tenant=$1 AND status='MATCHED') AS checks_awaiting_clear,
				  (SELECT count(*) FROM public.qa_reviews WHERE tenant=$1 AND status='PENDING') AS qa_pending,
				  (SELECT count(*) FROM public.intake_requests WHERE tenant=$1
				     AND status NOT IN ('CONVERTED','CLOSED_REFUNDED','INELIGIBLE')) AS intake_open,
				  (SELECT count(*) FROM public.stakeholder_applications WHERE tenant=$1
				     AND status IN ('SUBMITTED','PENDING_DOCS')) AS onboarding_pending`, tenant)
		},
		// Escalations trail (latest 10) — what supervisors needed to see.
		"escalations": func() (any, error) {
			return s.queryRows(r, `
				SELECT case_id, clock, level, escalated_to, created_at
				FROM public.escalations WHERE tenant=$1
				ORDER BY created_at DESC LIMIT 10`, tenant)
		},
		// ---- Chart series -------------------------------------------------
		// Daily intake pace, last 30 days (zero-filled by generate_series).
		"cases_trend": func() (any, error) {
			return s.queryRows(r, fmt.Sprintf(`
				SELECT d.day::date AS day, COUNT(c.id) AS opened
				FROM generate_series(CURRENT_DATE - 29, CURRENT_DATE, interval '1 day') d(day)
				LEFT JOIN tenant_%s.cases c ON c.opened_at::date = d.day
				GROUP BY d.day ORDER BY d.day`, st))
		},
		// Daily collections, last 30 days (from the unified event stream).
		"collections_trend": func() (any, error) {
			return s.queryRows(r, `
				SELECT d.day::date AS day,
				       COALESCE(sum(e.amount_cents) FILTER (WHERE e.kind='PAYMENT_PAID'),0) AS collected_cents,
				       COALESCE(sum(e.amount_cents) FILTER (WHERE e.kind='REFUND_ISSUED'),0) AS refunded_cents
				FROM generate_series(CURRENT_DATE - 29, CURRENT_DATE, interval '1 day') d(day)
				LEFT JOIN public.financial_events e ON e.tenant=$1 AND e.created_at::date = d.day
				GROUP BY d.day ORDER BY d.day`, tenant)
		},
		// Tasks completed per day, last 14 days — throughput sparkline.
		// completed_at is stamped by completeTask; COALESCE keeps legacy rows
		// (completed before the column existed) on their created_at day.
		"throughput_trend": func() (any, error) {
			return s.queryRows(r, `
				SELECT d.day::date AS day, COUNT(t.id) AS done
				FROM generate_series(CURRENT_DATE - 13, CURRENT_DATE, interval '1 day') d(day)
				LEFT JOIN public.tasks t ON t.tenant=$1 AND t.status='DONE'
				       AND COALESCE(t.completed_at, t.created_at)::date = d.day
				GROUP BY d.day ORDER BY d.day`, tenant)
		},
	}
	for key, fn := range panels {
		if v, err := fn(); err == nil {
			out[key] = v
		} else {
			out[key] = nil // panel degrades; rest of the board still renders
		}
	}
	writeJSON(w, http.StatusOK, out)
}
