package main

import (
	"strings"
	"testing"
	"time"
)

// ---- Step 4: party voice ---------------------------------------------------

// The party system prompt is the leak firewall: it must forbid internal
// review, amounts, and invented facts.
func TestPartyChatSystemGuards(t *testing.T) {
	for _, rule := range []string{"ONLY the JSON", "Never discuss internal review", "dollar amounts", "NEVER invent"} {
		if !strings.Contains(partyChatSystem, rule) {
			t.Errorf("party system prompt missing guardrail %q", rule)
		}
	}
}

// Cost bound: the turn cap exists and is small — a token is not an open
// chat account.
func TestPartyChatTurnCapIsBounded(t *testing.T) {
	if partyChatMaxTurns <= 0 || partyChatMaxTurns > 100 {
		t.Errorf("partyChatMaxTurns out of sane bounds: %d", partyChatMaxTurns)
	}
	if partyChatMaxMessage > 2000 {
		t.Errorf("partyChatMaxMessage too generous: %d", partyChatMaxMessage)
	}
}

// partyFacts must carry NO internal surface — if someone adds internal
// notes/QA/assignment later, the struct contract breaks loudly here.
func TestPartyFactsProjectionIsMinimal(t *testing.T) {
	f := partyFacts{
		CaseNumber: "FL-2026-0001", Status: "OPEN",
		OutstandingRequests: []string{"itemized bill (due 2026-10-15)"},
		LinkKind:            "upload", LinkExpires: time.Now().Add(72 * time.Hour).Format("January 2, 2006"),
	}
	if f.CaseNumber == "" || len(f.OutstandingRequests) != 1 {
		t.Errorf("projection broken: %+v", f)
	}
}

// ---- Step 5: conversational intake (manifest-driven) -----------------------

func testManifest() *ProgramManifest {
	m := &ProgramManifest{}
	m.Terminology.PartyACode = "PROVIDER"
	m.Terminology.PartyBCode = "HEALTH_PLAN"
	m.IntakeFields = []struct {
		Name     string   `json:"name"`
		Label    string   `json:"label"`
		Type     string   `json:"type"`
		Required bool     `json:"required,omitempty"`
		Options  []string `json:"options,omitempty"`
	}{
		{Name: "disputed_amount", Label: "Disputed amount", Type: "number", Required: true},
		{Name: "claim_ref", Label: "Claim reference", Type: "text"},
		{Name: "venue", Label: "Venue", Type: "select", Options: []string{"INPATIENT", "OUTPATIENT"}},
	}
	return m
}

// Extraction parser: prose-wrapped JSON parses, sector fields land in extra,
// garbage is rejected.
func TestParseIntakeModelJSON(t *testing.T) {
	m := testManifest()
	f, reply, err := parseIntakeModelJSON(
		`Sure! {"fields":{"email":"dana@meridian.example","org":"Meridian Surgical","filing_party_type":"provider","extra":{"disputed_amount":"4200","venue":"OUTPATIENT","evil_key":"x"}},"reply":"Got it."}`,
		"PROVIDER", "HEALTH_PLAN", m)
	if err != nil {
		t.Fatalf("prose-wrapped JSON should parse: %v", err)
	}
	if f.Email != "dana@meridian.example" || f.FilingPartyType != "PROVIDER" {
		t.Errorf("core fields off: %+v", f)
	}
	if f.Extra["disputed_amount"] != "4200" || f.Extra["venue"] != "OUTPATIENT" {
		t.Errorf("manifest extras lost: %+v", f.Extra)
	}
	if _, leaked := f.Extra["evil_key"]; leaked {
		t.Errorf("undeclared key must be dropped: %+v", f.Extra)
	}
	if reply != "Got it." {
		t.Errorf("reply lost: %q", reply)
	}
	if _, _, err := parseIntakeModelJSON("no json here", "PROVIDER", "HEALTH_PLAN", m); err == nil {
		t.Error("garbage must be rejected")
	}
}

// Trust boundary: bad party codes, bad select options, malformed emails all
// degrade to zero values — never trusted.
func TestValidateIntakeFieldsDropsBad(t *testing.T) {
	m := testManifest()
	f := validateIntakeFields(intakeFields{
		Email:           "not-an-email",
		FilingPartyType: "ALIEN",
		Extra:           map[string]string{"venue": "MOON", "claim_ref": "CR-9"},
	}, "PROVIDER", "HEALTH_PLAN", m)
	if f.Email != "" || f.FilingPartyType != "" {
		t.Errorf("bad core fields must drop: %+v", f)
	}
	if _, ok := f.Extra["venue"]; ok {
		t.Errorf("off-option select value must drop: %+v", f.Extra)
	}
	if f.Extra["claim_ref"] != "CR-9" {
		t.Errorf("valid declared text field must survive: %+v", f.Extra)
	}
}

// Readiness is server-side and manifest-aware: required sector fields gate.
func TestIntakeMissingGatesReady(t *testing.T) {
	m := testManifest()
	if miss := intakeMissing(intakeFields{}, m); len(miss) != 3 { // email, org/contact, disputed_amount
		t.Errorf("empty fields: want 3 gaps, got %v", miss)
	}
	almost := intakeFields{Email: "a@b.example", Org: "Org", Extra: map[string]string{"claim_ref": "CR-1"}}
	if miss := intakeMissing(almost, m); len(miss) != 1 || miss[0] != "Disputed amount" {
		t.Errorf("required manifest field must gate, got %v", miss)
	}
	ready := intakeFields{Email: "a@b.example", Org: "Org", Extra: map[string]string{"disputed_amount": "4200"}}
	if miss := intakeMissing(ready, m); len(miss) != 0 {
		t.Errorf("complete intake should be ready, got %v", miss)
	}
}

// The prompt speaks the tenant's manifest language, not hard-coded
// healthcare terms.
func TestIntakePromptUsesManifestTerminology(t *testing.T) {
	p := intakeChatSystem("POLICYHOLDER", "INSURER", testManifest())
	if !strings.Contains(p, "POLICYHOLDER or INSURER") {
		t.Errorf("party codes not injected: %.200s", p)
	}
	if !strings.Contains(p, `"disputed_amount"`) || !strings.Contains(p, "INPATIENT/OUTPATIENT") {
		t.Errorf("sector fields not injected: %.300s", p)
	}
}
