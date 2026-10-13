package main

import (
	"encoding/json"
	"fmt"
	"net/http"

	"github.com/go-chi/chi/v5"
)

// analytics.go — downstream intelligence riding on cases.details (extracted
// codes, denial dates, flags) plus the offers/determinations record. Every
// endpoint is feature-gated and tenant-isolated; parties never see any of
// this — it would tilt negotiations.

// winProbability: historical outcomes for disputes with the same payer and
// overlapping procedure codes, from actual determinations (which party's
// sealed offer was selected). Guards against small-sample theater: under 5
// determinations the response says so instead of quoting a number.
func (s *server) winProbability(w http.ResponseWriter, r *http.Request) {
	p := r.Context().Value(ctxPrincipal{}).(principal)
	tenant := r.Context().Value(ctxTenant{}).(string)
	caseID := chi.URLParam(r, "caseId")
	if !s.featureGate(w, r, tenant, "win_probability") {
		return
	}
	if !hasAnyRole(p, benchmarkRoles...) {
		http.Error(w, `{"error":"forbidden"}`, http.StatusForbidden)
		return
	}
	rows, err := s.queryRows(r, fmt.Sprintf(
		`SELECT payer_id, details FROM tenant_%s.cases WHERE id=$1`, sanitizeTenant(tenant)), caseID)
	if err != nil || len(rows) == 0 {
		http.Error(w, `{"error":"case not found"}`, http.StatusNotFound)
		return
	}
	payer, _ := rows[0]["payer_id"].(string)
	var d struct {
		CPT   []string `json:"cpt_codes"`
		HCPCS []string `json:"hcpcs_codes"`
	}
	switch v := rows[0]["details"].(type) {
	case []byte:
		_ = json.Unmarshal(v, &d)
	case string:
		_ = json.Unmarshal([]byte(v), &d)
	}
	codes := append(append([]string{}, d.CPT...), d.HCPCS...)
	if payer == "" || len(codes) == 0 {
		writeJSON(w, http.StatusOK, map[string]any{"sufficient": false,
			"note": "win-probability triage needs a payer and extracted procedure codes on this case"})
		return
	}
	stats, err := s.queryRows(r, fmt.Sprintf(`
		SELECT o.party_id, count(*) AS n
		FROM tenant_%s.cases c
		JOIN public.determinations d ON d.case_id = c.id AND d.tenant = $1
		JOIN public.offers o ON o.id = d.selected_offer_id
		WHERE c.tenant = $1 AND c.payer_id = $2
		  AND (c.details->'cpt_codes' ?| $3 OR c.details->'hcpcs_codes' ?| $3)
		GROUP BY o.party_id`, sanitizeTenant(tenant)), tenant, payer, codes)
	if err != nil {
		http.Error(w, `{"error":"query failed"}`, http.StatusInternalServerError)
		return
	}
	var initiating, responding int64
	for _, row := range stats {
		if fmt.Sprint(row["party_id"]) == "INITIATING" {
			initiating = toInt64(row["n"])
		} else if fmt.Sprint(row["party_id"]) == "RESPONDING" {
			responding = toInt64(row["n"])
		}
	}
	n := initiating + responding
	if n < 5 {
		writeJSON(w, http.StatusOK, map[string]any{"sufficient": false, "n": n,
			"note": "fewer than 5 comparable determinations on record — no statistically meaningful read yet"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"sufficient": true, "n": n,
		"initiating_win_pct": float64(initiating) / float64(n) * 100,
		"responding_win_pct": float64(responding) / float64(n) * 100,
		"basis":              "same payer + overlapping extracted procedure codes",
	})
}

// payerAnalytics: denial-pattern profile per payer — dispute volume, how
// often their remits trip bundling/necessity/duplicate screens, average
// denial-to-filing gap. Oversight-grade: STATE_AUDITOR included.
func (s *server) payerAnalytics(w http.ResponseWriter, r *http.Request) {
	p := r.Context().Value(ctxPrincipal{}).(principal)
	tenant := r.Context().Value(ctxTenant{}).(string)
	if !s.featureGate(w, r, tenant, "payer_analytics") {
		return
	}
	if !hasAnyRole(p, append(append([]string{}, benchmarkRoles...), "STATE_AUDITOR")...) {
		http.Error(w, `{"error":"forbidden"}`, http.StatusForbidden)
		return
	}
	rows, err := s.queryRows(r, fmt.Sprintf(`
		SELECT payer_id,
		       count(*) AS disputes,
		       count(*) FILTER (WHERE status IN ('DETERMINED','CLOSED_PAID')) AS resolved,
		       count(*) FILTER (WHERE coalesce(details->>'bundling_suspected','') = 'true') AS bundling_flags,
		       count(*) FILTER (WHERE details ? 'necessity_flags') AS necessity_flags,
		       count(*) FILTER (WHERE details ? 'duplicate_claim_with') AS duplicate_flags,
		       round(avg(disputed_amount_cents)) AS avg_disputed_cents,
		       round(avg((opened_at::date - nullif(details->>'eob_denial_date','')::date))) AS avg_denial_to_filing_days
		FROM tenant_%s.cases
		WHERE payer_id IS NOT NULL AND payer_id <> ''
		GROUP BY payer_id ORDER BY disputes DESC LIMIT 200`, sanitizeTenant(tenant)))
	if err != nil {
		http.Error(w, `{"error":"query failed"}`, http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"payers": rows})
}

// fraudSignals: provider-level clusters of the automated tripwires. This is a
// SURFACE for SIU/auditor review — the platform flags patterns, humans make
// referrals; nothing here accuses anyone of anything.
func (s *server) fraudSignals(w http.ResponseWriter, r *http.Request) {
	p := r.Context().Value(ctxPrincipal{}).(principal)
	tenant := r.Context().Value(ctxTenant{}).(string)
	if !s.featureGate(w, r, tenant, "fraud_signals") {
		return
	}
	if !hasAnyRole(p, "STATE_AUDITOR", "FEDERAL_ADMIN", "PLATFORM_ADMIN") {
		http.Error(w, `{"error":"forbidden: oversight roles only"}`, http.StatusForbidden)
		return
	}
	rows, err := s.queryRows(r, fmt.Sprintf(`
		SELECT provider_id,
		       count(*) AS disputes,
		       count(*) FILTER (WHERE details ? 'necessity_flags') AS necessity_flags,
		       count(*) FILTER (WHERE details ? 'duplicate_claim_with') AS duplicate_claims,
		       count(*) FILTER (WHERE coalesce(details->>'bundling_suspected','') = 'true') AS bundling_flags,
		       round(avg(qpa_cents)) AS avg_qpa_cents
		FROM tenant_%s.cases
		WHERE provider_id IS NOT NULL AND provider_id <> ''
		GROUP BY provider_id
		HAVING count(*) FILTER (WHERE details ? 'necessity_flags')
		     + count(*) FILTER (WHERE details ? 'duplicate_claim_with')
		     + count(*) FILTER (WHERE coalesce(details->>'bundling_suspected','') = 'true') > 0
		ORDER BY (necessity_flags + duplicate_claims + bundling_flags) DESC LIMIT 200`,
		sanitizeTenant(tenant)))
	if err != nil {
		http.Error(w, `{"error":"query failed"}`, http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"providers": rows})
}

// networkAdequacy: out-of-network dispute concentration by service line and
// payer — the regulatory-evidence view of where networks are thin.
func (s *server) networkAdequacy(w http.ResponseWriter, r *http.Request) {
	p := r.Context().Value(ctxPrincipal{}).(principal)
	tenant := r.Context().Value(ctxTenant{}).(string)
	if !s.featureGate(w, r, tenant, "network_adequacy") {
		return
	}
	if !hasAnyRole(p, "STATE_AUDITOR", "FEDERAL_ADMIN", "PLATFORM_ADMIN", "CASE_MANAGER") {
		http.Error(w, `{"error":"forbidden"}`, http.StatusForbidden)
		return
	}
	rows, err := s.queryRows(r, fmt.Sprintf(`
		SELECT service_line, payer_id, count(*) AS oon_disputes,
		       round(avg(disputed_amount_cents)) AS avg_disputed_cents,
		       min(opened_at) AS first_seen, max(opened_at) AS latest
		FROM tenant_%s.cases
		WHERE service_line IS NOT NULL AND service_line <> ''
		GROUP BY service_line, payer_id ORDER BY oon_disputes DESC LIMIT 300`,
		sanitizeTenant(tenant)))
	if err != nil {
		http.Error(w, `{"error":"query failed"}`, http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"segments": rows})
}
