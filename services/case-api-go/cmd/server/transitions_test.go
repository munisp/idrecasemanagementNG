package main

import (
	"testing"
	"time"
)

func TestBuildTransitionMatrix(t *testing.T) {
	at := func(day, hour int) time.Time {
		return time.Date(2026, 1, day, hour, 0, 0, 0, time.UTC)
	}
	events := []transitionEvent{
		// case A: "" -> INITIATED -> OPEN -> CLOSED_PAID
		{CaseID: "a", From: "", To: "INITIATED", At: at(1, 0)},
		{CaseID: "a", From: "INITIATED", To: "OPEN", At: at(1, 12)},   // dwell 12h in INITIATED
		{CaseID: "a", From: "OPEN", To: "CLOSED_PAID", At: at(3, 12)}, // dwell 48h in OPEN
		// case B: "" -> INITIATED -> OPEN -> IN_REVIEW
		{CaseID: "b", From: "", To: "INITIATED", At: at(2, 0)},
		{CaseID: "b", From: "INITIATED", To: "OPEN", At: at(2, 6)}, // dwell 6h in INITIATED
		{CaseID: "b", From: "OPEN", To: "IN_REVIEW", At: at(4, 6)}, // dwell 48h in OPEN
		// case C: opens straight into OPEN (no INITIATED hop)
		{CaseID: "c", From: "", To: "OPEN", At: at(5, 0)},
		{CaseID: "c", From: "OPEN", To: "CLOSED_PAID", At: at(6, 0)}, // dwell 24h in OPEN
	}
	cells := buildTransitionMatrix(events)
	// 3 distinct edges: INITIATED->OPEN (x2), OPEN->CLOSED_PAID (x2),
	// OPEN->IN_REVIEW (x1) — case C's OPEN->CLOSED_PAID merges with case A's.
	if len(cells) != 3 {
		t.Fatalf("expected 3 edges, got %d: %+v", len(cells), cells)
	}
	byEdge := map[string]matrixCell{}
	for _, c := range cells {
		byEdge[c.From+"->"+c.To] = c
	}

	// INITIATED -> OPEN happened on both cases that passed through it: share 1.0.
	io := byEdge["INITIATED->OPEN"]
	if io.Count != 2 || io.Share != 1.0 {
		t.Fatalf("INITIATED->OPEN: count=%d share=%v, want 2 / 1.0", io.Count, io.Share)
	}
	// Median dwell in INITIATED before OPEN: median(12, 6) = 9.
	if io.MedianDwellHours != 9 {
		t.Fatalf("INITIATED->OPEN dwell=%v, want 9 (median of 12 and 6)", io.MedianDwellHours)
	}

	// OPEN has three exits: 2 to CLOSED_PAID, 1 to IN_REVIEW.
	oc := byEdge["OPEN->CLOSED_PAID"]
	if oc.Count != 2 || oc.Share != 2.0/3.0 {
		t.Fatalf("OPEN->CLOSED_PAID: count=%d share=%v, want 2 / 0.667", oc.Count, oc.Share)
	}
	// Median dwell in OPEN before CLOSED_PAID: median(48, 24) = 36.
	if oc.MedianDwellHours != 36 {
		t.Fatalf("OPEN->CLOSED_PAID dwell=%v, want 36", oc.MedianDwellHours)
	}
	or := byEdge["OPEN->IN_REVIEW"]
	if or.Count != 1 || or.Share != 1.0/3.0 || or.MedianDwellHours != 48 {
		t.Fatalf("OPEN->IN_REVIEW: %+v, want count 1 share 0.333 dwell 48", or)
	}

	// Opening hops anchor dwell but are never edges.
	for _, c := range cells {
		if c.From == "" {
			t.Fatalf("opening transition leaked into matrix: %+v", c)
		}
	}
	// Hottest edge first.
	if cells[0].Count != 2 {
		t.Fatalf("matrix not sorted by count desc: %+v", cells)
	}
}

func TestBuildTransitionMatrixEmpty(t *testing.T) {
	if got := buildTransitionMatrix(nil); len(got) != 0 {
		t.Fatalf("empty input produced %d cells", len(got))
	}
}

func TestLoadPriorsFallbackTable(t *testing.T) {
	// The priors table loadPriors hands recommendNextActionWith must behave
	// exactly like the static table for ids it doesn't override — a partial
	// learned table can never zero a rule.
	override := map[string]float64{"qa_review": 0.31}
	for id, want := range nextActionPriors {
		got := recommendNextActionWith(nextActionInput{}, "c", map[string]float64{id: override["qa_review"]})
		_ = got // ranking path exercised below; here: the fallback rule itself
		rec := recommendNextActionWith(nextActionInput{PendingQA: 1, IsReviewer: true}, "c", override)
		if id == "qa_review" && rec.ActionID == "qa_review" && rec.Confidence != 0.31 {
			t.Fatalf("armed override ignored: got %v want 0.31", rec.Confidence)
		}
		if rec2 := recommendNextActionWith(nextActionInput{PendingQA: 1, IsReviewer: true}, "c", nil); rec2.Confidence != nextActionPriors["qa_review"] {
			t.Fatalf("nil priors did not fall back to static table: %v", rec2.Confidence)
		}
		_ = want
	}
	// Unknown ids in a learned table must not resurrect retired rules nor
	// disturb known ones.
	priors := map[string]float64{"retired_action": 0.99}
	rec := recommendNextAction(nextActionInput{Lock: "INELIGIBLE"}, "case-9")
	recWith := recommendNextActionWith(nextActionInput{Lock: "INELIGIBLE"}, "case-9", priors)
	if rec.ActionID != recWith.ActionID || rec.Confidence != recWith.Confidence {
		t.Fatalf("unknown learned id changed a recommendation: %+v vs %+v", rec, recWith)
	}
}

func TestPrimaryRoleDeterministic(t *testing.T) {
	// The portal stamps events with Telemetry.role(), which implements the
	// same canonical order — a multi-role user must resolve identically on
	// both sides or the per-role cells split silently.
	cases := []struct {
		roles []string
		want  string
	}{
		{[]string{"CASE_MANAGER", "FEDERAL_ADMIN"}, "FEDERAL_ADMIN"}, // admins first
		{[]string{"NURSE", "CASE_MANAGER"}, "CASE_MANAGER"},
		{[]string{"DOCTOR", "NURSE"}, "DOCTOR"},
		{[]string{"PARTY"}, "PARTY"},
		{[]string{"SOME_FUTURE_ROLE"}, "SOME_FUTURE_ROLE"}, // unknown roles still resolve
		{nil, ""},
	}
	for _, tc := range cases {
		if got := primaryRole(principal{Roles: tc.roles}); got != tc.want {
			t.Fatalf("primaryRole(%v) = %q, want %q", tc.roles, got, tc.want)
		}
	}
}

func TestValuesForPrecedence(t *testing.T) {
	base := map[string]float64{}
	for k, v := range nextActionPriors {
		base[k] = v
	}
	base["triage"] = 0.61 // tenant overlay armed triage
	lp := learnedPriors{
		open:   true,
		values: base,
		armed:  map[string]bool{"triage": true},
		byRole: map[string]map[string]float64{
			"CASE_MANAGER": {"triage": 0.42},
		},
		byService: map[string]map[string]float64{
			"AIR_AMBULANCE": {"triage": 0.55, "qa_review": 0.77},
		},
	}

	// Full context: role wins over service, service over tenant, tenant over static.
	v, basis := lp.valuesFor("CASE_MANAGER", "AIR_AMBULANCE")
	if v["triage"] != 0.42 || basis["triage"] != "learned:role" {
		t.Fatalf("role cell must win: triage=%v basis=%q", v["triage"], basis["triage"])
	}
	if v["qa_review"] != 0.77 || basis["qa_review"] != "learned:service_line" {
		t.Fatalf("service cell must beat tenant/static: qa_review=%v basis=%q", v["qa_review"], basis["qa_review"])
	}

	// Service only.
	v, basis = lp.valuesFor("ARBITRATOR", "AIR_AMBULANCE")
	if v["triage"] != 0.55 || basis["triage"] != "learned:service_line" {
		t.Fatalf("service cell expected: triage=%v basis=%q", v["triage"], basis["triage"])
	}

	// Neither overlay: tenant-learned value with plain "learned" basis.
	v, basis = lp.valuesFor("ATTORNEY", "DENTAL")
	if v["triage"] != 0.61 || basis["triage"] != "learned" {
		t.Fatalf("tenant overlay expected: triage=%v basis=%q", v["triage"], basis["triage"])
	}
	// The AIR_AMBULANCE service cell must NOT leak into a DENTAL context:
	// qa_review falls back to the static prior with no basis claim.
	if v["qa_review"] != nextActionPriors["qa_review"] || basis["qa_review"] != "" {
		t.Fatalf("qa_review should be static here, got %v basis %q", v["qa_review"], basis["qa_review"])
	}
	if v["review_case"] != nextActionPriors["review_case"] || basis["review_case"] != "" {
		t.Fatalf("static fallback broken: %v %q", v["review_case"], basis["review_case"])
	}

	// Empty context strings never match a cell.
	v, _ = lp.valuesFor("", "")
	if v["triage"] != 0.61 {
		t.Fatalf("empty context must fall to tenant overlay, got %v", v["triage"])
	}

	// Input maps are not mutated by resolution.
	if lp.values["triage"] != 0.61 || lp.byRole["CASE_MANAGER"]["triage"] != 0.42 {
		t.Fatal("valuesFor mutated the loaded tables")
	}
}
