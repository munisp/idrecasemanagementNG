// manifest.go — Program Manifest: the sector-agnostic keystone.
//
// A manifest (public.program_rules config.manifest) declares a program's
// terminology, lifecycle state machine, and statutory clocks as DATA. Both
// sides read it fresh per request: case-api validates intake transitions
// against it (falling back to the built-in FL/NSA set when absent), and the
// portal renders every label and pipeline from it. See docs/program-manifest.md.
//
// Design contract: an absent manifest = legacy behavior; a PRESENT but
// invalid manifest = fail closed (status transitions blocked, defaults never
// guessed) — policy must never be half-applied.
package main

import (
	"context"
	"encoding/json"
	"fmt"
)

type manifestStage struct {
	Name         string `json:"name"`
	Label        string `json:"label"`
	Terminal     bool   `json:"terminal,omitempty"`
	Completed    bool   `json:"completed,omitempty"`     // resting state for finished work
	AnchorsClock string `json:"anchors_clock,omitempty"` // intake only: DB column this stage anchors
}

type manifestClock struct {
	Name    string `json:"name"`
	Days    int    `json:"days"`
	DayType string `json:"day_type"` // calendar|business
	Basis   string `json:"basis"`    // fact/column the clock runs from
}

type ProgramManifest struct {
	Program     string `json:"program"`
	Version     string `json:"version"`
	Sector      string `json:"sector"`
	Terminology struct {
		CaseNoun   string `json:"case_noun"`
		CasePlural string `json:"case_plural"`
		PartyA     string `json:"party_a"`
		PartyB     string `json:"party_b"`
		Neutral    string `json:"neutral"`
		IntakeNoun string `json:"intake_noun"`
		// Filing-party codes (legacy healthcare default: PROVIDER/HEALTH_PLAN).
		// Sectors declare their own: POLICYHOLDER/INSURER, CLAIMANT/RESPONDENT…
		PartyACode string `json:"party_a_code,omitempty"`
		PartyBCode string `json:"party_b_code,omitempty"`
		// Display labels for the two healthcare-shaped data fields every
		// dispute has: the disputed AMOUNT and the service/subject line.
		// Healthcare: "QPA"/"Service line"; appraisal: "Estimate"/"Loss type";
		// tax: "Assessed value"/"Parcel"…
		AmountLabel  string `json:"amount_label,omitempty"`
		ServiceLabel string `json:"service_label,omitempty"`
	} `json:"terminology"`
	// IntakeFields: sector-specific intake form fields, rendered dynamically
	// by the portal and stored verbatim in intake_requests.details.
	IntakeFields []struct {
		Name     string   `json:"name"`
		Label    string   `json:"label"`
		Type     string   `json:"type"` // text|email|number|date|select
		Required bool     `json:"required,omitempty"`
		Options  []string `json:"options,omitempty"`
	} `json:"intake_fields,omitempty"`
	// Documents: doc-intel extraction schemas per doc_type (phase 2).
	Documents struct {
		Schemas map[string]struct {
			Fields []string `json:"fields"`
		} `json:"schemas,omitempty"`
	} `json:"documents,omitempty"`
	// Determination: which valuation engine this program uses (phase 3).
	Determination struct {
		Engine string `json:"engine"` // qpa|fee_schedule|comparable|final_offer
	} `json:"determination,omitempty"`
	// Letters: statutory notice template pack (phase 4).
	Letters struct {
		Pack      string   `json:"pack"` // object-store prefix, e.g. packs/fl-ahca/letters
		Templates []string `json:"templates,omitempty"`
	} `json:"letters,omitempty"`
	// Roster: neutral eligibility requirements (phase 5).
	Roster struct {
		Certifications []string `json:"certifications,omitempty"`
		MinCases       int      `json:"min_cases,omitempty"`
	} `json:"roster,omitempty"`
	Lifecycle struct {
		IntakeStatuses []manifestStage `json:"intake_statuses"`
		CaseStatuses   []manifestStage `json:"case_statuses"`
	} `json:"lifecycle"`
	Clocks []manifestClock `json:"clocks"`
}

// manifestFor loads the tenant's manifest fresh from config. Returns
// (nil, nil) when no manifest is configured — callers use legacy defaults.
// A syntactically present but invalid manifest returns an error so callers
// can fail closed.
func (s *server) manifestFor(ctx context.Context, tenant string) (*ProgramManifest, error) {
	var raw []byte
	err := s.db.QueryRow(ctx,
		`SELECT coalesce(config->'manifest','null'::jsonb) FROM public.program_rules WHERE tenant=$1`,
		tenant).Scan(&raw)
	if err != nil || string(raw) == "null" {
		return nil, nil
	}
	var m ProgramManifest
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, fmt.Errorf("stored manifest is invalid JSON: %w", err)
	}
	if err := validateManifest(&m); err != nil {
		return nil, fmt.Errorf("stored manifest invalid: %w", err)
	}
	return &m, nil
}

// validateManifest enforces structural soundness with one precise error per
// problem — an invalid manifest must never become (or remain) live policy.
func validateManifest(m *ProgramManifest) error {
	if m.Program == "" {
		return fmt.Errorf("program is required")
	}
	if m.Version == "" {
		return fmt.Errorf("version is required")
	}
	t := m.Terminology
	for what, v := range map[string]string{
		"terminology.case_noun": t.CaseNoun, "terminology.case_plural": t.CasePlural,
		"terminology.party_a": t.PartyA, "terminology.party_b": t.PartyB,
		"terminology.neutral": t.Neutral, "terminology.intake_noun": t.IntakeNoun,
	} {
		if v == "" {
			return fmt.Errorf("%s is required (every user-facing noun must be declared)", what)
		}
	}
	checkStages := func(kind string, stages []manifestStage) error {
		if len(stages) == 0 {
			return fmt.Errorf("lifecycle.%s must declare at least one status", kind)
		}
		seen := map[string]bool{}
		for _, st := range stages {
			if st.Name == "" {
				return fmt.Errorf("lifecycle.%s: every status needs a name", kind)
			}
			if seen[st.Name] {
				return fmt.Errorf("lifecycle.%s: duplicate status %q", kind, st.Name)
			}
			seen[st.Name] = true
			if st.Label == "" {
				return fmt.Errorf("lifecycle.%s: status %q needs a label", kind, st.Name)
			}
			if st.Terminal && st.AnchorsClock != "" {
				return fmt.Errorf("lifecycle.%s: terminal status %q cannot anchor a clock", kind, st.Name)
			}
		}
		return nil
	}
	if err := checkStages("intake_statuses", m.Lifecycle.IntakeStatuses); err != nil {
		return err
	}
	if err := checkStages("case_statuses", m.Lifecycle.CaseStatuses); err != nil {
		return err
	}
	clockSeen := map[string]bool{}
	for _, c := range m.Clocks {
		if c.Name == "" || clockSeen[c.Name] {
			return fmt.Errorf("clocks: missing or duplicate name %q", c.Name)
		}
		clockSeen[c.Name] = true
		if c.Days <= 0 {
			return fmt.Errorf("clocks.%s: days must be positive", c.Name)
		}
		if c.DayType != "calendar" && c.DayType != "business" {
			return fmt.Errorf("clocks.%s: day_type must be calendar or business", c.Name)
		}
		if c.Basis == "" {
			return fmt.Errorf("clocks.%s: basis (anchor fact) is required", c.Name)
		}
	}
	// Intake field declarations (phase 2): known types, select needs options.
	fieldSeen := map[string]bool{}
	for _, f := range m.IntakeFields {
		if !ruleDetailKey.MatchString(f.Name) {
			return fmt.Errorf("intake_fields: invalid field name %q", f.Name)
		}
		if fieldSeen[f.Name] {
			return fmt.Errorf("intake_fields: duplicate field %q", f.Name)
		}
		fieldSeen[f.Name] = true
		if f.Label == "" {
			return fmt.Errorf("intake_fields.%s: label required", f.Name)
		}
		switch f.Type {
		case "text", "email", "number", "date":
		case "select":
			if len(f.Options) == 0 {
				return fmt.Errorf("intake_fields.%s: select field needs options", f.Name)
			}
		default:
			return fmt.Errorf("intake_fields.%s: unknown type %q", f.Name, f.Type)
		}
	}
	// Determination engine (phase 3): must be a registered implementation.
	if m.Determination.Engine != "" && !knownEngines[m.Determination.Engine] {
		return fmt.Errorf("determination.engine %q unknown (registered: qpa, fee_schedule, comparable, final_offer)", m.Determination.Engine)
	}
	return nil
}

// intakeMachine exposes the manifest's intake state machine, or nil when the
// tenant has no manifest (caller falls back to the legacy whitelist).
type intakeMachine struct {
	allowed  map[string]bool
	terminal map[string]bool
	list     string
}

func (m *ProgramManifest) intakeMachine() *intakeMachine {
	if m == nil || len(m.Lifecycle.IntakeStatuses) == 0 {
		return nil
	}
	im := &intakeMachine{allowed: map[string]bool{}, terminal: map[string]bool{}}
	first := true
	for _, st := range m.Lifecycle.IntakeStatuses {
		im.allowed[st.Name] = true
		if st.Terminal {
			im.terminal[st.Name] = true
		}
		if !first {
			im.list += ", "
		}
		im.list += st.Name
		first = false
	}
	return im
}
