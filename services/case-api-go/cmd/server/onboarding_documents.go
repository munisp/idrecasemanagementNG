// Author: Patrick Munis at newwave
// onboarding_documents.go — document upload for a stakeholder application and
// the signal relay doc-intel uses to clear the PENDING_DOCS gate.
//
// StakeholderOnboardingWorkflow (idre-workflows) waits on a DOCS_VERIFIED
// signal fed by doc-intel, but no endpoint existed to upload a document
// against an application_id -- only /cases/{caseId}/documents did. Every
// application requiring docs sat in PENDING_DOCS until the 10-day SLA
// silently expired it. This file is the missing other half of that gate,
// mirroring documents.go's upload flow (vault seal -> MinIO -> Kafka event)
// closely but keyed on application_id instead of case_id, so case-document
// handling in documents.go is untouched.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/minio/minio-go/v7"
)

// uploadApplicationDocument: POST /v1/tenants/{tenant}/onboarding/applications/{appId}/documents
func (s *server) uploadApplicationDocument(w http.ResponseWriter, r *http.Request) {
	tenant := r.Context().Value(ctxTenant{}).(string)
	appID := chi.URLParam(r, "appId")
	p := r.Context().Value(ctxPrincipal{}).(principal)

	var exists bool
	if err := s.db.QueryRow(r.Context(),
		`SELECT true FROM public.stakeholder_applications WHERE id=$1 AND tenant=$2`,
		appID, tenant).Scan(&exists); err != nil {
		http.Error(w, `{"error":"application not found"}`, http.StatusNotFound)
		return
	}

	// Same abuse ceiling as case-document uploads.
	if n, err := s.rds.incrExpire("idre:rl:up:"+tenant+":"+p.Subject, 3600); err == nil && n > 120 {
		http.Error(w, `{"error":"upload rate limit exceeded (120/hour) — try again later"}`, http.StatusTooManyRequests)
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, maxDocBytes)
	if err := r.ParseMultipartForm(maxDocBytes); err != nil {
		http.Error(w, `{"error":"file too large or malformed"}`, http.StatusRequestEntityTooLarge)
		return
	}
	f, hdr, err := r.FormFile("file")
	if err != nil {
		http.Error(w, `{"error":"file field required"}`, http.StatusBadRequest)
		return
	}
	defer f.Close()
	raw, err := io.ReadAll(f)
	if err != nil {
		http.Error(w, `{"error":"read failed"}`, http.StatusBadRequest)
		return
	}

	// Parity with case-document uploads: content allowlist + ClamAV gate —
	// onboarding documents used to bypass BOTH, an unfiltered path into
	// encrypted storage and the doc-intel queue.
	if err := contentPolicy(hdr.Filename, raw); err != nil {
		http.Error(w, fmt.Sprintf(`{"error":%q}`, err.Error()), http.StatusUnsupportedMediaType)
		return
	}
	if _, ok := s.scanOrRefuse(w, bytes.NewReader(raw), "file"); !ok {
		return
	}

	docID := newUUID()
	objectKey := fmt.Sprintf("%s/onboarding/%s/%s.enc", tenant, appID, docID)

	ct, err := s.vaultSealDoc(r, tenant, objectKey, raw)
	if err != nil {
		http.Error(w, `{"error":"seal failed"}`, http.StatusBadGateway)
		return
	}
	_, err = s.docs.mc.PutObject(r.Context(), docBucket, objectKey,
		bytes.NewReader(ct), int64(len(ct)), minio.PutObjectOptions{
			ContentType:  "application/octet-stream",
			UserMetadata: map[string]string{"tenant": tenant, "application": appID},
		})
	if err != nil {
		http.Error(w, `{"error":"store failed"}`, http.StatusBadGateway)
		return
	}

	if _, err := s.db.Exec(r.Context(), `
		INSERT INTO public.application_documents
		  (id, tenant, application_id, object_key, size_bytes, content_type, uploaded_by)
		VALUES ($1,$2,$3,$4,$5,$6,$7)`,
		docID, tenant, appID, objectKey, len(raw), hdr.Header.Get("Content-Type"), p.Subject); err != nil {
		http.Error(w, `{"error":"db"}`, http.StatusInternalServerError)
		return
	}

	// No case_id in this event at all (not just empty) -- doc-intel branches
	// on which key is present before touching either, so an onboarding event
	// must never carry a zero-value case_id that looks like a real one.
	s.publish(r.Context(), tenant, "documents", map[string]any{
		"type": "doc.uploaded", "tenant": tenant, "application_id": appID,
		"doc_id": docID, "object_key": objectKey, "content_type": hdr.Header.Get("Content-Type"),
		"sealed": false, "at": time.Now().UTC(),
	})
	// Program rules (doc.upload) — onboarding docs are pre-case, so facts carry
	// application_id instead of case_id; block quarantines via analysis_status.
	analysis := "QUEUED"
	if blocked, msg := s.fireEventRules(r, tenant, "doc.upload", map[string]any{
		"application_id": appID, "doc_id": docID, "folder": "ONBOARDING", "sealed": false,
		"size_bytes": len(raw), "content_type": hdr.Header.Get("Content-Type"),
		"filename": hdr.Filename, "uploaded_by": p.Subject, "tenant": tenant,
	}); blocked {
		analysis = "BLOCKED"
		_, _ = s.db.Exec(r.Context(), `
			UPDATE public.application_documents SET analysis_status='BLOCKED' WHERE id=$1`, docID)
		s.logActivity(r.Context(), tenant, "", "RULE_BLOCKED",
			fmt.Sprintf("Onboarding upload %q (application %s) quarantined by program rule: %s", hdr.Filename, appID, msg))
	}

	writeJSON(w, http.StatusCreated, map[string]any{
		"doc_id": docID, "bytes": len(raw), "analysis": analysis,
	})
}

// signalApplication: POST /v1/tenants/{tenant}/onboarding/applications/{appId}/signal
// Internal relay from doc-intel to the StakeholderOnboardingWorkflow (workflow
// ID "ONB-{TENANT}-{appId}", set when submitApplication starts it) -- doc-intel
// has no Temporal client of its own, same reason case-api's own /cases/.../signal
// exists for the equivalent case-side gate. SERVICE_WORKER-only: this is not a
// route a human user has any business calling directly.
func (s *server) signalApplication(w http.ResponseWriter, r *http.Request) {
	p := r.Context().Value(ctxPrincipal{}).(principal)
	if !hasRole(p, serviceRole) {
		http.Error(w, `{"error":"forbidden: service callers only"}`, http.StatusForbidden)
		return
	}
	tenant := r.Context().Value(ctxTenant{}).(string)
	appID := chi.URLParam(r, "appId")
	var body struct {
		Signal string         `json:"signal"`
		Data   map[string]any `json:"data"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.Signal == "" {
		http.Error(w, `{"error":"bad request"}`, http.StatusBadRequest)
		return
	}
	wfID := fmt.Sprintf("ONB-%s-%s", tenant, appID)
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	if err := s.tc.SignalWorkflow(ctx, wfID, "", body.Signal, body.Data); err != nil {
		http.Error(w, `{"error":"signal failed"}`, http.StatusBadGateway)
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]string{"status": "signaled"})
}
