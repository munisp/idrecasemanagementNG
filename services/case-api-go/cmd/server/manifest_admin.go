// manifest_admin.go — admin API for Program Manifests.
//
// Same discipline as rules_admin.go: admin-only (Keycloak role + Permify
// program_rules.edit), validate-before-write, full replacement, append-only
// audit row in public.rule_changes carrying the before/after manifest.
package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"time"
)

// getManifest: GET /v1/tenants/{tenant}/manifest — any authenticated caller
// (the portal renders labels and pipelines from this). 404 when the tenant
// has no manifest (legacy behavior).
func (s *server) getManifest(w http.ResponseWriter, r *http.Request) {
	tenant := r.Context().Value(ctxTenant{}).(string)
	m, err := s.manifestFor(r, tenant)
	if err != nil {
		http.Error(w, fmt.Sprintf(`{"error":%q}`, err.Error()), http.StatusConflict)
		return
	}
	if m == nil {
		http.Error(w, `{"error":"no manifest configured — tenant runs legacy defaults"}`, http.StatusNotFound)
		return
	}
	writeJSON(w, http.StatusOK, m)
}

// putManifest: PUT /v1/tenants/{tenant}/manifest — validate, replace, audit.
// The write and the audit row commit in ONE transaction.
func (s *server) putManifest(w http.ResponseWriter, r *http.Request) {
	p, ok := s.rulesAdminGuard(w, r)
	if !ok {
		return
	}
	tenant := r.Context().Value(ctxTenant{}).(string)
	var in struct {
		Manifest json.RawMessage `json:"manifest"`
		Note     string          `json:"note"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil || len(in.Manifest) == 0 {
		http.Error(w, `{"error":"manifest (JSON) and note required"}`, http.StatusBadRequest)
		return
	}
	var m ProgramManifest
	if err := json.Unmarshal(in.Manifest, &m); err != nil {
		http.Error(w, fmt.Sprintf(`{"error":"manifest JSON invalid: %s"}`, err.Error()), http.StatusBadRequest)
		return
	}
	if err := validateManifest(&m); err != nil {
		http.Error(w, fmt.Sprintf(`{"error":%q}`, err.Error()), http.StatusBadRequest)
		return
	}

	tx, err := s.db.Begin(r.Context())
	if err != nil {
		http.Error(w, `{"error":"db"}`, http.StatusInternalServerError)
		return
	}
	defer tx.Rollback(r.Context())

	var before []byte
	_ = tx.QueryRow(r.Context(),
		`SELECT coalesce(config->'manifest','null'::jsonb) FROM public.program_rules WHERE tenant=$1`,
		tenant).Scan(&before)

	res, err := tx.Exec(r.Context(), `
		UPDATE public.program_rules
		SET config = jsonb_set(config, '{manifest}', $2::jsonb, true), updated_at=now()
		WHERE tenant=$1`, tenant, in.Manifest)
	if err != nil {
		http.Error(w, `{"error":"db"}`, http.StatusInternalServerError)
		return
	}
	if res.RowsAffected() == 0 {
		http.Error(w, `{"error":"tenant has no program_rules row — seed it first"}`, http.StatusConflict)
		return
	}
	if _, err := tx.Exec(r.Context(), `
		INSERT INTO public.rule_changes (tenant, changed_by, note, before, after)
		VALUES ($1,$2,$3,
		        jsonb_build_object('manifest', coalesce($4::jsonb,'null'::jsonb)),
		        jsonb_build_object('manifest', $5::jsonb))`,
		tenant, p.Subject, "manifest: "+in.Note, before, in.Manifest); err != nil {
		http.Error(w, `{"error":"audit write failed — manifest NOT saved"}`, http.StatusInternalServerError)
		return
	}
	if err := tx.Commit(r.Context()); err != nil {
		http.Error(w, `{"error":"commit"}`, http.StatusInternalServerError)
		return
	}
	s.logActivity(r.Context(), tenant, "", "MANIFEST_UPDATED",
		fmt.Sprintf("Program manifest %s@%s installed by %s — %s", m.Program, m.Version, p.Subject, in.Note))
	writeJSON(w, http.StatusOK, map[string]any{
		"program": m.Program, "version": m.Version, "at": time.Now().UTC(),
	})
}
