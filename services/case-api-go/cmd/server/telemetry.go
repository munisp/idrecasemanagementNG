package main

// telemetry.go — Phase 1 of the productivity program: the interaction
// baseline. POST /telemetry/events ingests batched portal events (privacy:
// keystroke COUNTS, never content); GET /telemetry/metrics aggregates them
// into per-workflow click/keystroke/session numbers so "we cut initiation
// from 140 keystrokes to 40" is a measured claim, not a vibe.

import (
	"encoding/json"
	"net/http"
	"regexp"
	"strconv"
	"time"
)

var telemClean = regexp.MustCompile(`[^a-z0-9_:/#-]`)

// telemetryIngest: POST /telemetry/events {events:[...]} — best-effort,
// cheap, and never the reason a portal interaction fails.
func (s *server) telemetryIngest(w http.ResponseWriter, r *http.Request) {
	tenant := r.Context().Value(ctxTenant{}).(string)
	p := r.Context().Value(ctxPrincipal{}).(principal)
	var in struct {
		SessionID string `json:"session_id"`
		Events    []struct {
			Workflow string         `json:"workflow"`
			Action   string         `json:"action"`
			Target   string         `json:"target"`
			Meta     map[string]any `json:"meta"`
		} `json:"events"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil || in.SessionID == "" {
		http.Error(w, `{"error":"bad json"}`, http.StatusBadRequest)
		return
	}
	if len(in.Events) > 200 {
		in.Events = in.Events[:200]
	}
	sess := telemClean.ReplaceAllString(in.SessionID, "")
	if len(sess) > 64 {
		sess = sess[:64]
	}
	stored := 0
	for _, e := range in.Events {
		wf := telemClean.ReplaceAllString(e.Workflow, "")
		act := telemClean.ReplaceAllString(e.Action, "")
		if wf == "" || act == "" || len(wf) > 40 || len(act) > 40 {
			continue
		}
		tgt := e.Target
		if len(tgt) > 120 {
			tgt = tgt[:120]
		}
		meta, _ := json.Marshal(e.Meta)
		if _, err := s.db.Exec(r.Context(), `
			INSERT INTO public.interaction_events (tenant, actor_sub, session_id, workflow, action, target, meta)
			VALUES ($1,$2,$3,$4,$5,$6,$7)`,
			tenant, p.Subject, sess, wf, act, tgt, meta); err == nil {
			stored++
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"stored": stored})
}

// telemetryMetrics: GET /telemetry/metrics?days=30 — per-workflow aggregates:
// sessions, clicks, keystrokes, submits, and the learning signals (prefill
// and next-action accept/override rates). Admin/manager roles only.
func (s *server) telemetryMetrics(w http.ResponseWriter, r *http.Request) {
	tenant := r.Context().Value(ctxTenant{}).(string)
	p := r.Context().Value(ctxPrincipal{}).(principal)
	if !hasAnyRole(p, "FEDERAL_ADMIN", "PLATFORM_ADMIN", "CASE_MANAGER") {
		http.Error(w, `{"error":"forbidden"}`, http.StatusForbidden)
		return
	}
	days := 30
	if d, err := strconv.Atoi(r.URL.Query().Get("days")); err == nil && d > 0 && d <= 365 {
		days = d
	}
	rows, err := s.db.Query(r.Context(), `
		SELECT workflow,
		       count(DISTINCT session_id) AS sessions,
		       count(*) FILTER (WHERE action='click') AS clicks,
		       coalesce(sum((meta->>'keys')::int) FILTER (WHERE action='keystroke'), 0) AS keystrokes,
		       count(*) FILTER (WHERE action='submit') AS submits,
		       count(*) FILTER (WHERE action='prefill_accept') AS prefill_accept,
		       count(*) FILTER (WHERE action='prefill_override') AS prefill_override,
		       count(*) FILTER (WHERE action='next_action_accept') AS na_accept,
		       count(*) FILTER (WHERE action='next_action_override') AS na_override
		FROM public.interaction_events
		WHERE tenant=$1 AND created_at > now() - make_interval(days => $2)
		GROUP BY workflow ORDER BY sessions DESC`, tenant, days)
	if err != nil {
		http.Error(w, `{"error":"query failed"}`, http.StatusInternalServerError)
		return
	}
	defer rows.Close()
	out := []map[string]any{}
	for rows.Next() {
		var wf string
		var sessions, clicks, keystrokes, submits, pa, po, naa, nao int64
		if rows.Scan(&wf, &sessions, &clicks, &keystrokes, &submits, &pa, &po, &naa, &nao) != nil {
			continue
		}
		row := map[string]any{
			"workflow": wf, "sessions": sessions, "clicks": clicks,
			"keystrokes": keystrokes, "submits": submits,
			"prefill_accept": pa, "prefill_override": po,
			"next_action_accept": naa, "next_action_override": nao,
		}
		if sessions > 0 {
			row["clicks_per_session"] = float64(clicks) / float64(sessions)
			row["keystrokes_per_session"] = float64(keystrokes) / float64(sessions)
		}
		if pa+po > 0 {
			row["prefill_accept_rate"] = float64(pa) / float64(pa+po)
		}
		if naa+nao > 0 {
			row["next_action_accept_rate"] = float64(naa) / float64(naa+nao)
		}
		out = append(out, row)
	}
	writeJSON(w, http.StatusOK, map[string]any{"days": days, "workflows": out, "generated_at": time.Now().UTC()})
}

// telemetryPaths: GET /telemetry/paths?days=30&workflow=case_initiate — the
// transition layer. Per workflow: how many sessions, how many reached a
// submit (completion) vs abandoned, and the most common step→step
// transitions. Two uses: (1) now — find where users stall or drop out of
// high-volume flows; (2) later — this per-tenant transition matrix is the
// substrate the learned next-action model trains on (Phase 5), so the
// aggregation exists and is validated long before any model consumes it.
func (s *server) telemetryPaths(w http.ResponseWriter, r *http.Request) {
	tenant := r.Context().Value(ctxTenant{}).(string)
	if p := r.Context().Value(ctxPrincipal{}).(principal); !hasAnyRole(p, "FEDERAL_ADMIN", "PLATFORM_ADMIN", "CASE_MANAGER") {
		http.Error(w, `{"error":"forbidden"}`, http.StatusForbidden)
		return
	}
	days := 30
	if d, err := strconv.Atoi(r.URL.Query().Get("days")); err == nil && d > 0 && d <= 365 {
		days = d
	}
	workflow := r.URL.Query().Get("workflow")

	if workflow == "" {
		// Summary mode: per-workflow funnel (sessions, completions, abandon).
		rows, err := s.db.Query(r.Context(), `
			SELECT workflow,
			       count(DISTINCT session_id) AS sessions,
			       count(DISTINCT session_id) FILTER (WHERE action='submit') AS completed,
			       count(DISTINCT session_id) FILTER (WHERE action='prefill_override'
			                                          OR action='next_action_override') AS overrides
			FROM public.interaction_events
			WHERE tenant=$1 AND created_at > now() - make_interval(days => $2)
			GROUP BY workflow ORDER BY sessions DESC`, tenant, days)
		if err != nil {
			http.Error(w, `{"error":"query failed"}`, http.StatusInternalServerError)
			return
		}
		defer rows.Close()
		out := []map[string]any{}
		for rows.Next() {
			var wf string
			var sess, comp, ovr int64
			if rows.Scan(&wf, &sess, &comp, &ovr) != nil {
				continue
			}
			row := map[string]any{"workflow": wf, "sessions": sess, "completed": comp, "abandoned": sess - comp, "override_sessions": ovr}
			if sess > 0 {
				row["completion_rate"] = float64(comp) / float64(sess)
			}
			out = append(out, row)
		}
		writeJSON(w, http.StatusOK, map[string]any{"days": days, "workflows": out})
		return
	}

	// Detail mode: step→step transitions within one workflow.
	workflow = telemClean.ReplaceAllString(workflow, "")
	type transition struct {
		From  string `json:"from"`
		To    string `json:"to"`
		Count int64  `json:"count"`
	}
	transitions := []transition{}
	rows, err := s.db.Query(r.Context(), `
		WITH ev AS (
			SELECT session_id, action, target,
			       lead(action) OVER w AS next_action,
			       lead(target) OVER w AS next_target
			FROM public.interaction_events
			WHERE tenant=$1 AND workflow=$2 AND created_at > now() - make_interval(days => $3)
			WINDOW w AS (PARTITION BY session_id ORDER BY created_at, id)
		)
		SELECT action || CASE WHEN target<>'' THEN ' → ' || target ELSE '' END AS from_step,
		       coalesce(next_action, '(session end)') ||
		       CASE WHEN coalesce(next_target,'')<>'' THEN ' → ' || next_target ELSE '' END AS to_step,
		       count(*)
		FROM ev
		GROUP BY 1, 2 ORDER BY 3 DESC LIMIT 25`, tenant, workflow, days)
	if err != nil {
		http.Error(w, `{"error":"query failed"}`, http.StatusInternalServerError)
		return
	}
	defer rows.Close()
	for rows.Next() {
		var t transition
		if rows.Scan(&t.From, &t.To, &t.Count) == nil {
			transitions = append(transitions, t)
		}
	}
	// Last steps of abandoned sessions: the "where do people give up" list.
	type exitPoint struct {
		LastStep string `json:"last_step"`
		Sessions int64  `json:"sessions"`
	}
	exits := []exitPoint{}
	rows2, err := s.db.Query(r.Context(), `
		WITH ev AS (
			SELECT session_id, action, target,
			       row_number() OVER (PARTITION BY session_id ORDER BY created_at DESC, id DESC) AS rn,
			       max(CASE WHEN action='submit' THEN 1 ELSE 0 END)
			         OVER (PARTITION BY session_id) AS submitted
			FROM public.interaction_events
			WHERE tenant=$1 AND workflow=$2 AND created_at > now() - make_interval(days => $3)
		)
		SELECT action || CASE WHEN target<>'' THEN ' → ' || target ELSE '' END, count(*)
		FROM ev WHERE rn=1 AND submitted=0
		GROUP BY 1 ORDER BY 2 DESC LIMIT 10`, tenant, workflow, days)
	if err == nil {
		defer rows2.Close()
		for rows2.Next() {
			var e exitPoint
			if rows2.Scan(&e.LastStep, &e.Sessions) == nil {
				exits = append(exits, e)
			}
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"days": days, "workflow": workflow,
		"top_transitions": transitions, "abandon_exits": exits})
}
