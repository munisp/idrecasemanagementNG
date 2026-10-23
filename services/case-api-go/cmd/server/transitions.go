package main

// Case status transitions (Phase 5 of the learning arc): structured capture
// of every status hop a case makes, and the transition-matrix model built
// from those hops. The matrix answers "where do cases like this go next,
// and how long do they sit here first" from the tenant's own history.
//
// Gate discipline, same as the rest of the arc: the matrix is only
// reported once the tenant has modelMinClosedCases closed cases behind it.
// Below that the endpoint says insufficient_data with the live count —
// capture runs from day one so the gate opens on real history, not on a
// model trained on three cases.
//
// Schema: scripts/case_transitions.sql.

import (
	"context"
	"fmt"
	"net/http"
	"sort"
	"time"
)

// modelMinClosedCases gates both learned-model surfaces (Phase 4 priors and
// the Phase 5 transition matrix): below it a tenant's history is a
// anecdote, not a distribution. 300 closed cases is the point where
// per-action accept samples and per-status dwell samples have both had
// room to stabilize.
const modelMinClosedCases = 300

// Transition axes. The platform runs two parallel status vocabularies and
// the matrix never mixes them in one chain (see scripts/case_transitions.sql).
const (
	axisLifecycle = "lifecycle" // tenant_X.cases.status — canonical federal NSA lifecycle
	axisProgram   = "program"   // tenant_X.cases.internal_status — program working board
)

// countClosedCases: the tenant's closed portfolio size — the gate input for
// both learned-model surfaces. "Closed" follows the convention already used
// by the stalled-case and attention queries: any CLOSED_% terminal status
// plus SETTLED_IN_NEGOTIATION (resolved before IDR, still a finished case).
func (s *server) countClosedCases(ctx context.Context, tenant string) int {
	var n int
	_ = s.db.QueryRow(ctx, fmt.Sprintf(
		`SELECT count(*) FROM tenant_%s.cases
		 WHERE status LIKE 'CLOSED%%' OR status='SETTLED_IN_NEGOTIATION'`,
		sanitizeTenant(tenant))).Scan(&n)
	return n
}

// recordTransition: best-effort structured capture of one status hop. Called
// immediately after a status write that reported RowsAffected > 0, with the
// status the case held just before. Empty to_status or a no-op hop records
// nothing; a write failure here must never fail the status change itself
// (the activity/audit trail remains the fallback record), so the error is
// intentionally dropped — same convention as logActivity.
func (s *server) recordTransition(ctx context.Context, tenant, caseID, axis, from, to, actor, source string) {
	if to == "" || from == to {
		return
	}
	_, _ = s.db.Exec(ctx, `
		INSERT INTO public.case_transitions
		    (tenant, case_id, axis, from_status, to_status, actor_sub, source)
		VALUES ($1, $2, $3, NULLIF($4,''), $5, NULLIF($6,''), $7)`,
		tenant, caseID, axis, from, to, actor, source)
}

// caseStatusNow reads the lifecycle status a case currently holds — the
// from-side for write sites whose UPDATE carries extra guards (so the old
// value can't be a constant like the auto-advance sites).
func (s *server) caseStatusNow(ctx context.Context, tenant, caseID string) string {
	var st string
	_ = s.db.QueryRow(ctx, fmt.Sprintf(
		`SELECT status FROM tenant_%s.cases WHERE id=$1`,
		sanitizeTenant(tenant)), caseID).Scan(&st)
	return st
}

// transitionEvent is one raw hop as read from case_transitions.
type transitionEvent struct {
	CaseID string
	From   string // "" = opening transition
	To     string
	At     time.Time
}

// matrixCell is one from→to edge of the transition matrix.
type matrixCell struct {
	From             string  `json:"from"`
	To               string  `json:"to"`
	Count            int     `json:"count"`
	Share            float64 `json:"share"` // fraction of from-status exits that went to `to`
	MedianDwellHours float64 `json:"median_dwell_hours"`
}

// buildTransitionMatrix: pure aggregation over a tenant's ordered hops.
// Dwell time is measured in the from-status: the wall time between the hop
// that entered it and the hop that left it toward `to`. The opening
// transition (from="") anchors the first status's entry time but is not
// itself an edge. Shares are computed within each from-status; medians are
// exact (sorted pick / midpoint average), no sampling.
func buildTransitionMatrix(events []transitionEvent) []matrixCell {
	byCase := map[string][]transitionEvent{}
	for _, e := range events {
		byCase[e.CaseID] = append(byCase[e.CaseID], e)
	}
	type key struct{ from, to string }
	counts := map[key]int{}
	exits := map[string]int{}
	dwells := map[key][]float64{}
	for _, evs := range byCase {
		sort.SliceStable(evs, func(i, j int) bool {
			if evs[i].At.Equal(evs[j].At) {
				return false // stable: same-tick hops keep insertion (id) order
			}
			return evs[i].At.Before(evs[j].At)
		})
		for i, e := range evs {
			if e.From == "" {
				continue // opening hop: anchors dwell, is not an edge
			}
			k := key{e.From, e.To}
			counts[k]++
			exits[e.From]++
			if i > 0 {
				d := e.At.Sub(evs[i-1].At).Hours()
				if d >= 0 {
					dwells[k] = append(dwells[k], d)
				}
			}
		}
	}
	cells := []matrixCell{}
	for k, n := range counts {
		cell := matrixCell{From: k.from, To: k.to, Count: n}
		if exits[k.from] > 0 {
			cell.Share = float64(n) / float64(exits[k.from])
		}
		if ds := dwells[k]; len(ds) > 0 {
			sort.Float64s(ds)
			if len(ds)%2 == 1 {
				cell.MedianDwellHours = ds[len(ds)/2]
			} else {
				cell.MedianDwellHours = (ds[len(ds)/2-1] + ds[len(ds)/2]) / 2
			}
		}
		cells = append(cells, cell)
	}
	// Hottest edges first; deterministic tie-break for stable output.
	sort.Slice(cells, func(i, j int) bool {
		if cells[i].Count != cells[j].Count {
			return cells[i].Count > cells[j].Count
		}
		if cells[i].From != cells[j].From {
			return cells[i].From < cells[j].From
		}
		return cells[i].To < cells[j].To
	})
	return cells
}

// analyticsTransitions: GET /analytics/transitions?axis=lifecycle|program —
// the tenant's own transition matrix. Gated: below modelMinClosedCases
// closed cases the response is insufficient_data with the live counts, so
// the portal can show gate progress instead of a matrix computed from a
// handful of anecdotes.
func (s *server) analyticsTransitions(w http.ResponseWriter, r *http.Request) {
	tenant := r.Context().Value(ctxTenant{}).(string)
	p := r.Context().Value(ctxPrincipal{}).(principal)
	if !hasAnyRole(p, "CASE_MANAGER", "ARBITRATOR", "ATTORNEY", "FEDERAL_ADMIN", "PLATFORM_ADMIN") {
		http.Error(w, `{"error":"forbidden"}`, http.StatusForbidden)
		return
	}
	axis := r.URL.Query().Get("axis")
	if axis != axisProgram {
		axis = axisLifecycle
	}
	closed := s.countClosedCases(r.Context(), tenant)
	if closed < modelMinClosedCases {
		writeJSON(w, http.StatusOK, map[string]any{
			"status": "insufficient_data", "axis": axis,
			"closed_cases": closed, "required": modelMinClosedCases,
		})
		return
	}
	rows, err := s.db.Query(r.Context(), `
		SELECT case_id, coalesce(from_status,''), to_status, created_at
		FROM public.case_transitions
		WHERE tenant=$1 AND axis=$2
		ORDER BY case_id, created_at, id`, tenant, axis)
	if err != nil {
		http.Error(w, `{"error":"db"}`, http.StatusInternalServerError)
		return
	}
	defer rows.Close()
	events := []transitionEvent{}
	for rows.Next() {
		var e transitionEvent
		if rows.Scan(&e.CaseID, &e.From, &e.To, &e.At) != nil {
			continue
		}
		events = append(events, e)
	}
	cells := buildTransitionMatrix(events)
	writeJSON(w, http.StatusOK, map[string]any{
		"status": "ok", "axis": axis,
		"closed_cases": closed, "transitions": len(events),
		"matrix": cells,
	})
}
