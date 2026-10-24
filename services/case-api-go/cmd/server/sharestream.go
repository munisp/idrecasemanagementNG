// Author: Patrick Munis at newwave
package main

// ShareBox streaming + resumable transfers.
//
// Downloads: HTTP Range support (RFC 7233) — single-part documents are
// vault-opened and served via http.ServeContent (Accept-Ranges + resume);
// multi-part documents decrypt and stream only the sealed parts overlapping
// the requested range, straight from MinIO to the socket.
//
// Uploads: tus-style resumable protocol over MinIO multipart:
//   POST   /api/share/{token}/uploads            create (Upload-Length, name) -> upload_id
//   HEAD   /api/share/{token}/uploads/{id}       resume probe -> Upload-Offset
//   PATCH  /api/share/{token}/uploads/{id}       next chunk at Upload-Offset (8–32MB)
//   POST   /api/share/{token}/uploads/{id}/complete
// Each part is vault-sealed independently before it lands in MinIO — plaintext
// never persists, and interrupted uploads resume at the exact byte offset.

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/minio/minio-go/v7"
)

const (
	sharePartMin = 8 << 20  // 8MB floor keeps per-part vault seals efficient
	sharePartMax = 32 << 20 // 32MB hard cap per PATCH
)

type partMeta struct {
	N           int    `json:"n"`
	ETag        string `json:"etag"`
	PlainBytes  int64  `json:"plain_bytes"`
	SealedBytes int64  `json:"sealed_bytes"`
}

// ---- Resumable upload --------------------------------------------------------

func (s *server) shareCreateUpload(w http.ResponseWriter, r *http.Request) {
	token := chi.URLParam(r, "token")
	if !s.shareThrottle(w, r, token) {
		return
	}
	g, err := s.peekShare(r, token)
	if err != nil || g.Kind != "upload" {
		http.Error(w, `{"error":"link expired or invalid"}`, http.StatusGone)
		return
	}
	var in struct {
		Filename string `json:"filename"`
		Size     int64  `json:"size"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil || in.Filename == "" || in.Size <= 0 {
		http.Error(w, `{"error":"filename and positive size required"}`, http.StatusBadRequest)
		return
	}
	if in.Size > maxDocBytes {
		http.Error(w, `{"error":"exceeds 100MB platform cap"}`, http.StatusRequestEntityTooLarge)
		return
	}
	if err := contentPolicy(in.Filename, nil); err != nil { // extension/name policy up front
		http.Error(w, fmt.Sprintf(`{"error":%q}`, err.Error()), http.StatusUnsupportedMediaType)
		return
	}
	if !s.checkLinkBudgets(w, r, token, in.Size) {
		return
	}
	objectKey := fmt.Sprintf("%s/cases/%s/share-%d.enc", g.Tenant, g.CaseID, time.Now().UnixNano())
	core := minio.Core{Client: s.docs.mc}
	upID, err := core.NewMultipartUpload(r.Context(), docBucket, objectKey, minio.PutObjectOptions{
		ContentType:  "application/octet-stream",
		UserMetadata: map[string]string{"tenant": g.Tenant, "case": g.CaseID, "via": "sharebox-multipart"},
	})
	if err != nil {
		http.Error(w, `{"error":"storage backend"}`, http.StatusBadGateway)
		return
	}
	var uploadID string
	_ = s.db.QueryRow(r.Context(), `
		INSERT INTO public.share_uploads (token, tenant, case_id, filename, size_bytes, object_key, minio_upload_id)
		VALUES ($1,$2,$3,$4,$5,$6,$7) RETURNING upload_id`,
		token, g.Tenant, g.CaseID, in.Filename, in.Size, objectKey, upID).Scan(&uploadID)
	w.Header().Set("Upload-Offset", "0")
	writeJSON(w, http.StatusCreated, map[string]any{
		"upload_id": uploadID, "offset": 0,
		"patch_to":  fmt.Sprintf("/api/share/%s/uploads/%s", token, uploadID),
		"part_size_hint": sharePartMin,
	})
}

func (s *server) shareUploadOffset(w http.ResponseWriter, r *http.Request) {
	up, code := s.loadShareUpload(w, r)
	if up == nil {
		return
	}
	w.Header().Set("Upload-Offset", strconv.FormatInt(up.offset, 10))
	w.Header().Set("Upload-Length", strconv.FormatInt(up.size, 10))
	w.WriteHeader(code)
}

type shareUploadRow struct {
	tenant, caseID, filename, objectKey, minioUpID string
	offset, size                                   int64
	parts                                          []partMeta
}

func (s *server) loadShareUpload(w http.ResponseWriter, r *http.Request) (*shareUploadRow, int) {
	token, id := chi.URLParam(r, "token"), chi.URLParam(r, "uploadId")
	// token must still be valid for every chunk (expiry governs the whole transfer)
	if _, err := s.peekShare(r, token); err != nil {
		http.Error(w, `{"error":"link expired or invalid"}`, http.StatusGone)
		return nil, 0
	}
	var up shareUploadRow
	var raw []byte
	var status string
	err := s.db.QueryRow(r.Context(), `
		SELECT tenant, case_id, filename, object_key, minio_upload_id, bytes_rcvd, size_bytes, parts, status
		FROM public.share_uploads WHERE upload_id=$1 AND token=$2`, id, token).
		Scan(&up.tenant, &up.caseID, &up.filename, &up.objectKey, &up.minioUpID, &up.offset, &up.size, &raw, &status)
	if err != nil || status != "OPEN" {
		http.Error(w, `{"error":"upload not open"}`, http.StatusNotFound)
		return nil, 0
	}
	_ = json.Unmarshal(raw, &up.parts)
	return &up, http.StatusOK
}

// shareUploadChunk accepts the next part. Offset must match exactly — gaps are
// rejected (409) so the client re-probes with HEAD and resumes correctly.
func (s *server) shareUploadChunk(w http.ResponseWriter, r *http.Request) {
	if !s.shareThrottle(w, r, chi.URLParam(r, "token")) {
		return
	}
	up, _ := s.loadShareUpload(w, r)
	if up == nil {
		return
	}
	off, _ := strconv.ParseInt(r.Header.Get("Upload-Offset"), 10, 64)
	if off != up.offset {
		w.Header().Set("Upload-Offset", strconv.FormatInt(up.offset, 10))
		http.Error(w, `{"error":"offset mismatch — resume from Upload-Offset"}`, http.StatusConflict)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, sharePartMax+1<<20)
	chunk, err := io.ReadAll(r.Body)
	if err != nil || len(chunk) == 0 {
		http.Error(w, `{"error":"chunk read failed"}`, http.StatusBadRequest)
		return
	}
	isLast := up.offset+int64(len(chunk)) >= up.size
	if int64(len(chunk)) < sharePartMin && !isLast {
		http.Error(w, `{"error":"non-final parts must be >= 8MB"}`, http.StatusBadRequest)
		return
	}
	// security gate per chunk: magic-byte policy on the head chunk, ClamAV on
	// every chunk's plaintext before sealing (an infected chunk never persists)
	if up.offset == 0 {
		if err := contentPolicy(up.filename, chunk); err != nil {
			http.Error(w, fmt.Sprintf(`{"error":%q}`, err.Error()), http.StatusUnsupportedMediaType)
			return
		}
	}
	sig, ok := s.scanOrRefuse(w, bytes.NewReader(chunk), "chunk")
	if !ok {
		if sig != "" {
			s.logActivity(r.Context(), up.tenant, up.caseID, "MALWARE_BLOCKED",
				fmt.Sprintf("Resumable upload %q aborted — chunk tripped ClamAV signature %s", up.filename, sig))
			// abort the multipart so nothing is retained
			core := minio.Core{Client: s.docs.mc}
			_ = core.AbortMultipartUpload(r.Context(), docBucket, up.objectKey, up.minioUpID)
			_, _ = s.db.Exec(r.Context(), `UPDATE public.share_uploads SET status='ABORTED', updated_at=now() WHERE upload_id=$1`,
				chi.URLParam(r, "uploadId"))
		}
		return
	}
	// seal this part before it touches storage
	partNo := len(up.parts) + 1
	sealed, err := s.vaultSealDoc(r, up.tenant, fmt.Sprintf("%s#part-%d", up.objectKey, partNo), chunk)
	if err != nil {
		http.Error(w, `{"error":"seal failed"}`, http.StatusBadGateway)
		return
	}
	core := minio.Core{Client: s.docs.mc}
	part, err := core.PutObjectPart(r.Context(), docBucket, up.objectKey, up.minioUpID, partNo,
		bytes.NewReader(sealed), int64(len(sealed)), minio.PutObjectPartOptions{})
	if err != nil {
		http.Error(w, `{"error":"storage backend"}`, http.StatusBadGateway)
		return
	}
	up.parts = append(up.parts, partMeta{N: partNo, ETag: part.ETag, PlainBytes: int64(len(chunk)), SealedBytes: part.Size})
	raw, _ := json.Marshal(up.parts)
	newOff := up.offset + int64(len(chunk))
	if _, err := s.db.Exec(r.Context(), `
		UPDATE public.share_uploads SET bytes_rcvd=$2, parts=$3, updated_at=now()
		WHERE upload_id=$1 AND status='OPEN'`,
		chi.URLParam(r, "uploadId"), newOff, raw); err != nil {
		// part is stored in MinIO; client can retry PATCH at the same offset safely
		http.Error(w, `{"error":"db"}`, http.StatusInternalServerError)
		return
	}
	w.Header().Set("Upload-Offset", strconv.FormatInt(newOff, 10))
	writeJSON(w, http.StatusOK, map[string]any{"offset": newOff, "complete": newOff >= up.size})
}

// shareCompleteUpload finalizes: MinIO compose -> documents row with the part
// manifest -> the share token use is spent HERE (chunk PATCHes don't consume).
func (s *server) shareCompleteUpload(w http.ResponseWriter, r *http.Request) {
	up, _ := s.loadShareUpload(w, r)
	if up == nil {
		return
	}
	if up.offset != up.size {
		w.Header().Set("Upload-Offset", strconv.FormatInt(up.offset, 10))
		http.Error(w, `{"error":"incomplete — bytes missing"}`, http.StatusConflict)
		return
	}
	cparts := make([]minio.CompletePart, len(up.parts))
	for i, p := range up.parts {
		cparts[i] = minio.CompletePart{PartNumber: p.N, ETag: p.ETag}
	}
	core := minio.Core{Client: s.docs.mc}
	if _, err := core.CompleteMultipartUpload(r.Context(), docBucket, up.objectKey, up.minioUpID, cparts, minio.PutObjectOptions{}); err != nil {
		http.Error(w, `{"error":"compose failed"}`, http.StatusBadGateway)
		return
	}
	// assembled rescan: per-chunk scans run before sealing, but malware can
	// span chunk boundaries — stream the fully decrypted assembly through
	// ClamAV before the document becomes visible on the docket.
	if sig, err := s.scanMultipartAssembled(r, up.tenant, up.objectKey, up.parts); err != nil {
		http.Error(w, `{"error":"malware scanner unavailable — upload refused (fail-closed)"}`, http.StatusServiceUnavailable)
		return
	} else if sig != "" {
		_ = s.docs.mc.RemoveObject(r.Context(), docBucket, up.objectKey, minio.RemoveObjectOptions{})
		_, _ = s.db.Exec(r.Context(), `UPDATE public.share_uploads SET status='ABORTED', updated_at=now() WHERE upload_id=$1`,
			chi.URLParam(r, "uploadId"))
		s.logActivity(r.Context(), up.tenant, up.caseID, "MALWARE_BLOCKED",
			fmt.Sprintf("Assembled rescan of %q tripped ClamAV signature %s — object destroyed", up.filename, sig))
		http.Error(w, fmt.Sprintf(`{"error":"rejected: malware detected (%s)"}`, sig), http.StatusUnprocessableEntity)
		return
	}

	token := chi.URLParam(r, "token")
	g, err := s.consumeShare(r, token, "upload") // spend the use on completion
	if err != nil {
		http.Error(w, `{"error":"link expired or invalid"}`, http.StatusGone)
		return
	}
	s.bumpLinkUsage(r, token, up.size)
	docID := newUUID()
	raw, _ := json.Marshal(up.parts)
	var total int64
	for _, p := range up.parts {
		total += p.SealedBytes
	}
	if _, err := s.db.Exec(r.Context(), fmt.Sprintf(`
		INSERT INTO tenant_%s.documents
		  (id, case_id, object_key, size_bytes, content_type, sealed, uploaded_by, version, parts, scan_status, filename, folder)
		VALUES ($1,$2,$3,$4,'application/octet-stream',false,$5,1,$6,'CLEAN',$7,'PARTY_UPLOADS')`, sanitizeTenant(up.tenant)),
		docID, up.caseID, up.objectKey, up.size, "sharebox:"+token[:8], raw, up.filename); err != nil {
		http.Error(w, `{"error":"db"}`, http.StatusInternalServerError)
		return
	}
	_, _ = s.db.Exec(r.Context(),
		`UPDATE public.share_uploads SET status='COMPLETE', updated_at=now() WHERE upload_id=$1`,
		chi.URLParam(r, "uploadId"))
	_ = total // retained for storage accounting if needed
	s.publish(r.Context(), up.tenant, "documents", map[string]any{
		"type": "doc.uploaded", "tenant": up.tenant, "case_id": up.caseID, "doc_id": docID,
		"object_key": up.objectKey, "via": "sharebox-multipart", "at": time.Now().UTC(),
	})
	// Program rules (doc.upload) — block quarantines the assembled document.
	if blocked, msg := s.fireEventRules(r, up.tenant, "doc.upload", map[string]any{
		"case_id": up.caseID, "doc_id": docID, "folder": "PARTY_UPLOADS", "sealed": false,
		"size_bytes": int(up.size), "content_type": "application/octet-stream",
		"filename": up.filename, "uploaded_by": "sharebox:" + token[:8],
		"via": "sharebox-multipart", "tenant": up.tenant,
	}); blocked {
		_, _ = s.db.Exec(r.Context(), fmt.Sprintf(`
			UPDATE tenant_%s.documents SET analysis_status='BLOCKED'
			WHERE id=$1`, sanitizeTenant(up.tenant)), docID)
		s.logActivity(r.Context(), up.tenant, up.caseID, "RULE_BLOCKED",
			fmt.Sprintf("Party upload %q quarantined by program rule: %s", up.filename, msg))
	}
	s.logCorrespondence(r, up.tenant, up.caseID, "IN", "sharebox_upload",
		fmt.Sprintf("Party upload via secure link (resumable): %s", up.filename), "", nil, nil, "sharebox:"+token[:8])
	s.logActivity(r.Context(), up.tenant, up.caseID, "DOCUMENT_UPLOADED",
		fmt.Sprintf("%q (%d bytes, %d sealed parts) received via resumable secure link — analysis queued",
			up.filename, up.size, len(up.parts)))
	s.notify(r, up.tenant, "*", "DOC_RECEIVED",
		fmt.Sprintf("Document %q arrived via secure link on case %s", up.filename, up.caseID), "#/cases/"+up.caseID)
	writeJSON(w, http.StatusOK, map[string]any{"doc_id": docID, "bytes": up.size, "parts": len(up.parts),
		"token_uses_remaining": g.MaxUses - g.Uses})
}

// scanMultipartAssembled streams the decrypted plaintext of every part, in
// order, through one ClamAV INSTREAM session — catching signatures that span
// chunk boundaries. Memory stays flat (pipe + per-part fetch).
func (s *server) scanMultipartAssembled(r *http.Request, tenant, objectKey string, parts []partMeta) (string, error) {
	pr, pw := io.Pipe()
	go func() {
		var sealedStart int64
		for _, p := range parts {
			obj, err := s.docs.mc.GetObject(r.Context(), docBucket, objectKey, minio.GetObjectOptions{
				VersionID: "",
			})
			if err != nil {
				pw.CloseWithError(err)
				return
			}
			if _, err := obj.Seek(sealedStart, io.SeekStart); err != nil {
				obj.Close()
				pw.CloseWithError(err)
				return
			}
			ct := make([]byte, p.SealedBytes)
			if _, err := io.ReadFull(obj, ct); err != nil {
				obj.Close()
				pw.CloseWithError(err)
				return
			}
			obj.Close()
			sealedStart += p.SealedBytes
			pt, err := s.vaultOpenDoc(r, tenant, fmt.Sprintf("%s#part-%d", objectKey, p.N), ct)
			if err != nil {
				pw.CloseWithError(err)
				return
			}
			if _, err := pw.Write(pt); err != nil {
				return
			}
		}
		pw.Close()
	}()
	return s.clamScan(pr)
}

// ---- Streaming download with Range --------------------------------------------

// streamDocRange serves a document (single sealed blob or sealed multipart)
// honoring one byte Range. Ciphertext is read from MinIO per part and
// decrypted per part, so a 100MB file never buffers wholly in memory on
// range requests.
func (s *server) streamDocRange(w http.ResponseWriter, r *http.Request, tenant, objectKey string, parts []partMeta, plainTotal int64, name string) {
	if len(parts) == 0 {
		// single sealed blob: decrypt, then ServeContent gives us RFC 7233
		// Range handling (resume) natively.
		obj, err := s.docs.mc.GetObject(r.Context(), docBucket, objectKey, minio.GetObjectOptions{})
		if err != nil {
			http.Error(w, `{"error":"object missing"}`, http.StatusNotFound)
			return
		}
		defer obj.Close()
		ct, _ := io.ReadAll(obj)
		pt, err := s.vaultOpenDoc(r, tenant, objectKey, ct)
		if err != nil {
			http.Error(w, `{"error":"unseal failed"}`, http.StatusBadGateway)
			return
		}
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%q", name))
		http.ServeContent(w, r, name, time.Now(), bytes.NewReader(pt))
		return
	}

	var start, end int64 = 0, plainTotal - 1
	status := http.StatusOK
	if rh := r.Header.Get("Range"); rh != "" && strings.HasPrefix(rh, "bytes=") {
		spec := strings.TrimPrefix(rh, "bytes=")
		if i := strings.Index(spec, "-"); i >= 0 {
			if v, err := strconv.ParseInt(spec[:i], 10, 64); err == nil {
				start = v
			}
			if tail := spec[i+1:]; tail != "" {
				if v, err := strconv.ParseInt(tail, 10, 64); err == nil && v < end {
					end = v
				}
			}
			if start > end || start >= plainTotal {
				w.Header().Set("Content-Range", fmt.Sprintf("bytes */%d", plainTotal))
				http.Error(w, "range not satisfiable", http.StatusRequestedRangeNotSatisfiable)
				return
			}
			status = http.StatusPartialContent
		}
	}
	w.Header().Set("Accept-Ranges", "bytes")
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%q", name))
	w.Header().Set("Content-Length", strconv.FormatInt(end-start+1, 10))
	if status == http.StatusPartialContent {
		w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, plainTotal))
	}
	w.WriteHeader(status)

	// multipart: walk manifests, decrypt only overlapping parts
	var pos int64 // plaintext offset
	for _, p := range parts {
		pStart, pEnd := pos, pos+p.PlainBytes-1
		pos = pEnd + 1
		if pEnd < start {
			continue
		}
		if pStart > end {
			break
		}
		// fetch only this part's sealed bytes from the composed object
		var sealedStart int64
		for _, q := range parts {
			if q.N == p.N {
				break
			}
			sealedStart += q.SealedBytes
		}
		obj, err := s.docs.mc.GetObject(r.Context(), docBucket, objectKey, minio.GetObjectOptions{})
		if err != nil {
			return
		}
		if _, err := obj.Seek(sealedStart, io.SeekStart); err != nil {
			obj.Close()
			return
		}
		ct := make([]byte, p.SealedBytes)
		if _, err := io.ReadFull(obj, ct); err != nil {
			obj.Close()
			return
		}
		obj.Close()
		pt, err := s.vaultOpenDoc(r, tenant, fmt.Sprintf("%s#part-%d", objectKey, p.N), ct)
		if err != nil {
			return
		}
		lo, hi := int64(0), p.PlainBytes-1
		if pStart < start {
			lo = start - pStart
		}
		if pEnd > end {
			hi = end - pStart
		}
		if int64(len(pt)) <= hi {
			hi = int64(len(pt)) - 1
		}
		if _, err := w.Write(pt[lo : hi+1]); err != nil {
			return // client disconnected mid-stream — safe to abandon
		}
		if f, ok := w.(http.Flusher); ok {
			f.Flush() // progressive delivery: client sees bytes per part
		}
	}
}
