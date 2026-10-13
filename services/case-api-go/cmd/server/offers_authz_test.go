package main

import "testing"

// Pins the role floors on the sealed-offer endpoints (45 CFR 149.520(b)
// double-blind protocol). Submitting an offer is open to parties and case
// staff; lifting the double-blind and issuing the baseball-style
// determination are ARBITRATOR-only -- an offer amount must never be
// influenceable by the same person who selects the winner, and no party or
// staff member may ever see the sealed amounts pre-reveal.
func TestOfferEndpointRoleFloors(t *testing.T) {
	submit := []string{"PARTY", "TPA", "CASE_MANAGER", "FEDERAL_ADMIN", "PLATFORM_ADMIN", serviceRole}
	arbitrator := []string{"ARBITRATOR", "FEDERAL_ADMIN", "PLATFORM_ADMIN", serviceRole}
	cases := []struct {
		name  string
		roles []string
		gate  []string
		want  bool
	}{
		{"submit: PARTY allowed (further scoped per-case by partyOwnsCase)", []string{"PARTY"}, submit, true},
		{"submit: TPA allowed (submits on behalf of its plan)", []string{"TPA"}, submit, true},
		{"submit: CASE_MANAGER allowed", []string{"CASE_MANAGER"}, submit, true},
		{"submit: ARBITRATOR rejected (decider never submits)", []string{"ARBITRATOR"}, submit, false},
		{"submit: FINANCE rejected", []string{"FINANCE"}, submit, false},
		{"reveal: ARBITRATOR allowed", []string{"ARBITRATOR"}, arbitrator, true},
		{"reveal: CASE_MANAGER rejected", []string{"CASE_MANAGER"}, arbitrator, false},
		{"reveal: PARTY rejected", []string{"PARTY"}, arbitrator, false},
		{"determination: ARBITRATOR allowed", []string{"ARBITRATOR"}, arbitrator, true},
		{"determination: CASE_MANAGER rejected", []string{"CASE_MANAGER"}, arbitrator, false},
		{"determination: FINANCE rejected", []string{"FINANCE"}, arbitrator, false},
	}
	for _, c := range cases {
		got := hasAnyRole(principal{Roles: c.roles}, c.gate...)
		if got != c.want {
			t.Errorf("%s: roles=%v: got %v, want %v", c.name, c.roles, got, c.want)
		}
	}
}

// Pins the review-queue discipline gates: NG's leaner role set maps CODING
// to CASE_MANAGER and CLINICAL to DOCTOR/NURSE; LEGAL stays ATTORNEY.
// Completion of a review is what signals the Temporal workflow, so a wrong
// gate here would let the wrong discipline advance the case.
func TestReviewQueueDisciplineFloors(t *testing.T) {
	allowed := func(discipline string, roles ...string) bool {
		meta, ok := disciplineMeta[discipline]
		if !ok {
			return false
		}
		return hasAnyRole(principal{Roles: roles}, append(append([]string{}, meta.roles...), serviceRole)...)
	}
	cases := []struct {
		name       string
		discipline string
		roles      []string
		want       bool
	}{
		{"CODING: CASE_MANAGER allowed", "CODING", []string{"CASE_MANAGER"}, true},
		{"CODING: DOCTOR rejected", "CODING", []string{"DOCTOR"}, false},
		{"CODING: ATTORNEY rejected", "CODING", []string{"ATTORNEY"}, false},
		{"CLINICAL: DOCTOR allowed", "CLINICAL", []string{"DOCTOR"}, true},
		{"CLINICAL: NURSE allowed", "CLINICAL", []string{"NURSE"}, true},
		{"CLINICAL: CASE_MANAGER rejected", "CLINICAL", []string{"CASE_MANAGER"}, false},
		{"LEGAL: ATTORNEY allowed", "LEGAL", []string{"ATTORNEY"}, true},
		{"LEGAL: FINANCE rejected", "LEGAL", []string{"FINANCE"}, false},
		{"any: PARTY rejected (no queue access)", "CODING", []string{"PARTY"}, false},
		{"unknown discipline rejected outright", "PODIATRY", []string{"FEDERAL_ADMIN"}, false},
	}
	for _, c := range cases {
		if got := allowed(c.discipline, c.roles...); got != c.want {
			t.Errorf("%s: got %v, want %v", c.name, got, c.want)
		}
	}
	// The workflow signals the queues fire must stay the ones signalCase
	// floors to the matching specialty roles.
	if disciplineMeta["CODING"].signal != "CODING_REVIEW_COMPLETE" ||
		disciplineMeta["CLINICAL"].signal != "CLINICAL_REVIEW_COMPLETE" ||
		disciplineMeta["LEGAL"].signal != "ATTORNEY_REVIEW_COMPLETE" {
		t.Errorf("disciplineMeta signals drifted: %+v", disciplineMeta)
	}
}

// Party workspace: the /party/* endpoints are PARTY-only (a party login
// alone still grants nothing -- partyOwnsCase enforces the per-case mapping
// from party_accounts), and staff-side party-account listing is staff plus
// STATE_AUDITOR oversight.
func TestPartyWorkspaceRoleFloors(t *testing.T) {
	party := []string{"PARTY"}
	staff := []string{"CASE_MANAGER", "FEDERAL_ADMIN", "PLATFORM_ADMIN", "STATE_AUDITOR", serviceRole}
	invite := []string{"CASE_MANAGER", "FEDERAL_ADMIN", "PLATFORM_ADMIN", serviceRole}
	cases := []struct {
		name  string
		roles []string
		gate  []string
		want  bool
	}{
		{"party cases: PARTY allowed", []string{"PARTY"}, party, true},
		{"party cases: CASE_MANAGER rejected (staff use the case workspace)", []string{"CASE_MANAGER"}, party, false},
		{"party cases: STATE_AUDITOR rejected", []string{"STATE_AUDITOR"}, party, false},
		{"invite: CASE_MANAGER allowed", []string{"CASE_MANAGER"}, invite, true},
		{"invite: PARTY rejected", []string{"PARTY"}, invite, false},
		{"invite: ARBITRATOR rejected", []string{"ARBITRATOR"}, invite, false},
		{"accounts list: STATE_AUDITOR allowed (oversight)", []string{"STATE_AUDITOR"}, staff, true},
		{"accounts list: PARTY rejected", []string{"PARTY"}, staff, false},
		{"accounts list: FINANCE rejected", []string{"FINANCE"}, staff, false},
	}
	for _, c := range cases {
		got := hasAnyRole(principal{Roles: c.roles}, c.gate...)
		if got != c.want {
			t.Errorf("%s: roles=%v: got %v, want %v", c.name, c.roles, got, c.want)
		}
	}
}
