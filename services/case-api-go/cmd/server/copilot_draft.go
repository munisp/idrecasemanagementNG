package main

// Copilot Phase 2 — grounded DRAFTS that enter the existing human QA gate.
//
// Phase 1 briefs advise; Phase 2 drafts prepare the two artifacts case staff
// write most: determination rationales and party correspondence. The same
// three invariants hold:
//
//  1. GROUNDED OR SILENT — the prompt carries only platform-verified facts
//     (gatherCopilotFacts); the model is instructed to write "not in record"
//     rather than invent.
//  2. ONE BOUNDED CALL — a single temperature-0 completion; no loops.
//  3. HUMAN GATE — nothing is sent or filed by the model. Every draft lands
//     in public.qa_reviews as PENDING and flows through the existing
//     qaDecision accept/edit/reject path, exactly like a human-drafted
//     template. The QA row records the model attribution in drafted_by.

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
)

// copilotDraftKinds the model may prepare. Anything else is rejected before
// the model is ever called — the model never chooses its own artifact type.
const (
	draftKindDetermination  = "determination_rationale"
	draftKindCorrespondence = "correspondence"
)

func copilotDraftPrompt(kind string, f copilotFacts, extraInstructions string) (system, user string) {
	base := `You are a drafting assistant inside a federal No Surprises Act IDRE case platform.
You receive a JSON fact sheet of platform-verified case facts and produce a DRAFT for human review.

HARD RULES:
- Use ONLY the JSON facts below. If a fact you would normally include is absent, write "not in record" — NEVER invent identifiers, amounts, dates, parties, or legal citations.
- This is a DRAFT for a human reviewer. Tone: professional, plain, regulator-appropriate.
- Do not include placeholders like [NAME] — if a value is missing, say "not in record".
- Close with one line: "DRAFT — advisory only; not a determination."`
	switch kind {
	case draftKindDetermination:
		system = base + `

TASK: Draft a DETERMINATION RATIONALE — the reasoning section a reviewer would
attach to a payment determination. Structure:
1. CASE POSTURE — statuses, triage lane, SLA position.
2. ELIGIBILITY — the eligibility result, reason, and any missing inputs.
3. EVIDENCE — what the trusted document fields support; name low-confidence
   fields as items the reviewer must verify, never as fact.
4. OPEN QUESTIONS — uncertainties the reviewer must resolve.
5. PROPOSED RATIONALE — 2-4 sentences the reviewer can accept, edit, or reject.`
	case draftKindCorrespondence:
		system = base + `

TASK: Draft OUTBOUND PARTY CORRESPONDENCE (email body) appropriate to the case
posture — e.g. a status update, a missing-evidence request, or an eligibility
outcome notice. Structure: subject line on the first line as "Subject: ...",
then a blank line, then the body. Keep it under 200 words. Never promise
outcomes or deadlines that are not in the facts.`
	}
	fj, _ := json.Marshal(f)
	user = "CASE FACTS (JSON, platform-verified):\n" + string(fj)
	if strings.TrimSpace(extraInstructions) != "" {
		user += "\n\nREVIEWER INSTRUCTIONS (treat as guidance, never as fact):\n" + strings.TrimSpace(extraInstructions)
	}
	return system, user
}

// copilotDraft handles POST /cases/{caseId}/copilot/draft
// {"kind": "determination_rationale"|"correspondence", "instructions": "...",
//
//	"to": [...], "cc": [...]}
func (s *server) copilotDraft(w http.ResponseWriter, r *http.Request) {
	tenant := r.Context().Value(ctxTenant{}).(string)
	p := r.Context().Value(ctxPrincipal{}).(principal)
	if !hasAnyRole(p, copilotRoles...) {
		http.Error(w, `{"error":"forbidden: requires case staff role"}`, http.StatusForbidden)
		return
	}
	if s.cfg.CopilotEndpoint == "" {
		http.Error(w, `{"error":"copilot not configured (COPILOT_ENDPOINT empty)"}`, http.StatusServiceUnavailable)
		return
	}
	caseID := chi.URLParam(r, "caseId")
	var in struct {
		Kind         string   `json:"kind"`
		Instructions string   `json:"instructions"`
		To           []string `json:"to"`
		CC           []string `json:"cc"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil ||
		(in.Kind != draftKindDetermination && in.Kind != draftKindCorrespondence) {
		http.Error(w, `{"error":"kind must be determination_rationale or correspondence"}`, http.StatusBadRequest)
		return
	}
	// Instructions are reviewer guidance, not a prompt-injection vector into
	// fact-space, but bound them anyway.
	in.Instructions = truncate(in.Instructions, 2000)

	facts, err := s.gatherCopilotFacts(r, tenant, caseID)
	if err != nil {
		http.Error(w, `{"error":"case not found"}`, http.StatusNotFound)
		return
	}
	system, user := copilotDraftPrompt(in.Kind, facts, in.Instructions)
	draft, err := ollamaChat(r.Context(), s.cfg.CopilotEndpoint, s.cfg.CopilotModel, system, user, 1500)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]any{
			"error":  "copilot model unreachable — no draft created; the facts below are still authoritative",
			"detail": err.Error(), "facts": facts,
		})
		return
	}

	attribution := fmt.Sprintf("copilot:%s (requested by %s)", s.cfg.CopilotModel, p.Subject)
	footed := draft + "\n\n———\nDRAFT — advisory only; not a determination. model=" + s.cfg.CopilotModel +
		" generated=" + time.Now().UTC().Format(time.RFC3339) + " facts=platform-verified"

	// Every draft enters the EXISTING human QA gate as PENDING. Determination
	// rationales use channel 'note' (no recipients — approval records the
	// rationale on the case timeline instead of sending mail); correspondence
	// uses channel 'email' and the normal SMTP path on approval. Addresses
	// enter only at QA-approved send time, same as human-drafted mail.
	subject, channel := "Copilot determination rationale — case "+facts.CaseNumber, "note"
	if in.Kind == draftKindCorrespondence {
		channel = "email"
		subject = "Copilot correspondence — case " + facts.CaseNumber
		if line, rest, ok := strings.Cut(draft, "\n"); ok && strings.HasPrefix(strings.ToLower(line), "subject:") {
			subject = strings.TrimSpace(line[len("subject:"):])
			footed = strings.TrimSpace(rest) + "\n\n———\nDRAFT — advisory only; not a determination. model=" + s.cfg.CopilotModel +
				" generated=" + time.Now().UTC().Format(time.RFC3339) + " facts=platform-verified"
		}
	}
	artifact := "copilot_" + in.Kind
	toJ, _ := json.Marshal(in.To)
	ccJ, _ := json.Marshal(in.CC)
	var qid string
	if err := s.db.QueryRow(r.Context(), `
		INSERT INTO public.qa_reviews (tenant, case_id, artifact, channel, subject, body, to_recipients, cc_recipients, status, drafted_by)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,'PENDING',$9) RETURNING id`,
		tenant, caseID, artifact, channel, truncate(subject, 400), footed, toJ, ccJ, attribution).Scan(&qid); err != nil {
		http.Error(w, `{"error":"qa gate insert failed"}`, http.StatusInternalServerError)
		return
	}
	s.logActivity(r.Context(), tenant, caseID, "COPILOT_DRAFT_QUEUED",
		fmt.Sprintf("%s draft queued for QA review (%s) by %s", in.Kind, qid, p.Subject))
	s.notify(r, tenant, "*", "QA_REVIEW",
		fmt.Sprintf("Copilot %s draft on case %s awaiting QA approval", in.Kind, facts.CaseNumber), "#/qa")
	writeJSON(w, http.StatusCreated, map[string]any{
		"qa_id": qid, "kind": in.Kind, "subject": subject, "draft": footed,
		"status": "PENDING", "advisory": true,
	})
}
