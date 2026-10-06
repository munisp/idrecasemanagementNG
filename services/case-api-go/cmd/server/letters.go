// letters.go — request letter generation from DOCX templates (lettergen worker).
package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
)

// requestLetterGen: POST /v1/tenants/{tenant}/cases/{caseId}/lettergen/{templateKey}
// Validates the template key against the program's letter_templates config and
// enqueues a letter.requested event through the transactional outbox — the
// lettergen worker renders DOCX→PDF, seals it, files it on the docket, and
// opens the QA review. 202 Accepted; the letter appears under Documents and
// the QA queue when ready.
func (s *server) requestLetterGen(w http.ResponseWriter, r *http.Request) {
	tenant := r.Context().Value(ctxTenant{}).(string)
	caseID := chi.URLParam(r, "caseId")
	key := chi.URLParam(r, "templateKey")
	p := r.Context().Value(ctxPrincipal{}).(principal)

	// Template must be configured for this program.
	prog := s.loadProgram(r, tenant)
	if prog == nil {
		http.Error(w, `{"error":"no program rules for tenant"}`, http.StatusBadRequest)
		return
	}
	var found bool
	var lts []map[string]any
	if raw, ok := progRaw(r, s, tenant, "letter_templates"); ok {
		_ = json.Unmarshal(raw, &lts)
		for _, lt := range lts {
			if lt["key"] == key {
				found = true
				break
			}
		}
	}
	if !found {
		http.Error(w, `{"error":"unknown letter template for this program"}`, http.StatusBadRequest)
		return
	}
	// Case must exist.
	var caseNumber string
	if err := s.db.QueryRow(r.Context(), fmt.Sprintf(
		`SELECT case_number FROM tenant_%s.cases WHERE id=$1`, sanitizeTenant(tenant)), caseID).
		Scan(&caseNumber); err != nil {
		http.Error(w, `{"error":"case not found"}`, http.StatusNotFound)
		return
	}

	payload, _ := json.Marshal(map[string]any{
		"type": "letter.requested", "tenant": tenant, "case_id": caseID,
		"template": key, "case_number": caseNumber,
		"requested_by": p.Subject, "at": time.Now().UTC(),
	})
	if _, err := s.db.Exec(r.Context(), fmt.Sprintf(
		`INSERT INTO tenant_%s.outbox (topic, key, payload) VALUES ($1,$2,$3)`,
		sanitizeTenant(tenant)),
		fmt.Sprintf("idre.%s.letters", tenant), caseID, payload); err != nil {
		http.Error(w, `{"error":"outbox"}`, http.StatusInternalServerError)
		return
	}
	s.logActivity(r.Context(), tenant, caseID, "LETTER_REQUESTED",
		fmt.Sprintf("Letter generation requested by %s — template %s", p.Subject, key))
	s.autoChecklist(r, tenant, caseID)
	s.maybeAdvanceStatus(r, tenant, caseID)
	writeJSON(w, http.StatusAccepted, map[string]string{"status": "queued", "template": key})
}

// progRaw fetches one key from the program config jsonb.
func progRaw(r *http.Request, s *server, tenant, key string) (json.RawMessage, bool) {
	var raw json.RawMessage
	err := s.db.QueryRow(r.Context(),
		`SELECT config->$2 FROM public.program_rules WHERE tenant=$1`, tenant, key).Scan(&raw)
	if err != nil || len(raw) == 0 || string(raw) == "null" {
		return nil, false
	}
	return raw, true
}
