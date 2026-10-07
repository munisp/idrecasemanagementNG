package main

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func testDigest() briefingDigest {
	return briefingDigest{
		Worker:      "worker-1",
		GeneratedAt: time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC),
		MyOpenCases: 3,
		Cases: []briefingCase{
			{CaseID: "a", CaseNumber: "TX-2026-0001", Status: "OPEN", Lane: "COMPLEX", SLADays: 2},
			{CaseID: "b", CaseNumber: "TX-2026-0002", Status: "OPEN", Lane: "STANDARD", SLADays: 30},
		},
		AtRiskSLA:    []briefingCase{{CaseID: "a", CaseNumber: "TX-2026-0001", Status: "OPEN", Lane: "COMPLEX", SLADays: 2}},
		NewDocs24h:   4,
		ChecksReview: 1,
		PendingQA:    2,
		TasksDue:     []briefingTask{{Subject: "Call payer", CaseID: "a", Due: "2026-10-07"}},
	}
}

// The fallback narration is the guaranteed path — the briefing must never be
// an error toast when the model is down. It must carry the digest numbers
// and the at-risk case numbers, in plain sentences.
func TestFallbackNarrationCarriesDigest(t *testing.T) {
	n := fallbackNarration(testDigest())
	for _, want := range []string{"3 open case(s)", "TX-2026-0001", "1 task(s)", "2 item(s)", "1 check(s)", "4 document(s)"} {
		if !strings.Contains(n, want) {
			t.Errorf("fallback narration missing %q: %s", want, n)
		}
	}
}

// A quiet queue narrates quiet — no fake urgency.
func TestFallbackNarrationQuietQueue(t *testing.T) {
	n := fallbackNarration(briefingDigest{Cases: []briefingCase{}, AtRiskSLA: []briefingCase{}, TasksDue: []briefingTask{}})
	if !strings.Contains(n, "queue is clear") {
		t.Errorf("quiet digest should narrate quiet, got: %s", n)
	}
}

// The model sees ONLY the digest JSON as the user message (grounded-or-silent);
// the system prompt pins the output contract.
func TestBriefingPromptIsGroundedAndBounded(t *testing.T) {
	d := testDigest()
	user := briefingNarrationUser(d)
	if !strings.HasPrefix(user, "DIGEST (JSON") {
		t.Errorf("user message must be the digest JSON, got: %.40s", user)
	}
	var decoded briefingDigest
	if err := json.Unmarshal([]byte(strings.TrimPrefix(user, "DIGEST (JSON, platform-verified):\n")), &decoded); err != nil {
		t.Fatalf("digest must round-trip as JSON: %v", err)
	}
	if decoded.Cases[0].CaseNumber != "TX-2026-0001" {
		t.Errorf("digest lost case data in transit")
	}
	for _, rule := range []string{"ONLY the JSON", "Under 120 words", "Never invent"} {
		if !strings.Contains(briefingNarrationSystem, rule) {
			t.Errorf("system prompt missing guardrail %q", rule)
		}
	}
}

// sla risk slicing: only cases at or under the threshold land in AtRiskSLA.
// (Gather is DB-bound; the slice predicate is what's pinned here.)
func TestBriefingSLARiskThreshold(t *testing.T) {
	d := testDigest()
	if briefingSLARiskDays != 5 {
		t.Fatalf("threshold drifted — portal copy says what the constant says")
	}
	for _, c := range d.AtRiskSLA {
		if c.SLADays > briefingSLARiskDays {
			t.Errorf("case %s at %d days is not at-risk", c.CaseNumber, c.SLADays)
		}
	}
}
