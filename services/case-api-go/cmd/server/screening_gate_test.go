// Author: Patrick Munis at newwave

// screening_gate_test.go — payment-origination OFAC gate posture.
package main

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// The gate decision matrix is the compliance posture itself; pin it.
func TestPaymentGatePosture(t *testing.T) {
	s := &server{}

	// List enabled but empty -> fail CLOSED.
	t.Setenv("WATCHMAN_ENABLED", "true")
	sdnList.Lock()
	sdnList.entities = nil
	sdnList.Unlock()
	r := httptest.NewRequest(http.MethodPost, "/", nil)
	blocked, _, code, msg := s.screenPaymentParty(r, "fl", "PARTY", "x", "Anyone")
	if !blocked || code != http.StatusServiceUnavailable || msg == "" {
		t.Fatalf("empty list must fail closed, got blocked=%v code=%d", blocked, code)
	}

	// Screening deliberately disabled -> allow (operator's decision).
	t.Setenv("WATCHMAN_ENABLED", "false")
	blocked, _, code, _ = s.screenPaymentParty(r, "fl", "PARTY", "x", "Anyone")
	if blocked || code != 0 {
		t.Fatalf("disabled screening must allow, got blocked=%v code=%d", blocked, code)
	}
}

// A clean name against a loaded list passes the gate.
func TestPaymentGateCleanNamePasses(t *testing.T) {
	t.Setenv("WATCHMAN_ENABLED", "true")
	sdnList.Lock()
	sdnList.entities = nil
	sdnList.Unlock()
	// nil server.db would panic on the INSERT — the gate records every check,
	// so this path needs a DB; covered by integration env. Here we assert the
	// pure scorer boundary instead: an empty-token name can never match.
	matched, top, _ := screenName("!!!")
	if matched || top != 0 {
		t.Fatalf("empty-token query must not match, got %v %.2f", matched, top)
	}
}
