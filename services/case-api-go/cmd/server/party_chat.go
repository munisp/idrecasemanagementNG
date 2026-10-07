package main

// Party voice — conversation-first migration, step 4.
//
// The counterparty (filing provider or health plan) holding a secure share
// link can now ASK the case instead of only pushing documents at it: a chat
// panel on the ShareBox landing page, backed by one bounded local-model call
// grounded on a PARTY-SAFE fact projection. "Where is my dispute?" and
// "what do you still need from me?" stop being phone calls.
//
// Invariants, tightened for an unauthenticated surface:
//  1. TOKEN IS THE CREDENTIAL — the chat is gated by the same share-link
//     expiry as the page it sits on, but chatting NEVER consumes upload
//     uses (a spent upload link must still answer "did it arrive?").
//  2. PARTY-SAFE FACTS ONLY — the model sees case number, external status,
//     outstanding document requests, and link expiry. Never internal notes,
//     QA drafts, assignments, other cases, or dollar amounts (a forwarded
//     link must not leak strategy or money).
//  3. GROUNDED OR SILENT, ONE BOUNDED CALL, ADVISORY — same as staff side;
//     the model cannot act, only explain and point at the upload button.
//  4. COST-BOUND — messages capped, and a per-token turn cap makes a
//     scripted caller expensive-bounded rather than a free LLM proxy.
//  5. ON THE RECORD — every turn persists to public.party_threads with a
//     token FINGERPRINT (sha256 prefix), never the token itself, and each
//     exchange lands on the case timeline.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
)

// partyChatMaxTurns bounds total party+assistant turns per token — a share
// link is a narrow credential, not an open chat account.
const partyChatMaxTurns = 40

const partyChatMaxMessage = 1000

// partyFacts is the ENTIRE world the party-facing model may know. Fields are
// chosen so that even a forwarded link leaks nothing beyond what the holding
// party already knows about their own dispute.
type partyFacts struct {
	CaseNumber          string   `json:"case_number"`
	Status              string   `json:"status"`
	OutstandingRequests []string `json:"outstanding_requests"` // deliverable names + due dates, not yet delivered
	LinkKind            string   `json:"link_kind"`
	LinkExpires         string   `json:"link_expires"`
}

const partyChatSystem = `You are the party-facing assistant on a secure document-exchange page of a federal No Surprises Act IDRE case platform. You are talking to an external party (a healthcare provider's billing office or a health plan's claims team) about ONE dispute they are involved in.

HARD RULES:
- Use ONLY the JSON facts below. If an answer is not in the facts, say you don't have that information and suggest contacting the case coordinator — NEVER invent dates, amounts, names, deadlines, or outcomes.
- Be brief (under 100 words), plain-language, and courteous. No legal jargon, no legal advice, no predictions about outcomes.
- If documents are outstanding, name them and point to the upload button on this page.
- Never discuss internal review, QA drafts, staffing, other cases, or dollar amounts, even if asked.
- Never claim you took any action (you cannot). If asked to change something, explain that the case coordinator handles it.
- If asked about identity or eligibility details not in the facts, decline.

CASE FACTS (JSON, platform-verified):`

// partyChatFacts gathers the party-safe projection.
func (s *server) partyChatFacts(ctx context.Context, tenant, caseID, kind string, expires time.Time) partyFacts {
	f := partyFacts{
		LinkKind:            kind,
		LinkExpires:         expires.Format("January 2, 2006"),
		OutstandingRequests: []string{},
	}
	tbl := sanitizeTenant(tenant)
	_ = s.db.QueryRow(ctx, fmt.Sprintf(
		`SELECT case_number, status FROM tenant_%s.cases WHERE id=$1`, tbl), caseID).
		Scan(&f.CaseNumber, &f.Status)
	rows, err := s.db.Query(ctx, `
		SELECT name, coalesce(to_char(due_date,'YYYY-MM-DD'),'no stated due date')
		FROM public.deliverables
		WHERE tenant=$1 AND case_id=$2 AND delivered_at IS NULL
		ORDER BY due_date ASC NULLS LAST LIMIT 8`, tenant, caseID)
	if err == nil {
		defer rows.Close()
		for rows.Next() {
			var name, due string
			if rows.Scan(&name, &due) == nil {
				f.OutstandingRequests = append(f.OutstandingRequests, name+" (due "+due+")")
			}
		}
	}
	return f
}

// partyChat handles POST /api/share/{token}/chat {"message": "..."}.
// Unauthenticated: the token in the path is the credential (expiry-gated).
func (s *server) partyChat(w http.ResponseWriter, r *http.Request) {
	token := chi.URLParam(r, "token")
	// Expiry-gated, NOT use-gated: a spent upload link must still answer.
	var tenant, caseID, kind string
	var expires time.Time
	if err := s.db.QueryRow(r.Context(), `
		SELECT tenant, case_id, kind, expires_at FROM public.share_links
		WHERE token=$1 AND expires_at > now()`, token).
		Scan(&tenant, &caseID, &kind, &expires); err != nil {
		http.Error(w, `{"error":"link expired or invalid"}`, http.StatusGone)
		return
	}
	fp := sha256.Sum256([]byte(token))
	tokenFP := hex.EncodeToString(fp[:])[:16]

	var in struct {
		Message string `json:"message"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil || strings.TrimSpace(in.Message) == "" {
		http.Error(w, `{"error":"message required"}`, http.StatusBadRequest)
		return
	}
	in.Message = truncate(strings.TrimSpace(in.Message), partyChatMaxMessage)

	// Turn cap: count prior turns under this token fingerprint.
	var turns int
	_ = s.db.QueryRow(r.Context(), `
		SELECT count(*) FROM public.party_threads WHERE token_fp=$1`, tokenFP).Scan(&turns)
	if turns >= partyChatMaxTurns*2 {
		http.Error(w, `{"error":"chat limit for this link reached — contact your case coordinator"}`, http.StatusTooManyRequests)
		return
	}

	facts := s.partyChatFacts(r.Context(), tenant, caseID, kind, expires)
	factsJSON, _ := json.Marshal(facts)

	reply, model := "", ""
	if s.cfg.CopilotEndpoint != "" {
		var err error
		reply, err = ollamaChat(r.Context(), s.cfg.CopilotEndpoint, s.cfg.CopilotModel,
			partyChatSystem, string(factsJSON)+"\n\nPARTY QUESTION:\n"+in.Message, 300)
		if err != nil {
			reply = ""
		} else {
			model = s.cfg.CopilotModel
		}
	}
	if reply == "" {
		// Grounded fallback: the deterministic core of every likely answer.
		if len(facts.OutstandingRequests) > 0 {
			reply = fmt.Sprintf("Case %s is %s. We are still waiting on: %s — use the upload button on this page. For anything else, contact your case coordinator.",
				facts.CaseNumber, facts.Status, strings.Join(facts.OutstandingRequests, "; "))
		} else {
			reply = fmt.Sprintf("Case %s is %s. There are no outstanding document requests. For anything else, contact your case coordinator.",
				facts.CaseNumber, facts.Status)
		}
	}

	// Persist both directions; the thread is part of the case record. The
	// token itself is never stored — the fingerprint correlates without
	// becoming a credential leak.
	_, dbErr := s.db.Exec(r.Context(), `
		INSERT INTO public.party_threads (tenant, case_id, token_fp, role, body, model)
		VALUES ($1,$2,$3,'party',$4,NULL), ($1,$2,$3,'assistant',$5,nullif($6,''))`,
		tenant, caseID, tokenFP, in.Message, reply, model)
	if dbErr != nil {
		http.Error(w, `{"error":"db (party_threads migrated?)"}`, http.StatusInternalServerError)
		return
	}
	s.logActivity(r.Context(), tenant, caseID, "PARTY_CHAT",
		fmt.Sprintf("Party exchange via secure link (turn %d): %.120s", turns/2+1, in.Message))

	writeJSON(w, http.StatusOK, map[string]any{"reply": reply, "model": model})
}
