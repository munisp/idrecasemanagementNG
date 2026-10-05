package main

import "testing"

func validManifest() *ProgramManifest {
	var m ProgramManifest
	m.Program, m.Version, m.Sector = "test", "1.0", "testing"
	m.Terminology.CaseNoun = "Dispute"
	m.Terminology.CasePlural = "Disputes"
	m.Terminology.PartyA = "Claimant"
	m.Terminology.PartyB = "Respondent"
	m.Terminology.Neutral = "Arbitrator"
	m.Terminology.IntakeNoun = "Intake"
	m.Lifecycle.IntakeStatuses = []manifestStage{
		{Name: "OPEN", Label: "Open"},
		{Name: "DONE", Label: "Done", Terminal: true},
	}
	m.Lifecycle.CaseStatuses = []manifestStage{{Name: "ACTIVE", Label: "Active"}}
	return &m
}

func TestValidateManifestGood(t *testing.T) {
	m := validManifest()
	m.Clocks = []manifestClock{{Name: "review", Days: 10, DayType: "calendar", Basis: "packet_complete_at"}}
	if err := validateManifest(m); err != nil {
		t.Fatalf("valid manifest rejected: %v", err)
	}
}

func TestValidateManifestBad(t *testing.T) {
	cases := map[string]func(*ProgramManifest){
		"missing program":      func(m *ProgramManifest) { m.Program = "" },
		"missing terminology":  func(m *ProgramManifest) { m.Terminology.Neutral = "" },
		"duplicate status":     func(m *ProgramManifest) { m.Lifecycle.IntakeStatuses = append(m.Lifecycle.IntakeStatuses, m.Lifecycle.IntakeStatuses[0]) },
		"empty case statuses":  func(m *ProgramManifest) { m.Lifecycle.CaseStatuses = nil },
		"terminal anchors":     func(m *ProgramManifest) { m.Lifecycle.IntakeStatuses[1].AnchorsClock = "x" },
		"clock zero days":      func(m *ProgramManifest) { m.Clocks = []manifestClock{{Name: "c", Days: 0, DayType: "calendar", Basis: "b"}} },
		"clock bad day type":   func(m *ProgramManifest) { m.Clocks = []manifestClock{{Name: "c", Days: 5, DayType: "lunar", Basis: "b"}} },
		"clock missing basis":  func(m *ProgramManifest) { m.Clocks = []manifestClock{{Name: "c", Days: 5, DayType: "business"}} },
		"missing status label": func(m *ProgramManifest) { m.Lifecycle.IntakeStatuses[0].Label = "" },
	}
	for name, mutate := range cases {
		m := validManifest()
		mutate(m)
		if err := validateManifest(m); err == nil {
			t.Fatalf("%s: invalid manifest accepted", name)
		}
	}
}

func TestIntakeMachineFromManifest(t *testing.T) {
	m := validManifest()
	im := m.intakeMachine()
	if im == nil || !im.allowed["OPEN"] || !im.terminal["DONE"] || im.terminal["OPEN"] {
		t.Fatalf("machine wrong: %+v", im)
	}
	if im.list != "OPEN, DONE" {
		t.Fatalf("list = %q", im.list)
	}
	var nilManifest *ProgramManifest
	if nilManifest.intakeMachine() != nil {
		t.Fatal("nil manifest must yield nil machine (legacy fallback)")
	}
}
