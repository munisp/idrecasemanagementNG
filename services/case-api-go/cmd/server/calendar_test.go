package main

import (
	"testing"
	"time"
)

func d(s string) time.Time { t, _ := time.Parse("2006-01-02", s); return t }

func TestBusinessDaysBetweenWeekends(t *testing.T) {
	// Fri → Mon = 1 business day
	if n := businessDaysBetween(d("2026-10-02"), d("2026-10-05"), nil); n != 1 {
		t.Fatalf("Fri→Mon = %d, want 1", n)
	}
	// Full week = 5
	if n := businessDaysBetween(d("2026-10-05"), d("2026-10-12"), nil); n != 5 {
		t.Fatalf("week = %d, want 5", n)
	}
}

func TestBusinessDaysBetweenHolidays(t *testing.T) {
	hol := map[string]bool{"2026-10-12": true} // Columbus Day
	if n := businessDaysBetween(d("2026-10-05"), d("2026-10-12"), hol); n != 4 {
		t.Fatalf("week with holiday = %d, want 4", n)
	}
}

func TestManifestRejectsUnknownEngine(t *testing.T) {
	m := validManifest()
	m.Determination.Engine = "magic_8ball"
	if err := validateManifest(m); err == nil {
		t.Fatal("unknown determination engine accepted")
	}
	m.Determination.Engine = "comparable"
	if err := validateManifest(m); err != nil {
		t.Fatalf("registered engine rejected: %v", err)
	}
}

func TestManifestIntakeFieldValidation(t *testing.T) {
	m := validManifest()
	m.IntakeFields = []struct {
		Name     string   `json:"name"`
		Label    string   `json:"label"`
		Type     string   `json:"type"`
		Required bool     `json:"required,omitempty"`
		Options  []string `json:"options,omitempty"`
	}{{Name: "policy_number", Label: "Policy number", Type: "text", Required: true}}
	if err := validateManifest(m); err != nil {
		t.Fatalf("valid intake field rejected: %v", err)
	}
	m.IntakeFields[0].Type = "select" // select without options must fail
	if err := validateManifest(m); err == nil {
		t.Fatal("select without options accepted")
	}
}
