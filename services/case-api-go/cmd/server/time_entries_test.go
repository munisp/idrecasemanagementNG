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
