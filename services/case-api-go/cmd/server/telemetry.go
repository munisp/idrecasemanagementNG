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

// telemetryActionStats: GET /telemetry/action-stats?days=90 — the calibration
// loop made visible. Per next-action action_id: how often it was shown, how
// often accepted, the hand-set prior, and the confidence the recommender
// would report TODAY (prior below the sample threshold, Laplace-smoothed
// observed rate at or above it — the same calibrateConfidence the live
// endpoint uses, so this table is the answer to "why did the button say
// 0.83?"). Sorted by sample size so the actions closest to flipping from
// prior to observed are on top where a reviewer can watch them.
func (s *server) telemetryActionStats(w http.ResponseWriter, r *http.Request) {
	tenant := r.Context().Value(ctxTenant{}).(string)
	p := r.Context().Value(ctxPrincipal{}).(principal)
	if !hasAnyRole(p, "FEDERAL_ADMIN", "PLATFORM_ADMIN", "CASE_MANAGER") {
		http.Error(w, `{"error":"forbidden"}`, http.StatusForbidden)
		return
	}
	days := 90 // matches the calibration window in caseNextAction
	if d, err := strconv.Atoi(r.URL.Query().Get("days")); err == nil && d > 0 && d <= 365 {
		days = d
	}
	rows, err := s.db.Query(r.Context(), `
		SELECT target,
		       count(*) FILTER (WHERE action='next_action_shown') AS shown,
		       count(*) FILTER (WHERE action='next_action_accept') AS accepts
		FROM public.interaction_events
		WHERE tenant=$1 AND action IN ('next_action_shown','next_action_accept')
		  AND created_at > now() - make_interval(days => $2)
		GROUP BY target ORDER BY shown DESC`, tenant, days)
	if err != nil {
		http.Error(w, `{"error":"query failed"}`, http.StatusInternalServerError)
		return
	}
	defer rows.Close()
	type actionOutcome struct {
		AcceptedN                  int      `json:"accepted_n"`
		AcceptedMedianHoursToClose *float64 `json:"accepted_median_hours_to_close,omitempty"`
		NotAcceptedN               int      `json:"not_accepted_n"`
		NotAcceptedMedianHours     *float64 `json:"not_accepted_median_hours_to_close,omitempty"`
	}
	type actionStat struct {
		ActionID   string         `json:"action_id"`
		Shown      int            `json:"shown"`
		Accepts    int            `json:"accepts"`
		Prior      float64        `json:"prior"`
		Confidence float64        `json:"confidence"` // what the recommender reports now
		Basis      string         `json:"basis"`      // "prior" | "observed"
		Outcome    *actionOutcome `json:"outcome,omitempty"`
	}
	out := []actionStat{}
	for rows.Next() {
		var st actionStat
		if rows.Scan(&st.ActionID, &st.Shown, &st.Accepts) != nil {
			continue
		}
		st.Prior = nextActionPriors[st.ActionID] // 0 for action_ids the rules no longer emit — a visible drift signal
		st.Confidence, st.Basis = calibrateConfidence(st.Prior, st.Shown, st.Accepts)
		out = append(out, st)
	}
	// The learned-model gate, reported alongside the per-action numbers so
	// this one table answers both "how calibrated are we?" and "when do the
	// priors themselves start learning?".
	closed := s.countClosedCases(r.Context(), tenant)
	gateOpen := closed >= modelMinClosedCases

	// Delayed-outcome estimates (gated with the priors): per action, the
	// median wall time from first showing to the case's first terminal
	// lifecycle transition, split by whether anyone accepted the suggestion.
	// Cohorts are per (action, case); each cohort reports only at or above
	// calibrateMinShown members. OBSERVATIONAL — cases that accept a
	// suggestion differ from cases that don't, so this is an association a
	// reviewer watches over time, never a causal claim the model acts on.
	if gateOpen {
		outcomes := map[string]*actionOutcome{}
		oRows, oErr := s.db.Query(r.Context(), `
			WITH ev AS (
				SELECT target, meta->>'case_id' AS case_id,
				       bool_or(action='next_action_accept') AS accepted,
				       min(created_at) AS first_at
				FROM public.interaction_events
				WHERE tenant=$1 AND action IN ('next_action_shown','next_action_accept')
				  AND created_at > now() - make_interval(days => $2)
				  AND meta ? 'case_id' AND meta->>'case_id' <> ''
				GROUP BY target, meta->>'case_id'
			), term AS (
				SELECT ev.target, ev.accepted,
				       EXTRACT(EPOCH FROM (t.created_at - ev.first_at))/3600.0 AS hours
				FROM ev
				JOIN LATERAL (
					SELECT created_at FROM public.case_transitions ct
					WHERE ct.tenant=$1 AND ct.case_id=ev.case_id AND ct.axis='lifecycle'
					  AND (ct.to_status LIKE 'CLOSED%%' OR ct.to_status='SETTLED_IN_NEGOTIATION')
					  AND ct.created_at >= ev.first_at
					ORDER BY ct.created_at LIMIT 1
				) t ON true
			)
			SELECT target, accepted, count(*),
			       percentile_cont(0.5) WITHIN GROUP (ORDER BY hours)
			FROM term GROUP BY target, accepted`, tenant, days)
		if oErr == nil {
			for oRows.Next() {
				var target string
				var accepted bool
				var n int
				var median float64
				if oRows.Scan(&target, &accepted, &n, &median) != nil {
					continue
				}
				o := outcomes[target]
				if o == nil {
					o = &actionOutcome{}
					outcomes[target] = o
				}
				if accepted {
					o.AcceptedN = n
					if n >= calibrateMinShown {
						o.AcceptedMedianHoursToClose = &median
					}
				} else {
					o.NotAcceptedN = n
					if n >= calibrateMinShown {
						o.NotAcceptedMedianHours = &median
					}
				}
			}
			oRows.Close()
			for i := range out {
				out[i].Outcome = outcomes[out[i].ActionID]
			}
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"days": days, "actions": out, "generated_at": time.Now().UTC(),
		"model_gate": map[string]any{
			"closed_cases": closed, "required": modelMinClosedCases,
			"open": gateOpen,
		},
		"outcome_caveat": "Observational: cases that accept a suggestion differ from cases that do not — association, not causation.",
	})
}

// telemetryPrefillStats: GET /telemetry/prefill-stats?days=90 — per initiation
// form field: how often the prefill suggestion was accepted as-is vs typed
// over. This is the field-level mirror of action-stats: a field with a low
// accept rate is either extracting the wrong value or carrying a confidence
// badge users have learned to distrust — both are extraction-pipeline fixes,
// and this table is how you find them without reading individual cases.
// No smoothing here: unlike action confidence, nothing is RECOMMENDED from
// these numbers, so a raw rate with its sample size is the honest display.
func (s *server) telemetryPrefillStats(w http.ResponseWriter, r *http.Request) {
	tenant := r.Context().Value(ctxTenant{}).(string)
	p := r.Context().Value(ctxPrincipal{}).(principal)
	if !hasAnyRole(p, "FEDERAL_ADMIN", "PLATFORM_ADMIN", "CASE_MANAGER") {
		http.Error(w, `{"error":"forbidden"}`, http.StatusForbidden)
		return
	}
	days := 90
	if d, err := strconv.Atoi(r.URL.Query().Get("days")); err == nil && d > 0 && d <= 365 {
		days = d
	}
	rows, err := s.db.Query(r.Context(), `
		SELECT target,
		       count(*) FILTER (WHERE action='prefill_accept') AS accepts,
		       count(*) FILTER (WHERE action='prefill_override') AS overrides
		FROM public.interaction_events
		WHERE tenant=$1 AND action IN ('prefill_accept','prefill_override') AND target <> ''
		  AND created_at > now() - make_interval(days => $2)
		GROUP BY target ORDER BY accepts + overrides DESC`, tenant, days)
	if err != nil {
		http.Error(w, `{"error":"query failed"}`, http.StatusInternalServerError)
		return
	}
	defer rows.Close()
	type fieldStat struct {
		Field      string   `json:"field"`
		Accepts    int      `json:"accepts"`
		Overrides  int      `json:"overrides"`
		AcceptRate *float64 `json:"accept_rate,omitempty"`
	}
	out := []fieldStat{}
	for rows.Next() {
		var st fieldStat
		if rows.Scan(&st.Field, &st.Accepts, &st.Overrides) != nil {
			continue
		}
		if n := st.Accepts + st.Overrides; n > 0 {
			r := float64(st.Accepts) / float64(n)
			st.AcceptRate = &r
		}
		out = append(out, st)
	}
	writeJSON(w, http.StatusOK, map[string]any{"days": days, "fields": out, "generated_at": time.Now().UTC()})
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
		// Summary mode: per-workflow funnel (sessions, completions, abandon)
		// plus timing — median session length and median time-to-first-submit.
		// The timing columns are what make the productivity arc's core claim
		// checkable: prefill and next-action exist to shrink time-to-submit,
		// and without a measured baseline that claim would be unfalsifiable.
		rows, err := s.db.Query(r.Context(), `
			WITH per_sess AS (
				SELECT workflow, session_id,
				       min(created_at) AS started,
				       max(created_at) AS last_ev,
				       min(created_at) FILTER (WHERE action='submit') AS first_submit,
				       bool_or(action IN ('prefill_override','next_action_override')) AS had_override
				FROM public.interaction_events
				WHERE tenant=$1 AND created_at > now() - make_interval(days => $2)
				GROUP BY workflow, session_id
			)
			SELECT workflow,
			       count(*) AS sessions,
			       count(*) FILTER (WHERE first_submit IS NOT NULL) AS completed,
			       count(*) FILTER (WHERE had_override) AS overrides,
			       percentile_cont(0.5) WITHIN GROUP (ORDER BY extract(epoch FROM last_ev - started)) AS median_secs,
			       percentile_cont(0.5) WITHIN GROUP (ORDER BY extract(epoch FROM first_submit - started))
			         FILTER (WHERE first_submit IS NOT NULL) AS median_tts_secs
			FROM per_sess GROUP BY workflow ORDER BY sessions DESC`, tenant, days)
		if err != nil {
			http.Error(w, `{"error":"query failed"}`, http.StatusInternalServerError)
			return
		}
		defer rows.Close()
		out := []map[string]any{}
		for rows.Next() {
			var wf string
			var sess, comp, ovr int64
			var medSecs, medTTS *float64
			if rows.Scan(&wf, &sess, &comp, &ovr, &medSecs, &medTTS) != nil {
				continue
			}
			row := map[string]any{"workflow": wf, "sessions": sess, "completed": comp, "abandoned": sess - comp, "override_sessions": ovr}
			if sess > 0 {
				row["completion_rate"] = float64(comp) / float64(sess)
			}
			if medSecs != nil {
				row["median_session_secs"] = *medSecs
			}
			if medTTS != nil {
				row["median_time_to_submit_secs"] = *medTTS
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
