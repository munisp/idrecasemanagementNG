package main

// Copilot conversational thread — the Assistant surface.
//
// Phases 1-3 wrapped the local model in single-shot primitives (brief,
// draft, action batch). This is the conversational shell over the same
// primitives: a per-case thread where the worker asks questions in plain
// language and every answer is grounded on the SAME platform-verified fact
// sheet (re-gathered every turn — the case may have changed since the last
// turn, and a stale fact is worse than no fact).
//
// Invariants carried over unchanged:
//  1. GROUNDED OR SILENT — facts come from gatherCopilotFacts only; the
//     model writes "not in record" rather than invent.
//  2. ONE BOUNDED CALL per turn, temperature 0. No loops, no tools.
//  3. ADVISORY — the thread can DISCUSS actions but cannot take them;
//     execution stays behind the QA gate / batch-approval chips, which are
//     the same endpoints the screen UI calls.
//
// Every turn (both directions) persists to public.copilot_threads with the
// model name and attribution — the thread is part of the case record.

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
)

// copilotChatMaxHistory bounds how much thread goes back to the model.
// Recent turns carry the context that matters; older turns are in the DB
// for the record, not in the prompt.
const copilotChatMaxHistory = 12

func copilotChatPrompt(f copilotFacts) string {
	return `You are the case assistant inside a federal No Surprises Act IDRE case platform,
in a conversation with a case worker about ONE case.

HARD RULES:
- Use ONLY the JSON facts below. If a fact is absent, say "not in record" — NEVER invent identifiers, amounts, dates, parties, or legal citations.
- Answer conversationally and concisely (under 150 words unless detail is asked for). This is a chat, not a report.
- You are ADVISORY: you can explain the case, its eligibility posture, evidence, deadlines, and options — but you cannot change case state. If asked to DO something (draft, send, approve, assign), say which on-screen action does it (the chips below the input: brief / draft rationale / draft correspondence / propose actions), never claim you did it.
- Amounts are cents in the JSON — speak in dollars.
- If the question is outside this case's record, say so plainly.
- Never role-play as a lawyer making a determination; determinations are human decisions.

CASE FACTS (JSON, platform-verified, refreshed this turn):
` + func() string { b, _ := json.Marshal(f); return string(b) }()
}

type copilotChatTurn struct {
	Role      string    `json:"role"` // user | assistant
	Body      string    `json:"body"`
	Model     string    `json:"model,omitempty"`
	CreatedAt time.Time `json:"created_at"`
}

// copilotChatHistory handles GET /cases/{caseId}/copilot/chat — latest turns,
// oldest first for rendering.
func (s *server) copilotChatHistory(w http.ResponseWriter, r *http.Request) {
	tenant := r.Context().Value(ctxTenant{}).(string)
	p := r.Context().Value(ctxPrincipal{}).(principal)
	if !hasAnyRole(p, copilotRoles...) {
		http.Error(w, `{"error":"forbidden: requires case staff role"}`, http.StatusForbidden)
		return
	}
	caseID := chi.URLParam(r, "caseId")
	rows, err := s.db.Query(r.Context(), `
		SELECT role, body, coalesce(model,''), created_at
		FROM (SELECT role, body, model, created_at FROM public.copilot_threads
		      WHERE tenant=$1 AND case_id=$2 ORDER BY created_at DESC LIMIT 50) t
		ORDER BY created_at`, tenant, caseID)
	if err != nil {
		http.Error(w, `{"error":"db (copilot_threads migrated?)"}`, http.StatusInternalServerError)
		return
	}
	defer rows.Close()
	out := []copilotChatTurn{}
	for rows.Next() {
		var t copilotChatTurn
		if rows.Scan(&t.Role, &t.Body, &t.Model, &t.CreatedAt) == nil {
			out = append(out, t)
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"turns": out})
}

// copilotChat handles POST /cases/{caseId}/copilot/chat {"message": "..."}.
// One grounded turn in, one grounded turn out; both persisted.
func (s *server) copilotChat(w http.ResponseWriter, r *http.Request) {
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
		Message string `json:"message"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil || strings.TrimSpace(in.Message) == "" {
		http.Error(w, `{"error":"message required"}`, http.StatusBadRequest)
		return
	}
	in.Message = truncate(strings.TrimSpace(in.Message), 2000)

	// Facts first — a question about a case that doesn't exist (or isn't
	// visible in this tenant) gets 404, not a hallucinated answer.
	facts, err := s.gatherCopilotFacts(r, tenant, caseID)
	if err != nil {
		http.Error(w, `{"error":"case not found"}`, http.StatusNotFound)
		return
	}

	// Thread context: the last N turns, oldest first, as prior messages.
	rows, err := s.db.Query(r.Context(), `
		SELECT role, body FROM (SELECT role, body FROM public.copilot_threads
		  WHERE tenant=$1 AND case_id=$2 ORDER BY created_at DESC LIMIT $3) t
		ORDER BY created_at`, tenant, caseID, copilotChatMaxHistory)
	if err != nil {
		http.Error(w, `{"error":"db (copilot_threads migrated?)"}`, http.StatusInternalServerError)
		return
	}
	history := []map[string]string{}
	for rows.Next() {
		var role, body string
		if rows.Scan(&role, &body) == nil {
			history = append(history, map[string]string{"role": role, "content": body})
		}
	}
	rows.Close()

	reply, err := ollamaChatWithHistory(r.Context(), s.cfg.CopilotEndpoint, s.cfg.CopilotModel,
		copilotChatPrompt(facts), history, in.Message, 800)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]any{
			"error":  "copilot model unreachable — no reply generated; the facts below are still authoritative",
			"detail": err.Error(), "facts": facts,
		})
		return
	}

	// Persist both directions — the thread is part of the case record.
	_, _ = s.db.Exec(r.Context(), `
		INSERT INTO public.copilot_threads (tenant, case_id, role, body) VALUES ($1,$2,'user',$3)`,
		tenant, caseID, in.Message)
	_, _ = s.db.Exec(r.Context(), `
		INSERT INTO public.copilot_threads (tenant, case_id, role, body, model) VALUES ($1,$2,'assistant',$3,$4)`,
		tenant, caseID, reply, s.cfg.CopilotModel)
	s.logActivity(r.Context(), tenant, caseID, "COPILOT_CHAT",
		fmt.Sprintf("Assistant turn by %s (%d chars in, %d out)", p.Subject, len(in.Message), len(reply)))
	writeJSON(w, http.StatusOK, map[string]any{
		"reply": reply, "model": s.cfg.CopilotModel, "advisory": true,
		"case_number": facts.CaseNumber,
	})
}

// ollamaChatWithHistory is ollamaChat plus prior-turn context. Same wire
// format, same temperature-0 bounding; the messages array just grows a
// middle section.
func ollamaChatWithHistory(ctx context.Context, endpoint, model, system string, history []map[string]string, user string, maxTokens int) (string, error) {
	messages := append([]map[string]string{{"role": "system", "content": system}}, history...)
	messages = append(messages, map[string]string{"role": "user", "content": user})
	return ollamaChatMessages(ctx, endpoint, model, messages, maxTokens)
}
