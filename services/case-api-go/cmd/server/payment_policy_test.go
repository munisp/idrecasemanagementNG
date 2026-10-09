package main

import "testing"

func bptr(b bool) *bool { return &b }

func TestPaymentPolicyModes(t *testing.T) {
	open := PaymentPolicyConfig{Mode: "open"}
	if open.paidBeforeDocs() || open.paidBeforeConvert() {
		t.Fatal("open mode must not gate anything")
	}
	pf := PaymentPolicyConfig{Mode: "payment_first"}
	if !pf.paidBeforeDocs() || !pf.paidBeforeConvert() {
		t.Fatal("payment_first must gate docs and convert")
	}
	// Explicit flags override the mode default.
	override := PaymentPolicyConfig{Mode: "payment_first", RequirePaidBeforeDocs: bptr(false)}
	if override.paidBeforeDocs() {
		t.Fatal("explicit require_paid_before_docs=false must override payment_first")
	}
	if !override.paidBeforeConvert() {
		t.Fatal("convert gate should still default from payment_first")
	}
	custom := PaymentPolicyConfig{Mode: "custom", RequirePaidBeforeConvert: bptr(true)}
	if custom.paidBeforeDocs() || !custom.paidBeforeConvert() {
		t.Fatal("custom mode must take flags verbatim")
	}
}

func TestValidatePaymentPolicy(t *testing.T) {
	if err := validatePaymentPolicy(PaymentPolicyConfig{Mode: "bogus"}); err == nil {
		t.Fatal("unknown mode must be rejected")
	}
	if err := validatePaymentPolicy(PaymentPolicyConfig{Mode: "custom"}); err == nil {
		t.Fatal("custom with no flags must be rejected")
	}
	if err := validatePaymentPolicy(PaymentPolicyConfig{Mode: "payment_first"}); err != nil {
		t.Fatalf("payment_first valid: %v", err)
	}
}

func TestEnforcePaymentGate(t *testing.T) {
	pf := PaymentPolicyConfig{Mode: "payment_first"}
	if msg := enforcePaymentGate(pf, "DOCS_RECEIVED", "INSTRUCTED", "PROVIDER"); msg == "" {
		t.Fatal("unpaid docs acceptance must be blocked under payment_first")
	}
	if msg := enforcePaymentGate(pf, "CONVERTED", "PACKET_COMPLETE", "PROVIDER"); msg == "" {
		t.Fatal("unpaid conversion must be blocked under payment_first")
	}
	if msg := enforcePaymentGate(pf, "CONVERTED", "PAID", "PROVIDER"); msg != "" {
		t.Fatalf("paid intake must convert freely: %s", msg)
	}
	if msg := enforcePaymentGate(pf, "DOCS_RECEIVED", "PAID", "PROVIDER"); msg != "" {
		t.Fatalf("paid intake must accept docs: %s", msg)
	}
	// Exemptions bypass both gates, case-insensitively.
	ex := PaymentPolicyConfig{Mode: "payment_first", ExemptFilingPartyTypes: []string{"health_plan"}}
	if msg := enforcePaymentGate(ex, "CONVERTED", "INSTRUCTED", "HEALTH_PLAN"); msg != "" {
		t.Fatalf("exempt party type must bypass gates: %s", msg)
	}
	// Open policy never blocks.
	if msg := enforcePaymentGate(PaymentPolicyConfig{Mode: "open"}, "CONVERTED", "INSTRUCTED", "PROVIDER"); msg != "" {
		t.Fatalf("open policy must not block: %s", msg)
	}
}
