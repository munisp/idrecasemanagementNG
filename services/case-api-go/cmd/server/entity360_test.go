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
