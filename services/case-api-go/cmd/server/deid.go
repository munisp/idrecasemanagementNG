// Author: Patrick Munis at newwave
package main

// deid.go — case-api side of the de-identification service (services/deid-py).
//
// Phase 1: every outbound artifact path (QA-approved correspondence; the
// lakehouse sync via graph-intel) is gated by a PII scan. Findings at
// direct-identifier risk block delivery (fail-closed by default — for PHI
// leaving the building, "the scrubber is down" must mean "the letter waits",
// not "the letter goes"). DEID_FAIL_OPEN=1 downgrades a block to a logged
// hold for environments that explicitly accept that risk.
//
// Phase 2: document_clinical persistence + the clinical-overlap duplicate
// signal.
//
// Active when DEID_URL is set and the tenant's "deid" feature flag is on
// (default on). Every decision lands in public.deid_reports; input text is
// never stored — only hashes, spans, and policy versions.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
)

var deidHTTP = &http.Client{Timeout: 30 * time.Second}

type deidFinding struct {
	Type     string  `json:"type"`
	Start    int     `json:"start"`
	End      int     `json:"end"`
	Score    float64 `json:"score"`
	Text     string  `json:"text"`
	Detector string  `json:"detector"`
}

type deidAudit struct {
	Policy    string         `json:"policy"`
	Detectors []string       `json:"detectors"`
	Counts    map[string]int `json:"counts"`
	Findings  []deidFinding  `json:"findings"`
	Risk      float64        `json:"risk"`
	TextHash  string         `json:"text_hash"`
}

type deidScanResp struct {
	Findings []deidFinding `json:"findings"`
	Risk     float64       `json:"risk"`
	Audit    deidAudit     `json:"audit"`
}

// deidActive: the service is configured and the tenant feature is on.
func (s *server) deidActive(r *http.Request, tenant string) bool {
	return s.cfg.DeidURL != "" && s.featureEnabled(r, tenant, "deid")
}

// deidScan runs one text through /v1/scan.
func (s *server) deidScan(ctx context.Context, tenant, caseID, artifact, text string) (*deidScanResp, error) {
	payload, _ := json.Marshal(map[string]any{
		"tenant": tenant, "case_id": caseID, "artifact": artifact, "text": text})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		s.cfg.DeidURL+"/v1/scan", bytes.NewReader(payload))
	if err != nil {
		return nil, err
	}
	req.Header.Set("content-type", "application/json")
	resp, err := deidHTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("deid scan: status %d", resp.StatusCode)
	}
	var out deidScanResp
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, err
	}
	return &out, nil
}

// deidPersistReport writes the audit row. Best-effort: the caller decides
// enforcement; persistence failure is logged, never fatal to the case flow.
func (s *server) deidPersistReport(ctx context.Context, tenant, caseID, artifact, refID, action string, a deidAudit) {
	detectors, _ := json.Marshal(a.Detectors)
	counts, _ := json.Marshal(a.Counts)
	findings, _ := json.Marshal(a.Findings)
	_, _ = s.db.Exec(ctx, `
		INSERT INTO public.deid_reports
		  (tenant, case_id, artifact, ref_id, action, policy, detectors, counts, findings, risk, text_hash)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11)`,
		tenant, nilIfEmpty(caseID), artifact, refID, action, a.Policy,
		detectors, counts, findings, a.Risk, a.TextHash)
}

// deidBlockReason: pure policy — a scan result at direct-identifier risk is
// held, everything else is released. Split from the gate so the policy is
// unit-testable without a database.
func deidBlockReason(res *deidScanResp) string {
	if res.Risk < 1.0 {
		return ""
	}
	types := map[string]bool{}
	for _, f := range res.Findings {
		types[f.Type] = true
	}
	list := []string{}
	for t := range types {
		list = append(list, t)
	}
	return strings.Join(list, ", ")
}

// deidGateOutbound scans an outbound artifact before it leaves. Returns ""
// when clear (or when the service is unconfigured); a human-readable block
// reason otherwise. The decision is always persisted + activity-logged.
func (s *server) deidGateOutbound(r *http.Request, tenant, caseID, artifact, refID, text string) string {
	if !s.deidActive(r, tenant) || strings.TrimSpace(text) == "" {
		return ""
	}
	res, err := s.deidScan(r.Context(), tenant, caseID, artifact, text)
	if err != nil {
		if envOr("DEID_FAIL_OPEN", "") == "1" {
			s.logActivity(r.Context(), tenant, caseID, "DEID_UNAVAILABLE",
				fmt.Sprintf("deid scan failed (%s); %s released under DEID_FAIL_OPEN", err, artifact))
			return ""
		}
		return fmt.Sprintf("de-identification service unavailable (%s) — %s held; retry when deid is healthy", err, artifact)
	}
	s.deidPersistReport(r.Context(), tenant, caseID, artifact, refID, "SCAN", res.Audit)
	if reason := deidBlockReason(res); reason != "" {
		s.deidPersistReport(r.Context(), tenant, caseID, artifact, refID, "BLOCK", res.Audit)
		s.logActivity(r.Context(), tenant, caseID, "DEID_BLOCKED",
			fmt.Sprintf("Outbound %s held: direct identifiers detected (%s)", artifact, reason))
		return fmt.Sprintf("held by de-identification policy: direct identifiers detected (%s) — redact and resubmit", reason)
	}
	if len(res.Findings) > 0 {
		s.logActivity(r.Context(), tenant, caseID, "DEID_SCANNED",
			fmt.Sprintf("Outbound %s scanned: %d finding(s), risk %.1f — released", artifact, len(res.Findings), res.Risk))
	}
	return ""
}

// ---- Read endpoints --------------------------------------------------------

// deidReports: GET /cases/{caseId}/deid-reports — the audit trail of every
// de-identification decision touching this case.
func (s *server) deidReports(w http.ResponseWriter, r *http.Request) {
	tenant := r.Context().Value(ctxTenant{}).(string)
	caseID := chi.URLParam(r, "caseId")
	rows, err := s.db.Query(r.Context(), `
		SELECT id::text, artifact, coalesce(ref_id,''), action, policy, detectors, counts,
		       findings, risk, text_hash, created_at
		FROM public.deid_reports WHERE tenant=$1 AND case_id=$2
		ORDER BY created_at DESC LIMIT 100`, tenant, caseID)
	if err != nil {
		http.Error(w, `{"error":"query failed"}`, http.StatusInternalServerError)
		return
	}
	defer rows.Close()
	out := []map[string]any{}
	for rows.Next() {
		var id, artifact, refID, action, policy, textHash string
		var detectors, counts, findings []byte
		var risk float64
		var ts time.Time
		if rows.Scan(&id, &artifact, &refID, &action, &policy, &detectors, &counts, &findings, &risk, &textHash, &ts) != nil {
			continue
		}
		out = append(out, map[string]any{
			"id": id, "artifact": artifact, "ref_id": refID, "action": action,
			"policy": policy, "detectors": json.RawMessage(detectors),
			"counts": json.RawMessage(counts), "findings": json.RawMessage(findings),
			"risk": risk, "text_hash": textHash, "created_at": ts})
	}
	writeJSON(w, http.StatusOK, map[string]any{"reports": out})
}

// caseClinical: GET /cases/{caseId}/clinical — aggregated clinical entities
// across the case's analyzed documents.
func (s *server) caseClinical(w http.ResponseWriter, r *http.Request) {
	tenant := r.Context().Value(ctxTenant{}).(string)
	caseID := chi.URLParam(r, "caseId")
	rows, err := s.db.Query(r.Context(), `
		SELECT doc_id, entities, summary, detectors, created_at
		FROM public.document_clinical WHERE tenant=$1 AND case_id=$2
		ORDER BY created_at`, tenant, caseID)
	if err != nil {
		http.Error(w, `{"error":"query failed"}`, http.StatusInternalServerError)
		return
	}
	defer rows.Close()
	docs := []map[string]any{}
	totals := map[string]int{}
	for rows.Next() {
		var docID string
		var entities, summary, detectors []byte
		var ts time.Time
		if rows.Scan(&docID, &entities, &summary, &detectors, &ts) != nil {
			continue
		}
		var sum map[string]int
		if json.Unmarshal(summary, &sum) == nil {
			for k, v := range sum {
				totals[k] += v
			}
		}
		docs = append(docs, map[string]any{"doc_id": docID,
			"entities": json.RawMessage(entities), "summary": json.RawMessage(summary),
			"detectors": json.RawMessage(detectors), "created_at": ts})
	}
	writeJSON(w, http.StatusOK, map[string]any{"documents": docs, "totals": totals})
}

// clinicalOverlaps: GET /cases/{caseId}/clinical/overlaps — the duplicate /
// related-case signal: other cases in the tenant whose documents share
// clinical entities (normalized surface forms) with this case, ranked by
// overlap count. A shared "ceftriaxone + pneumonia + ct scan" fingerprint on
// two cases is the strongest automated same-episode signal the platform has.
func (s *server) clinicalOverlaps(w http.ResponseWriter, r *http.Request) {
	tenant := r.Context().Value(ctxTenant{}).(string)
	caseID := chi.URLParam(r, "caseId")
	rows, err := s.db.Query(r.Context(), `
		WITH mine AS (
			SELECT DISTINCT lower(e->>'text') AS term
			FROM public.document_clinical d, jsonb_array_elements(d.entities) e
			WHERE d.tenant=$1 AND d.case_id=$2
			  AND e->>'type' IN ('DRUG','CONDITION','PROCEDURE')
		),
		theirs AS (
			SELECT d.case_id, lower(e->>'text') AS term, e->>'type' AS typ
			FROM public.document_clinical d, jsonb_array_elements(d.entities) e
			WHERE d.tenant=$1 AND d.case_id<>$2 AND d.case_id IS NOT NULL
			  AND e->>'type' IN ('DRUG','CONDITION','PROCEDURE')
		)
		SELECT t.case_id, count(DISTINCT t.term) AS overlap,
		       array_agg(DISTINCT t.term ORDER BY t.term) AS shared
		FROM theirs t JOIN mine m ON m.term=t.term
		GROUP BY t.case_id HAVING count(DISTINCT t.term) >= 2
		ORDER BY overlap DESC LIMIT 20`, tenant, caseID)
	if err != nil {
		http.Error(w, `{"error":"query failed"}`, http.StatusInternalServerError)
		return
	}
	defer rows.Close()
	out := []map[string]any{}
	for rows.Next() {
		var cid string
		var overlap int
		var shared []string
		if rows.Scan(&cid, &overlap, &shared) == nil {
			out = append(out, map[string]any{"case_id": cid, "overlap": overlap, "shared_terms": shared})
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"overlaps": out})
}

// deidClinicalIngest: POST /internal/documents/{docId}/clinical — doc-intel
// worker callback carrying one document's clinical extraction (worker token).
func (s *server) deidClinicalIngest(w http.ResponseWriter, r *http.Request) {
	// Worker-token only — tenant members must not fabricate clinical signals.
	if p := r.Context().Value(ctxPrincipal{}).(principal); !hasAnyRole(p, serviceRole) {
		http.Error(w, `{"error":"forbidden"}`, http.StatusForbidden)
		return
	}
	tenant := r.Context().Value(ctxTenant{}).(string)
	docID := chi.URLParam(r, "docId")
	var in struct {
		CaseID    string          `json:"case_id"`
		Entities  json.RawMessage `json:"entities"`
		Summary   json.RawMessage `json:"summary"`
		Detectors json.RawMessage `json:"detectors"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		http.Error(w, `{"error":"bad json"}`, http.StatusBadRequest)
		return
	}
	if len(in.Entities) == 0 {
		in.Entities = []byte("[]")
	}
	if len(in.Summary) == 0 {
		in.Summary = []byte("{}")
	}
	if len(in.Detectors) == 0 {
		in.Detectors = []byte("[]")
	}
	if _, err := s.db.Exec(r.Context(), `
		INSERT INTO public.document_clinical (tenant, doc_id, case_id, entities, summary, detectors)
		VALUES ($1,$2,$3,$4,$5,$6)
		ON CONFLICT (tenant, doc_id) DO UPDATE SET
		  case_id=EXCLUDED.case_id, entities=EXCLUDED.entities,
		  summary=EXCLUDED.summary, detectors=EXCLUDED.detectors, created_at=now()`,
		tenant, docID, nilIfEmpty(in.CaseID), in.Entities, in.Summary, in.Detectors); err != nil {
		http.Error(w, `{"error":"persist failed"}`, http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// deidInternalReport: POST /internal/deid/reports — worker callback
// persisting an audit report produced by another service (graph-intel's
// lakehouse pre-write deidentify, …).
func (s *server) deidInternalReport(w http.ResponseWriter, r *http.Request) {
	// Worker-token only — audit reports are written by services, not users.
	if p := r.Context().Value(ctxPrincipal{}).(principal); !hasAnyRole(p, serviceRole) {
		http.Error(w, `{"error":"forbidden"}`, http.StatusForbidden)
		return
	}
	tenant := r.Context().Value(ctxTenant{}).(string)
	var in struct {
		CaseID   string    `json:"case_id"`
		Artifact string    `json:"artifact"`
		RefID    string    `json:"ref_id"`
		Action   string    `json:"action"`
		Audit    deidAudit `json:"audit"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil || in.Artifact == "" || in.Action == "" {
		http.Error(w, `{"error":"bad json"}`, http.StatusBadRequest)
		return
	}
	s.deidPersistReport(r.Context(), tenant, in.CaseID, in.Artifact, in.RefID, in.Action, in.Audit)
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// deidProxy: POST /deid/deidentify — authenticated tenant-facing passthrough
// for the portal's "preview redaction" affordance. The response's audit is
// persisted like any other decision.
func (s *server) deidProxy(w http.ResponseWriter, r *http.Request) {
	tenant := r.Context().Value(ctxTenant{}).(string)
	if !s.deidActive(r, tenant) {
		http.Error(w, `{"error":"de-identification not configured (DEID_URL unset)"}`, http.StatusServiceUnavailable)
		return
	}
	var in struct {
		Text     string `json:"text"`
		CaseID   string `json:"case_id"`
		Artifact string `json:"artifact"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil || strings.TrimSpace(in.Text) == "" {
		http.Error(w, `{"error":"text required"}`, http.StatusBadRequest)
		return
	}
	if len(in.Text) > 500000 {
		http.Error(w, `{"error":"text too long"}`, http.StatusRequestEntityTooLarge)
		return
	}
	payload, _ := json.Marshal(map[string]any{
		"tenant": tenant, "case_id": in.CaseID,
		"artifact": orDefault(in.Artifact, "preview"), "text": in.Text})
	req, _ := http.NewRequestWithContext(r.Context(), http.MethodPost,
		s.cfg.DeidURL+"/v1/deidentify", bytes.NewReader(payload))
	req.Header.Set("content-type", "application/json")
	resp, err := deidHTTP.Do(req)
	if err != nil {
		http.Error(w, `{"error":"deid unavailable"}`, http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()
	var out struct {
		Text  string    `json:"text"`
		Audit deidAudit `json:"audit"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		http.Error(w, `{"error":"deid response"}`, http.StatusBadGateway)
		return
	}
	s.deidPersistReport(r.Context(), tenant, in.CaseID, orDefault(in.Artifact, "preview"), "", "DEIDENTIFY", out.Audit)
	writeJSON(w, http.StatusOK, map[string]any{"text": out.Text, "audit": out.Audit})
}

func nilIfEmpty(v string) any {
	if v == "" {
		return nil
	}
	return v
}

func orDefault(v, d string) string {
	if v == "" {
		return d
	}
	return v
}
