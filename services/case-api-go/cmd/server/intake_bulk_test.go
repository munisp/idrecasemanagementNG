package main

import (
	"encoding/json"
	"testing"
)

// testManifest() (conversation45_test.go) is reused for the healthcare-shaped
// fixture; this one builds a foreign-sector manifest via JSON so anonymous
// struct types round-trip exactly as the loader produces them.
func bulkForeignManifest(t *testing.T) *ProgramManifest {
	t.Helper()
	m := &ProgramManifest{}
	if err := json.Unmarshal([]byte(`{
		"program": "test", "version": "1", "sector": "insurance",
		"terminology": {"party_a_code": "POLICYHOLDER", "party_b_code": "INSURER"},
		"intake_fields": [{"name": "policy_no", "label": "Policy", "type": "text", "required": true}]
	}`), m); err != nil {
		t.Fatalf("fixture manifest: %v", err)
	}
	return m
}

func TestBulkValidateRequiresEmail(t *testing.T) {
	if msg := validateBulkIntakeItem(bulkIntakeItem{Email: "nope"}, "PROVIDER", "HEALTH_PLAN", nil); msg == "" {
		t.Fatal("bad email must be rejected")
	}
}

func TestBulkValidateManifestPartyCodes(t *testing.T) {
	// A tenant whose manifest names different parties (POLICYHOLDER/INSURER)
	// must reject healthcare-legacy codes.
	m := bulkForeignManifest(t)
	if msg := validateBulkIntakeItem(bulkIntakeItem{Email: "a@b.c", FilingPartyType: "PROVIDER"}, "POLICYHOLDER", "INSURER", m); msg == "" {
		t.Fatal("foreign party code must be rejected under this manifest")
	}
	if msg := validateBulkIntakeItem(bulkIntakeItem{Email: "a@b.c", FilingPartyType: "INSURER", Fields: map[string]any{"policy_no": "P-1"}}, "POLICYHOLDER", "INSURER", m); msg != "" {
		t.Fatalf("manifest party code rejected: %s", msg)
	}
}

func TestBulkValidateRequiredManifestFields(t *testing.T) {
	m := testManifest()
	if msg := validateBulkIntakeItem(bulkIntakeItem{Email: "a@b.c"}, "PROVIDER", "HEALTH_PLAN", m); msg == "" {
		t.Fatal("missing required manifest field must be rejected")
	}
	it := bulkIntakeItem{Email: "a@b.c", Fields: map[string]any{"disputed_amount": 420000}}
	if msg := validateBulkIntakeItem(it, "PROVIDER", "HEALTH_PLAN", m); msg != "" {
		t.Fatalf("complete row rejected: %s", msg)
	}
}

func TestBulkMaxItemsGuard(t *testing.T) {
	if bulkIntakeMaxItems <= 0 || bulkIntakeMaxItems > 1000 {
		t.Fatalf("bulkIntakeMaxItems out of sane range: %d", bulkIntakeMaxItems)
	}
}
