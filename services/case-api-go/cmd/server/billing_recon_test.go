package main

import (
	"net/url"
	"strings"
	"testing"
)

func TestPeriodFromParams(t *testing.T) {
	// month preset
	f, to, _, err := periodFromParams(url.Values{"month": {"2026-10"}})
	if err != nil || f != "2026-10-01" || to != "2026-11-01" {
		t.Fatalf("month: %v %v %v", f, to, err)
	}
	// custom range
	f, to, _, err = periodFromParams(url.Values{"start": {"2026-10-05"}, "end": {"2026-10-20"}})
	if err != nil || f != "2026-10-05" || to != "2026-10-20" {
		t.Fatalf("range: %v %v %v", f, to, err)
	}
	// week preset anchors to Monday, end exclusive
	f, to, _, err = periodFromParams(url.Values{"week": {"2026-10-08"}}) // Thursday
	if err != nil || f != "2026-10-05" || to != "2026-10-12" {
		t.Fatalf("week: %v %v %v", f, to, err)
	}
	// errors
	if _, _, _, err = periodFromParams(url.Values{"start": {"2026-10-20"}, "end": {"2026-10-05"}}); err == nil {
		t.Fatal("expected end-after-start error")
	}
	if _, _, _, err = periodFromParams(url.Values{"month": {"2026-13"}}); err == nil {
		t.Fatal("expected bad-month error")
	}
	if _, _, _, err = periodFromParams(url.Values{"start": {"2024-01-01"}, "end": {"2026-01-01"}}); err == nil {
		t.Fatal("expected range-too-large error")
	}
}

func TestLineAmount(t *testing.T) {
	// 90 minutes at $150/h -> $225.00
	if got := lineAmount(90, 15000); got != 22500 {
		t.Fatalf("lineAmount: got %d want 22500", got)
	}
	// rounding: 1 minute at $100/h -> 166.67c -> 167c (half-up)
	if got := lineAmount(1, 10000); got != 167 {
		t.Fatalf("rounding: got %d want 167", got)
	}
	if got := lineAmount(0, 15000); got != 0 {
		t.Fatalf("zero minutes: got %d", got)
	}
}

func TestCSVReconAdapter(t *testing.T) {
	ad := csvReconAdapter{}
	qb := builtinCSVMappings()["quickbooks"]
	csvText := "Date,Description,Amount,Ref\n10/03/2026,STRIPE PAYOUT,\"$1,234.56\",SVC-FL-2026-0007\n2026-10-04,check fee,(12.34),\n"
	items, err := ad.parse(strings.NewReader(csvText), qb)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(items) != 2 {
		t.Fatalf("want 2 items, got %d", len(items))
	}
	if items[0].Date != "2026-10-03" || items[0].AmountCents != 123456 || items[0].Reference != "SVC-FL-2026-0007" {
		t.Fatalf("item0: %+v", items[0])
	}
	if items[1].AmountCents != -1234 {
		t.Fatalf("paren negative: %+v", items[1])
	}
	// missing mapped column -> error
	_, err = ad.parse(strings.NewReader("Foo,Bar\n1,2\n"), qb)
	if err == nil {
		t.Fatal("expected missing-column error")
	}
}

func TestParseMoneyToCents(t *testing.T) {
	cases := map[string]int64{"100.00": 10000, "$1,234.56": 123456, "(5.50)": -550, "-7": -700, "0.01": 1}
	for in, want := range cases {
		got, err := parseMoneyToCents(in)
		if err != nil || got != want {
			t.Fatalf("%q: got %d err %v want %d", in, got, err, want)
		}
	}
}

func TestAutoMatch(t *testing.T) {
	items := []reconItem{
		{Date: "2026-10-03", AmountCents: 10000, Reference: "pi_123"},     // exact ref
		{Date: "2026-10-04", AmountCents: 5005},                          // tolerance ($1 cap)
		{Date: "2026-10-05", AmountCents: 7000},                          // ambiguous (two candidates)
		{Date: "2026-10-06", AmountCents: 99999},                         // none
	}
	events := []finEvent{
		{ID: 1, AmountCents: 10000, Ref: "pi_123", Date: "2026-10-01"},
		{ID: 2, AmountCents: 5000, Date: "2026-10-03"},
		{ID: 3, AmountCents: 7000, Date: "2026-10-05"},
		{ID: 4, AmountCents: 7000, Date: "2026-10-06"},
	}
	res := autoMatch(items, events, 100, 5)
	if res[0].Kind != "exact_ref" || res[0].EventID != 1 {
		t.Fatalf("exact: %+v", res[0])
	}
	if res[1].Kind != "tolerance" || res[1].EventID != 2 {
		t.Fatalf("tolerance: %+v", res[1])
	}
	if res[2].Kind != "ambiguous" || res[2].Candidate != 2 {
		t.Fatalf("ambiguous: %+v", res[2])
	}
	if res[3].Kind != "none" {
		t.Fatalf("none: %+v", res[3])
	}
	// date window: candidate outside window must not match
	items2 := []reconItem{{Date: "2026-10-01", AmountCents: 5000}}
	events2 := []finEvent{{ID: 9, AmountCents: 5000, Date: "2026-10-20"}}
	res2 := autoMatch(items2, events2, 100, 5)
	if res2[0].Kind != "none" {
		t.Fatalf("window: %+v", res2[0])
	}
}

func TestAgingBucket(t *testing.T) {
	cases := map[int]string{-3: "current", 0: "current", 1: "1-30", 30: "1-30", 31: "31-60", 60: "31-60", 61: "61-90", 90: "61-90", 91: "90+"}
	for d, want := range cases {
		if got := agingBucket(d); got != want {
			t.Fatalf("agingBucket(%d) = %s want %s", d, got, want)
		}
	}
}

func TestAllocatePayment(t *testing.T) {
	// exact proportional split
	got := allocatePayment(10000, []int64{5000, 3000, 2000})
	if got[0] != 5000 || got[1] != 3000 || got[2] != 2000 {
		t.Fatalf("exact: %v", got)
	}
	// rounding: 100c across 3 equal weights -> 34/33/33, sums exactly
	got = allocatePayment(100, []int64{1, 1, 1})
	var sum int64
	for _, a := range got {
		sum += a
	}
	if sum != 100 {
		t.Fatalf("sum: %v = %d", got, sum)
	}
	if got[0] != 34 || got[1] != 33 || got[2] != 33 {
		t.Fatalf("largest-remainder: %v", got)
	}
	// zero weights -> zeros, no panic
	if got := allocatePayment(500, []int64{0, 0}); got[0] != 0 || got[1] != 0 {
		t.Fatalf("zero weights: %v", got)
	}
}
