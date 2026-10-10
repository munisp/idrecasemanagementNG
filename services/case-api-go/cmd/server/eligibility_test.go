package main

// Eligibility decision-core tests. The vectors come straight from the FL
// AHCA SOP threshold matrix and filing-window rule seeded in
// scripts/program-rules.sql — if the SOP changes, the seed changes, and
// these vectors must change with it. That coupling is intentional: the test
// file is the executable copy of the SOP's eligibility page.

import (
	"testing"
	"time"
)

func flTestConfig() *ProgramConfig {
	var cfg ProgramConfig
	cfg.Eligibility.Thresholds = []ThresholdRule{
		{ProviderType: "hospital_inpatient", Contracted: boolP(true), MinCents: 2500000},
		{ProviderType: "hospital_inpatient", Contracted: boolP(false), MinCents: 1000000},
		{ProviderType: "hospital_outpatient", Contracted: boolP(true), MinCents: 1000000},
		{ProviderType: "hospital_outpatient", Contracted: boolP(false), MinCents: 300000},
		{ProviderType: "physician_dentist", MinCents: 50000},
		{ProviderType: "rural_hospital", MinCents: 0},
		{ProviderType: "other", MinCents: 0},
	}
	cfg.Eligibility.FilingWindowMonths = 12
	cfg.Eligibility.Reasons = []string{"late_payment_only", "interest_only", "medicare_grievance",
		"plan_not_fl_regulated", "provider_not_fl_licensed", "over_12_months", "below_threshold"}
	cfg.Statuses.Internal = []string{"Initial Review Pending", "QA Initial Review", "Review In Progress", "Final Order Issued"}
	return &cfg
}

func boolP(b bool) *bool { return &b }

func TestEligibilitySOPVectors(t *testing.T) {
	cfg := flTestConfig()
	vectors := []struct {
		name       string
		in         eligibilityInput
		wantResult string
		wantReason string
	}{
		{"inpatient contracted at threshold", eligibilityInput{ProviderType: "hospital_inpatient", Contracted: boolP(true), DisputedAmountCents: 2500000}, "ELIGIBLE", ""},
		{"inpatient contracted below threshold", eligibilityInput{ProviderType: "hospital_inpatient", Contracted: boolP(true), DisputedAmountCents: 2499999}, "INELIGIBLE", "below_threshold"},
		{"inpatient non-contracted threshold", eligibilityInput{ProviderType: "hospital_inpatient", Contracted: boolP(false), DisputedAmountCents: 1000000}, "ELIGIBLE", ""},
		{"outpatient non-contracted below", eligibilityInput{ProviderType: "hospital_outpatient", Contracted: boolP(false), DisputedAmountCents: 299999}, "INELIGIBLE", "below_threshold"},
		{"physician at $500", eligibilityInput{ProviderType: "physician_dentist", DisputedAmountCents: 50000}, "ELIGIBLE", ""},
		{"physician at $499.99", eligibilityInput{ProviderType: "physician_dentist", DisputedAmountCents: 49999}, "INELIGIBLE", "below_threshold"},
		{"rural hospital no minimum", eligibilityInput{ProviderType: "rural_hospital", DisputedAmountCents: 1}, "ELIGIBLE", ""},
		{"explicit flag wins over amount", eligibilityInput{ProviderType: "rural_hospital", DisputedAmountCents: 99999999, Flags: []string{"plan_not_fl_regulated"}}, "INELIGIBLE", "plan_not_fl_regulated"},
		{"medicare grievance excluded", eligibilityInput{ProviderType: "other", DisputedAmountCents: 100, Flags: []string{"medicare_grievance"}}, "INELIGIBLE", "medicare_grievance"},
		{"AOR hold regardless of merits", eligibilityInput{ProviderType: "other", DisputedAmountCents: 100, AORValid: boolP(false)}, "HOLD_AOR", "aor_invalid_pending_attorney"},
	}
	for _, v := range vectors {
		t.Run(v.name, func(t *testing.T) {
			res, reason, _ := evalEligibility(cfg, v.in)
			if res != v.wantResult || reason != v.wantReason {
				t.Fatalf("got %s/%s, want %s/%s", res, reason, v.wantResult, v.wantReason)
			}
		})
	}
}

func TestEligibilityFilingWindow(t *testing.T) {
	cfg := flTestConfig()
	old := time.Now().AddDate(-1, -1, 0).Format("2006-01-02") // 13 months ago
	res, reason, ev := evalEligibility(cfg, eligibilityInput{
		ProviderType: "other", DisputedAmountCents: 100, FinalDeterminationAt: old})
	if res != "INELIGIBLE" || reason != "over_filing_window" {
		t.Fatalf("stale filing: got %s/%s", res, reason)
	}
	if ev["filing_deadline"] == nil {
		t.Fatal("deadline not recorded in evidence")
	}
	recent := time.Now().AddDate(0, -2, 0).Format("2006-01-02")
	res, _, _ = evalEligibility(cfg, eligibilityInput{
		ProviderType: "other", DisputedAmountCents: 100, FinalDeterminationAt: recent})
	if res != "ELIGIBLE" {
		t.Fatalf("timely filing: got %s", res)
	}
}

func TestEligibilityBorderline(t *testing.T) {
	cfg := flTestConfig()
	// $540 vs the $500 physician threshold: +8% — borderline.
	in := eligibilityInput{ProviderType: "physician_dentist", DisputedAmountCents: 54000}
	_, _, ev := evalEligibility(cfg, in)
	if got := eligibilityBorderline(cfg, in, ev); len(got) != 1 {
		t.Fatalf("expected borderline, got %v", got)
	}
	// $460: -8% below — also borderline (and INELIGIBLE, which is exactly the
	// call dual control exists to catch).
	in = eligibilityInput{ProviderType: "physician_dentist", DisputedAmountCents: 46000}
	res, _, ev := evalEligibility(cfg, in)
	if res != "INELIGIBLE" || len(eligibilityBorderline(cfg, in, ev)) != 1 {
		t.Fatalf("below-margin case: res=%s borderline=%v", res, eligibilityBorderline(cfg, in, ev))
	}
	// $1,000: far clear of the threshold — not borderline.
	in = eligibilityInput{ProviderType: "physician_dentist", DisputedAmountCents: 100000}
	_, _, ev = evalEligibility(cfg, in)
	if got := eligibilityBorderline(cfg, in, ev); len(got) != 0 {
		t.Fatalf("unexpected borderline: %v", got)
	}
	// Deadline within 30 days — borderline.
	in = eligibilityInput{ProviderType: "other", DisputedAmountCents: 100,
		FinalDeterminationAt: time.Now().AddDate(0, -11, -15).Format("2006-01-02")}
	_, _, ev = evalEligibility(cfg, in)
	if got := eligibilityBorderline(cfg, in, ev); len(got) != 1 {
		t.Fatalf("deadline-margin case: %v", got)
	}
}

func TestEligibilityRuleVersionStable(t *testing.T) {
	cfg := flTestConfig()
	v1 := eligibilityRuleVersion(cfg)
	v2 := eligibilityRuleVersion(cfg)
	if v1 == "" || v1 != v2 {
		t.Fatalf("rule version unstable: %q vs %q", v1, v2)
	}
	cfg.Eligibility.Thresholds[4].MinCents = 60000
	if eligibilityRuleVersion(cfg) == v1 {
		t.Fatal("rule version did not change when thresholds changed")
	}
}

func TestPastEligibilityGate(t *testing.T) {
	cfg := flTestConfig()
	if pastEligibilityGate(cfg, "Initial Review Pending") || pastEligibilityGate(cfg, "QA Initial Review") {
		t.Fatal("intake-phase statuses must not trip the gate")
	}
	if !pastEligibilityGate(cfg, "Review In Progress") || !pastEligibilityGate(cfg, "Final Order Issued") {
		t.Fatal("post-review statuses must trip the gate")
	}
	if pastEligibilityGate(nil, "Review In Progress") {
		t.Fatal("federal tenants (no program) have no gate")
	}
}

func TestValidOverrideReason(t *testing.T) {
	if !validOverrideReason("ten chars ok") {
		t.Fatal("10 chars is the floor")
	}
	if validOverrideReason("too short") {
		t.Fatal("9 chars must be rejected")
	}
	if validOverrideReason("  no  ") {
		t.Fatal("whitespace is not a reason")
	}
}
