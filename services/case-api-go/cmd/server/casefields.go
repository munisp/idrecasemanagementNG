// casefields.go — program-specific case fields (details jsonb), case-document
// zip bundles (plan notification packages), and on-request deliverables.
package main

import (
	"archive/zip"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/minio/minio-go/v7"
)

// setCaseDetails: PATCH /v1/tenants/{tenant}/cases/{caseId}/details
// Merges program-specific fields into cases.details — the PLUM Field Criteria
// set (line_of_business, disputed_issue, out_of_network, case_outcome,
// final_amount_awarded_cents, withdrawal_dismissed_reason, party_billed,
// provider address block, …). Keys listed in the program's field_schema with
// an allowed-value list are validated against it; unlisted keys pass through
// so a state can extend its own field set without a deploy.
func (s *server) setCaseDetails(w http.ResponseWriter, r *http.Request) {
	tenant := r.Context().Value(ctxTenant{}).(string)
	caseID := chi.URLParam(r, "caseId")
	var in map[string]any
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil || len(in) == 0 {
		http.Error(w, `{"error":"details object required"}`, http.StatusBadRequest)
		return
	}
	// Validate list-valued fields against the program's field_schema (if seeded).
	if prog := s.loadProgram(r, tenant); prog != nil {
		for key, allowed := range prog.FieldSchema {
			if v, present := in[key]; present && len(allowed) > 0 {
				sv, _ := v.(string)
				ok := false
				for _, a := range allowed {
					if strings.EqualFold(a, sv) {
						ok = true
						break
					}
				}
				if !ok {
					http.Error(w, fmt.Sprintf(`{"error":"%s must be one of %s"}`, key, strings.Join(allowed, ", ")), http.StatusBadRequest)
					return
				}
			}
		}
	}
	merge, _ := json.Marshal(in)
	res, err := s.db.Exec(r.Context(), fmt.Sprintf(`
		UPDATE tenant_%s.cases SET details = details || $2::jsonb, updated_at=now() WHERE id=$1`,
		sanitizeTenant(tenant)), caseID, string(merge))
	if err != nil {
		http.Error(w, `{"error":"db"}`, http.StatusInternalServerError)
		return
	}
	if res.RowsAffected() == 0 {
		http.Error(w, `{"error":"not found"}`, http.StatusNotFound)
		return
	}
	keys := make([]string, 0, len(in))
	for k := range in {
		keys = append(keys, k)
	}
	// Award recorded/changed/cleared -> keep the AP subledger in step.
	if _, ok := in["final_amount_awarded_cents"]; ok {
		actor := "staff"
		if pr, ok2 := r.Context().Value(ctxPrincipal{}).(principal); ok2 {
			actor = pr.Subject
		}
		s.syncAwardPayable(r, tenant, caseID, in, actor)
	}
	s.logActivity(r.Context(), tenant, caseID, "DETAILS_UPDATED", "fields set: "+strings.Join(keys, ", "))
	writeJSON(w, http.StatusOK, map[string]any{"status": "updated", "fields": keys})
}

// zipCaseDocuments: GET /v1/tenants/{tenant}/cases/{caseId}/documents.zip
// Produces the "Filing party dispute documentation FL2X-0XX.zip" bundle the
// plan-notification workflow mails to the health plan. Sealed offer documents
// are excluded until lawful reveal. Streams: each entry is decrypted straight
// into the zip writer — no temp files.
func (s *server) zipCaseDocuments(w http.ResponseWriter, r *http.Request) {
	tenant := r.Context().Value(ctxTenant{}).(string)
	caseID := chi.URLParam(r, "caseId")
	var caseNumber string
	_ = s.db.QueryRow(r.Context(), fmt.Sprintf(
		`SELECT case_number FROM tenant_%s.cases WHERE id=$1`, sanitizeTenant(tenant)), caseID).Scan(&caseNumber)

	rows, err := s.db.Query(r.Context(), fmt.Sprintf(`
		SELECT id, object_key, coalesce(filename, id::text), sealed, size_bytes, parts
		FROM tenant_%s.documents WHERE case_id=$1 AND scan_status='CLEAN'
		ORDER BY folder, created_at`, sanitizeTenant(tenant)), caseID)
	if err != nil {
		http.Error(w, `{"error":"db"}`, http.StatusInternalServerError)
		return
	}
	type docRow struct {
		id, key, name string
		sealed        bool
		size          int64
		parts         []byte
	}
	var docs []docRow
	defer rows.Close()
	for rows.Next() {
		var d docRow
		var sz *int64
		if rows.Scan(&d.id, &d.key, &d.name, &d.sealed, &sz, &d.parts) == nil {
			if sz != nil {
				d.size = *sz
			}
			docs = append(docs, d)
		}
	}
	if len(docs) == 0 {
		http.Error(w, `{"error":"no documents"}`, http.StatusNotFound)
		return
	}

	zipName := fmt.Sprintf("dispute documentation %s.zip", caseNumber)
	w.Header().Set("Content-Type", "application/zip")
	w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%q", zipName))
	zw := zip.NewWriter(w)
	fl, _ := w.(http.Flusher)
	used := map[string]int{}
	for _, d := range docs {
		if d.sealed && !s.revealLawful(r, caseID) {
			continue // double-blind: sealed offers never ship in party bundles
		}
		name := d.name
		if n := used[name]; n > 0 {
			name = fmt.Sprintf("%s-%d", d.name, n)
		}
		used[d.name]++
		entry, err := zw.Create(name)
		if err != nil {
			continue
		}
		if err := s.writeDocPlaintext(r, tenant, d.key, d.parts, entry); err != nil {
			http.Error(w, `{"error":"decrypt failed"}`, http.StatusBadGateway)
			return
		}
		if fl != nil {
			fl.Flush()
		}
	}
	zw.Close()
	s.logActivity(r.Context(), tenant, caseID, "ZIP_DOWNLOAD",
		fmt.Sprintf("document bundle (%d files) downloaded", len(docs)))
}

// writeDocPlaintext decrypts one document (single blob or multipart) into w.
func (s *server) writeDocPlaintext(r *http.Request, tenant, objectKey string, partsJSON []byte, w io.Writer) error {
	var parts []partMeta
	if len(partsJSON) > 0 {
		_ = json.Unmarshal(partsJSON, &parts)
	}
	if len(parts) == 0 {
		obj, err := s.docs.mc.GetObject(r.Context(), docBucket, objectKey, minio.GetObjectOptions{})
		if err != nil {
			return err
		}
		defer obj.Close()
		ct, err := io.ReadAll(obj)
		if err != nil {
			return err
		}
		pt, err := s.vaultOpenDoc(r, tenant, objectKey, ct)
		if err != nil {
			return err
		}
		_, err = w.Write(pt)
		return err
	}
	var sealedStart int64
	for _, p := range parts {
		opt := minio.GetObjectOptions{}
		if err := opt.SetRange(sealedStart, sealedStart+p.SealedBytes-1); err != nil {
			return err
		}
		obj, err := s.docs.mc.GetObject(r.Context(), docBucket, objectKey, opt)
		if err != nil {
			return err
		}
		ct, err := io.ReadAll(obj)
		obj.Close()
		if err != nil {
			return err
		}
		pt, err := s.vaultOpenDoc(r, tenant, fmt.Sprintf("%s#part-%d", objectKey, p.N), ct)
		if err != nil {
			return err
		}
		if _, err := w.Write(pt); err != nil {
			return err
		}
		sealedStart += p.SealedBytes
	}
	return nil
}

// requestAdhocDeliverable: POST /v1/tenants/{tenant}/deliverables/request
// Contract deliverable 1.3 (§2.4.4): ad hoc reports are due within 10 business
// days of the request. The clock starts here — not in a spreadsheet.
func (s *server) requestAdhocDeliverable(w http.ResponseWriter, r *http.Request) {
	tenant := r.Context().Value(ctxTenant{}).(string)
	var in struct {
		Name string `json:"name"`
		Ref  string `json:"contract_ref"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil || in.Name == "" {
		http.Error(w, `{"error":"name required"}`, http.StatusBadRequest)
		return
	}
	due := addBusinessDays(time.Now(), 10, nil)
	var id int64
	err := s.db.QueryRow(r.Context(), `
		INSERT INTO public.deliverables (tenant, name, contract_ref, due_rule, due_date)
		VALUES ($1,$2,$3,'on_request:10bd',$4) RETURNING id`,
		tenant, in.Name, in.Ref, due).Scan(&id)
	if err != nil {
		http.Error(w, `{"error":"db"}`, http.StatusInternalServerError)
		return
	}
	s.notify(r, tenant, "*", "DELIVERABLE",
		fmt.Sprintf("Ad hoc report requested: %s — due %s (10 business days)", in.Name, due.Format("2006-01-02")), "#/deliverables")
	writeJSON(w, http.StatusCreated, map[string]any{"id": id, "due_date": due.Format("2006-01-02")})
}
