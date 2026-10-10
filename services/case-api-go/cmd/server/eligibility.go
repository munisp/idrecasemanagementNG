package main

// eligibility.go — the "bulletproof + fast to analyze" layer on top of the
// G2 eligibility core (triage.go).
//
// Why eligibility is treated as load-bearing: it is the proceed / do-not-
// proceed gate for the entire dispute. A wrong ELIGIBLE creates unlawful
// billing downstream; a wrong INELIGIBLE denies a party its statutory remedy.
// So the design is deliberately boring:
//
//   1. DETERMINISTIC DECISIONS ONLY. evalEligibility (triage.go) is a pure
//      function of the seeded program rules — the same SOP config the state
//      published. The LLM/doc-intel pipeline never decides; it only extracts
//      facts (provider_type, amounts, dates) with source citations, and a
//      human-entered value on the case always beats an extraction
//      (deriveEligibilityInput).
//   2. RULE VERSION PINNING. Every stored decision carries rule_version, the
//      hash of the exact eligibility config subtree that produced it. When
//      rules change, old decisions remain explainable — "eligible under
//      rules v9f2…, which had threshold X" — instead of silently drifting.
//   3. APPEND-ONLY RECORD. eligibility_reviews cannot be updated or deleted
//      (DB trigger, program-rules.sql). A correction is a new row; the full
//      history is the legal record.
//   4. BORDERLINE DUAL CONTROL. Amounts within ±10% of a threshold, or
//      filing dates within 30 days of the window deadline, are flagged
//      borderline and routed to the PM for a second pair of eyes — the two
//      places a data-entry slip most often flips the outcome.
//   5. WARN-ONLY TRANSITION GATE. Moving a case past initial review without
//      an ELIGIBLE decision attaches a loud warning to the response (and the
//      portal shows it) but does not block: legitimate exceptions exist
//      (dismissals, withdrawals), and blocking them would be its own legal
//      risk.
//   6. OVERRIDE WITH ACCOUNTABILITY. A reviewer may override an INELIGIBLE
//      to ELIGIBLE only with a mandatory written reason; the override, its
//      author, and the original computed result are all recorded and the PM
//      is notified.

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"
)

// eligibilityPersistOpts carries the accountability metadata stored with
// every decision row.
type eligibilityPersistOpts struct {
	ruleVersion       string
	borderline        bool
	borderlineReasons []string
	override          bool
	overrideReason    string
}

// eligibilityRuleVersion hashes the eligibility subtree of the program
// config. json.Marshal of a struct is deterministic (field order is
// declaration order), so identical rules always yield the identical version.
func eligibilityRuleVersion(cfg *ProgramConfig) string {
	if cfg == nil {
		return ""
	}
	raw, _ := json.Marshal(cfg.Eligibility)
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:8]) // 16 hex chars — enough to pin a config
}

// borderlineMarginBps: an amount within this fraction of the threshold (on
// either side) is borderline — 1000 bps = ±10%.
const borderlineMarginBps = 1000

// borderlineDeadlineDays: a filing date this close to the window deadline is
// borderline.
const borderlineDeadlineDays = 30

// eligibilityBorderline inspects the evidence evalEligibility produced and
// returns the borderline reasons (empty = not borderline). Runs on evidence
// rather than re-deciding, so it can never disagree with the decision core.
func eligibilityBorderline(cfg *ProgramConfig, in eligibilityInput, evidence map[string]any) []string {
	var out []string
	if minRaw, ok := evidence["threshold_min_cents"]; ok {
		if min, ok2 := minRaw.(int64); ok2 && min > 0 {
			margin := min * borderlineMarginBps / 10000
			diff := in.DisputedAmountCents - min
			if diff < 0 {
				diff = -diff
			}
			if diff <= margin {
				out = append(out, fmt.Sprintf("disputed amount within ±10%% of the $%d.%02d threshold", min/100, min%100))
			}
		}
	}
	if dl, ok := evidence["filing_deadline"].(string); ok {
		if d, err := time.Parse("2006-01-02", dl); err == nil {
			days := int(time.Until(d).Hours() / 24)
			if days >= 0 && days <= borderlineDeadlineDays {
				out = append(out, fmt.Sprintf("filing window deadline in %d days (%s)", days, dl))
			}
		}
	}
	return out
}

// pastEligibilityGate reports whether moving to this internal status leaves
// the pre-decision phase. The first two configured internal statuses are the
// intake/initial-review phase ("Initial Review Pending", "QA Initial Review"
// in FL); everything beyond them presumes an eligibility decision exists.
func pastEligibilityGate(cfg *ProgramConfig, status string) bool {
	if cfg == nil || len(cfg.Statuses.Internal) == 0 {
		return false
	}
	for i, s := range cfg.Statuses.Internal {
		if s == status {
			return i >= 2
		}
	}
	return false
}

// hasEligibleDecision: an ELIGIBLE row exists in the append-only record.
func (s *server) hasEligibleDecision(r *http.Request, tenant, caseID string) bool {
	var n int
	_ = s.db.QueryRow(r.Context(), `
		SELECT count(*) FROM public.eligibility_reviews
		WHERE tenant=$1 AND case_id=$2 AND result='ELIGIBLE'`, tenant, caseID).Scan(&n)
	return n > 0
}

// validOverrideReason: an override must carry a substantive written reason.
func validOverrideReason(s string) bool { return len(strings.TrimSpace(s)) >= 10 }
