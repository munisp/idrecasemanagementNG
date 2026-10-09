package main

// payment_policy.go — NG-only flexible payment policy (config.payment).
//
// Per-tenant, runtime-administrable control over WHERE in the intake
// lifecycle payment is required. Three modes:
//
//   "open"           — no payment gates (back-compatible default when unset)
//   "payment_first"  — fee settles before documents are accepted AND before
//                      the intake can convert to a case (mirrors the legacy
//                      service's gated intake)
//   "custom"         — the two require_paid_* flags are taken verbatim
//
// Exemptions: exempt_filing_party_types (manifest party codes, uppercase)
// bypass the gates — e.g. a state may waive payment-first for plan-initiated
// filings while enforcing it for provider filings.
//
// Deliberately NOT in the migration-only guard set (fees/billing/recon):
// payment policy is operational posture a program admin may tune at runtime
// via PUT /program/payment-policy (rulesAdminGuard: platform admins +
// Permify program_rules.edit, fail-closed). Every change lands in
// program_config_history via the guard trigger.

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
)

// PaymentPolicyConfig lives at config.payment in public.program_rules.
type PaymentPolicyConfig struct {
	Mode                     string   `json:"mode"` // open | payment_first | custom
	RequirePaidBeforeDocs    *bool    `json:"require_paid_before_docs,omitempty"`
	RequirePaidBeforeConvert *bool    `json:"require_paid_before_convert,omitempty"`
	ExemptFilingPartyTypes   []string `json:"exempt_filing_party_types,omitempty"`
}

// paidBeforeDocs: does the policy demand settlement before DOCS_RECEIVED /
// PACKET_COMPLETE? payment_first implies yes unless explicitly overridden.
func (p PaymentPolicyConfig) paidBeforeDocs() bool {
	if p.RequirePaidBeforeDocs != nil {
		return *p.RequirePaidBeforeDocs
	}
	return p.Mode == "payment_first"
}

// paidBeforeConvert: does the policy demand settlement before CONVERTED?
func (p PaymentPolicyConfig) paidBeforeConvert() bool {
	if p.RequirePaidBeforeConvert != nil {
		return *p.RequirePaidBeforeConvert
	}
	return p.Mode == "payment_first"
}

// exempt: is this filing-party type excused from the payment gates?
func (p PaymentPolicyConfig) exempt(filingPartyType string) bool {
	fpt := strings.ToUpper(strings.TrimSpace(filingPartyType))
	for _, e := range p.ExemptFilingPartyTypes {
		if strings.ToUpper(strings.TrimSpace(e)) == fpt {
			return true
		}
	}
	return false
}

// validatePaymentPolicy rejects unknown modes and contradictory shapes.
func validatePaymentPolicy(p PaymentPolicyConfig) error {
	switch p.Mode {
	case "", "open", "payment_first", "custom":
	default:
		return fmt.Errorf("mode must be open|payment_first|custom")
	}
	if p.Mode == "custom" && p.RequirePaidBeforeDocs == nil && p.RequirePaidBeforeConvert == nil {
		return fmt.Errorf("custom mode requires at least one require_paid_* flag")
	}
	return nil
}

// enforcePaymentGate runs the policy against an intake transition. target is
// the requested status; currentStatus and filingPartyType describe the intake
// row. Returns "" when the transition is allowed, else a 409 message.
func enforcePaymentGate(pol PaymentPolicyConfig, target, currentStatus, filingPartyType string) string {
	if pol.exempt(filingPartyType) {
		return ""
	}
	paid := currentStatus == "PAID"
	switch target {
	case "DOCS_RECEIVED", "PACKET_COMPLETE":
		if pol.paidBeforeDocs() && !paid {
			return "payment-first policy: the filing fee must settle (intake status PAID) before documents are accepted"
		}
	case "CONVERTED":
		if pol.paidBeforeConvert() && !paid {
			return "payment-first policy: the filing fee must settle (intake status PAID) before this intake can convert to a case"
		}
	}
	return ""
}

// putPaymentPolicy: PUT /program/payment-policy — runtime administration of
// config.payment. Guarded like rules/manifest edits; the DB trigger records
// the change in program_config_history.
func (s *server) putPaymentPolicy(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.rulesAdminGuard(w, r); !ok {
		return
	}
	tenant := r.Context().Value(ctxTenant{}).(string)
	var pol PaymentPolicyConfig
	if err := json.NewDecoder(r.Body).Decode(&pol); err != nil {
		http.Error(w, `{"error":"payment policy JSON required"}`, http.StatusBadRequest)
		return
	}
	pol.Mode = strings.ToLower(strings.TrimSpace(pol.Mode))
	if pol.Mode == "" {
		pol.Mode = "open"
	}
	if err := validatePaymentPolicy(pol); err != nil {
		http.Error(w, fmt.Sprintf(`{"error":%q}`, err.Error()), http.StatusUnprocessableEntity)
		return
	}
	raw, _ := json.Marshal(pol)
	res, err := s.db.Exec(r.Context(), `
		UPDATE public.program_rules SET config = jsonb_set(config, '{payment}', $2::jsonb, true), updated_at=now()
		WHERE tenant=$1`, tenant, string(raw))
	if err != nil {
		http.Error(w, `{"error":"db"}`, http.StatusInternalServerError)
		return
	}
	if res.RowsAffected() == 0 {
		http.Error(w, `{"error":"tenant has no program_rules row — seed it first"}`, http.StatusConflict)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"payment": pol})
}
