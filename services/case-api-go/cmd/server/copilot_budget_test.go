package main

import (
	"strings"
	"testing"
)

func msgs(sizes ...int) []map[string]string {
	roles := []string{"system", "user", "assistant", "user"}
	out := make([]map[string]string, len(sizes))
	for i, n := range sizes {
		out[i] = map[string]string{"role": roles[i%len(roles)], "content": strings.Repeat("x", n)}
	}
	return out
}

func TestBudgetMessagesUnderBudgetUntouched(t *testing.T) {
	in := msgs(500, 1000, 800)
	out := budgetMessages(in)
	for i := range in {
		if out[i]["content"] != in[i]["content"] {
			t.Fatalf("message %d modified under budget", i)
		}
	}
}

func TestBudgetMessagesTruncatesLargestNonSystem(t *testing.T) {
	big := strings.Repeat("a", 20000)
	in := []map[string]string{
		{"role": "system", "content": "you are grounded"},
		{"role": "user", "content": big},
	}
	out := budgetMessages(in)
	if out[0]["content"] != "you are grounded" {
		t.Fatal("system prompt must never be truncated")
	}
	got := out[1]["content"]
	if len(got) >= len(big) {
		t.Fatalf("largest message not truncated: %d bytes", len(got))
	}
	if !strings.Contains(got, "middle truncated") {
		t.Fatal("truncation marker missing")
	}
	if !strings.HasPrefix(got, big[:100]) || !strings.HasSuffix(got, big[len(big)-100:]) {
		t.Fatal("head and tail must survive truncation")
	}
}

func TestBudgetMessagesKeepsMinimumFragment(t *testing.T) {
	// An absurdly oversized prompt still leaves a usable fragment, and the
	// total is driven down toward the budget rather than to zero.
	in := []map[string]string{
		{"role": "system", "content": strings.Repeat("s", 11000)},
		{"role": "user", "content": strings.Repeat("u", 50000)},
	}
	out := budgetMessages(in)
	if len(out[1]["content"]) < 2000 {
		t.Fatalf("fragment gutted below floor: %d", len(out[1]["content"]))
	}
}

func TestHTTPTimeoutWithinLatencyBudget(t *testing.T) {
	if copilotHTTPTimeout > 90*1e9 {
		t.Fatalf("client timeout %v exceeds the response-time budget guardrail", copilotHTTPTimeout)
	}
}
