// voice_ext.go — CRM activity timeline + outbound calling.
//
// - recordVoiceActivity: every inbound voice event with a case_number is
//   auto-attached to that case's activity timeline (the CRM record), and
//   voice intake referencing a known case is auto-linked.
// - outboundCall: triggers an outbound call through the tenant's configured
//   voice platform (getline.ai-style outbound-call API), logs OUTBOUND_TRIGGER.
package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
)

// recordVoiceActivity attaches a voice event to the case timeline + links intake.
func (s *server) recordVoiceActivity(r *http.Request, tenant, caseNumber, summary string) {
	if caseNumber == "" || caseNumber == "<nil>" {
		return
	}
	var caseID string
	err := s.db.QueryRow(r.Context(), fmt.Sprintf(
		`SELECT id FROM tenant_%s.cases WHERE case_number=$1`, sanitizeTenant(tenant)),
		caseNumber).Scan(&caseID)
	if err != nil {
		return // unknown case: log row already records it for manual triage
	}
	excerpt := summary
	if len(excerpt) > 500 {
		excerpt = excerpt[:500] + "…"
	}
	_, _ = s.db.Exec(r.Context(), `
		INSERT INTO public.case_activities (tenant, case_id, type, body)
		VALUES ($1,$2,'VOICE_CALL',$3)`, tenant, caseID, excerpt)
	// Auto-link any NEW voice intake that referenced this case.
	_, _ = s.db.Exec(r.Context(), `
		UPDATE public.voice_intake_requests SET status='LINKED'
		WHERE tenant=$1 AND status='NEW' AND summary ILIKE '%'||$2||'%'`, tenant, caseNumber)
}

// listActivities: case timeline (CRM record feed) for the portal.
func (s *server) listActivities(w http.ResponseWriter, r *http.Request) {
	tenant := r.Context().Value(ctxTenant{}).(string)
	limit, offset := pageParams(r, 100, 500)
	rows, err := s.db.Query(r.Context(), fmt.Sprintf(`
		SELECT type, body, created_at FROM public.case_activities
		WHERE tenant=$1 AND case_id=$2 ORDER BY created_at DESC
		LIMIT %d OFFSET %d`, limit, offset),
		tenant, chi.URLParam(r, "caseId"))
	if err != nil {
		http.Error(w, `{"error":"db"}`, http.StatusInternalServerError)
		return
	}
	defer rows.Close()
	out := []map[string]any{}
	for rows.Next() {
		var typ, body string
		var at time.Time
		if rows.Scan(&typ, &body, &at) == nil {
			out = append(out, map[string]any{"type": typ, "body": body, "at": at})
		}
	}
	writeJSON(w, http.StatusOK, out)
}

// outboundCall: POST /v1/tenants/{tenant}/voice/outbound
// Body: { "to": "+1…", "case_number": "…", "script": "window_closing" }
// Calls the tenant-configured voice platform outbound API and logs the trigger.
func (s *server) outboundCall(w http.ResponseWriter, r *http.Request) {
	tenant := r.Context().Value(ctxTenant{}).(string)
	var in struct {
		To         string         `json:"to"`
		CaseNumber string         `json:"case_number"`
		Script     string         `json:"script"`
		Variables  map[string]any `json:"variables"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil || in.To == "" {
		http.Error(w, `{"error":"to is required"}`, http.StatusBadRequest)
		return
	}
	var baseURL, apiKey string
	var enabled bool
	err := s.db.QueryRow(r.Context(), `
		SELECT outbound_enabled, COALESCE(platform_base_url,''), COALESCE(outbound_api_key,'')
		FROM public.voice_configs WHERE tenant=$1`, tenant).Scan(&enabled, &baseURL, &apiKey)
	if err != nil || !enabled || baseURL == "" {
		http.Error(w, `{"error":"outbound calling not enabled for this tenant"}`, http.StatusConflict)
		return
	}
	payload, _ := json.Marshal(map[string]any{
		"to": in.To, "agent_script": in.Script,
		"dynamic_variables": map[string]any{
			"case_number": in.CaseNumber, "tenant": tenant,
		},
		"metadata": in.Variables,
	})
	req, _ := http.NewRequestWithContext(r.Context(), http.MethodPost,
		baseURL+"/v1/calls/outbound", bytes.NewReader(payload))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+apiKey)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		http.Error(w, `{"error":"voice platform unreachable"}`, http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))

	status := "OK"
	if resp.StatusCode >= 300 {
		status = "FAILED"
	}
	_, _ = s.db.Exec(r.Context(), `
		INSERT INTO public.voice_call_logs (tenant, direction, tool, caller_phone, case_number, summary, status)
		VALUES ($1,'OUTBOUND_TRIGGER',$2,$3,$4,$5,$6)`,
		tenant, in.Script, in.To, in.CaseNumber, string(respBody), status)
	s.recordVoiceActivity(r, tenant, in.CaseNumber,
		fmt.Sprintf("Outbound call triggered (%s) to %s", in.Script, in.To))

	writeJSON(w, http.StatusAccepted, map[string]any{
		"status": status, "platform_status": resp.StatusCode,
	})
}
