package main

// Copilot Phase 3 — bounded agentic ACTION BATCHES via Temporal.
//
// The model may now PROPOSE actions, but the execution envelope stays tight:
//
//  1. BOUNDED — one grounded proposal call, returning JSON validated against
//     a server-side allowlist (max copilotMaxActions per batch). The model
//     cannot invent action types, and cannot touch case state directly.
//  2. HUMAN GATE — a batch executes nothing until a staff member approves
//     it. Approval signals the Temporal workflow; rejection ends it. The
//     workflow auto-expires after copilotBatchApprovalTTL.
//  3. DURABLE + AUDITED — Temporal owns the wait/execute lifecycle (this is
//     exactly the orchestration LangGraph would have duplicated). Execution
//     itself goes through worker-authenticated internal endpoints that
//     re-validate every action before any side effect; each result is
//     recorded on the batch row and the case timeline.

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	temporalclient "go.temporal.io/sdk/client"
)

// copilotMaxActions bounds a single batch. Small on purpose: a human reviews
// every proposal, and each action is individually applied + audited.
const copilotMaxActions = 5

// copilotBatchApprovalTTL is how long a proposal waits for a human before
// the workflow expires it. Proposals go stale — facts change.
const copilotBatchApprovalTTL = 72 * time.Hour

// copilotAction is one allowlisted, parameter-bounded action. Params are
// validated per type before the batch is ever persisted.
type copilotAction struct {
	Type   string         `json:"type"`
	Params map[string]any `json:"params,omitempty"`
	Result string         `json:"result,omitempty"`
	Error  string         `json:"error,omitempty"`
}

// copilotActionTypes is the full allowlist. Every type maps to ONE internal
// executor below; anything the model emits outside this set is dropped.
var copilotActionTypes = map[string]bool{
	"create_task":                    true, // CRM follow-up task on the case
	"request_document_rescan":        true, // clearer-scan request task (bad OCR)
	"flag_missing_eligibility_input": true, // timeline flag + staff notification
	"draft_followup_correspondence":  true, // queues a QA-gated draft (human-gated AGAIN)
}

// copilotActionsPrompt asks the model for a STRICT JSON action plan. The
// parser below is defensive: anything unparseable or off-allowlist yields a
// validation error, never a partial application.
func copilotActionsPrompt(f copilotFacts) (system, user string) {
	system = `You are an operations planner inside a federal No Surprises Act IDRE case platform.
You receive a JSON fact sheet of platform-verified case facts and propose a SMALL batch of next actions for a human to approve.

HARD RULES:
- Use ONLY the JSON facts below. If a fact is absent, treat it as unknown — NEVER invent identifiers, amounts, dates, or parties.
- Reply with STRICT JSON ONLY, no prose, no markdown fences, exactly this shape:
  {"rationale":"one short paragraph","actions":[{"type":"...","params":{...}}]}
- Allowed action types and their params:
    create_task                    {"subject": string, "due_days": integer 1-30}
    request_document_rescan        {"reason": string}
    flag_missing_eligibility_input {"inputs": [string]}
    draft_followup_correspondence  {"purpose": string}
- Propose at most 5 actions. Propose ZERO actions ({"actions":[]}) if nothing is clearly useful — a human reviews everything you propose.
- Every param string must reference only facts present in the sheet.`
	fj, _ := json.Marshal(f)
	return system, "CASE FACTS (JSON, platform-verified):\n" + string(fj)
}

// parseCopilotActions extracts and validates the model's JSON plan. Strict:
// the whole reply must decode, every type must be allowlisted, every param
// shape must check out, and the batch is capped at copilotMaxActions.
func parseCopilotActions(reply string) (rationale string, actions []copilotAction, err error) {
	reply = strings.TrimSpace(reply)
	// Tolerate leading/trailing prose by cutting to the outermost braces —
	// but if there is no JSON object at all, that is a hard failure.
	if i, j := strings.Index(reply, "{"), strings.LastIndex(reply, "}"); i >= 0 && j > i {
		reply = reply[i : j+1]
	}
	var raw struct {
		Rationale string          `json:"rationale"`
		Actions   []copilotAction `json:"actions"`
	}
	if err = json.Unmarshal([]byte(reply), &raw); err != nil {
		return "", nil, fmt.Errorf("model reply not valid JSON: %w", err)
	}
	if len(raw.Actions) > copilotMaxActions {
		return "", nil, fmt.Errorf("model proposed %d actions (max %d)", len(raw.Actions), copilotMaxActions)
	}
	for i, a := range raw.Actions {
		if !copilotActionTypes[a.Type] {
			return "", nil, fmt.Errorf("action %d: type %q not allowlisted", i, a.Type)
		}
		if verr := validateCopilotActionParams(a); verr != nil {
			return "", nil, fmt.Errorf("action %d (%s): %v", i, a.Type, verr)
		}
	}
	return truncate(raw.Rationale, 2000), raw.Actions, nil
}

func paramStr(p map[string]any, key string, maxLen int) (string, bool) {
	v, ok := p[key]
	if !ok {
		return "", false
	}
	s, ok := v.(string)
	s = strings.TrimSpace(s)
	if !ok || s == "" || len(s) > maxLen {
		return "", false
	}
	return s, true
}

// validateCopilotActionParams enforces the per-type param contract BEFORE
// persistence — the executor can then trust the shape it re-reads.
func validateCopilotActionParams(a copilotAction) error {
	switch a.Type {
	case "create_task":
		if _, ok := paramStr(a.Params, "subject", 400); !ok {
			return fmt.Errorf("subject (1-400 chars) required")
		}
		if d, ok := a.Params["due_days"]; ok {
			f, isNum := d.(float64)
			if !isNum || f < 1 || f > 30 || f != float64(int(f)) {
				return fmt.Errorf("due_days must be an integer 1-30")
			}
		}
	case "request_document_rescan":
		if _, ok := paramStr(a.Params, "reason", 400); !ok {
			return fmt.Errorf("reason (1-400 chars) required")
		}
	case "flag_missing_eligibility_input":
		v, ok := a.Params["inputs"].([]any)
		if !ok || len(v) == 0 || len(v) > 10 {
			return fmt.Errorf("inputs must be 1-10 strings")
		}
		for _, it := range v {
			s, isStr := it.(string)
			if !isStr || strings.TrimSpace(s) == "" || len(s) > 120 {
				return fmt.Errorf("each input must be a 1-120 char string")
			}
		}
	case "draft_followup_correspondence":
		if _, ok := paramStr(a.Params, "purpose", 400); !ok {
			return fmt.Errorf("purpose (1-400 chars) required")
		}
	}
	return nil
}

// copilotProposeActions handles POST /cases/{caseId}/copilot/actions —
// one bounded proposal call; the validated batch is persisted as
// PENDING_APPROVAL and its Temporal workflow started (waiting on the human).
func (s *server) copilotProposeActions(w http.ResponseWriter, r *http.Request) {
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
	facts, err := s.gatherCopilotFacts(r, tenant, caseID)
	if err != nil {
		http.Error(w, `{"error":"case not found"}`, http.StatusNotFound)
		return
	}
	// One open batch per case at a time — stacked proposals rot and double-apply.
	var openID string
	if err := s.db.QueryRow(r.Context(),
		`SELECT id FROM public.copilot_action_batches WHERE tenant=$1 AND case_id=$2 AND status='PENDING_APPROVAL' LIMIT 1`,
		tenant, caseID).Scan(&openID); err == nil {
		writeJSON(w, http.StatusConflict, map[string]any{
			"error": "a proposal batch is already awaiting decision on this case", "batch_id": openID,
		})
		return
	}
	system, user := copilotActionsPrompt(facts)
	reply, err := ollamaChat(r.Context(), s.cfg.CopilotEndpoint, s.cfg.CopilotModel, system, user, 1200)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]any{
			"error":  "copilot model unreachable — no batch proposed; the facts below are still authoritative",
			"detail": err.Error(), "facts": facts,
		})
		return
	}
	rationale, actions, err := parseCopilotActions(reply)
	if err != nil {
		s.logActivity(r.Context(), tenant, caseID, "COPILOT_BATCH_REJECTED_INVALID",
			fmt.Sprintf("model proposal failed validation: %v", err))
		writeJSON(w, http.StatusUnprocessableEntity, map[string]any{
			"error": "model proposal failed validation — nothing was queued", "detail": err.Error(),
		})
		return
	}
	actionsJ, _ := json.Marshal(actions)
	var batchID string
	if err := s.db.QueryRow(r.Context(), `
		INSERT INTO public.copilot_action_batches (tenant, case_id, proposed_by, model, actions, rationale)
		VALUES ($1,$2,$3,$4,$5,$6) RETURNING id`,
		tenant, caseID, p.Subject, s.cfg.CopilotModel, actionsJ, rationale).Scan(&batchID); err != nil {
		http.Error(w, `{"error":"batch insert failed (copilot_action_batches migrated?)"}`, http.StatusInternalServerError)
		return
	}
	wfID := fmt.Sprintf("COPILOT-%s-%s", strings.ToUpper(tenant), batchID)
	_, err = s.tc.ExecuteWorkflow(r.Context(), temporalclient.StartWorkflowOptions{
		ID:        wfID,
		TaskQueue: "idre-cases",
	}, "CopilotActionBatchWorkflow", map[string]any{
		"tenant": tenant, "case_id": caseID, "batch_id": batchID,
		"approval_ttl_seconds": int(copilotBatchApprovalTTL.Seconds()),
	})
	if err != nil {
		_, _ = s.db.Exec(r.Context(),
			`UPDATE public.copilot_action_batches SET status='FAILED', updated_at=now() WHERE id=$1`, batchID)
		http.Error(w, `{"error":"workflow start failed"}`, http.StatusBadGateway)
		return
	}
	_, _ = s.db.Exec(r.Context(),
		`UPDATE public.copilot_action_batches SET workflow_id=$3, updated_at=now() WHERE tenant=$1 AND id=$2`,
		tenant, batchID, wfID)
	s.logActivity(r.Context(), tenant, caseID, "COPILOT_BATCH_PROPOSED",
		fmt.Sprintf("Copilot proposed %d action(s) awaiting approval (batch %s): %s", len(actions), batchID, rationale))
	s.notify(r, tenant, p.Subject, "COPILOT_BATCH",
		fmt.Sprintf("Copilot proposed %d action(s) on case %s — review and approve/reject", len(actions), facts.CaseNumber), "#/cases/"+caseID)
	writeJSON(w, http.StatusCreated, map[string]any{
		"batch_id": batchID, "workflow_id": wfID, "rationale": rationale,
		"actions": actions, "status": "PENDING_APPROVAL",
		"expires_in_hours": int(copilotBatchApprovalTTL.Hours()), "advisory": true,
	})
}

// copilotListActionBatches handles GET /cases/{caseId}/copilot/actions.
func (s *server) copilotListActionBatches(w http.ResponseWriter, r *http.Request) {
	tenant := r.Context().Value(ctxTenant{}).(string)
	p := r.Context().Value(ctxPrincipal{}).(principal)
	if !hasAnyRole(p, copilotRoles...) {
		http.Error(w, `{"error":"forbidden: requires case staff role"}`, http.StatusForbidden)
		return
	}
	caseID := chi.URLParam(r, "caseId")
	rows, err := s.db.Query(r.Context(), `
		SELECT id, status, proposed_by, model, actions, coalesce(rationale,''),
		       coalesce(decided_by,''), coalesce(decided_at::text,''), created_at
		FROM public.copilot_action_batches
		WHERE tenant=$1 AND case_id=$2 ORDER BY created_at DESC LIMIT 10`, tenant, caseID)
	if err != nil {
		http.Error(w, `{"error":"db"}`, http.StatusInternalServerError)
		return
	}
	defer rows.Close()
	out := []map[string]any{}
	for rows.Next() {
		var id, status, proposedBy, model, rationale, decidedBy, decidedAt string
		var actionsJ []byte
		var created time.Time
		if rows.Scan(&id, &status, &proposedBy, &model, &actionsJ, &rationale, &decidedBy, &decidedAt, &created) == nil {
			var actions []copilotAction
			_ = json.Unmarshal(actionsJ, &actions)
			out = append(out, map[string]any{
				"batch_id": id, "status": status, "proposed_by": proposedBy, "model": model,
				"actions": actions, "rationale": rationale, "decided_by": decidedBy,
				"decided_at": decidedAt, "created_at": created,
			})
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"batches": out})
}

// copilotDecideActions handles POST /cases/{caseId}/copilot/actions/{batchId}/decision
// {"decision":"APPROVE"|"REJECT"} — the human gate. APPROVE signals the
// waiting workflow to execute; REJECT ends it. Both are recorded now (the
// workflow is the executor, the batch row is the UI's source of truth).
func (s *server) copilotDecideActions(w http.ResponseWriter, r *http.Request) {
	tenant := r.Context().Value(ctxTenant{}).(string)
	p := r.Context().Value(ctxPrincipal{}).(principal)
	if !hasAnyRole(p, copilotRoles...) {
		http.Error(w, `{"error":"forbidden: requires case staff role"}`, http.StatusForbidden)
		return
	}
	caseID, batchID := chi.URLParam(r, "caseId"), chi.URLParam(r, "batchId")
	var in struct {
		Decision string `json:"decision"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil ||
		(in.Decision != "APPROVE" && in.Decision != "REJECT") {
		http.Error(w, `{"error":"decision must be APPROVE or REJECT"}`, http.StatusBadRequest)
		return
	}
	var status, wfID string
	var actionsJ []byte
	if err := s.db.QueryRow(r.Context(), `
		SELECT status, coalesce(workflow_id,''), actions
		FROM public.copilot_action_batches WHERE tenant=$1 AND id=$2 AND case_id=$3`,
		tenant, batchID, caseID).Scan(&status, &wfID, &actionsJ); err != nil {
		http.Error(w, `{"error":"batch not found"}`, http.StatusNotFound)
		return
	}
	if status != "PENDING_APPROVAL" {
		http.Error(w, `{"error":"batch already decided (`+status+`)"}`, http.StatusConflict)
		return
	}
	newStatus := map[string]string{"APPROVE": "APPROVED", "REJECT": "REJECTED"}[in.Decision]
	// Record the human decision FIRST — if the signal fails, the batch shows
	// its decision and the workflow's own TTL still bounds any execution.
	if _, err := s.db.Exec(r.Context(), `
		UPDATE public.copilot_action_batches
		SET status=$3, decided_by=$4, decided_at=now(), updated_at=now()
		WHERE tenant=$1 AND id=$2 AND status='PENDING_APPROVAL'`,
		tenant, batchID, newStatus, p.Subject); err != nil {
		http.Error(w, `{"error":"db"}`, http.StatusInternalServerError)
		return
	}
	signaled := true
	if wfID != "" {
		if err := s.tc.SignalWorkflow(r.Context(), wfID, "", "DECISION",
			map[string]any{"decision": in.Decision, "by": p.Subject}); err != nil {
			signaled = false
			s.logActivity(r.Context(), tenant, caseID, "COPILOT_BATCH_SIGNAL_FAILED",
				fmt.Sprintf("batch %s decided %s but workflow signal failed: %v", batchID, in.Decision, err))
		}
	}
	s.logActivity(r.Context(), tenant, caseID, "COPILOT_BATCH_"+in.Decision+"D",
		fmt.Sprintf("Copilot action batch %s %s by %s", batchID, strings.ToLower(in.Decision)+"d", p.Subject))
	out := map[string]any{"batch_id": batchID, "status": newStatus, "signaled": signaled}
	if in.Decision == "APPROVE" && !signaled {
		out["error"] = "decision recorded but workflow signal failed — actions will NOT execute; repropose if still needed"
	}
	writeJSON(w, http.StatusOK, out)
}

// copilotApplyAction handles POST /internal/copilot/actions/apply — the
// worker-token-authenticated executor the Temporal activities call. It
// re-reads the batch row, re-validates the batch state (APPROVED, not
// expired, action index in range), applies ONE action's side effect, and
// records the result back onto the row. Idempotent per index: an already
// applied action returns its recorded result without re-applying.
func (s *server) copilotApplyAction(w http.ResponseWriter, r *http.Request) {
	tenant := r.Context().Value(ctxTenant{}).(string)
	p := r.Context().Value(ctxPrincipal{}).(principal)
	if !hasAnyRole(p, serviceRole) {
		http.Error(w, `{"error":"forbidden: worker token required"}`, http.StatusForbidden)
		return
	}
	var in struct {
		BatchID string `json:"batch_id"`
		Index   int    `json:"index"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil || in.BatchID == "" {
		http.Error(w, `{"error":"batch_id required"}`, http.StatusBadRequest)
		return
	}
	var status, caseID, decidedBy string
	var decidedAt *time.Time
	var actionsJ []byte
	if err := s.db.QueryRow(r.Context(), `
		SELECT status, case_id, coalesce(decided_by,''), decided_at, actions
		FROM public.copilot_action_batches WHERE tenant=$1 AND id=$2`,
		tenant, in.BatchID).Scan(&status, &caseID, &decidedBy, &decidedAt, &actionsJ); err != nil {
		http.Error(w, `{"error":"batch not found"}`, http.StatusNotFound)
		return
	}
	if status != "APPROVED" && status != "PARTIAL" && status != "APPLIED" {
		http.Error(w, `{"error":"batch not in an executable state (`+status+`)"}`, http.StatusConflict)
		return
	}
	// Approval staleness guard, mirroring the workflow TTL: facts change.
	if decidedAt != nil && time.Since(*decidedAt) > copilotBatchApprovalTTL {
		http.Error(w, `{"error":"approval expired"}`, http.StatusConflict)
		return
	}
	var actions []copilotAction
	if err := json.Unmarshal(actionsJ, &actions); err != nil || in.Index < 0 || in.Index >= len(actions) {
		http.Error(w, `{"error":"action index out of range"}`, http.StatusBadRequest)
		return
	}
	a := actions[in.Index]
	if a.Result != "" && a.Error == "" {
		writeJSON(w, http.StatusOK, map[string]any{"status": "already_applied", "result": a.Result})
		return
	}
	if err := validateCopilotActionParams(a); err != nil {
		http.Error(w, `{"error":"stored action failed re-validation"}`, http.StatusInternalServerError)
		return
	}
	result, applyErr := s.executeCopilotAction(r, tenant, caseID, decidedBy, a)
	if applyErr != nil {
		actions[in.Index].Error = applyErr.Error()
		s.persistCopilotActionResult(r, tenant, in.BatchID, actions)
		http.Error(w, `{"error":`+jsonString(applyErr.Error())+`}`, http.StatusBadGateway)
		return
	}
	actions[in.Index].Result, actions[in.Index].Error = result, ""
	s.persistCopilotActionResult(r, tenant, in.BatchID, actions)
	writeJSON(w, http.StatusOK, map[string]any{"status": "applied", "result": result})
}

func (s *server) persistCopilotActionResult(r *http.Request, tenant, batchID string, actions []copilotAction) {
	actionsJ, _ := json.Marshal(actions)
	// Recompute batch status from per-action outcomes: APPLIED when all
	// succeeded, PARTIAL when any failed (some applied), FAILED when none did.
	applied, failed := 0, 0
	for _, a := range actions {
		if a.Result != "" {
			applied++
		} else if a.Error != "" {
			failed++
		}
	}
	status := "APPROVED"
	switch {
	case len(actions) > 0 && applied == len(actions):
		status = "APPLIED"
	case applied > 0 && failed > 0:
		status = "PARTIAL"
	case failed == len(actions) && failed > 0:
		status = "FAILED"
	}
	_, _ = s.db.Exec(r.Context(), `
		UPDATE public.copilot_action_batches SET actions=$3, status=$4, updated_at=now()
		WHERE tenant=$1 AND id=$2`, tenant, batchID, actionsJ, status)
}

func jsonString(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

// executeCopilotAction performs ONE allowlisted side effect. Each branch is
// deliberately small and reuses the exact mechanism the human UI uses.
func (s *server) executeCopilotAction(r *http.Request, tenant, caseID, approvedBy string, a copilotAction) (string, error) {
	switch a.Type {
	case "create_task", "request_document_rescan":
		subject, _ := paramStr(a.Params, "subject", 400)
		dueDays := 3
		if a.Type == "request_document_rescan" {
			reason, _ := paramStr(a.Params, "reason", 400)
			subject = "Request clearer scan / document re-upload: " + reason
		}
		if d, ok := a.Params["due_days"].(float64); ok && a.Type == "create_task" {
			dueDays = int(d)
		}
		var id, ref string
		err := s.db.QueryRow(r.Context(), `
			WITH ins AS (
				INSERT INTO public.tasks (tenant, subject, case_id, due_date, created_by)
				VALUES ($1,$2,$3,$4,$5) RETURNING id
			)
			UPDATE public.tasks t
			SET task_ref = 'TASK-' || to_char(now(),'YYYY') || '-' ||
			       lpad(nextval('public.task_ref_seq')::text, 5, '0')
			FROM ins WHERE t.id = ins.id RETURNING t.id, t.task_ref`,
			tenant, "[copilot] "+subject, caseID,
			time.Now().Add(time.Duration(dueDays)*24*time.Hour).Format("2006-01-02"),
			"copilot (approved by "+approvedBy+")").Scan(&id, &ref)
		if err != nil {
			return "", fmt.Errorf("task insert: %w", err)
		}
		return fmt.Sprintf("task %s created (due in %dd)", ref, dueDays), nil

	case "flag_missing_eligibility_input":
		v, _ := a.Params["inputs"].([]any)
		names := make([]string, 0, len(v))
		for _, it := range v {
			names = append(names, fmt.Sprint(it))
		}
		body := fmt.Sprintf("Copilot flagged missing eligibility input(s) — %s (batch approved by %s)",
			strings.Join(names, ", "), approvedBy)
		s.logActivity(r.Context(), tenant, caseID, "COPILOT_MISSING_INPUT_FLAG", body)
		s.notify(r, tenant, "*", "ELIGIBILITY_INPUT", body, "#/cases/"+caseID)
		return "flagged: " + strings.Join(names, ", "), nil

	case "draft_followup_correspondence":
		purpose, _ := paramStr(a.Params, "purpose", 400)
		// Human-gated AGAIN: this queues a QA PENDING row; nothing is sent.
		var qid string
		err := s.db.QueryRow(r.Context(), `
			INSERT INTO public.qa_reviews (tenant, case_id, artifact, channel, subject, body, status, drafted_by)
			VALUES ($1,$2,'copilot_followup','email',$3,$4,'PENDING',$5) RETURNING id`,
			tenant, caseID,
			truncate("Follow-up: "+purpose, 400),
			"[copilot skeleton — QA reviewer writes the body before approval]\n\nPurpose: "+purpose+
				"\n\nDRAFT — advisory only; not a determination.",
			"copilot (batch approved by "+approvedBy+")").Scan(&qid)
		if err != nil {
			return "", fmt.Errorf("qa insert: %w", err)
		}
		s.notify(r, tenant, "*", "QA_REVIEW",
			fmt.Sprintf("Copilot follow-up draft on case %s awaiting QA approval", caseID), "#/qa")
		return "QA draft queued: " + qid, nil
	}
	return "", fmt.Errorf("unknown action type %q", a.Type)
}
