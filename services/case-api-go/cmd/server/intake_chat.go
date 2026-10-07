package main

// Conversational intake — conversation-first migration, step 5 (NG).
//
// Intake stops being a form a worker must know how to fill and becomes a
// conversation. The model's ONLY job is extraction: one bounded call per
// turn returning strict JSON — the currently-known intake fields plus its
// next follow-up question.
//
// NG divergence from the original repo: intake shape is MANIFEST-DRIVEN.
// Filing-party codes come from the manifest's terminology (POLICYHOLDER/
// INSURER, CLAIMANT/RESPONDENT…, healthcare legacy PROVIDER/HEALTH_PLAN) and
// sector fields (amounts, claim refs, dates) are the manifest's declared
// intake_fields — extracted into a generic "extra" map and validated against
// the declaration, never free-form keys.
//
// Invariants:
//  1. THE MODEL NEVER FILES — stateless form-filler; the human reviews the
//     prefilled form and the EXISTING createIntake endpoint files (with its
//     own manifest validation + required-field enforcement). Nothing
//     persists pre-filing; the filing itself is the record.
//  2. STRICT JSON, SERVER-VALIDATED — extra keys are intersected with the
//     manifest declaration, select values against declared options, email
//     shape-checked; anything malformed drops to its zero value.
//  3. ONE BOUNDED CALL per turn, temperature 0; conversation state lives in
//     the client, so the endpoint is idempotent and replay-safe.
//  4. GROUNDED — the model extracts only what the worker actually said.

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
)

// intakeFields mirrors createIntake's payload (core + manifest extras).
// Keep them in lockstep: the portal hands extracted values straight to the
// existing filing endpoint without translation.
type intakeFields struct {
	Email           string            `json:"email,omitempty"`
	ContactName     string            `json:"contact_name,omitempty"`
	Org             string            `json:"org,omitempty"`
	FilingPartyType string            `json:"filing_party_type,omitempty"` // manifest party codes
	Notes           string            `json:"notes,omitempty"`
	Extra           map[string]string `json:"extra,omitempty"` // manifest intake_fields, by name
}

type intakeConverseReply struct {
	Fields  intakeFields `json:"fields"`
	Missing []string     `json:"missing"`
	Ready   bool         `json:"ready"`
	Reply   string       `json:"reply"`
	Model   string       `json:"model,omitempty"`
}

// intakeChatSystem builds the extraction prompt for THIS tenant's manifest —
// party codes and sector field names are injected so the model speaks the
// program's language instead of hard-coded healthcare terms.
func intakeChatSystem(codeA, codeB string, m *ProgramManifest) string {
	var extraSpec strings.Builder
	if m != nil {
		for _, f := range m.IntakeFields {
			fmt.Fprintf(&extraSpec, "\n- %q (%s, %s%s): %s", f.Name, f.Type,
				map[bool]string{true: "required", false: "optional"}[f.Required],
				func() string {
					if len(f.Options) > 0 {
						return ", one of: " + strings.Join(f.Options, "/")
					}
					return ""
				}(), f.Label)
		}
	}
	return `You are the intake assistant inside a dispute-resolution case platform. A case worker is describing a new intake in plain language. Your ONLY job is to extract structured fields and ask the next question.

OUTPUT: strict JSON only, no prose, no markdown fences:
{"fields":{"email":"","contact_name":"","org":"","filing_party_type":"","notes":"","extra":{}},"reply":""}

RULES:
- Extract ONLY what the worker actually stated across the conversation. Never guess emails, names, or values. Unknown strings stay "", extra keys absent when unknown.
- filing_party_type is ` + codeA + ` or ` + codeB + ` only; leave "" if unclear.
- "extra" holds ONLY these declared sector fields (by exact name):` + extraSpec.String() + `
- Dates as YYYY-MM-DD; numbers as plain numbers (no currency symbols, no commas).
- notes: one sentence summarizing the situation in the worker's own terms, plus anything else they mentioned (urgency, reference ids).
- reply: if email, organization/contact, or any required sector field is still unknown, ask ONE short conversational follow-up for the most important gap. If everything needed is present, reply with a one-sentence summary for the worker to confirm, ending with "File it?".
- Keep prior extracted fields stable unless the worker corrects them.`
}

// intakeMissing computes what still blocks filing — server-side, so the
// model can never declare an intake ready early.
func intakeMissing(f intakeFields, m *ProgramManifest) []string {
	var miss []string
	if !strings.Contains(f.Email, "@") {
		miss = append(miss, "contact email")
	}
	if f.Org == "" && f.ContactName == "" {
		miss = append(miss, "organization or contact name")
	}
	if m != nil {
		for _, mf := range m.IntakeFields {
			if mf.Required && strings.TrimSpace(f.Extra[mf.Name]) == "" {
				miss = append(miss, mf.Label)
			}
		}
	}
	return miss
}

// validateIntakeFields is the trust boundary: model output becomes platform
// data only through these checks. Extra keys not declared in the manifest
// are dropped; select values outside declared options are dropped.
func validateIntakeFields(f intakeFields, codeA, codeB string, m *ProgramManifest) intakeFields {
	out := intakeFields{
		Email:       truncate(strings.TrimSpace(f.Email), 254),
		ContactName: truncate(strings.TrimSpace(f.ContactName), 200),
		Org:         truncate(strings.TrimSpace(f.Org), 200),
		Notes:       truncate(strings.TrimSpace(f.Notes), 2000),
		Extra:       map[string]string{},
	}
	if !strings.Contains(out.Email, "@") {
		out.Email = ""
	}
	fpt := strings.ToUpper(strings.TrimSpace(f.FilingPartyType))
	if fpt == codeA || fpt == codeB {
		out.FilingPartyType = fpt
	}
	if m != nil {
		for _, mf := range m.IntakeFields {
			v, ok := f.Extra[mf.Name]
			if !ok {
				continue
			}
			v = truncate(strings.TrimSpace(v), 300)
			if v == "" {
				continue
			}
			if mf.Type == "select" && len(mf.Options) > 0 {
				okOpt := false
				for _, o := range mf.Options {
					if v == o {
						okOpt = true
						break
					}
				}
				if !okOpt {
					continue
				}
			}
			out.Extra[mf.Name] = v
		}
	}
	if len(out.Extra) == 0 {
		out.Extra = nil
	}
	return out
}

// parseIntakeModelJSON tolerates prose around the JSON object (brace-cut,
// same discipline as the action-batch parser) and validates every field.
func parseIntakeModelJSON(raw, codeA, codeB string, m *ProgramManifest) (intakeFields, string, error) {
	start := strings.Index(raw, "{")
	end := strings.LastIndex(raw, "}")
	if start < 0 || end <= start {
		return intakeFields{}, "", fmt.Errorf("no JSON object in model output")
	}
	var out struct {
		Fields intakeFields `json:"fields"`
		Reply  string       `json:"reply"`
	}
	if err := json.Unmarshal([]byte(raw[start:end+1]), &out); err != nil {
		return intakeFields{}, "", fmt.Errorf("model JSON not parseable: %w", err)
	}
	return validateIntakeFields(out.Fields, codeA, codeB, m), truncate(strings.TrimSpace(out.Reply), 500), nil
}

// intakeConverse handles POST /intake/converse {message, fields, history}.
// One extraction call in, one validated field-set + follow-up question out.
func (s *server) intakeConverse(w http.ResponseWriter, r *http.Request) {
	p := r.Context().Value(ctxPrincipal{}).(principal)
	if !hasAnyRole(p, copilotRoles...) {
		http.Error(w, `{"error":"forbidden: requires case staff role"}`, http.StatusForbidden)
		return
	}
	if s.cfg.CopilotEndpoint == "" {
		http.Error(w, `{"error":"intake assistant not configured (COPILOT_ENDPOINT empty)"}`, http.StatusServiceUnavailable)
		return
	}
	tenant := r.Context().Value(ctxTenant{}).(string)
	var in struct {
		Message string       `json:"message"`
		Fields  intakeFields `json:"fields"`
		History []string     `json:"history"` // prior worker turns, plain text
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil || strings.TrimSpace(in.Message) == "" {
		http.Error(w, `{"error":"message required"}`, http.StatusBadRequest)
		return
	}
	in.Message = truncate(strings.TrimSpace(in.Message), 2000)
	if len(in.History) > 20 {
		in.History = in.History[len(in.History)-20:]
	}

	codeA, codeB := "PROVIDER", "HEALTH_PLAN"
	var manifest *ProgramManifest
	if m, err := s.manifestFor(r.Context(), tenant); err == nil && m != nil {
		manifest = m
		if m.Terminology.PartyACode != "" {
			codeA = m.Terminology.PartyACode
		}
		if m.Terminology.PartyBCode != "" {
			codeB = m.Terminology.PartyBCode
		}
	}

	var convo strings.Builder
	for _, h := range in.History {
		convo.WriteString("Worker: " + truncate(h, 500) + "\n")
	}
	convo.WriteString("Worker: " + in.Message + "\n")
	fieldsJSON, _ := json.Marshal(validateIntakeFields(in.Fields, codeA, codeB, manifest))
	user := "CURRENT EXTRACTED FIELDS (JSON, keep stable unless corrected):\n" + string(fieldsJSON) +
		"\n\nCONVERSATION SO FAR:\n" + convo.String()

	raw, err := ollamaChat(r.Context(), s.cfg.CopilotEndpoint, s.cfg.CopilotModel,
		intakeChatSystem(codeA, codeB, manifest), user, 500)
	if err != nil {
		http.Error(w, `{"error":"model unavailable — you can still use the form directly"}`, http.StatusBadGateway)
		return
	}
	fields, reply, err := parseIntakeModelJSON(raw, codeA, codeB, manifest)
	if err != nil {
		http.Error(w, `{"error":"model output not usable — rephrase or use the form directly"}`, http.StatusBadGateway)
		return
	}
	missing := intakeMissing(fields, manifest)
	if len(missing) == 0 && reply == "" {
		reply = "I have everything needed. Review the fields and file when ready."
	}
	writeJSON(w, http.StatusOK, intakeConverseReply{
		Fields:  fields,
		Missing: missing,
		Ready:   len(missing) == 0,
		Reply:   reply,
		Model:   s.cfg.CopilotModel,
	})
}
