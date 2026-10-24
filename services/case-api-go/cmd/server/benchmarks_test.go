// Author: Patrick Munis at newwave
package main

import "testing"

// Pins percentile math used when SQL percentile_cont isn't available
// (unit-level) and the role floor on both analytics endpoints.
func TestPercentileStats(t *testing.T) {
	cases := []struct {
		name                    string
		in                      []int64
		p25, median, p75        int64
	}{
		{"empty", nil, 0, 0, 0},
		{"single", []int64{1000}, 1000, 1000, 1000},
		{"spread of 5", []int64{100, 200, 300, 400, 500}, 200, 300, 400},
		{"duplicates", []int64{100, 100, 900, 900}, 100, 500, 900},
	}
	for _, c := range cases {
		p25, med, p75 := percentileStats(c.in)
		if p25 != c.p25 || med != c.median || p75 != c.p75 {
			t.Errorf("%s: got (%d,%d,%d), want (%d,%d,%d)", c.name, p25, med, p75, c.p25, c.median, c.p75)
		}
	}
}

// Benchmarks expose QPA distributions and batchable-case suggestions to case
// staff and the arbitrator -- but never to parties (a party seeing the
// tenant's QPA curve would tilt the sealed-offer game).
func TestBenchmarkRoleFloors(t *testing.T) {
	gate := benchmarkRoles
	cases := []struct {
		name  string
		roles []string
		want  bool
	}{
		{"ARBITRATOR allowed (statutory QPA criteria)", []string{"ARBITRATOR"}, true},
		{"CASE_MANAGER allowed", []string{"CASE_MANAGER"}, true},
		{"DOCTOR allowed", []string{"DOCTOR"}, true},
		{"PARTY rejected (would tilt sealed offers)", []string{"PARTY"}, false},
		{"FINANCE rejected (not a finance surface)", []string{"FINANCE"}, false},
		{"STATE_AUDITOR rejected", []string{"STATE_AUDITOR"}, false},
	}
	for _, c := range cases {
		if got := hasAnyRole(principal{Roles: c.roles}, gate...); got != c.want {
			t.Errorf("%s: got %v, want %v", c.name, got, c.want)
		}
	}
}
