package main

import "testing"

func TestTimeEntryNormalize(t *testing.T) {
	// minutes win over hours
	m, d, err := (&timeEntryIn{Minutes: 45, Hours: 2, EntryDate: "2026-10-08"}).normalize()
	if err != nil || m != 45 || d != "2026-10-08" {
		t.Fatalf("minutes precedence: %d %s %v", m, d, err)
	}
	// hours convert
	m, _, err = (&timeEntryIn{Hours: 1.5}).normalize()
	if err != nil || m != 90 {
		t.Fatalf("hours conversion: %d %v", m, err)
	}
	// zero/negative/over-day rejected
	for _, bad := range []timeEntryIn{{Minutes: 0}, {Minutes: -5}, {Minutes: 1441}, {Hours: 25}} {
		if _, _, err := bad.normalize(); err == nil {
			t.Errorf("expected rejection: %+v", bad)
		}
	}
	// bad date rejected
	if _, _, err := (&timeEntryIn{Minutes: 30, EntryDate: "10/08/2026"}).normalize(); err == nil {
		t.Error("non-ISO date accepted")
	}
}

func TestTimeEntryRole(t *testing.T) {
	// clinical split: DOCTOR/NURSE record as themselves, not the legacy blend
	if got := timeEntryRole(principal{Subject: "x", Roles: []string{"DOCTOR"}}); got != "DOCTOR" {
		t.Errorf("doctor: %s", got)
	}
	if got := timeEntryRole(principal{Subject: "x", Roles: []string{"NURSE", "PLATFORM_ADMIN"}}); got != "NURSE" {
		t.Errorf("nurse preferred over admin: %s", got)
	}
	if got := timeEntryRole(principal{Subject: "x", Roles: []string{"PLATFORM_ADMIN"}}); got != "PLATFORM_ADMIN" {
		t.Errorf("admin fallback: %s", got)
	}
}

func TestToInt64(t *testing.T) {
	if toInt64(int64(7)) != 7 || toInt64(2.0) != 2 || toInt64("42") != 42 || toInt64(nil) != 0 {
		t.Error("toInt64 coercion wrong")
	}
}

func TestTimeReportPeriod(t *testing.T) {
	// Range wins over month/week.
	from, to, _, err := timeReportPeriod("2026-10-05", "2026-10", "2026-10-01", "2026-10-08")
	if err != nil || from != "2026-10-01" || to != "2026-10-08" {
		t.Fatalf("range: %s %s %v", from, to, err)
	}
	// Month preset.
	from, to, _, err = timeReportPeriod("", "2026-02", "", "")
	if err != nil || from != "2026-02-01" || to != "2026-03-01" {
		t.Fatalf("month: %s %s %v", from, to, err)
	}
	// Week preset: any day resolves to the Monday..+7d window.
	from, to, label, err := timeReportPeriod("2026-10-08", "", "", "") // a Thursday
	if err != nil || from != "2026-10-05" || to != "2026-10-12" {
		t.Fatalf("week: %s %s %v", from, to, err)
	}
	if label != "week of 2026-10-05" {
		t.Fatalf("label: %s", label)
	}
	// Errors.
	if _, _, _, err = timeReportPeriod("", "", "2026-10-08", "2026-10-01"); err == nil {
		t.Fatal("reversed range must fail")
	}
	if _, _, _, err = timeReportPeriod("", "", "2025-01-01", "2026-02-01"); err == nil {
		t.Fatal(">366 days must fail")
	}
	if _, _, _, err = timeReportPeriod("", "2026-13", "", ""); err == nil {
		t.Fatal("bad month must fail")
	}
}

func TestCsvCell(t *testing.T) {
	if got := csvCell("plain"); got != "plain" {
		t.Fatal(got)
	}
	if got := csvCell(`a,b`); got != `"a,b"` {
		t.Fatal(got)
	}
	if got := csvCell(`she said "hi"`); got != `"she said ""hi"""` {
		t.Fatal(got)
	}
	if got := moneyCell(nil); got != "" {
		t.Fatal(got)
	}
	var c int64 = 123456
	if got := moneyCell(&c); got != "1234.56" {
		t.Fatal(got)
	}
}
