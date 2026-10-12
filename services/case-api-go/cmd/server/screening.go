package main

// screening.go — OFAC sanctions screening backed by moov-io/watchman's OFAC
// tooling (Apache-2.0): the SDN list is downloaded from the U.S. Treasury on a
// schedule, parsed with watchman's reader/mapper, and searched in-process with
// a deterministic normalized token-overlap + edit-distance scorer. No vendor
// API, no per-lookup network call; list refresh is the only outbound traffic
// (gated by WATCHMAN_ENABLED, default on; failure keeps the last good list).
//
// Screening is warn-only by platform convention (same posture as the
// eligibility gate): a hit never auto-blocks an onboarding decision — it is
// recorded in the append-only screening_checks table, audit-logged, and
// broadcast to staff so a compliance officer reviews before the subject
// touches a case.

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	moovlog "github.com/moov-io/base/log"
	"github.com/moov-io/watchman/pkg/ofac"
	"github.com/moov-io/watchman/pkg/search"
)

var sdnList struct {
	sync.RWMutex
	entities    []search.Entity[search.Value]
	refreshedAt time.Time
	err         string
}

// watchmanRefresh downloads and parses the OFAC SDN list. Exported for tests:
// the downloader is the only network touch; scoring is pure.
func watchmanRefresh(ctx context.Context) error {
	dir := os.Getenv("WATCHMAN_DATA_DIR")
	if dir == "" {
		dir = "/data/watchman"
	}
	_ = os.MkdirAll(dir, 0o755)
	files, err := ofac.Download(ctx, moovlog.NewNopLogger(), dir)
	if err != nil {
		return err
	}
	defer files.Close()
	res, err := ofac.Read(files)
	if err != nil {
		return err
	}
	entities := ofac.GroupIntoEntities(res.SDNs, res.Addresses, res.SDNComments, res.AlternateIdentities)
	sdnList.Lock()
	sdnList.entities = entities
	sdnList.refreshedAt = time.Now()
	sdnList.err = ""
	sdnList.Unlock()
	return nil
}

func (s *server) startWatchmanRefresher() {
	if strings.EqualFold(os.Getenv("WATCHMAN_ENABLED"), "false") {
		return
	}
	go func() {
		ctx := context.Background()
		if err := watchmanRefresh(ctx); err != nil {
			sdnList.Lock()
			sdnList.err = err.Error()
			sdnList.Unlock()
		}
		for range time.Tick(24 * time.Hour) {
			if err := watchmanRefresh(ctx); err != nil {
				sdnList.Lock()
				sdnList.err = err.Error() // stale list stays live; age is visible via /screening/status
				sdnList.Unlock()
			}
		}
	}()
}

// ---------- deterministic scorer ----------

func normName(s string) []string {
	s = strings.ToUpper(s)
	fields := strings.FieldsFunc(s, func(r rune) bool {
		return !(r >= 'A' && r <= 'Z' || r >= '0' && r <= '9')
	})
	out := fields[:0]
	for _, f := range fields {
		if len(f) > 1 {
			out = append(out, f)
		}
	}
	return out
}

// levenshteinRatio: 1.0 identical, 0.0 disjoint.
func levenshteinRatio(a, b string) float64 {
	if a == b {
		return 1.0
	}
	la, lb := len(a), len(b)
	if la == 0 || lb == 0 {
		return 0
	}
	prev := make([]int, lb+1)
	for j := range prev {
		prev[j] = j
	}
	for i := 1; i <= la; i++ {
		cur := make([]int, lb+1)
		cur[0] = i
		for j := 1; j <= lb; j++ {
			cost := 1
			if a[i-1] == b[j-1] {
				cost = 0
			}
			cur[j] = min3(cur[j-1]+1, prev[j]+1, prev[j-1]+cost)
		}
		prev = cur
	}
	d := prev[lb]
	return 1.0 - float64(d)/float64(max(la, lb))
}

func min3(a, b, c int) int {
	if a < b {
		if a < c {
			return a
		}
		return c
	}
	if b < c {
		return b
	}
	return c
}

// nameSimilarity: token coverage weighted by per-token best fuzzy score.
// Coverage guards against partial-name false positives ("John Smith" vs
// "John Smithson Holdings"); fuzz guards against transliteration variance.
func nameSimilarity(query, candidate string) float64 {
	q, c := normName(query), normName(candidate)
	if len(q) == 0 || len(c) == 0 {
		return 0
	}
	var covered float64
	for _, qt := range q {
		best := 0.0
		for _, ct := range c {
			if r := levenshteinRatio(qt, ct); r > best {
				best = r
			}
		}
		covered += best
	}
	coverage := covered / float64(len(q))
	// Penalize long unmatched candidate tails slightly.
	balance := float64(len(q)) / float64(max(len(q), len(c)))
	return coverage * (0.7 + 0.3*balance)
}

type screeningMatch struct {
	Name     string  `json:"name"`
	SourceID string  `json:"source_id"`
	Score    float64 `json:"score"`
}

// screenName scores a name against the cached SDN list. Threshold default
// 0.85 (WATCHMAN_MATCH_THRESHOLD overrides).
func screenName(query string) (matched bool, top float64, matches []screeningMatch) {
	sdnList.RLock()
	entities := sdnList.entities
	sdnList.RUnlock()
	threshold := 0.85
	if v := os.Getenv("WATCHMAN_MATCH_THRESHOLD"); v != "" {
		if f, err := strconv.ParseFloat(v, 64); err == nil && f > 0 && f <= 1 {
			threshold = f
		}
	}
	for _, e := range entities {
		names := []string{e.Name}
		if e.Person != nil {
			names = append(names, e.Person.AltNames...)
		}
		if e.Business != nil {
			names = append(names, e.Business.AltNames...)
		}
		if e.Organization != nil {
			names = append(names, e.Organization.AltNames...)
		}
		best := 0.0
		bestName := ""
		for _, n := range names {
			if sc := nameSimilarity(query, n); sc > best {
				best, bestName = sc, n
			}
		}
		if best >= threshold {
			matches = append(matches, screeningMatch{Name: bestName, SourceID: e.SourceID, Score: best})
			if best > top {
				top = best
			}
		}
	}
	return len(matches) > 0, top, matches
}

// ---------- HTTP surface ----------

func (s *server) screeningStatus(w http.ResponseWriter, r *http.Request) {
	sdnList.RLock()
	defer sdnList.RUnlock()
	writeJSON(w, http.StatusOK, map[string]any{
		"entity_count": len(sdnList.entities), "refreshed_at": sdnList.refreshedAt,
		"last_error": sdnList.err,
		"enabled":    !strings.EqualFold(os.Getenv("WATCHMAN_ENABLED"), "false"),
	})
}

func (s *server) screenSubject(w http.ResponseWriter, r *http.Request) {
	p := r.Context().Value(ctxPrincipal{}).(principal)
	if !hasAnyRole(p, "CASE_MANAGER", "FINANCE", "ARBITRATOR", "FEDERAL_ADMIN", "PLATFORM_ADMIN") {
		http.Error(w, `{"error":"forbidden"}`, http.StatusForbidden)
		return
	}
	tenant := r.Context().Value(ctxTenant{}).(string)
	var in struct {
		Name        string `json:"name"`
		SubjectType string `json:"subject_type"` // ARBITRATOR|TPA|PARTY|VENDOR|MANUAL
		SubjectRef  string `json:"subject_ref"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil || strings.TrimSpace(in.Name) == "" {
		http.Error(w, `{"error":"name required"}`, http.StatusBadRequest)
		return
	}
	if strings.TrimSpace(in.SubjectType) == "" {
		in.SubjectType = "MANUAL"
	}
	sdnList.RLock()
	loaded := len(sdnList.entities)
	refreshed := sdnList.refreshedAt
	sdnList.RUnlock()
	if loaded == 0 {
		http.Error(w, `{"error":"sanctions list not loaded yet — refresh in progress or WATCHMAN_ENABLED=false"}`, http.StatusServiceUnavailable)
		return
	}
	matched, top, matches := screenName(in.Name)
	if len(matches) > 10 {
		matches = matches[:10]
	}
	mj, _ := json.Marshal(matches)
	var id int64
	_ = s.db.QueryRow(r.Context(), `
		INSERT INTO public.screening_checks
		  (tenant, subject_type, subject_ref, query_name, matched, top_score, matches, list_entity_count, list_refreshed_at, screened_by)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10) RETURNING id`,
		tenant, strings.ToUpper(in.SubjectType), in.SubjectRef, in.Name, matched, top, mj, loaded, refreshed, p.Subject).Scan(&id)
	if matched {
		s.notify(r, tenant, "*", "SCREENING_MATCH",
			"OFAC screening hit: "+in.Name+" (score "+strconv.FormatFloat(top, 'f', 2, 64)+") — compliance review required before proceeding.", "")
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"screening_id": id, "matched": matched, "top_score": top,
		"matches": matches, "list_entity_count": loaded, "list_refreshed_at": refreshed,
	})
}

// screenOnboardingSubject: hooked from decideApplication on APPROVE — the
// onboarding decision is never blocked (warn-only), but every approval carries
// a screening record; a hit broadcasts to staff.
func (s *server) screenOnboardingSubject(r *http.Request, tenant, subjectType, subjectRef, name string) {
	sdnList.RLock()
	loaded := len(sdnList.entities)
	refreshed := sdnList.refreshedAt
	sdnList.RUnlock()
	if loaded == 0 || strings.TrimSpace(name) == "" {
		return // no list, no screening — visible via /screening/status
	}
	matched, top, matches := screenName(name)
	if len(matches) > 10 {
		matches = matches[:10]
	}
	mj, _ := json.Marshal(matches)
	_, _ = s.db.Exec(r.Context(), `
		INSERT INTO public.screening_checks
		  (tenant, subject_type, subject_ref, query_name, matched, top_score, matches, list_entity_count, list_refreshed_at, screened_by)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,'system:onboarding')`,
		tenant, subjectType, subjectRef, name, matched, top, mj, loaded, refreshed)
	if matched {
		s.notify(r, tenant, "*", "SCREENING_MATCH",
			"OFAC screening hit on approved "+subjectType+" onboarding: "+name+" — compliance review required.", "")
	}
}
