package main

// qa_learning.go — Phase 3-lite: capture QA decisions as draft-quality
// observations and aggregate them into per-template effectiveness stats.
//
// The capture side (captureQAEdit) runs synchronously inside qaDecision —
// it's one INSERT, and losing an observation because a queue was down would
// silently bias the very dataset that decides which templates work. The
// stats side (qaDraftQuality) is pure SQL over those observations.
//
// What this deliberately does NOT do yet: learn to rewrite drafts. That
// needs volume (per-tenant, per-template) that won't exist for a quarter;
// capturing now means the data is there when it's time.

import (
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"
)

// tokenEditDistance: Levenshtein over whitespace-separated tokens. Tokens,
// not runes: reviewer edits to correspondence are word/phrase-level, token
// DP is O(n*m) on counts two orders of magnitude smaller than characters,
// and the ratio is interpretable ("the reviewer rewrote 12% of the draft").
// Inputs are capped — a pathological 50k-token paste degrades to a prefix
// comparison rather than an O(n²) stall on the request path.
func tokenEditDistance(a, b string) (dist, aTokens int) {
	const capTokens = 2000
	ta := strings.Fields(a)
	tb := strings.Fields(b)
	if len(ta) > capTokens {
		ta = ta[:capTokens]
	}
	if len(tb) > capTokens {
		tb = tb[:capTokens]
	}
	aTokens = len(ta)
	if aTokens == 0 {
		return len(tb), 0
	}
	prev := make([]int, len(tb)+1)
	for j := range prev {
		prev[j] = j
	}
	for i := 1; i <= len(ta); i++ {
		cur := make([]int, len(tb)+1)
		cur[0] = i
		for j := 1; j <= len(tb); j++ {
			cost := 1
			if ta[i-1] == tb[j-1] {
				cost = 0
			}
			cur[j] = min3(cur[j-1]+1, prev[j]+1, prev[j-1]+cost)
		}
		prev = cur
	}
	return prev[len(tb)], aTokens
}

// min3 is defined in screening.go.

// captureQAEdit records one draft-quality observation. original is the body
// as drafted; final is the body the decision landed on (== original unless
// the reviewer used the EDIT prong).
func (s *server) captureQAEdit(r *http.Request, tenant, qid, caseID, artifact, channel, draftedBy, decision, original, final, decidedBy string) {
	edited := strings.TrimSpace(original) != strings.TrimSpace(final)
	dist, tokens := tokenEditDistance(original, final)
	var distPtr *int
	var ratioPtr *float64
	if edited {
		distPtr = &dist
		if tokens > 0 {
			ratio := float64(dist) / float64(tokens)
			ratioPtr = &ratio
		}
	}
	_, _ = s.db.Exec(r.Context(), `
		INSERT INTO public.qa_edit_diffs
			(tenant, qa_id, case_id, artifact, channel, drafted_by, decision, edited, draft_tokens, edit_distance, edit_ratio, decided_by)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12)`,
		tenant, qid, caseID, artifact, channel, draftedBy, decision, edited, tokens, distPtr, ratioPtr, decidedBy)
}

// qaDraftQuality: GET /qa/draft-quality?days=90 — per artifact+channel:
// how many drafts, how often approved, how often approved UNTOUCHED (the
// real quality bar), rejection rate, and mean edit ratio when edited.
func (s *server) qaDraftQuality(w http.ResponseWriter, r *http.Request) {
	tenant := r.Context().Value(ctxTenant{}).(string)
	if p := r.Context().Value(ctxPrincipal{}).(principal); !hasAnyRole(p, "CASE_MANAGER", "ARBITRATOR", "ATTORNEY", "FEDERAL_ADMIN", "PLATFORM_ADMIN") {
		http.Error(w, `{"error":"forbidden"}`, http.StatusForbidden)
		return
	}
	days := 90
	if d, err := strconv.Atoi(r.URL.Query().Get("days")); err == nil && d > 0 && d <= 365 {
		days = d
	}
	rows, err := s.db.Query(r.Context(), `
		SELECT artifact, channel,
		       count(*) AS drafts,
		       count(*) FILTER (WHERE decision='APPROVE') AS approved,
		       count(*) FILTER (WHERE decision='APPROVE' AND NOT edited) AS approved_unedited,
		       count(*) FILTER (WHERE decision='REJECT') AS rejected,
		       round(avg(edit_ratio)::numeric, 4) AS avg_edit_ratio,
		       count(DISTINCT decided_by) AS reviewers
		FROM public.qa_edit_diffs
		WHERE tenant=$1 AND created_at > now() - make_interval(days => $2)
		GROUP BY artifact, channel
		ORDER BY drafts DESC`, tenant, days)
	if err != nil {
		http.Error(w, `{"error":"db"}`, http.StatusInternalServerError)
		return
	}
	defer rows.Close()
	type row struct {
		Artifact         string   `json:"artifact"`
		Channel          string   `json:"channel"`
		Drafts           int      `json:"drafts"`
		Approved         int      `json:"approved"`
		ApprovedUnedited int      `json:"approved_unedited"`
		Rejected         int      `json:"rejected"`
		AvgEditRatio     *float64 `json:"avg_edit_ratio"`
		Reviewers        int      `json:"reviewers"`
	}
	out := []row{}
	for rows.Next() {
		var v row
		if rows.Scan(&v.Artifact, &v.Channel, &v.Drafts, &v.Approved, &v.ApprovedUnedited, &v.Rejected, &v.AvgEditRatio, &v.Reviewers) == nil {
			out = append(out, v)
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"days": days, "templates": out})
}

// qaTemplateStats: one template's track record at this tenant's QA gate.
// Returned inside qaGet so the reviewer sees the evidence next to the draft.
func (s *server) qaTemplateStats(r *http.Request, tenant, artifact string) map[string]any {
	var drafts, approved, unedited, rejected int64
	var avgRatio *float64
	err := s.db.QueryRow(r.Context(), `
		SELECT count(*),
		       count(*) FILTER (WHERE decision='APPROVE'),
		       count(*) FILTER (WHERE decision='APPROVE' AND NOT edited),
		       count(*) FILTER (WHERE decision='REJECT'),
		       avg(edit_ratio)
		FROM public.qa_edit_diffs
		WHERE tenant=$1 AND artifact=$2 AND created_at > now() - interval '90 days'`,
		tenant, artifact).Scan(&drafts, &approved, &unedited, &rejected, &avgRatio)
	if err != nil || drafts == 0 {
		return nil
	}
	out := map[string]any{
		"drafts": drafts, "approved": approved, "approved_unedited": unedited, "rejected": rejected,
	}
	if avgRatio != nil {
		out["avg_edit_ratio"] = *avgRatio
	}
	return out
}

// templateHealthVerdict: pure. Turns a template's decision record into one
// word a templates admin can act on.
//
//	insufficient_data — fewer than minN decisions; small samples would
//	                    retire templates for one reviewer's taste.
//	healthy           — accepted mostly as drafted.
//	watch             — rejection rate above 25% or reviewers rewrite more
//	                    than a fifth of the body on average.
//	retire_candidate  — rejection rate above 40% or average rewrite above a
//	                    third: the template is producing work, not saving it.
//
// Thresholds are deliberately round and documented — when they move, the
// portal copy and this comment move together.
func templateHealthVerdict(drafts, rejected int64, avgRatio *float64) string {
	const minN = 20
	if drafts < minN {
		return "insufficient_data"
	}
	rejRate := float64(rejected) / float64(drafts)
	ratio := 0.0
	if avgRatio != nil {
		ratio = *avgRatio
	}
	switch {
	case rejRate > 0.4 || ratio > 0.35:
		return "retire_candidate"
	case rejRate > 0.25 || ratio > 0.2:
		return "watch"
	default:
		return "healthy"
	}
}

// qaTemplateHealth: GET /qa/template-health?days=90 — per-template decision
// record across the whole tenant (qaTemplateStats covers one template inside
// the review screen; this is the portfolio view for the templates admin
// page). Worst verdict first so the page reads top-down as a work list.
func (s *server) qaTemplateHealth(w http.ResponseWriter, r *http.Request) {
	tenant := r.Context().Value(ctxTenant{}).(string)
	if p := r.Context().Value(ctxPrincipal{}).(principal); !hasAnyRole(p, "CASE_MANAGER", "ARBITRATOR", "ATTORNEY", "FEDERAL_ADMIN", "PLATFORM_ADMIN") {
		http.Error(w, `{"error":"forbidden"}`, http.StatusForbidden)
		return
	}
	days := 90
	if d, err := strconv.Atoi(r.URL.Query().Get("days")); err == nil && d > 0 && d <= 365 {
		days = d
	}
	rows, err := s.db.Query(r.Context(), `
		SELECT artifact,
		       count(*),
		       count(*) FILTER (WHERE decision='APPROVE'),
		       count(*) FILTER (WHERE decision='APPROVE' AND NOT edited),
		       count(*) FILTER (WHERE decision='REJECT'),
		       avg(edit_ratio)
		FROM public.qa_edit_diffs
		WHERE tenant=$1 AND created_at > now() - make_interval(days => $2)
		GROUP BY artifact`, tenant, days)
	if err != nil {
		http.Error(w, `{"error":"query failed"}`, http.StatusInternalServerError)
		return
	}
	defer rows.Close()
	type health struct {
		Artifact         string   `json:"artifact"`
		Drafts           int64    `json:"drafts"`
		Approved         int64    `json:"approved"`
		ApprovedUnedited int64    `json:"approved_unedited"`
		Rejected         int64    `json:"rejected"`
		AvgEditRatio     *float64 `json:"avg_edit_ratio,omitempty"`
		Verdict          string   `json:"verdict"`
	}
	out := []health{}
	for rows.Next() {
		var h health
		if rows.Scan(&h.Artifact, &h.Drafts, &h.Approved, &h.ApprovedUnedited, &h.Rejected, &h.AvgEditRatio) != nil {
			continue
		}
		h.Verdict = templateHealthVerdict(h.Drafts, h.Rejected, h.AvgEditRatio)
		out = append(out, h)
	}
	rank := map[string]int{"retire_candidate": 0, "watch": 1, "healthy": 2, "insufficient_data": 3}
	sort.SliceStable(out, func(i, j int) bool {
		if rank[out[i].Verdict] != rank[out[j].Verdict] {
			return rank[out[i].Verdict] < rank[out[j].Verdict]
		}
		return out[i].Drafts > out[j].Drafts
	})
	writeJSON(w, http.StatusOK, map[string]any{"days": days, "templates": out, "generated_at": time.Now().UTC()})
}
