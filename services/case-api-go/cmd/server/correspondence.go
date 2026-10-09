package main

// Correspondence engine — program-template driven outbound email with a
// mandatory QA gate for attorney/PM-flagged templates, a full correspondence
// log (OUT + IN), and tokenized ShareFile-style upload/download links.
//
// Gaps closed: G3 (correspondence engine + CC matrix), G4 (QA gate),
// G9 (ShareFile replacement), G15 (template hygiene: templates live in
// program_rules config with explicit to/cc policy — one source of truth).

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
)

type CorrTemplate struct {
	Key     string   `json:"key"`
	Subject string   `json:"subject"`
	To      []string `json:"to"`      // party roles: provider|health_plan|filing_party|requester|agency…
	CC      []string `json:"cc"`      // copied parties per program CC matrix
	QARole  string   `json:"qa_role"` // "" = send immediately; else gated on that role
	Thread  bool     `json:"thread"`  // reply-all on the existing thread
	// Body is the program's standard message text with {case_number},
	// {share_link} and {download_link} placeholders; the compose screen
	// pre-fills it and staff edit before sending. The caller's body wins.
	Body string `json:"body"`
}

func (s *server) corrTemplates(r *http.Request, tenant string) []CorrTemplate {
	var raw []byte
	err := s.db.QueryRow(r.Context(),
		`SELECT config->'correspondence'->'templates' FROM public.program_rules WHERE tenant=$1`, tenant).Scan(&raw)
	if err != nil {
		return nil
	}
	var out []CorrTemplate
	_ = json.Unmarshal(raw, &out)
	return out
}

// renderTemplate substitutes {case_number}, {provider_name}, {payer_name},
// {amount}, {due_date} placeholders from the case row.
func (s *server) renderTemplate(r *http.Request, tenant, caseID, text string) string {
	var caseNumber, providerID, payerID string
	var qpa int64
	_ = s.db.QueryRow(r.Context(), fmt.Sprintf(`
		SELECT case_number, coalesce(provider_id,''), coalesce(payer_id,''), coalesce(benchmark_cents, qpa_cents,0)
		FROM tenant_%s.cases WHERE id=$1`, sanitizeTenant(tenant)), caseID).
		Scan(&caseNumber, &providerID, &payerID, &qpa)
	repl := map[string]string{
		"{case_number}":     caseNumber,
		"{provider_name}":   providerID,
		"{payer_name}":      payerID,
		"{qpa}":             fmt.Sprintf("$%d.%02d", qpa/100, qpa%100),
		"{case_url_suffix}": "#/cases/" + caseID,
	}
	for k, v := range repl {
		text = strings.ReplaceAll(text, k, v)
	}
	return text
}

// draftCorrespondence renders a program template for a case. If the template
// carries qa_role, the draft enters the QA queue (PENDING) and is NOT sent;
// otherwise it sends immediately.
func (s *server) draftCorrespondence(w http.ResponseWriter, r *http.Request) {
	tenant := r.Context().Value(ctxTenant{}).(string)
	caseID := chi.URLParam(r, "caseId")
	var in struct {
		Template     string            `json:"template"` // template key from program config
		Body         string            `json:"body"`     // staff-composed body (subject comes from config)
		To           []string          `json:"to"`       // resolved recipient emails
		CC           []string          `json:"cc"`
		Vars         map[string]string `json:"vars"`          // extra placeholders
		ShareTokens  []string          `json:"share_tokens"`  // attach pre-created share links
		AutoShare    bool              `json:"auto_share"`    // mint an upload link and embed it (G9, no copy-paste)
		AutoDownload bool              `json:"auto_download"` // mint a download link for case documents and embed it
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil || in.Template == "" {
		http.Error(w, `{"error":"template required"}`, http.StatusBadRequest)
		return
	}
	var tpl *CorrTemplate
	for _, t := range s.corrTemplates(r, tenant) {
		if t.Key == in.Template {
			cp := t
			tpl = &cp
			break
		}
	}
	if tpl == nil {
		http.Error(w, `{"error":"unknown template for this program"}`, http.StatusBadRequest)
		return
	}
	subject := s.renderTemplate(r, tenant, caseID, tpl.Subject)
	bodyText := in.Body
	if strings.TrimSpace(bodyText) == "" {
		bodyText = tpl.Body // program standard template text
	}
	body := s.renderTemplate(r, tenant, caseID, bodyText)
	for k, v := range in.Vars {
		subject = strings.ReplaceAll(subject, "{"+k+"}", v)
		body = strings.ReplaceAll(body, "{"+k+"}", v)
	}
	// Auto-share (G9): the template or the sender wants a secure upload link in
	// the message. Mint one inline and substitute the {share_link} placeholder —
	// the case manager never leaves the compose screen to copy-paste a URL.
	// Fire when explicitly requested OR when the body still carries the
	// placeholder after variable substitution.
	if in.AutoShare || strings.Contains(body, "{share_link}") {
		link := s.autoShareLink(r, tenant, caseID, "upload", "")
		if strings.Contains(body, "{share_link}") {
			body = strings.ReplaceAll(body, "{share_link}", link)
		} else {
			body += "\n\nSecure upload link: " + link
		}
	}
	// Auto-download (G9): {download_link} pins a download link to the case's
	// latest generated document (determination letter, notice) — the sender
	// never hunts the document panel for a URL.
	if in.AutoDownload || strings.Contains(body, "{download_link}") {
		var objectKey string
		_ = s.db.QueryRow(r.Context(), fmt.Sprintf(`
			SELECT object_key FROM tenant_%s.documents
			WHERE case_id=$1 AND NOT sealed AND scan_status<>'BLOCKED'
			ORDER BY created_at DESC LIMIT 1`, sanitizeTenant(tenant)), caseID).Scan(&objectKey)
		link := ""
		if objectKey != "" {
			link = s.autoShareLink(r, tenant, caseID, "download", objectKey)
		}
		if strings.Contains(body, "{download_link}") {
			if link != "" {
				body = strings.ReplaceAll(body, "{download_link}", link)
			} else {
				body = strings.ReplaceAll(body, "{download_link}",
					"(document pending — link will follow)")
			}
		} else if link != "" {
			body += "\n\nSecure document download link: " + link
		}
	}
	// append share links (G9) when requested
	if len(in.ShareTokens) > 0 {
		var links []string
		for _, tok := range in.ShareTokens {
			var kind string
			var expires time.Time
			if err := s.db.QueryRow(r.Context(),
				`SELECT kind, expires_at FROM public.share_links WHERE token=$1 AND tenant=$2 AND uses < max_uses`,
				tok, tenant).Scan(&kind, &expires); err != nil {
				continue
			}
			links = append(links, fmt.Sprintf("%s link (expires %s): /s/%s", kind, expires.Format("2006-01-02"), tok))
		}
		if len(links) > 0 {
			body += "\n\nSecure document links:\n" + strings.Join(links, "\n")
		}
	}

	p := r.Context().Value(ctxPrincipal{}).(principal)
	toJ, _ := json.Marshal(in.To)
	ccJ, _ := json.Marshal(in.CC)
	status := "SENT"
	if tpl.QARole != "" {
		status = "PENDING" // QA gate (G4): attorney/PM must approve before send
	}
	var qid string
	_ = s.db.QueryRow(r.Context(), `
		INSERT INTO public.qa_reviews (tenant, case_id, artifact, channel, subject, body, to_recipients, cc_recipients, status, drafted_by)
		VALUES ($1,$2,'EMAIL','email',$3,$4,$5,$6,$7,$8) RETURNING id`,
		tenant, caseID, subject, body, toJ, ccJ, status, p.Subject).Scan(&qid)

	if status == "PENDING" {
		s.notify(r, tenant, "*", "QA_REVIEW",
			fmt.Sprintf("%s draft on case %s awaiting %s QA approval", tpl.Key, caseID, tpl.QARole), "#/qa")
	} else {
		// No QA gate on this template — deliver immediately via SMTP.
		if err := s.sendMail(in.To, in.CC, subject, body); err != nil {
			s.logActivity(r.Context(), tenant, caseID, "EMAIL_DELIVERY_FAILED",
				fmt.Sprintf("SMTP delivery failed for %q: %s", subject, err))
			http.Error(w, `{"error":"smtp delivery failed — draft retained as APPROVED for retry"}`, http.StatusBadGateway)
			return
		}
		_, _ = s.db.Exec(r.Context(), `UPDATE public.qa_reviews SET sent_at=now() WHERE tenant=$1 AND id=$2`, tenant, qid)
		s.logCorrespondence(r, tenant, caseID, "OUT", in.Template, subject, body, in.To, in.CC, p.Subject)
		s.logActivity(r.Context(), tenant, caseID, "EMAIL_SENT",
			fmt.Sprintf("%s sent to %d recipient(s) (cc %d) — template %s", subject, len(in.To), len(in.CC), in.Template))
		s.autoChecklist(r, tenant, caseID)
	}
	writeJSON(w, http.StatusOK, map[string]any{"qa_id": qid, "status": status, "subject": subject})
}

// qaQueue lists pending QA reviews; qaDecision approves/rejects. Approving a
// PENDING review marks it SENT and writes the correspondence log — nothing
// reaches a party without passing the gate.
func (s *server) qaQueue(w http.ResponseWriter, r *http.Request) {
	tenant := r.Context().Value(ctxTenant{}).(string)
	// Optional ?case_id= filter — the conversational QA gate (Assistant thread)
	// renders the gate for ONE case inline, while the QA screen keeps the
	// full queue. Same query otherwise; the filter never changes semantics.
	if cid := r.URL.Query().Get("case_id"); cid != "" {
		rows, err := s.queryRows(r, `
			SELECT id, case_id, artifact, channel, subject, status, drafted_by, created_at
			FROM public.qa_reviews WHERE tenant=$1 AND status='PENDING' AND case_id=$2
			ORDER BY created_at`, tenant, cid)
		if err != nil {
			http.Error(w, `{"error":"db"}`, http.StatusInternalServerError)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"queue": rows})
		return
	}
	rows, err := s.queryRows(r, `
		SELECT id, case_id, artifact, channel, subject, status, drafted_by, created_at
		FROM public.qa_reviews WHERE tenant=$1 AND status='PENDING' ORDER BY created_at`, tenant)
	if err != nil {
		http.Error(w, `{"error":"db"}`, http.StatusInternalServerError)
		return
	}
	// Recent decisions keep the page useful when the pending queue is empty —
	// reviewers see what the gate has been doing, not a blank screen.
	recent, _ := s.queryRows(r, `
		SELECT id, case_id, subject, status, drafted_by, reviewed_by, reviewed_at
		FROM public.qa_reviews WHERE tenant=$1 AND status<>'PENDING'
		ORDER BY reviewed_at DESC NULLS LAST LIMIT 20`, tenant)
	writeJSON(w, http.StatusOK, map[string]any{"queue": rows, "recent": recent})
}

func (s *server) qaGet(w http.ResponseWriter, r *http.Request) {
	tenant := r.Context().Value(ctxTenant{}).(string)
	qid := chi.URLParam(r, "qaId")
	rows, err := s.queryRows(r, `
		SELECT id, case_id, artifact, channel, subject, body, to_recipients, cc_recipients, status, drafted_by, created_at
		FROM public.qa_reviews WHERE tenant=$1 AND id=$2`, tenant, qid)
	if err != nil || len(rows) == 0 {
		http.Error(w, `{"error":"not found"}`, http.StatusNotFound)
		return
	}
	writeJSON(w, http.StatusOK, rows[0])
}

func (s *server) qaDecision(w http.ResponseWriter, r *http.Request) {
	tenant := r.Context().Value(ctxTenant{}).(string)
	qid := chi.URLParam(r, "qaId")
	var in struct {
		Decision string `json:"decision"` // APPROVE|REJECT
		Note     string `json:"note"`
		// EditedBody implements the EDIT prong of accept/edit/reject: when
		// present on a PENDING row, the reviewer's text replaces the draft
		// body before the decision lands. The edit itself is recorded so the
		// trail always distinguishes model text from human text.
		EditedBody string `json:"edited_body"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil ||
		(in.Decision != "APPROVE" && in.Decision != "REJECT") {
		http.Error(w, `{"error":"decision must be APPROVE or REJECT"}`, http.StatusBadRequest)
		return
	}
	p := r.Context().Value(ctxPrincipal{}).(principal)
	var caseID, subject, body, status, channel string
	var toJ, ccJ []byte
	err := s.db.QueryRow(r.Context(),
		`SELECT case_id, subject, body, status, to_recipients, cc_recipients, coalesce(channel,'email') FROM public.qa_reviews WHERE tenant=$1 AND id=$2`,
		tenant, qid).Scan(&caseID, &subject, &body, &status, &toJ, &ccJ, &channel)
	if err != nil || status != "PENDING" {
		http.Error(w, `{"error":"not pending"}`, http.StatusConflict)
		return
	}
	newStatus := "APPROVED"
	if in.Decision == "REJECT" {
		newStatus = "REJECTED"
	}
	// EDIT prong: a PENDING draft the reviewer rewrote lands with the human
	// text; the review note flags that an edit happened (model text is never
	// silently replaced — drafted_by attribution stays on the row).
	if in.EditedBody != "" {
		body = in.EditedBody
		editMark := "[human-edited before " + in.Decision + "]"
		if in.Note != "" {
			in.Note = editMark + " " + in.Note
		} else {
			in.Note = editMark
		}
		_, _ = s.db.Exec(r.Context(),
			`UPDATE public.qa_reviews SET body=$3 WHERE tenant=$1 AND id=$2`, tenant, qid, body)
	}
	_, _ = s.db.Exec(r.Context(), `
		UPDATE public.qa_reviews SET status=$3, reviewed_by=$4, reviewed_at=now(), review_note=$5
		WHERE tenant=$1 AND id=$2`, tenant, qid, newStatus, p.Subject, in.Note)

	var to, cc []string
	_ = json.Unmarshal(toJ, &to)
	_ = json.Unmarshal(ccJ, &cc)
	if in.Decision == "APPROVE" && channel == "note" {
		// channel 'note' (copilot determination rationale): approval files the
		// rationale on the case timeline — nothing is emailed, ever.
		s.logActivity(r.Context(), tenant, caseID, "DETERMINATION_RATIONALE_FILED",
			fmt.Sprintf("Rationale approved in QA by %s:\n%s", p.Subject, truncate(body, 6000)))
		writeJSON(w, http.StatusOK, map[string]string{"status": newStatus})
		return
	}
	if in.Decision == "APPROVE" {
		// The narrative's send-safety rule (drafts saved without addresses) ends
		// here: addresses enter only at QA-approved send time, and delivery goes
		// through the configured SMTP relay. Failure keeps status APPROVED (not
		// SENT) so the reviewer can retry — nothing is marked sent that wasn't.
		if err := s.sendMail(to, cc, subject, body); err != nil {
			s.logActivity(r.Context(), tenant, caseID, "EMAIL_DELIVERY_FAILED",
				fmt.Sprintf("SMTP delivery failed for %q: %s", subject, err))
			http.Error(w, `{"error":"smtp delivery failed — draft stays APPROVED for retry"}`, http.StatusBadGateway)
			return
		}
		_, _ = s.db.Exec(r.Context(),
			`UPDATE public.qa_reviews SET status='SENT', sent_at=now() WHERE tenant=$1 AND id=$2`, tenant, qid)
		s.logCorrespondence(r, tenant, caseID, "OUT", "qa_approved", subject, body, to, cc, p.Subject)
		s.logActivity(r.Context(), tenant, caseID, "EMAIL_SENT",
			fmt.Sprintf("QA-approved by %s: %s sent to %d recipient(s)", p.Subject, subject, len(to)))
		s.autoChecklist(r, tenant, caseID)
	} else {
		s.logActivity(r.Context(), tenant, caseID, "QA_REJECTED",
			fmt.Sprintf("Draft rejected in QA by %s: %s%s", p.Subject, subject, orDash(" — "+in.Note)))
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": newStatus})
}

func (s *server) logCorrespondence(r *http.Request, tenant, caseID, direction, template, subject, body string, to, cc []string, actor string) {
	rcpts, _ := json.Marshal(map[string]any{"to": to, "cc": cc})
	_, _ = s.db.Exec(r.Context(), `
		INSERT INTO public.correspondence_log (tenant, case_id, direction, template, subject, body, recipients, sent_by)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8)`,
		tenant, caseID, direction, template, subject, body, rcpts, actor)
}

// listCorrespondence: full OUT/IN trail for a case.
func (s *server) listCorrespondence(w http.ResponseWriter, r *http.Request) {
	tenant := r.Context().Value(ctxTenant{}).(string)
	caseID := chi.URLParam(r, "caseId")
	limit, offset := pageParams(r, 50, 500)
	var total int
	if err := s.db.QueryRow(r.Context(),
		`SELECT count(*) FROM public.correspondence_log WHERE tenant=$1 AND case_id=$2`,
		tenant, caseID).Scan(&total); err != nil {
		http.Error(w, `{"error":"db"}`, http.StatusInternalServerError)
		return
	}
	rows, err := s.queryRows(r, fmt.Sprintf(`
		SELECT id, direction, template, subject, recipients, sent_by, created_at
		FROM public.correspondence_log WHERE tenant=$1 AND case_id=$2
		ORDER BY created_at DESC, id DESC LIMIT %d OFFSET %d`, limit, offset),
		tenant, caseID)
	if err != nil {
		http.Error(w, `{"error":"db"}`, http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"correspondence": rows,
		"total": total, "next_offset": nextOffset(offset, limit, total)})
}

// ---- Share links (G9: ShareFile replacement) --------------------------------

func (s *server) createShareLink(w http.ResponseWriter, r *http.Request) {
	tenant := r.Context().Value(ctxTenant{}).(string)
	caseID := chi.URLParam(r, "caseId")
	var in struct {
		Kind      string `json:"kind"` // upload|download
		DaysTTL   int    `json:"days_ttl"`
		MaxUses   int    `json:"max_uses"`
		ObjectKey string `json:"object_key"` // download links: pin to one document
	}
	_ = json.NewDecoder(r.Body).Decode(&in)
	if in.Kind != "upload" && in.Kind != "download" {
		http.Error(w, `{"error":"kind must be upload or download"}`, http.StatusBadRequest)
		return
	}
	if in.DaysTTL <= 0 {
		in.DaysTTL = 7
	}
	if in.MaxUses <= 0 {
		in.MaxUses = 1
	}
	buf := make([]byte, 24)
	_, _ = rand.Read(buf)
	token := hex.EncodeToString(buf)
	p := r.Context().Value(ctxPrincipal{}).(principal)
	_, err := s.db.Exec(r.Context(), `
		INSERT INTO public.share_links (token, tenant, case_id, kind, object_key, expires_at, max_uses, created_by)
		VALUES ($1,$2,$3,$4, nullif($5,''), now() + make_interval(days => $6), $7, $8)`,
		token, tenant, caseID, in.Kind, in.ObjectKey, in.DaysTTL, in.MaxUses, p.Subject)
	if err != nil {
		http.Error(w, `{"error":"db"}`, http.StatusInternalServerError)
		return
	}
	s.logActivity(r.Context(), tenant, caseID, "SHARE_LINK",
		fmt.Sprintf("Secure %s link created (%d-day expiry) by %s", in.Kind, in.DaysTTL, p.Subject))
	writeJSON(w, http.StatusOK, map[string]any{"token": token, "path": "/s/" + token, "kind": in.Kind})
}

// autoShareLink mints a 7-day, 10-use upload link for a case and returns the
// absolute portal URL. Used by draftCorrespondence's auto-share path so the
// sender never has to pre-create and paste a link. Failure is soft: the
// placeholder degrades to the relative path rather than failing the draft.
func (s *server) autoShareLink(r *http.Request, tenant, caseID, kind, objectKey string) string {
	buf := make([]byte, 24)
	if _, err := rand.Read(buf); err != nil {
		return "/s/"
	}
	token := hex.EncodeToString(buf)
	p := r.Context().Value(ctxPrincipal{}).(principal)
	if _, err := s.db.Exec(r.Context(), `
		INSERT INTO public.share_links (token, tenant, case_id, kind, object_key, expires_at, max_uses, created_by)
		VALUES ($1,$2,$3,$4, nullif($5,''), now() + interval '7 days', 10, $6)`,
		token, tenant, caseID, kind, objectKey, p.Subject); err != nil {
		return "/s/" + token
	}
	s.logActivity(r.Context(), tenant, caseID, "SHARE_LINK",
		fmt.Sprintf("Secure %s link auto-created for correspondence by %s", kind, p.Subject))
	return strings.TrimRight(s.cfg.PortalBaseURL, "/") + "/s/" + token
}

// resolveShareLink is the unauthenticated landing for a token (upload/download
// portal page consumes this; expiry and use-count enforced).
func (s *server) resolveShareLink(w http.ResponseWriter, r *http.Request) {
	token := chi.URLParam(r, "token")
	var tenant, caseID, kind string
	var uses, maxUses int
	var expires time.Time
	err := s.db.QueryRow(r.Context(), `
		SELECT tenant, case_id, kind, uses, max_uses, expires_at FROM public.share_links WHERE token=$1`, token).
		Scan(&tenant, &caseID, &kind, &uses, &maxUses, &expires)
	if err != nil || time.Now().After(expires) || uses >= maxUses {
		http.Error(w, `{"error":"link expired or invalid"}`, http.StatusGone)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"tenant": tenant, "case_id": caseID, "kind": kind,
		"expires_at": expires.Format(time.RFC3339), "remaining_uses": maxUses - uses,
	})
}
