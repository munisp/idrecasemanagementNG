package main

// ShareBox — Dropbox/ShareFile-style party document exchange (G9).
//
// Staff mint a tokenized link per case (upload or download, expiry + use
// limits). The counterparty needs NO account: the token is the credential.
// GET  /api/share/{token}          -> self-contained landing page (HTML)
// POST /api/share/{token}/upload   -> multipart file, vault-sealed, MinIO,
//                                     documents row, doc-intel event, timeline
// GET  /api/share/{token}/download -> streams the linked document (unsealed only)
//
// Uses are consumed atomically (UPDATE … WHERE uses < max_uses RETURNING), so
// concurrent double-spend of a one-shot link is impossible.

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/minio/minio-go/v7"
)

type shareGrant struct {
	Token, Tenant, CaseID, Kind, ObjectKey string
	Uses, MaxUses                          int
	ExpiresAt                              time.Time
}

// consumeShare atomically validates + spends one use of a token.
func (s *server) consumeShare(r *http.Request, token, wantKind string) (*shareGrant, error) {
	var g shareGrant
	err := s.db.QueryRow(r.Context(), `
		UPDATE public.share_links SET uses = uses + 1
		WHERE token=$1 AND kind=$2 AND expires_at > now() AND uses < max_uses
		RETURNING token, tenant, case_id, kind, coalesce(object_key,''), uses, max_uses, expires_at`,
		token, wantKind).
		Scan(&g.Token, &g.Tenant, &g.CaseID, &g.Kind, &g.ObjectKey, &g.Uses, &g.MaxUses, &g.ExpiresAt)
	if err != nil {
		return nil, err
	}
	return &g, nil
}

// peekShare validates without consuming (for the landing page).
func (s *server) peekShare(r *http.Request, token string) (*shareGrant, error) {
	var g shareGrant
	err := s.db.QueryRow(r.Context(), `
		SELECT token, tenant, case_id, kind, coalesce(object_key,''), uses, max_uses, expires_at
		FROM public.share_links WHERE token=$1 AND expires_at > now() AND uses < max_uses`, token).
		Scan(&g.Token, &g.Tenant, &g.CaseID, &g.Kind, &g.ObjectKey, &g.Uses, &g.MaxUses, &g.ExpiresAt)
	if err != nil {
		return nil, err
	}
	return &g, nil
}

// shareLanding serves the public no-login page for a token.
func (s *server) shareLanding(w http.ResponseWriter, r *http.Request) {
	token := chi.URLParam(r, "token")
	g, err := s.peekShare(r, token)
	if err != nil {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.WriteHeader(http.StatusGone)
		fmt.Fprint(w, sharePage("Link expired or invalid", `<p class="muted">This secure link has expired, been fully used, or never existed. Contact your case coordinator for a new one.</p>`, ""))
		return
	}
	var caseNumber string
	_ = s.db.QueryRow(r.Context(),
		fmt.Sprintf(`SELECT case_number FROM tenant_%s.cases WHERE id=$1`, sanitizeTenant(g.Tenant)), g.CaseID).
		Scan(&caseNumber)

	var body string
	if g.Kind == "upload" {
		body = fmt.Sprintf(`
			<h1>Secure document upload</h1>
			<p class="muted">Case <b>%s</b> · link expires %s · %d of %d use(s) remaining</p>
			<input type="file" id="f" accept="image/*,application/pdf" capture="environment" />
			<p id="qual" class="muted"></p>
			<button id="go" disabled>Upload securely</button>
			<div id="bar" style="display:none;height:8px;background:#e5e7eb;border-radius:4px;margin:14px 0">
			  <div id="fill" style="height:8px;width:0%%;background:#123B2F;border-radius:4px"></div></div>
			<p id="msg" class="muted"></p>
			<p class="fine">Files are encrypted before storage and attached directly to the case docket. Large files upload in chunks and <b>resume automatically</b> if the connection drops. Do not upload documents for any other case.</p>
			<script>%s</script>`,
			escHTML(caseNumber), g.ExpiresAt.Format("Jan 2, 2006 15:04 MST"), g.MaxUses-g.Uses, g.MaxUses,
			resumableJS(token))
	} else {
		body = fmt.Sprintf(`
			<h1>Secure document download</h1>
			<p class="muted">Case <b>%s</b> · link expires %s · %d of %d use(s) remaining</p>
			<p><a class="btn" href="/api/share/%s/download">Download document</a></p>
			<p class="fine">This link grants access to one specific document. Downloads are logged to the case record.</p>`,
			escHTML(caseNumber), g.ExpiresAt.Format("Jan 2, 2006 15:04 MST"), g.MaxUses-g.Uses, g.MaxUses, escHTML(token))
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	fmt.Fprint(w, sharePage("Secure exchange", body, ""))
}

// shareUpload accepts a file over a token — no session required.
func (s *server) shareUpload(w http.ResponseWriter, r *http.Request) {
	token := chi.URLParam(r, "token")
	if !s.shareThrottle(w, r, token) {
		return
	}
	g, err := s.peekShare(r, token) // validate first; consume only after the file passes security
	if err != nil || g.Kind != "upload" {
		http.Error(w, `{"error":"link expired or invalid"}`, http.StatusGone)
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
	if !s.checkLinkBudgets(w, r, token, int64(len(raw))) {
		return
	}
	// security gate: content policy + ClamAV, BEFORE anything is sealed/stored
	if err := contentPolicy(hdr.Filename, raw); err != nil {
		http.Error(w, fmt.Sprintf(`{"error":%q}`, err.Error()), http.StatusUnsupportedMediaType)
		return
	}
	sig, ok := s.scanOrRefuse(w, bytes.NewReader(raw), "file")
	if !ok {
		if sig != "" {
			s.logActivity(r.Context(), g.Tenant, g.CaseID, "MALWARE_BLOCKED",
				fmt.Sprintf("ShareBox upload %q rejected — ClamAV signature %s; nothing stored", hdr.Filename, sig))
		}
		return
	}
	if _, err := s.consumeShare(r, token, "upload"); err != nil {
		http.Error(w, `{"error":"link expired or invalid"}`, http.StatusGone)
		return
	}
	s.bumpLinkUsage(r, token, int64(len(raw)))

	docID := newUUID()
	objectKey := fmt.Sprintf("%s/cases/%s/%s.enc", g.Tenant, g.CaseID, docID)
	ct, err := s.vaultSealDoc(r, g.Tenant, objectKey, raw)
	if err != nil {
		http.Error(w, `{"error":"seal failed"}`, http.StatusBadGateway)
		return
	}
	if _, err = s.docs.mc.PutObject(r.Context(), docBucket, objectKey,
		bytes.NewReader(ct), int64(len(ct)), minio.PutObjectOptions{
			ContentType:  "application/octet-stream",
			UserMetadata: map[string]string{"tenant": g.Tenant, "case": g.CaseID, "via": "sharebox"},
		}); err != nil {
		http.Error(w, `{"error":"store failed"}`, http.StatusBadGateway)
		return
	}
	actor := "sharebox:" + token[:8]
	if _, err := s.db.Exec(r.Context(), fmt.Sprintf(`
		INSERT INTO tenant_%s.documents
		  (id, case_id, object_key, size_bytes, content_type, sealed, uploaded_by, version, scan_status, filename, folder)
		VALUES ($1,$2,$3,$4,$5,false,$6,1,'CLEAN',$7,'PARTY_UPLOADS')`, sanitizeTenant(g.Tenant)),
		docID, g.CaseID, objectKey, len(raw), hdr.Header.Get("Content-Type"), actor, hdr.Filename); err != nil {
		http.Error(w, `{"error":"db"}`, http.StatusInternalServerError)
		return
	}
	s.publish(r.Context(), g.Tenant, "documents", map[string]any{
		"type": "doc.uploaded", "tenant": g.Tenant, "case_id": g.CaseID,
		"doc_id": docID, "object_key": objectKey, "content_type": hdr.Header.Get("Content-Type"),
		"sealed": false, "via": "sharebox", "at": time.Now().UTC(),
	})
	// Program rules (doc.upload) — party uploads are the highest-risk path,
	// so rule enforcement matters most here. Block = quarantine, not delete.
	if blocked, msg := s.fireEventRules(r, g.Tenant, "doc.upload", map[string]any{
		"case_id": g.CaseID, "doc_id": docID, "folder": "PARTY_UPLOADS", "sealed": false,
		"size_bytes": len(raw), "content_type": hdr.Header.Get("Content-Type"),
		"filename": hdr.Filename, "uploaded_by": actor, "via": "sharebox", "tenant": g.Tenant,
	}); blocked {
		_, _ = s.db.Exec(r.Context(), fmt.Sprintf(`
			UPDATE tenant_%s.documents SET analysis_status='BLOCKED'
			WHERE id=$1`, sanitizeTenant(g.Tenant)), docID)
		s.logActivity(r.Context(), g.Tenant, g.CaseID, "RULE_BLOCKED",
			fmt.Sprintf("Party upload %q quarantined by program rule: %s", hdr.Filename, msg))
	}
	s.logCorrespondence(r, g.Tenant, g.CaseID, "IN", "sharebox_upload",
		fmt.Sprintf("Party upload via secure link: %s", hdr.Filename), "", nil, nil, actor)
	s.logActivity(r.Context(), g.Tenant, g.CaseID, "DOCUMENT_UPLOADED",
		fmt.Sprintf("%q (%d bytes) received via secure upload link — analysis queued", hdr.Filename, len(raw)))
	s.notify(r, g.Tenant, "*", "DOC_RECEIVED",
		fmt.Sprintf("Document %q arrived via secure link on case %s", hdr.Filename, g.CaseID), "#/cases/"+g.CaseID)

	if strings.Contains(r.Header.Get("Accept"), "text/html") {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		fmt.Fprint(w, sharePage("Upload complete",
			fmt.Sprintf(`<h1>Upload complete</h1><p class="muted">%s was securely delivered to the case docket. You may close this window.</p>`, escHTML(hdr.Filename)), ""))
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"doc_id": docID, "bytes": len(raw)})
}

// shareDownload streams the document a download token points at.
func (s *server) shareDownload(w http.ResponseWriter, r *http.Request) {
	token := chi.URLParam(r, "token")
	if !s.shareThrottle(w, r, token) {
		return
	}
	// resolve target before spending the use
	g, err := s.peekShare(r, token)
	if err != nil || g.Kind != "download" {
		http.Error(w, `{"error":"link expired or invalid"}`, http.StatusGone)
		return
	}
	objectKey := g.ObjectKey
	if objectKey == "" {
		// default: newest unsealed document on the case
		_ = s.db.QueryRow(r.Context(), fmt.Sprintf(`
			SELECT object_key FROM tenant_%s.documents
			WHERE case_id=$1 AND sealed=false ORDER BY created_at DESC LIMIT 1`, sanitizeTenant(g.Tenant)),
			g.CaseID).Scan(&objectKey)
	}
	if objectKey == "" {
		http.Error(w, `{"error":"no document attached to this link"}`, http.StatusNotFound)
		return
	}
	// sealed documents never leave via sharebox
	var sealed bool
	_ = s.db.QueryRow(r.Context(), fmt.Sprintf(`
		SELECT sealed FROM tenant_%s.documents WHERE object_key=$1`, sanitizeTenant(g.Tenant)),
		objectKey).Scan(&sealed)
	if sealed {
		http.Error(w, `{"error":"document is sealed"}`, http.StatusForbidden)
		return
	}
	if _, err := s.consumeShare(r, token, "download"); err != nil {
		http.Error(w, `{"error":"link expired or invalid"}`, http.StatusGone)
		return
	}
	// load part manifest (sealed multipart) if present
	var partsRaw []byte
	var plainSize int64
	_ = s.db.QueryRow(r.Context(), fmt.Sprintf(`
		SELECT coalesce(parts::text,''), coalesce(size_bytes,0) FROM tenant_%s.documents WHERE object_key=$1`,
		sanitizeTenant(g.Tenant)), objectKey).Scan(&partsRaw, &plainSize)
	var parts []partMeta
	_ = json.Unmarshal(partsRaw, &parts)
	name := strings.TrimSuffix(objectKey[strings.LastIndex(objectKey, "/")+1:], ".enc")
	s.logCorrespondence(r, g.Tenant, g.CaseID, "OUT", "sharebox_download",
		fmt.Sprintf("Document downloaded via secure link: %s", name), "", nil, nil, "sharebox:"+token[:8])
	s.logActivity(r.Context(), g.Tenant, g.CaseID, "SHARE_DOWNLOAD",
		fmt.Sprintf("Document %q downloaded via secure link (use %d/%d)", name, g.Uses+1, g.MaxUses))
	s.streamDocRange(w, r, g.Tenant, objectKey, parts, plainSize, name)
}

// sharePage is the minimal public chrome for the no-login exchange.
func sharePage(title, body, extra string) string {
	return `<!doctype html><html lang="en"><head><meta charset="utf-8">
<meta name="viewport" content="width=device-width,initial-scale=1">
<title>` + escHTML(title) + ` — Secure Document Exchange</title>
<style>
:root{color-scheme:light dark}
body{font-family:system-ui,-apple-system,sans-serif;max-width:520px;margin:8vh auto;padding:0 20px;line-height:1.5}
h1{font-size:22px}form{margin:24px 0}
input[type=file]{display:block;margin:12px 0}
button,.btn{display:inline-block;background:#123B2F;color:#fff;border:0;border-radius:8px;padding:12px 22px;font-size:15px;cursor:pointer;text-decoration:none}
.muted{color:#6b7280}.fine{font-size:12px;color:#9ca3af;margin-top:28px;border-top:1px solid #e5e7eb;padding-top:14px}
</style></head><body>` + body + extra + `
<p class="fine">NSA IDRE Platform · secure, logged, single-purpose link — no account required.</p>
</body></html>`
}

func escHTML(s string) string {
	r := strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;", `"`, "&quot;")
	return r.Replace(s)
}

// resumableJS is the landing-page uploader: small files use the one-shot
// endpoint; anything larger goes through the chunked protocol with HEAD-probe
// resume and per-chunk retry. No libraries, no accounts.
func resumableJS(token string) string {
	return `(function(){
var T = ` + "`" + token + "`" + `, CHUNK = 8*1024*1024, MAX = 100*1024*1024;
var f = document.getElementById("f"), go = document.getElementById("go"),
    fill = document.getElementById("fill"), bar = document.getElementById("bar"),
    msg = document.getElementById("msg");
f.onchange = function(){
  go.disabled = !f.files.length; msg.textContent=""; qualCheck(f.files[0]);
};
// Capture guidance: grade photos BEFORE upload (blur via Laplacian variance
// on a downscaled canvas, brightness, resolution). Warns, never blocks — the
// server-side gate makes the final call. No libraries; nothing leaves the
// browser.
var qual = document.getElementById("qual");
function qualCheck(file){
  qual.textContent = "";
  if (!file || !/^image\//.test(file.type)) return;
  var url = URL.createObjectURL(file), im = new Image();
  im.onload = function(){
    try {
      var W = 320, sc = Math.min(1, W / im.width), H = Math.max(1, Math.round(im.height * sc));
      var cv = document.createElement("canvas"); cv.width = W; cv.height = H;
      var cx = cv.getContext("2d", { willReadFrequently: true });
      cx.drawImage(im, 0, 0, W, H);
      var d = cx.getImageData(0, 0, W, H).data, g = new Float32Array(W * H), sum = 0;
      for (var i = 0; i < W * H; i++){ var y = (d[4*i]*0.299 + d[4*i+1]*0.587 + d[4*i+2]*0.114); g[i] = y; sum += y; }
      var lap = 0, n = 0;
      for (var y2 = 1; y2 < H - 1; y2++) for (var x2 = 1; x2 < W - 1; x2++){
        var v = 4*g[y2*W+x2] - g[y2*W+x2-1] - g[y2*W+x2+1] - g[(y2-1)*W+x2] - g[(y2+1)*W+x2];
        lap += v*v; n++;
      }
      var blur = lap / Math.max(1, n), bright = sum / (W * H), mp = im.width * im.height / 1e6;
      var warn = [];
      if (blur < 60) warn.push("the photo looks blurry — hold steady and tap to focus");
      if (bright < 80) warn.push("the photo looks dark — more light will help");
      if (mp < 0.3) warn.push("resolution is low — move closer to the document");
      if (im.width < im.height) warn.push("checks scan best in landscape (sideways)");
      qual.textContent = warn.length
        ? "Tip: " + warn.join("; ") + ". You can still upload, but a clearer photo processes faster."
        : "Photo quality looks good.";
    } catch(e) {}
    URL.revokeObjectURL(url);
  };
  im.src = url;
}
go.onclick = async function(){
  var file = f.files[0]; if(!file) return;
  if(file.size > MAX){ msg.textContent = "File exceeds the 100MB limit."; return; }
  go.disabled = true; bar.style.display = "block";
  try {
    if (file.size <= CHUNK) {
      var fd = new FormData(); fd.append("file", file);
      var r0 = await fetch("/api/share/"+T+"/upload", {method:"POST", body:fd});
      if(!r0.ok) throw new Error("upload failed ("+r0.status+")");
    } else {
      var cr = await (await fetch("/api/share/"+T+"/uploads", {method:"POST",
        headers:{"Content-Type":"application/json"},
        body: JSON.stringify({filename:file.name, size:file.size})})).json();
      var up = cr.upload_id;
      var off = 0;
      while (off < file.size) {
        // probe first: resumes correctly after a dropped connection
        var hd = await fetch("/api/share/"+T+"/uploads/"+up, {method:"HEAD"});
        off = parseInt(hd.headers.get("Upload-Offset")||"0",10);
        var done = false;
        while (!done) {
          var chunk = file.slice(off, Math.min(off+CHUNK, file.size));
          var rp = await fetch("/api/share/"+T+"/uploads/"+up, {method:"PATCH",
            headers:{"Upload-Offset": String(off), "Content-Type":"application/octet-stream"}, body: chunk});
          if (rp.status === 409) { // offset mismatch — re-probe and continue
            hd = await fetch("/api/share/"+T+"/uploads/"+up, {method:"HEAD"});
            off = parseInt(hd.headers.get("Upload-Offset")||"0",10);
            continue;
          }
          if (!rp.ok) throw new Error("chunk failed ("+rp.status+") — retrying");
          var jr = await rp.json(); off = jr.offset; done = true;
          fill.style.width = Math.round(100*off/file.size)+"%";
          msg.textContent = "Uploaded "+Math.round(off/1048576)+" of "+Math.round(file.size/1048576)+" MB";
        }
      }
      var fin = await fetch("/api/share/"+T+"/uploads/"+up+"/complete", {method:"POST"});
      if(!fin.ok) throw new Error("finalize failed ("+fin.status+")");
    }
    fill.style.width = "100%";
    msg.innerHTML = "<b>Upload complete.</b> You may close this window.";
    f.disabled = true;
  } catch(e){ msg.textContent = e.message + " — click Upload to resume."; go.disabled = false; }
};
})();`
}
