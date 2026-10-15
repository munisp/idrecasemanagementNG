package main

import "testing"

func TestNormEntityName(t *testing.T) {
	cases := map[string]string{
		"St. Mary's Hospital, LLC": "st mary s hospital llc",
		"  ACME   Health  Plan  ":  "acme health plan",
		"O'Brien & Sons, Inc.":     "o brien sons inc",
		"":                         "",
	}
	for in, want := range cases {
		if got := normEntityName(in); got != want {
			t.Errorf("normEntityName(%q) = %q, want %q", in, got, want)
		}
	}
	// The resolver's promise: punctuation/legal-suffix variants of the same
	// name normalize to overlapping token strings (suggestion candidates).
	if normEntityName("St Marys Hospital") == "" {
		t.Error("normalized name must never be empty for a real name")
	}
}

func TestRegexpDigits(t *testing.T) {
	if got := regexpDigits("12-3456789"); got != "123456789" {
		t.Errorf("regexpDigits EIN = %q", got)
	}
	if got := regexpDigits("(800) 555-1234"); got != "8005551234" {
		t.Errorf("regexpDigits phone = %q", got)
	}
	if got := regexpDigits(""); got != "" {
		t.Errorf("regexpDigits empty = %q", got)
	}
}

// NG: the feature-flag vocabulary lives in the manifest validator
// (manifest.go knownFeatures); entity360 was added there. Absent = ON, the
// server gate 404s when off.

// The resolver only links automatically on identity keys; the source-table
// vocabulary is closed so no caller can point ensureEntity at an arbitrary
// table.
func TestEntity360SourcesClosed(t *testing.T) {
	for _, tbl := range []string{"accounts", "tpas", "tpa_clients", "health_plans", "hp_parties"} {
		if _, ok := entity360Sources[tbl]; !ok {
			t.Errorf("source %s missing", tbl)
		}
	}
	if _, ok := entity360Sources["cases"]; ok {
		t.Error("cases must never be a linkable source — it is tenant-scoped data, not an organization registry")
	}
}

func TestCaseStage(t *testing.T) {
	cases := map[string]int{
		"OPEN": 0, "SUBMITTED": 0,
		"IN_NEGOTIATION": 1, "SETTLED_IN_NEGOTIATION": 1,
		"IN_IDRE": 2, "DISPUTE_REVIEW": 2, "Selected for IDRE": 2,
		"DECIDED": 3, "Determined": 3,
		"Decided - Invoice Paid": 4, "Invoice Paid": 4,
		"CLOSED": 5, "Dismissed": 5, "Withdrawn": 5, "Ineligible": 5, "Plan Opt-Out": 5,
	}
	for st, want := range cases {
		if got := caseStage(st); got != want {
			t.Errorf("caseStage(%q) = %d, want %d", st, got, want)
		}
	}
}

func TestCaseFunnel(t *testing.T) {
	funnel := caseFunnel([]map[string]any{
		{"status": "OPEN", "count": 10},
		{"status": "IN_NEGOTIATION", "count": 4},
		{"status": "IN_IDRE", "count": 3},
		{"status": "Invoice Paid", "count": 2},
		{"status": "Dismissed", "count": 1},
	})
	if len(funnel) != 6 {
		t.Fatalf("funnel stages = %d, want 6", len(funnel))
	}
	if funnel[0]["reached"] != 20 || funnel[4]["reached"] != 2 || funnel[5]["reached"] != 1 {
		t.Errorf("reached = %v/%v/%v, want 20/2/1", funnel[0]["reached"], funnel[4]["reached"], funnel[5]["reached"])
	}
	conv, ok := funnel[1]["conversion_pct"].(float64)
	if !ok || conv < 44.9 || conv > 45.1 {
		t.Errorf("conversion_pct = %v, want 45.0", funnel[1]["conversion_pct"])
	}
}

func TestTokenJaccard(t *testing.T) {
	a := entityNameTokens("Gulf Coast Emergency Physicians, P.A.")
	b := entityNameTokens("GULF COAST EMERGENCY PHYSICIANS PA")
	if got := tokenJaccard(a, b); got != 1.0 {
		t.Errorf("identical token sets: jaccard = %v, want 1.0", got)
	}
	c := entityNameTokens("Bayside Pediatrics")
	if got := tokenJaccard(a, c); got != 0 {
		t.Errorf("disjoint token sets: jaccard = %v, want 0", got)
	}
}
