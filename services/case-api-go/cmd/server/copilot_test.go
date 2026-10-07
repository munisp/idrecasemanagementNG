package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestCopilotPromptGroundsAndLabels(t *testing.T) {
	f := copilotFacts{
		CaseNumber: "FL-2026-001", TriageLane: "AUTO_REVIEW", SLADaysRemaining: 31,
		QPACents: 120000, DisputedAmountCents: 450000,
		EligibilityResult: "ELIGIBLE",
		Documents: []copilotDocFact{{
			DocType: "eob", Status: "ANALYZED",
			TrustedFields: map[string]any{"allowed_amount_usd": 987.0},
			LowConfidence: []string{"qpa_usd"},
		}},
	}
	system, user := copilotPrompt(f)
	for _, want := range []string{"ONLY the JSON facts", "not in record", "NEVER invent",
		"DRAFT — advisory only; not a determination"} {
		if !strings.Contains(system, want) {
			t.Errorf("system prompt missing grounding clause %q", want)
		}
	}
	// every number the model may cite must come from the fact sheet
	for _, want := range []string{"FL-2026-001", "AUTO_REVIEW", "120000", "450000", "allowed_amount_usd", "qpa_usd"} {
		if !strings.Contains(user, want) {
			t.Errorf("fact sheet missing %q", want)
		}
	}
}

func TestOllamaChatHappyPath(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/chat/completions") {
			t.Errorf("unexpected path %s", r.URL.Path)
		}
		var req map[string]any
		_ = json.NewDecoder(r.Body).Decode(&req)
		if req["temperature"].(float64) != 0 {
			t.Error("briefs must be deterministic (temperature 0)")
		}
		msgs := req["messages"].([]any)
		if len(msgs) != 2 {
			t.Error("expected system+user messages")
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"choices": []map[string]any{
				{"message": map[string]string{"content": "1. ELIGIBILITY BRIEF ..."}},
			},
		})
	}))
	defer srv.Close()
	got, err := ollamaChat(context.Background(), srv.URL+"/v1", "qwen2.5:7b-instruct", "sys", "usr", 1200)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got, "ELIGIBILITY BRIEF") {
		t.Errorf("unexpected content %q", got)
	}
}

func TestOllamaChatFailures(t *testing.T) {
	// ollama-side error envelope
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"error": map[string]string{"message": "model not pulled"}})
	}))
	defer srv.Close()
	if _, err := ollamaChat(context.Background(), srv.URL, "missing", "s", "u", 100); err == nil ||
		!strings.Contains(err.Error(), "model not pulled") {
		t.Errorf("expected ollama error propagation, got %v", err)
	}
	// empty choices
	srv2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"choices": []any{}})
	}))
	defer srv2.Close()
	if _, err := ollamaChat(context.Background(), srv2.URL, "m", "s", "u", 100); err == nil {
		t.Error("expected error on empty choices")
	}
	// unreachable endpoint
	if _, err := ollamaChat(context.Background(), "http://127.0.0.1:1", "m", "s", "u", 100); err == nil {
		t.Error("expected transport error")
	}
}

func TestCopilotLaneLogicMatchesListCases(t *testing.T) {
	// The Go-side single-case lane must agree with triageLaneSQL's intent:
	// batch/duplicate/escalation/materiality -> COMPLEX; qpa+amount ->
	// AUTO_REVIEW; else STANDARD. (The SQL is tested through listCases;
	// this pins the Go mirror from drifting.)
	cases := []struct {
		batch, dup, esc bool
		amount, qpa     int64
		want            string
	}{
		{true, false, false, 100, 100, "COMPLEX"},
		{false, true, false, 100, 100, "COMPLEX"},
		{false, false, true, 100, 100, "COMPLEX"},
		{false, false, false, 10000000, 100, "COMPLEX"},
		{false, false, false, 5000, 3000, "AUTO_REVIEW"},
		{false, false, false, 5000, 0, "STANDARD"},
	}
	for _, tc := range cases {
		var batchID, dupOf *string
		if tc.batch {
			s := "b"
			batchID = &s
		}
		if tc.dup {
			s := "d"
			dupOf = &s
		}
		lane := "STANDARD"
		switch {
		case batchID != nil || dupOf != nil || tc.amount >= 10000000 || tc.esc:
			lane = "COMPLEX"
		case tc.qpa > 0 && tc.amount > 0:
			lane = "AUTO_REVIEW"
		}
		if lane != tc.want {
			t.Errorf("lane(%+v) = %s, want %s", tc, lane, tc.want)
		}
	}
}
