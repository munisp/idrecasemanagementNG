package main

import (
	"encoding/json"
	"fmt"
	"net/http"

	"github.com/go-chi/chi/v5"
)

// benchmarks.go — downstream analytics riding on data doc-intel already
// lifts onto cases.details (extracted CPT/HCPCS codes, QPA, payer).
//
// caseBenchmarks: per-code QPA distribution (n, p25, median, p75) across the
// tenant's own cases carrying that code. Arbitrators get "this offer is
// 1.4x median QPA for 99285" instead of gut feel — the statutory criteria
// (45 CFR 149.520(b)(2)(ii)) weigh the QPA directly. Tenants stay isolated:
// a state's benchmark is built only from that state's cases.
//
// batchableCases: 45 CFR 149.510(c) lets a party batch disputes sharing the
// same payer and same/similar service codes. With extracted CPT codes on the
// case row this is a computation, not a manual review: same payer_id AND at
// least one CPT/HCPCS code in common, limited to cases still early enough to
// batch (before the offer window opens).

// percentileStats computes p25/median/p75 for a sorted ascending slice.
// Median averages the two middle ranks on even counts; quartiles use
// nearest-rank.
func percentileStats(sorted []int64) (int64, int64, int64) {
	n := len(sorted)
	if n == 0 {
		return 0, 0, 0
	}
	pick := func(q float64) int64 {
		idx := int(q*float64(n-1) + 0.5)
		if idx < 0 {
			idx = 0
		}
		if idx >= n {
			idx = n - 1
		}
		return sorted[idx]
	}
	median := pick(0.50)
	if n%2 == 0 {
		median = (sorted[n/2-1] + sorted[n/2]) / 2
	}
	return pick(0.25), median, pick(0.75)
}

// benchmarkRoles: NG's leaner role set — no CODER/NURSE_PHYSICIAN/PM.
var benchmarkRoles = []string{"CASE_MANAGER", "DOCTOR", "NURSE", "ATTORNEY", "ARBITRATOR", "FEDERAL_ADMIN", "PLATFORM_ADMIN", serviceRole}

func (s *server) caseBenchmarks(w http.ResponseWriter, r *http.Request) {
	p := r.Context().Value(ctxPrincipal{}).(principal)
	tenant := r.Context().Value(ctxTenant{}).(string)
	caseID := chi.URLParam(r, "caseId")
	if !hasAnyRole(p, benchmarkRoles...) {
		http.Error(w, `{"error":"forbidden"}`, http.StatusForbidden)
		return
	}
	// The case's own extracted codes drive the benchmark set.
	rows, err := s.queryRows(r, fmt.Sprintf(`SELECT details FROM tenant_%s.cases WHERE id=$1`, sanitizeTenant(tenant)), caseID)
	if err != nil || len(rows) == 0 {
		http.Error(w, `{"error":"case not found"}`, http.StatusNotFound)
		return
	}
	var details []byte
	switch v := rows[0]["details"].(type) {
	case []byte:
		details = v
	case string:
		details = []byte(v)
	}
	var d struct {
		CPT   []string `json:"cpt_codes"`
		HCPCS []string `json:"hcpcs_codes"`
	}
	_ = json.Unmarshal(details, &d)
	codes := append(append([]string{}, d.CPT...), d.HCPCS...)
	if len(codes) == 0 {
		writeJSON(w, http.StatusOK, map[string]any{"benchmarks": []any{},
			"note": "no extracted procedure codes on this case yet — benchmarks appear after document analysis"})
		return
	}
	type bench struct {
		Code      string `json:"code"`
		N         int    `json:"n"`
		P25Cents  int64  `json:"p25_cents"`
		MedianCents int64 `json:"median_cents"`
		P75Cents  int64 `json:"p75_cents"`
	}
	out := []bench{}
	for _, code := range codes {
		rows, err := s.queryRows(r, fmt.Sprintf(`
			SELECT percentile_cont(0.25) WITHIN GROUP (ORDER BY qpa_cents) AS p25,
			       percentile_cont(0.50) WITHIN GROUP (ORDER BY qpa_cents) AS median,
			       percentile_cont(0.75) WITHIN GROUP (ORDER BY qpa_cents) AS p75,
			       count(*) AS n
			FROM tenant_%s.cases
			WHERE qpa_cents IS NOT NULL AND qpa_cents > 0
			  AND (details->'cpt_codes' ? $1 OR details->'hcpcs_codes' ? $1)`, sanitizeTenant(tenant)), code)
		if err != nil || len(rows) == 0 {
			continue
		}
		row := rows[0]
		n := int(toInt64(row["n"]))
		if n == 0 {
			continue
		}
		out = append(out, bench{
			Code:        code,
			N:           n,
			P25Cents:    toInt64(row["p25"]),
			MedianCents: toInt64(row["median"]),
			P75Cents:    toInt64(row["p75"]),
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"benchmarks": out, "codes": codes})
}

func (s *server) batchableCases(w http.ResponseWriter, r *http.Request) {
	p := r.Context().Value(ctxPrincipal{}).(principal)
	tenant := r.Context().Value(ctxTenant{}).(string)
	caseID := chi.URLParam(r, "caseId")
	if !hasAnyRole(p, benchmarkRoles...) {
		http.Error(w, `{"error":"forbidden"}`, http.StatusForbidden)
		return
	}
	rows, err := s.queryRows(r, fmt.Sprintf(`SELECT payer_id, details FROM tenant_%s.cases WHERE id=$1`, sanitizeTenant(tenant)), caseID)
	if err != nil || len(rows) == 0 {
		http.Error(w, `{"error":"case not found"}`, http.StatusNotFound)
		return
	}
	payer, _ := rows[0]["payer_id"].(string)
	var d struct {
		CPT   []string `json:"cpt_codes"`
		HCPCS []string `json:"hcpcs_codes"`
	}
	if b, ok := rows[0]["details"].([]byte); ok {
		_ = json.Unmarshal(b, &d)
	} else if str, ok := rows[0]["details"].(string); ok {
		_ = json.Unmarshal([]byte(str), &d)
	}
	codes := append(append([]string{}, d.CPT...), d.HCPCS...)
	if payer == "" || len(codes) == 0 {
		writeJSON(w, http.StatusOK, map[string]any{"batchable": []any{},
			"note": "batching needs a payer and extracted procedure codes on this case"})
		return
	}
	// Same payer, at least one procedure code in common, and still early
	// enough to lawfully batch (before sealed offers open).
	cands, err := s.queryRows(r, fmt.Sprintf(`
		SELECT id, case_number, status, service_line, qpa_cents, opened_at,
		       details->'cpt_codes' AS cpt_codes
		FROM tenant_%s.cases
		WHERE id <> $1 AND payer_id = $2
		  AND status NOT LIKE 'CLOSED%%' AND status NOT IN
		      ('OFFER_WINDOW_OPEN','OFFERS_REVEALED','SELECTION_FINALIZED','DETERMINED','SETTLED_IN_NEGOTIATION')
		  AND (details->'cpt_codes' ?| $3 OR details->'hcpcs_codes' ?| $3)
		ORDER BY opened_at DESC LIMIT 100`, sanitizeTenant(tenant)),
		caseID, payer, codes)
	if err != nil {
		http.Error(w, `{"error":"query failed"}`, http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"batchable": cands, "payer_id": payer, "codes": codes})
}
