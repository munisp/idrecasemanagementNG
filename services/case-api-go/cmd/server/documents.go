// documents.go — document lifecycle: encrypted upload, authorized download,
// retention/legal-hold, and analysis-status tracking.
//
// Flow (upload):  multipart -> vault /docs/seal (AES-256-GCM, tenant data key)
//                 -> MinIO ciphertext object -> metadata row -> Kafka event
//                 (doc-intel service consumes and analyzes).
// Flow (download): authorize -> sealed-document guard -> MinIO fetch
//                 -> vault /docs/open -> plaintext stream.
package main

import (
	"bytes"
	crand "crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
)

const (
	docBucket      = "idre-docs"
	maxDocBytes    = 100 << 20 // 100 MB hard cap
	retentionYears = 6         // federal retention mandate
	// tenantStorageQuota bounds cumulative document bytes per tenant (25 GiB).
	// Deployments can raise it per program; it exists to cap abuse/accident
	// blast radius, not to constrain legitimate dockets.
	tenantStorageQuota = 25 << 30
)

type docStore struct{ mc *minio.Client }

func newDocStore(endpoint, user, pass string) (*docStore, error) {
	mc, err := minio.New(endpoint, &minio.Options{
		Creds:  credentials.NewStaticV4(user, pass, ""),
		Secure: false, // TLS terminated at ingress in prod; in-cluster plaintext
	})
	if err != nil {
		return nil, err
	}
	return &docStore{mc: mc}, nil
}

type vaultDocReq struct {
	Tenant  string `json:"tenant"`
	Key     string `json:"key"`
	DataB64 string `json:"data_b64"`
}

// docFolders are the docket folders staff can file documents into.
var docFolders = map[string]bool{
	"GENERAL": true, "INTAKE": true, "EVIDENCE": true, "CORRESPONDENCE": true,
	"OFFERS": true, "DETERMINATION": true, "INVOICES": true, "PARTY_UPLOADS": true,
}

// hasAnyRole reports whether the principal holds any of the listed roles.
func hasAnyRole(p principal, roles ...string) bool {
	for _, want := range roles {
		if hasRole(p, want) {
			return true
		}
	}
	return false
}

// uploadDocument: POST /v1/tenants/{tenant}/cases/{caseId}/documents (multipart)
func (s *server) uploadDocument(w http.ResponseWriter, r *http.Request) {
	tenant := r.Context().Value(ctxTenant{}).(string)
	caseID := chi.URLParam(r, "caseId")
	p := r.Context().Value(ctxPrincipal{}).(principal)
	// RBAC: auditors and external viewers never write to the docket.
	if !hasAnyRole(p, "PARTY", "CASE_MANAGER", "ARBITRATOR", "FINANCE", "FEDERAL_ADMIN", "PLATFORM_ADMIN") {
		http.Error(w, `{"error":"role may not upload documents"}`, http.StatusForbidden)
		return
	}

	// Abuse throttle: authenticated uploads were previously unlimited — a
	// compromised or careless account could flood MinIO + the doc-intel queue
	// (every document triggers OCR + VLM work). Per-user ceiling, fail-open
	// only if Redis is down (authenticated surface; ShareBox stays fail-closed).
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
	sealedDoc := r.FormValue("sealed") == "true" // offer justifications: sealed until reveal

	// security gate: content policy + ClamAV before anything is sealed/stored
	if err := contentPolicy(hdr.Filename, raw); err != nil {
		http.Error(w, fmt.Sprintf(`{"error":%q}`, err.Error()), http.StatusUnsupportedMediaType)
		return
	}
	sig, ok := s.scanOrRefuse(w, bytes.NewReader(raw), "file")
	if !ok {
		if sig != "" {
			s.logActivity(r.Context(), tenant, caseID, "MALWARE_BLOCKED",
				fmt.Sprintf("Upload %q rejected — ClamAV signature %s; nothing stored", hdr.Filename, sig))
		}
		return
	}

	// Storage quota: cumulative per-tenant cap stops a slow-bleed fill of
	// object storage that per-request limits alone would miss.
	var used int64
	if err := s.db.QueryRow(r.Context(), fmt.Sprintf(
		"SELECT COALESCE(SUM(size_bytes),0) FROM tenant_%s.documents", sanitizeTenant(tenant))).Scan(&used); err == nil {
		if used+int64(len(raw)) > tenantStorageQuota {
			http.Error(w, `{"error":"tenant document storage quota exceeded — contact your program administrator"}`, http.StatusRequestEntityTooLarge)
			return
		}
	}

	docID := newUUID()
	objectKey := fmt.Sprintf("%s/cases/%s/%s.enc", tenant, caseID, docID)

	// 1. Encrypt through the vault (plaintext never touches disk/object storage).
	ct, err := s.vaultSealDoc(r, tenant, objectKey, raw)
	if err != nil {
		http.Error(w, `{"error":"seal failed"}`, http.StatusBadGateway)
		return
	}

	// 2. Persist ciphertext to MinIO with retention metadata.
	_, err = s.docs.mc.PutObject(r.Context(), docBucket, objectKey,
		bytes.NewReader(ct), int64(len(ct)), minio.PutObjectOptions{
			ContentType:  "application/octet-stream",
			UserMetadata: map[string]string{"tenant": tenant, "case": caseID},
			// Legal hold: retention governs object-lock in prod (WORM bucket policy).
		})
	if err != nil {
		http.Error(w, `{"error":"store failed"}`, http.StatusBadGateway)
		return
	}

	// 3. Metadata row + outbox event in one tx.
	folder := r.FormValue("folder")
	if !docFolders[folder] {
		folder = "GENERAL"
	}
	var version int
	err = s.db.QueryRow(r.Context(), fmt.Sprintf(`
		INSERT INTO tenant_%s.documents
		  (id, case_id, object_key, size_bytes, content_type, sealed, uploaded_by, version, scan_status, filename, folder)
		VALUES ($1,$2,$3,$4,$5,$6,$7,1,'CLEAN',$8,$9) RETURNING version`, sanitizeTenant(tenant)),
		docID, caseID, objectKey, len(raw), hdr.Header.Get("Content-Type"), sealedDoc, p.Subject, hdr.Filename, folder).
		Scan(&version)
	if err != nil {
		http.Error(w, `{"error":"db"}`, http.StatusInternalServerError)
		return
	}

	// 4. Event for the doc-intel pipeline (Kafka via outbox relay).
	s.publish(r.Context(), tenant, "documents", map[string]any{
		"type": "doc.uploaded", "tenant": tenant, "case_id": caseID,
		"doc_id": docID, "object_key": objectKey, "content_type": hdr.Header.Get("Content-Type"),
		"sealed": sealedDoc, "at": time.Now().UTC(),
	})

	// 5. Program rules (doc.upload). The file is already stored, so
	// block_request enforces by QUARANTINE (analysis_status=BLOCKED, staff
	// notified) rather than a rejected HTTP status — the bytes exist either
	// way; what matters is whether they enter the review pipeline.
	quarantined := false
	if blocked, msg := s.fireEventRules(r, tenant, "doc.upload", map[string]any{
		"case_id": caseID, "doc_id": docID, "folder": folder, "sealed": sealedDoc,
		"size_bytes": len(raw), "content_type": hdr.Header.Get("Content-Type"),
		"filename": hdr.Filename, "uploaded_by": p.Subject, "tenant": tenant,
	}); blocked {
		quarantined = true
		_, _ = s.db.Exec(r.Context(), fmt.Sprintf(`
			UPDATE tenant_%s.documents SET analysis_status='BLOCKED'
			WHERE id=$1`, sanitizeTenant(tenant)), docID)
		s.logActivity(r.Context(), tenant, caseID, "RULE_BLOCKED",
			fmt.Sprintf("Upload of %q quarantined by program rule: %s", hdr.Filename, msg))
	}

	// 6. Unified timeline entry (visible in caseDetail + account 360 + voice).
	s.logActivity(r.Context(), tenant, caseID, "DOCUMENT_UPLOADED",
		fmt.Sprintf("%s uploaded %q (%d bytes, sealed=%v) by %s — analysis queued",
			hdr.Filename, hdr.Filename, len(raw), sealedDoc, p.Subject))
	analysis := "QUEUED" // doc-intel consumes doc.uploaded
	if quarantined {
		analysis = "BLOCKED"
	}
	// Docs on the docket — checklist items whose predicate is "docs exist"
	// (and any status that follows from them) update themselves.
	s.autoChecklist(r, tenant, caseID)
	s.maybeAdvanceStatus(r, tenant, caseID)
	writeJSON(w, http.StatusCreated, map[string]any{
		"doc_id": docID, "version": version, "bytes": len(raw),
		"analysis": analysis,
	})
}

// downloadDocument: GET .../documents/{docId}/download — sealed guard enforced.
func (s *server) downloadDocument(w http.ResponseWriter, r *http.Request) {
	tenant := r.Context().Value(ctxTenant{}).(string)
	docID := chi.URLParam(r, "docId")
	var key, ct, fname string
	var sealed bool
	err := s.db.QueryRow(r.Context(), fmt.Sprintf(`
		SELECT object_key, content_type, sealed, coalesce(filename,'') FROM tenant_%s.documents WHERE id=$1`,
		sanitizeTenant(tenant)), docID).Scan(&key, &ct, &sealed, &fname)
	if err != nil {
		http.Error(w, `{"error":"not found"}`, http.StatusNotFound)
		return
	}
	// Double-blind guard: sealed docs (offer justifications) only open after lawful reveal.
	if sealed {
		if !s.revealLawful(r, chi.URLParam(r, "caseId")) {
			http.Error(w, `{"error":"document sealed until lawful offer reveal"}`, http.StatusLocked)
			return
		}
		// RBAC floor (requirePerm below allows everyone when Permify is
		// undeployed): only staff who could plausibly need to see a
		// revealed sealed offer, not every member of the tenant.
		p := r.Context().Value(ctxPrincipal{}).(principal)
		if !hasAnyRole(p, "ARBITRATOR", "CASE_MANAGER", "FEDERAL_ADMIN", "PLATFORM_ADMIN") {
			http.Error(w, `{"error":"forbidden: requires ARBITRATOR, CASE_MANAGER, FEDERAL_ADMIN, or PLATFORM_ADMIN"}`, http.StatusForbidden)
			return
		}
		// ReBAC second gate: only the assigned arbitrator may open revealed offers.
		if !s.requirePerm(w, r, "dispute_case", chi.URLParam(r, "caseId"), "reveal") {
			return
		}
	}
	obj, err := s.docs.mc.GetObject(r.Context(), docBucket, key, minio.GetObjectOptions{})
	if err != nil {
		http.Error(w, `{"error":"object missing"}`, http.StatusNotFound)
		return
	}
	defer obj.Close()
	ct2, err := io.ReadAll(obj)
	if err != nil {
		http.Error(w, `{"error":"fetch failed"}`, http.StatusBadGateway)
		return
	}
	pt, err := s.vaultOpenDoc(r, tenant, key, ct2)
	if err != nil {
		http.Error(w, `{"error":"decrypt failed"}`, http.StatusBadGateway)
		return
	}
	if fname == "" {
		fname = docID
	}
	w.Header().Set("Content-Type", ct)
	w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="%s"`, fname))
	w.Write(pt)
}

// moveDocument: PATCH /cases/{caseId}/documents/{docId} — re-file a document
// into another docket folder. Staff roles only (CASE_MANAGER/FEDERAL_ADMIN/
// PLATFORM_ADMIN); parties and auditors cannot reorganize the docket.
func (s *server) moveDocument(w http.ResponseWriter, r *http.Request) {
	tenant := r.Context().Value(ctxTenant{}).(string)
	p := r.Context().Value(ctxPrincipal{}).(principal)
	if !hasAnyRole(p, "CASE_MANAGER", "FEDERAL_ADMIN", "PLATFORM_ADMIN") {
		http.Error(w, `{"error":"role may not re-file documents"}`, http.StatusForbidden)
		return
	}
	var in struct {
		Folder string `json:"folder"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil || !docFolders[in.Folder] {
		http.Error(w, `{"error":"unknown folder"}`, http.StatusBadRequest)
		return
	}
	caseID, docID := chi.URLParam(r, "caseId"), chi.URLParam(r, "docId")
	if _, err := s.db.Exec(r.Context(), fmt.Sprintf(`
		UPDATE tenant_%s.documents SET folder=$3 WHERE id=$1 AND case_id=$2`,
		sanitizeTenant(tenant)), docID, caseID, in.Folder); err != nil {
		http.Error(w, `{"error":"db"}`, http.StatusInternalServerError)
		return
	}
	s.logActivity(r.Context(), tenant, caseID, "DOCUMENT_MOVED",
		fmt.Sprintf("Document re-filed to %s by %s", in.Folder, p.Subject))
	writeJSON(w, http.StatusOK, map[string]string{"folder": in.Folder})
}

// listDocuments + analysis status.
func (s *server) listDocuments(w http.ResponseWriter, r *http.Request) {
	tenant := r.Context().Value(ctxTenant{}).(string)
	rows, err := s.db.Query(r.Context(), fmt.Sprintf(`
		SELECT d.id, d.content_type, d.size_bytes, d.sealed, d.created_at,
		       COALESCE(a.status,'QUEUED'), COALESCE(a.doc_type,''),
		       COALESCE(d.filename,''), COALESCE(d.folder,'GENERAL'),
		       COALESCE(d.scan_status,'PENDING'), COALESCE(d.uploaded_by,'')
		FROM tenant_%s.documents d
		LEFT JOIN public.doc_analysis a ON a.doc_id = d.id
		WHERE d.case_id=$1 ORDER BY d.folder, d.created_at DESC`, sanitizeTenant(tenant)),
		chi.URLParam(r, "caseId"))
	if err != nil {
		http.Error(w, `{"error":"db"}`, http.StatusInternalServerError)
		return
	}
	defer rows.Close()
	out := []map[string]any{}
	for rows.Next() {
		var id, ct, status, dtype, fname, folder, scan, by string
		var size int
		var sealed bool
		var at time.Time
		if rows.Scan(&id, &ct, &size, &sealed, &at, &status, &dtype, &fname, &folder, &scan, &by) == nil {
			out = append(out, map[string]any{
				"doc_id": id, "content_type": ct, "size_bytes": size, "sealed": sealed,
				"uploaded_at": at, "analysis_status": status, "doc_type": dtype,
				"filename": fname, "folder": folder, "scan_status": scan, "uploaded_by": by,
			})
		}
	}
	writeJSON(w, http.StatusOK, out)
}

// documentAnalysis: full structured result from the PaddleOCR+VLM pipeline.
func (s *server) documentAnalysis(w http.ResponseWriter, r *http.Request) {
	var payload, docType, status string
	err := s.db.QueryRow(r.Context(), `
		SELECT status, doc_type, COALESCE(result::text,'{}') FROM public.doc_analysis
		WHERE doc_id=$1`, chi.URLParam(r, "docId")).Scan(&status, &docType, &payload)
	if err != nil {
		http.Error(w, `{"error":"not found"}`, http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	fmt.Fprintf(w, `{"status":%q,"doc_type":%q,"result":%s}`, status, docType, payload)
}

// ---- vault helpers ---------------------------------------------------------

func (s *server) vaultSealDoc(r *http.Request, tenant, key string, pt []byte) ([]byte, error) {
	body, _ := json.Marshal(vaultDocReq{Tenant: tenant, Key: key, DataB64: b64enc(pt)})
	resp, err := http.Post(s.cfg.VaultURL+"/docs/seal", "application/json", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var out struct {
		SealedB64 string `json:"sealed_b64"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, err
	}
	return b64dec(out.SealedB64)
}

func (s *server) vaultOpenDoc(r *http.Request, tenant, key string, ct []byte) ([]byte, error) {
	body, _ := json.Marshal(vaultDocReq{Tenant: tenant, Key: key, DataB64: b64enc(ct)})
	resp, err := http.Post(s.cfg.VaultURL+"/docs/open", "application/json", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var out struct {
		DataB64 string `json:"data_b64"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, err
	}
	return b64dec(out.DataB64)
}

// revealLawful mirrors the vault's reveal-check for document-level guards.
func (s *server) revealLawful(r *http.Request, caseID string) bool {
	var revealed bool
	tenant := r.Context().Value(ctxTenant{}).(string)
	_ = s.db.QueryRow(r.Context(), fmt.Sprintf(
		`SELECT status IN ('OFFERS_REVEALED','DETERMINED','CLOSED_PAID') FROM tenant_%s.cases WHERE id=$1`,
		sanitizeTenant(tenant)), caseID).Scan(&revealed)
	return revealed
}

func newUUID() string { // uuid v4 without an extra dep
	var b [16]byte
	if _, err := crand.Read(b[:]); err != nil {
		panic(err)
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

func b64enc(b []byte) string        { return base64.StdEncoding.EncodeToString(b) }
func b64dec(s string) ([]byte, error) { return base64.StdEncoding.DecodeString(s) }
