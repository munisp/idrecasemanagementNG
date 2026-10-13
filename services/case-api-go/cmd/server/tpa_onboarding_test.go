package main

import "testing"

func TestNPIValid(t *testing.T) {
	// 1234567893 is the canonical valid NPI example from CMS docs.
	if !npiValid("1234567893") {
		t.Fatal("canonical valid NPI rejected")
	}
	for _, bad := range []string{"", "123456789", "12345678901", "1234567894", "12345678a3"} {
		if npiValid(bad) {
			t.Fatalf("invalid NPI %q accepted", bad)
		}
	}
}

func TestTINValid(t *testing.T) {
	if !tinValid("123456789") {
		t.Fatal("9-digit TIN rejected")
	}
	for _, bad := range []string{"", "12345678", "1234567890", "1234 6789", "abcdefghi"} {
		if tinValid(bad) {
			t.Fatalf("invalid TIN %q accepted", bad)
		}
	}
}

func TestRoutingValid(t *testing.T) {
	// 021000021 (JPMorgan Chase) — canonical valid ABA number.
	if !routingValid("021000021") {
		t.Fatal("canonical valid routing number rejected")
	}
	for _, bad := range []string{"", "02100002", "0210000211", "021000022", "02100002a"} {
		if routingValid(bad) {
			t.Fatalf("invalid routing %q accepted", bad)
		}
	}
}

func TestTPASlotFromFilename(t *testing.T) {
	cases := map[string]string{
		"EOB_claim42.pdf":              "EOB",
		"itemized_bill.pdf":            "ITEMIZED_BILL",
		"proof_of_coverage.pdf":        "PROOF_OF_COVERAGE",
		"open_negotiation_notice.pdf":  "OPEN_NEGOTIATION_NOTICE",
		"offer_substantiation.pdf":     "OFFER_SUBSTANTIATION",
		"idre_attestation.pdf":         "IDRE_ATTESTATION",
		"random_notes.txt":             "",
	}
	for name, want := range cases {
		if got := tpaSlotFromFilename(name); got != want {
			t.Fatalf("%s: got %q want %q", name, got, want)
		}
	}
}

func TestTPASlotsComplete(t *testing.T) {
	if len(tpaSlots) != 6 {
		t.Fatalf("expected 6 required slots, got %d", len(tpaSlots))
	}
	seen := map[string]bool{}
	for _, s := range tpaSlots {
		if seen[s] || !tpaSlotValid(s) {
			t.Fatalf("slot %q duplicate or invalid", s)
		}
		seen[s] = true
	}
}
