// valuation.go — pluggable determination/valuation engines (NG phase 3).
//
// The most sector-specific question on the platform is "what is the disputed
// thing worth?" — QPA in NSA IDR, fee schedules in workers' comp, comparable
// sales in tax appeals, amount-of-loss in appraisal. That logic now sits
// behind one interface, selected per program by the manifest
// (determination.engine). Adding a sector = registering an engine here +
// naming it in the manifest; the evidence chain and audit trail are shared.
//
// Engines READ evidence and PROPOSE a valuation with its basis — the neutral
// still decides. Every computation is returned with its evidence so the
// determination letter can cite exactly what was used.
package main

import (
	"fmt"
	"net/http"
	"sort"
	"time"

	"github.com/go-chi/chi/v5"
)

// knownEngines is the registry consulted by manifest validation.
var knownEngines = map[string]bool{
	"qpa":          true, // NSA IDR: qualifying payment amount (stored on the case)
	"fee_schedule": true, // WC / fee-matrix programs: effective-dated rate lookup
	"comparable":   true, // tax/appraisal: median of the evidence set
	"final_offer":  true, // baseball arbitration: evidence summary over revealed offers
}

// Valuation is an engine's proposal plus its full evidence trail.
type Valuation struct {
	Engine      string         `json:"engine"`
	AmountCents int64          `json:"amount_cents"`
	Basis       string         `json:"basis"`    // human-readable justification for the letter
	Evidence    map[string]any `json:"evidence"` // inputs used (claim counts, rates, dates)
	ComputedAt  time.Time      `json:"computed_at"`
}

func (s *server) computeValuation(r *http.Request, tenant, caseID string) (*Valuation, error) {
	engine := "qpa" // legacy default: NSA programs before manifests
	if m, err := s.manifestFor(r.Context(), tenant); err != nil {
		return nil, err // invalid manifest: fail closed
	} else if m != nil && m.Determination.Engine != "" {
		engine = m.Determination.Engine
	}
	switch engine {
	case "qpa":
		return s.engineQPA(r, tenant, caseID)
	case "fee_schedule":
		return s.engineFeeSchedule(r, tenant, caseID)
	case "comparable":
		return s.engineComparable(r, tenant, caseID)
	case "final_offer":
		return s.engineFinalOffer(r, tenant, caseID)
	}
	return nil, fmt.Errorf("engine %q has no implementation", engine)
}

// engineQPA: NSA IDR — the qualifying payment amount recorded on the case.
func (s *server) engineQPA(r *http.Request, tenant, caseID string) (*Valuation, error) {
	var qpa int64
	if err := s.db.QueryRow(r.Context(), fmt.Sprintf(
		`SELECT coalesce(qpa_cents,0) FROM tenant_%s.cases WHERE id=$1`, sanitizeTenant(tenant)), caseID).Scan(&qpa); err != nil {
		return nil, err
	}
	return &Valuation{Engine: "qpa", AmountCents: qpa, ComputedAt: time.Now().UTC(),
		Basis:    "Qualifying payment amount (median in-network rate) as recorded at initiation",
		Evidence: map[string]any{"qpa_cents": qpa}}, nil
}

// engineFeeSchedule: workers'-comp / fee-matrix programs — each claim line's
// code is priced from the tenant's fee schedule in effect on the case's
// initiation date (rates are effective-dated; historical cases price against
// historical rates). Unpriced codes are reported, never silently zeroed.
func (s *server) engineFeeSchedule(r *http.Request, tenant, caseID string) (*Valuation, error) {
	var initiatedAt time.Time
	_ = s.db.QueryRow(r.Context(), fmt.Sprintf(
		`SELECT coalesce(opened_at, created_at, now()) FROM tenant_%s.cases WHERE id=$1`,
		sanitizeTenant(tenant)), caseID).Scan(&initiatedAt)
	rows, err := s.db.Query(r.Context(), `
		SELECT c.cpt, coalesce(f.amount_cents, -1)
		FROM public.case_claims c
		LEFT JOIN public.fee_schedules f
		  ON f.tenant=c.tenant AND f.code=c.cpt
		 AND f.effective_from <= $3::date
		 AND (f.effective_to IS NULL OR f.effective_to >= $3::date)
		WHERE c.tenant=$1 AND c.case_id=$2`, tenant, caseID, initiatedAt)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var total int64
	priced, unpriced := 0, []string{}
	for rows.Next() {
		var code string
		var amount int64
		if err := rows.Scan(&code, &amount); err != nil {
			return nil, err
		}
		if amount < 0 {
			unpriced = append(unpriced, code)
		} else {
			total += amount
			priced++
		}
	}
	basis := fmt.Sprintf("Fee schedule in effect %s: %d line(s) priced", initiatedAt.Format("2006-01-02"), priced)
	if len(unpriced) > 0 {
		basis += fmt.Sprintf("; %d code(s) NOT in schedule — manual pricing required", len(unpriced))
	}
	return &Valuation{Engine: "fee_schedule", AmountCents: total, ComputedAt: time.Now().UTC(),
		Basis: basis,
		Evidence: map[string]any{"priced_lines": priced, "unpriced_codes": unpriced,
			"schedule_as_of": initiatedAt.Format("2006-01-02")}}, nil
}

// engineComparable: tax/appraisal — median of the billed amounts on the
// case's claim lines (the evidence set), reported with the full distribution.
func (s *server) engineComparable(r *http.Request, tenant, caseID string) (*Valuation, error) {
	rows, err := s.db.Query(r.Context(), `
		SELECT billed_cents FROM public.case_claims
		WHERE tenant=$1 AND case_id=$2 AND billed_cents > 0 ORDER BY billed_cents`, tenant, caseID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var vals []int64
	for rows.Next() {
		var v int64
		if err := rows.Scan(&v); err != nil {
			return nil, err
		}
		vals = append(vals, v)
	}
	if len(vals) == 0 {
		return &Valuation{Engine: "comparable", ComputedAt: time.Now().UTC(),
			Basis: "No evidence lines on the case — valuation requires at least one comparable",
			Evidence: map[string]any{"comparables": 0}}, nil
	}
	sort.Slice(vals, func(i, j int) bool { return vals[i] < vals[j] })
	median := vals[len(vals)/2]
	if len(vals)%2 == 0 {
		median = (vals[len(vals)/2-1] + vals[len(vals)/2]) / 2
	}
	return &Valuation{Engine: "comparable", AmountCents: median, ComputedAt: time.Now().UTC(),
		Basis:    fmt.Sprintf("Median of %d evidence line(s) (range $%d.%02d–$%d.%02d)", len(vals), vals[0]/100, vals[0]%100, vals[len(vals)-1]/100, vals[len(vals)-1]%100),
		Evidence: map[string]any{"comparables": len(vals), "min_cents": vals[0], "max_cents": vals[len(vals)-1], "median_cents": median}}, nil
}

// engineFinalOffer: baseball arbitration — the neutral selects one of the
// parties' offers; amounts stay vault-sealed until lawful reveal. The engine
// contributes the procedural evidence summary (offer counts, reveal state),
// not a number: the selection itself is the neutral's statutory act.
func (s *server) engineFinalOffer(r *http.Request, tenant, caseID string) (*Valuation, error) {
	var total, revealed int
	if err := s.db.QueryRow(r.Context(), fmt.Sprintf(`
		SELECT count(*), count(*) FILTER (WHERE revealed)
		FROM tenant_%s.sealed_offers WHERE case_id=$1`, sanitizeTenant(tenant)), caseID).
		Scan(&total, &revealed); err != nil {
		return nil, err
	}
	return &Valuation{Engine: "final_offer", ComputedAt: time.Now().UTC(),
		Basis: fmt.Sprintf("%d sealed offer(s) submitted, %d revealed — neutral selects between the parties' final offers", total, revealed),
		Evidence: map[string]any{"offers_submitted": total, "offers_revealed": revealed,
			"selection": "neutral"}}, nil
}

// getValuation: GET /v1/tenants/{tenant}/cases/{caseId}/valuation — run the
// program's determination engine and return the proposal + evidence.
func (s *server) getValuation(w http.ResponseWriter, r *http.Request) {
	tenant := r.Context().Value(ctxTenant{}).(string)
	caseID := chi.URLParam(r, "caseId")
	v, err := s.computeValuation(r, tenant, caseID)
	if err != nil {
		http.Error(w, fmt.Sprintf(`{"error":%q}`, err.Error()), http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, v)
}
