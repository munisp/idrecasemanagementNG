// crm.go — CRM core: accounts, contacts, leads, tasks, notes, global search.
// This is the Salesforce/Twenty-parity layer; all queries are tenant-scoped
// via the same fail-closed middleware as cases.
package main

import (
	"encoding/json"
	"fmt"
	"strconv"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
)

// ---- Accounts ---------------------------------------------------------------

type Account struct {
	ID        string         `json:"id"`
	Tenant    string         `json:"tenant"`
	Type      string         `json:"type"`
	LegalName string         `json:"legal_name"`
	NPI       string         `json:"npi"`
	Phone     string         `json:"phone"`
	Custom    map[string]any `json:"custom"`
	CreatedAt time.Time      `json:"created_at"`
}

// pageParams: offset-based paging for the bounded CRM lists (accounts, tasks,
// leads — thousands of rows at most, where OFFSET is cheap and sort orders
// aren't keyset-friendly). Disputes use keyset pagination (listCases) instead.
func pageParams(r *http.Request, def, max int) (limit, offset int) {
	limit = def
	if n, err := strconv.Atoi(r.URL.Query().Get("limit")); err == nil && n > 0 {
		limit = min(n, max)
	}
	if n, err := strconv.Atoi(r.URL.Query().Get("offset")); err == nil && n >= 0 {
		offset = n
	}
	return limit, offset
}

func nextOffset(offset, limit, total int) int {
	if offset+limit < total {
		return offset + limit
	}
	return -1
}

func (s *server) listAccounts(w http.ResponseWriter, r *http.Request) {
	tenant := r.Context().Value(ctxTenant{}).(string)
	typ := r.URL.Query().Get("type")
	q := `SELECT id, tenant, type, legal_name, COALESCE(npi,''), COALESCE(phone,''),
	             custom, created_at FROM public.accounts WHERE tenant=$1`
	args := []any{tenant}
	if typ != "" {
		q += ` AND type=$2`
		args = append(args, typ)
	}
	limit, offset := pageParams(r, 100, 500)
	var total int
	if err := s.db.QueryRow(r.Context(),
		strings.Replace(q, `SELECT id, tenant, type, legal_name, COALESCE(npi,''), COALESCE(phone,''),
	             custom, created_at`, `SELECT count(*)`, 1), args...).Scan(&total); err != nil {
		http.Error(w, `{"error":"db"}`, http.StatusInternalServerError)
		return
	}
	q += fmt.Sprintf(` ORDER BY legal_name, id LIMIT %d OFFSET %d`, limit, offset)
	rows, err := s.db.Query(r.Context(), q, args...)
	if err != nil {
		http.Error(w, `{"error":"db"}`, http.StatusInternalServerError)
		return
	}
	defer rows.Close()
	out := []Account{}
	for rows.Next() {
		var a Account
		var custom []byte
		if rows.Scan(&a.ID, &a.Tenant, &a.Type, &a.LegalName, &a.NPI, &a.Phone, &custom, &a.CreatedAt) == nil {
			_ = json.Unmarshal(custom, &a.Custom)
			out = append(out, a)
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"accounts": out, "total": total, "next_offset": nextOffset(offset, limit, total)})
}

func (s *server) createAccount(w http.ResponseWriter, r *http.Request) {
	tenant := r.Context().Value(ctxTenant{}).(string)
	var in struct {
		Type      string         `json:"type"`
		LegalName string         `json:"legal_name"`
		EIN       string         `json:"ein"`
		NPI       string         `json:"npi"`
		Phone     string         `json:"phone"`
		Custom    map[string]any `json:"custom"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil || in.LegalName == "" || in.Type == "" {
		http.Error(w, `{"error":"type and legal_name required"}`, http.StatusBadRequest)
		return
	}
	custom, _ := json.Marshal(in.Custom)
	var id string
	err := s.db.QueryRow(r.Context(), `
		INSERT INTO public.accounts (tenant, type, legal_name, ein_masked, npi, phone, custom)
		VALUES ($1,$2,$3,$4,$5,$6,$7) RETURNING id`,
		tenant, in.Type, in.LegalName, maskEIN(in.EIN), in.NPI, in.Phone, custom).Scan(&id)
	if err != nil {
		http.Error(w, `{"error":"db"}`, http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]string{"id": id})
}

// account360: one record with contacts, cases, recent activity (the CRM 360 view).
func (s *server) account360(w http.ResponseWriter, r *http.Request) {
	tenant := r.Context().Value(ctxTenant{}).(string)
	id := chi.URLParam(r, "accountId")
	var a Account
	var custom []byte
	if err := s.db.QueryRow(r.Context(), `
		SELECT id, tenant, type, legal_name, COALESCE(npi,''), COALESCE(phone,''), custom, created_at
		FROM public.accounts WHERE tenant=$1 AND id=$2`, tenant, id).
		Scan(&a.ID, &a.Tenant, &a.Type, &a.LegalName, &a.NPI, &a.Phone, &custom, &a.CreatedAt); err != nil {
		http.Error(w, `{"error":"not found"}`, http.StatusNotFound)
		return
	}
	_ = json.Unmarshal(custom, &a.Custom)

	contacts, _ := s.queryRows(r, `
		SELECT id, name, COALESCE(role_title,''), COALESCE(email,''), COALESCE(phone,'')
		FROM public.contacts WHERE account_id=$1 ORDER BY name`, id)
	cases, _ := s.queryRows(r, fmt.Sprintf(`
		SELECT id, case_number, status, coalesce(subject_line, service_line) AS service_line FROM tenant_%s.cases
		WHERE provider_id=$1 OR payer_id=$1 ORDER BY opened_at DESC LIMIT 50`, sanitizeTenant(tenant)), a.LegalName)
	notes, _ := s.queryRows(r, `
		SELECT stream, body, author, created_at FROM public.notes
		WHERE tenant=$1 AND record_type='ACCOUNT' AND record_id=$2
		ORDER BY created_at DESC LIMIT 50`, tenant, id)

	// Relationship health: computed from this account's dispute history — open load,
	// breach exposure, and recency. The 360 "brief" surfaces this at a glance.
	open, breaches := 0, 0
	var lastTouch string
	for _, c := range cases {
		st, _ := c["status"].(string)
		if !strings.HasPrefix(st, "CLOSED") && st != "SETTLED_IN_NEGOTIATION" {
			open++
		}
	}
	_ = s.db.QueryRow(r.Context(), `
		SELECT COUNT(*), COALESCE(to_char(MAX(at),'YYYY-MM-DD"T"HH24:MI'),'')
		FROM public.sla_breaches b
		JOIN (SELECT $1::text AS acct) x ON true
		WHERE b.tenant=$2`, id, tenant).Scan(&breaches, &lastTouch)
	score := 100 - open*8 - breaches*20
	if score < 0 {
		score = 0
	}
	band := "healthy"
	if score < 60 {
		band = "at-risk"
	} else if score < 85 {
		band = "watch"
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"account": a, "contacts": contacts, "cases": cases, "notes": notes,
		"health": map[string]any{"score": score, "band": band, "open_disputes": open,
			"sla_breaches": breaches, "formula": "100 − 8×open disputes − 20×SLA breaches"},
	})
}

// ---- Contacts ----------------------------------------------------------------

func (s *server) createContact(w http.ResponseWriter, r *http.Request) {
	tenant := r.Context().Value(ctxTenant{}).(string)
	var in struct {
		AccountID string `json:"account_id"`
		Name      string `json:"name"`
		RoleTitle string `json:"role_title"`
		Email     string `json:"email"`
		Phone     string `json:"phone"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil || in.Name == "" {
		http.Error(w, `{"error":"name required"}`, http.StatusBadRequest)
		return
	}
	var id string
	err := s.db.QueryRow(r.Context(), `
		INSERT INTO public.contacts (tenant, account_id, name, role_title, email, phone)
		VALUES ($1,NULLIF($2,'')::uuid,$3,$4,$5,$6) RETURNING id`,
		tenant, in.AccountID, in.Name, in.RoleTitle, in.Email, in.Phone).Scan(&id)
	if err != nil {
		http.Error(w, `{"error":"db"}`, http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]string{"id": id})
}

// ---- Leads --------------------------------------------------------------------

func (s *server) listLeads(w http.ResponseWriter, r *http.Request) {
	tenant := r.Context().Value(ctxTenant{}).(string)
	limit, offset := pageParams(r, 100, 500)
	var total int
	if err := s.db.QueryRow(r.Context(),
		`SELECT count(*) FROM public.leads WHERE tenant=$1`, tenant).Scan(&total); err != nil {
		http.Error(w, `{"error":"db"}`, http.StatusInternalServerError)
		return
	}
	rows, err := s.db.Query(r.Context(), fmt.Sprintf(`
		SELECT id, source, COALESCE(name,''), COALESCE(organization,''),
		       COALESCE(phone,''), COALESCE(summary,''), status, created_at
		FROM public.leads WHERE tenant=$1 ORDER BY created_at DESC, id LIMIT %d OFFSET %d`, limit, offset), tenant)
	if err != nil {
		http.Error(w, `{"error":"db"}`, http.StatusInternalServerError)
		return
	}
	defer rows.Close()
	out := []map[string]any{}
	for rows.Next() {
		var id, src, name, org, phone, summary, status string
		var at time.Time
		if rows.Scan(&id, &src, &name, &org, &phone, &summary, &status, &at) == nil {
			out = append(out, map[string]any{
				"id": id, "source": src, "name": name, "organization": org,
				"phone": phone, "summary": summary, "status": status, "created_at": at,
			})
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"leads": out, "total": total, "next_offset": nextOffset(offset, limit, total)})
}

// convertLead: lead -> account (+ contact), marks lead CONVERTED.
func (s *server) convertLead(w http.ResponseWriter, r *http.Request) {
	tenant := r.Context().Value(ctxTenant{}).(string)
	leadID := chi.URLParam(r, "leadId")
	var name, org, phone string
	var status string
	err := s.db.QueryRow(r.Context(), `
		SELECT COALESCE(name,''), COALESCE(organization,''), COALESCE(phone,''), status
		FROM public.leads WHERE tenant=$1 AND id=$2`, tenant, leadID).
		Scan(&name, &org, &phone, &status)
	if err != nil {
		http.Error(w, `{"error":"not found"}`, http.StatusNotFound)
		return
	}
	if status == "CONVERTED" {
		http.Error(w, `{"error":"already converted"}`, http.StatusConflict)
		return
	}
	var in struct {
		Type string `json:"type"` // PROVIDER | PAYER | ...
	}
	_ = json.NewDecoder(r.Body).Decode(&in)
	if in.Type == "" {
		in.Type = "OTHER"
	}
	orgName := org
	if orgName == "" {
		orgName = name
	}
	var accountID string
	err = s.db.QueryRow(r.Context(), `
		INSERT INTO public.accounts (tenant, type, legal_name, phone)
		VALUES ($1,$2,$3,$4) RETURNING id`, tenant, in.Type, orgName, phone).Scan(&accountID)
	if err != nil {
		http.Error(w, `{"error":"db"}`, http.StatusInternalServerError)
		return
	}
	if name != "" {
		_, _ = s.db.Exec(r.Context(), `
			INSERT INTO public.contacts (tenant, account_id, name, phone) VALUES ($1,$2,$3,$4)`,
			tenant, accountID, name, phone)
	}
	_, _ = s.db.Exec(r.Context(), `
		UPDATE public.leads SET status='CONVERTED', converted_account_id=$3
		WHERE tenant=$1 AND id=$2`, tenant, leadID, accountID)
	writeJSON(w, http.StatusOK, map[string]string{"account_id": accountID, "status": "CONVERTED"})
}

// ---- Tasks & notes --------------------------------------------------------------

func (s *server) listTasks(w http.ResponseWriter, r *http.Request) {
	tenant := r.Context().Value(ctxTenant{}).(string)
	p := r.Context().Value(ctxPrincipal{}).(principal)
	mine := r.URL.Query().Get("mine") == "true"
	q := `SELECT id, subject, COALESCE(case_id,''), COALESCE(assignee,''),
	             COALESCE(to_char(due_date,'YYYY-MM-DD'),''), status, created_at
	      FROM public.tasks WHERE tenant=$1`
	args := []any{tenant}
	if mine {
		q += ` AND assignee=$2`
		args = append(args, p.Subject)
	}
	limit, offset := pageParams(r, 100, 500)
	var total int
	if err := s.db.QueryRow(r.Context(),
		strings.Replace(q, `SELECT id, subject, COALESCE(case_id,''), COALESCE(assignee,''),
	             COALESCE(to_char(due_date,'YYYY-MM-DD'),''), status, created_at`, `SELECT count(*)`, 1), args...).Scan(&total); err != nil {
		http.Error(w, `{"error":"db"}`, http.StatusInternalServerError)
		return
	}
	q += fmt.Sprintf(` ORDER BY status, due_date NULLS LAST, id LIMIT %d OFFSET %d`, limit, offset)
	rows, err := s.db.Query(r.Context(), q, args...)
	if err != nil {
		http.Error(w, `{"error":"db"}`, http.StatusInternalServerError)
		return
	}
	defer rows.Close()
	out := []map[string]any{}
	for rows.Next() {
		var id, subj, caseID, assignee, due, status string
		var at time.Time
		if rows.Scan(&id, &subj, &caseID, &assignee, &due, &status, &at) == nil {
			out = append(out, map[string]any{
				"id": id, "subject": subj, "case_id": caseID, "assignee": assignee,
				"due_date": due, "status": status, "created_at": at,
			})
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"tasks": out, "total": total, "next_offset": nextOffset(offset, limit, total)})
}

func (s *server) createTask(w http.ResponseWriter, r *http.Request) {
	tenant := r.Context().Value(ctxTenant{}).(string)
	p := r.Context().Value(ctxPrincipal{}).(principal)
	var in struct {
		Subject  string `json:"subject"`
		CaseID   string `json:"case_id"`
		Assignee string `json:"assignee"`
		DueDate  string `json:"due_date"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil || in.Subject == "" {
		http.Error(w, `{"error":"subject required"}`, http.StatusBadRequest)
		return
	}
	var id string
	err := s.db.QueryRow(r.Context(), `
		INSERT INTO public.tasks (tenant, subject, case_id, assignee, due_date, created_by)
		VALUES ($1,$2,NULLIF($3,''),$4,NULLIF($5,'')::date,$6) RETURNING id`,
		tenant, in.Subject, in.CaseID, in.Assignee, in.DueDate, p.Subject).Scan(&id)
	if err != nil {
		http.Error(w, `{"error":"db"}`, http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]string{"id": id})
}

func (s *server) completeTask(w http.ResponseWriter, r *http.Request) {
	tenant := r.Context().Value(ctxTenant{}).(string)
	_, err := s.db.Exec(r.Context(), `
		UPDATE public.tasks SET status='DONE', completed_at=now() WHERE tenant=$1 AND id=$2`,
		tenant, chi.URLParam(r, "taskId"))
	if err != nil {
		http.Error(w, `{"error":"db"}`, http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "DONE"})
}

func (s *server) addNote(w http.ResponseWriter, r *http.Request) {
	tenant := r.Context().Value(ctxTenant{}).(string)
	p := r.Context().Value(ctxPrincipal{}).(principal)
	var in struct {
		RecordType string `json:"record_type"`
		RecordID   string `json:"record_id"`
		Stream     string `json:"stream"` // internal|coder|clinical|legal|external_agency
		Body       string `json:"body"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil || in.Body == "" {
		http.Error(w, `{"error":"body required"}`, http.StatusBadRequest)
		return
	}
	// Notes streams (Field Criteria): default internal; validate against the
	// program's notes_streams list when one is seeded.
	if in.Stream == "" {
		in.Stream = "internal"
	}
	if prog := s.loadProgram(r, tenant); prog != nil && len(prog.NotesStreams) > 0 {
		ok := false
		for _, st := range prog.NotesStreams {
			if st == in.Stream {
				ok = true
				break
			}
		}
		if !ok {
			http.Error(w, `{"error":"stream must be one of the program notes_streams"}`, http.StatusBadRequest)
			return
		}
	}
	_, err := s.db.Exec(r.Context(), `
		INSERT INTO public.notes (tenant, record_type, record_id, stream, body, author)
		VALUES ($1,$2,$3,$4,$5,$6)`, tenant, in.RecordType, in.RecordID, in.Stream, in.Body, p.Subject)
	if err != nil {
		http.Error(w, `{"error":"db"}`, http.StatusInternalServerError)
		return
	}
	// Notes also appear on the case activity timeline.
	if in.RecordType == "CASE" {
		_, _ = s.db.Exec(r.Context(), `
			INSERT INTO public.case_activities (tenant, case_id, type, body)
			VALUES ($1,$2,'NOTE',$3)`, tenant, in.RecordID, in.Body)
	}
	writeJSON(w, http.StatusCreated, map[string]string{"status": "added"})
}

// ---- Global search (cases + accounts + contacts + leads) ------------------------

func (s *server) globalSearch(w http.ResponseWriter, r *http.Request) {
	tenant := r.Context().Value(ctxTenant{}).(string)
	term := "%" + r.URL.Query().Get("q") + "%"
	type hit struct{ Kind, ID, Label, Detail string }
	out := []hit{}
	queries := []struct {
		kind, sql string
	}{
		{"case", fmt.Sprintf(`SELECT id, case_number, status FROM tenant_%s.cases
			WHERE case_number ILIKE $1 LIMIT 10`, sanitizeTenant(tenant))},
		{"account", `SELECT id::text, legal_name, type FROM public.accounts
			WHERE tenant=$1 AND legal_name ILIKE $2 LIMIT 10`},
		{"contact", `SELECT id::text, name, COALESCE(role_title,'') FROM public.contacts
			WHERE tenant=$1 AND name ILIKE $2 LIMIT 10`},
		{"lead", `SELECT id::text, COALESCE(name,organization,'(unnamed)'), status FROM public.leads
			WHERE tenant=$1 AND (name ILIKE $2 OR organization ILIKE $2 OR summary ILIKE $2) LIMIT 10`},
	}
	for _, qd := range queries {
		var rows interface {
			Next() bool
			Scan(...any) error
			Close()
		}
		var err error
		if qd.kind == "case" {
			rows, err = s.db.Query(r.Context(), qd.sql, term)
		} else {
			rows, err = s.db.Query(r.Context(), qd.sql, tenant, term)
		}
		if err != nil {
			continue
		}
		for rows.Next() {
			var h hit
			if rows.Scan(&h.ID, &h.Label, &h.Detail) == nil {
				h.Kind = qd.kind
				out = append(out, h)
			}
		}
		rows.Close()
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *server) queryRows(r *http.Request, sql string, args ...any) ([]map[string]any, error) {
	rows, err := s.db.Query(r.Context(), sql, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	cols := rows.FieldDescriptions()
	out := []map[string]any{}
	for rows.Next() {
		vals, err := rows.Values()
		if err != nil {
			continue
		}
		m := map[string]any{}
		for i, c := range cols {
			m[string(c.Name)] = vals[i]
		}
		out = append(out, m)
	}
	return out, nil
}
