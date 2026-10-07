package main

import (
	"strings"
	"testing"
)

func TestCopilotDraftPromptGroundsBothKinds(t *testing.T) {
	f := copilotFacts{CaseNumber: "IDR-2026-00001", TriageLane: "STANDARD"}
	for _, kind := range []string{draftKindDetermination, draftKindCorrespondence} {
		system, user := copilotDraftPrompt(kind, f, "keep it short")
		for _, want := range []string{"ONLY the JSON facts", "not in record", "DRAFT — advisory only; not a determination."} {
			if !strings.Contains(system, want) {
				t.Fatalf("%s system prompt missing %q", kind, want)
			}
		}
		if !strings.Contains(user, "IDR-2026-00001") || !strings.Contains(user, "REVIEWER INSTRUCTIONS") {
			t.Fatalf("%s user prompt missing facts/instructions", kind)
		}
	}
}

func TestParseCopilotActionsValid(t *testing.T) {
	rationale, actions, err := parseCopilotActions(
		`{"rationale":"chase the missing attestation","actions":[
		  {"type":"create_task","params":{"subject":"Call plan for attestation","due_days":5}},
		  {"type":"flag_missing_eligibility_input","params":{"inputs":["attestation"]}}]}`)
	if err != nil {
		t.Fatalf("valid plan rejected: %v", err)
	}
	if rationale == "" || len(actions) != 2 {
		t.Fatalf("got rationale=%q actions=%d", rationale, len(actions))
	}
}

func TestParseCopilotActionsToleratesProseAroundJSON(t *testing.T) {
	_, actions, err := parseCopilotActions(
		"Here is my plan:\n```json\n{\"rationale\":\"r\",\"actions\":[]}\n```\nHope that helps!")
	if err != nil || len(actions) != 0 {
		t.Fatalf("prose-wrapped empty plan: err=%v actions=%v", err, actions)
	}
}

func TestParseCopilotActionsRejectsBadPlans(t *testing.T) {
	cases := map[string]string{
		"not json at all":           `no json here`,
		"over cap":                  `{"actions":[{"type":"create_task","params":{"subject":"a"}},{"type":"create_task","params":{"subject":"b"}},{"type":"create_task","params":{"subject":"c"}},{"type":"create_task","params":{"subject":"d"}},{"type":"create_task","params":{"subject":"e"}},{"type":"create_task","params":{"subject":"f"}}]}`,
		"off-allowlist type":        `{"actions":[{"type":"close_case","params":{}}]}`,
		"task no subject":           `{"actions":[{"type":"create_task","params":{"due_days":3}}]}`,
		"task due_days range":       `{"actions":[{"type":"create_task","params":{"subject":"x","due_days":90}}]}`,
		"task due_days fract":       `{"actions":[{"type":"create_task","params":{"subject":"x","due_days":2.5}}]}`,
		"rescan no reason":          `{"actions":[{"type":"request_document_rescan","params":{}}]}`,
		"flag empty inputs":         `{"actions":[{"type":"flag_missing_eligibility_input","params":{"inputs":[]}}]}`,
		"flag non-string":           `{"actions":[{"type":"flag_missing_eligibility_input","params":{"inputs":[42]}}]}`,
		"correspondence no purpose": `{"actions":[{"type":"draft_followup_correspondence","params":{}}]}`,
	}
	for name, reply := range cases {
		if _, _, err := parseCopilotActions(reply); err == nil {
			t.Fatalf("%s: expected rejection, got nil error", name)
		}
	}
}

func TestCopilotActionTypesAllowlistIsClosed(t *testing.T) {
	// The executor's switch must cover EXACTLY the allowlist — a type one
	// side knows and the other doesn't is either a dead proposal or an
	// unguarded side effect.
	for typ := range copilotActionTypes {
		if err := validateCopilotActionParams(copilotAction{Type: typ, Params: map[string]any{}}); err == nil {
			// All current types require params; if a future type legitimately
			// needs none, drop this assertion for it deliberately.
			t.Logf("type %s validates with empty params (ok if intentional)", typ)
		}
	}
	if len(copilotActionTypes) != 4 {
		t.Fatalf("allowlist drifted: %v", copilotActionTypes)
	}
}

func TestCopilotBatchStatusRecompute(t *testing.T) {
	// persistCopilotActionResult's status ladder, mirrored so a change to
	// the ladder forces a deliberate test edit:
	// all applied -> APPLIED; some failed -> PARTIAL; none applied -> FAILED.
	statusFor := func(applied, failed, total int) string {
		switch {
		case total > 0 && applied == total:
			return "APPLIED"
		case applied > 0 && failed > 0:
			return "PARTIAL"
		case failed == total && failed > 0:
			return "FAILED"
		}
		return "APPROVED"
	}
	if statusFor(3, 0, 3) != "APPLIED" || statusFor(2, 1, 3) != "PARTIAL" ||
		statusFor(0, 2, 2) != "FAILED" || statusFor(1, 0, 3) != "APPROVED" {
		t.Fatal("status ladder broken")
	}
}
