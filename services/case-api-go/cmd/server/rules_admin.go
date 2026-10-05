// rules_admin.go — admin-managed rule configuration with audit trail.
//
// Rules are live policy (see rules.go). Editing them changes statutory
// behavior, so: (1) only FEDERAL_ADMIN / PLATFORM_ADMIN principals may read
// or write rules; (2) every write is validated structurally BEFORE it
// persists; (3) every write appends an immutable row to public.rule_changes
// (tenant, actor, before, after) — the audit trail is append-only by design,
// there is deliberately no UPDATE/DELETE endpoint for it.
package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
)

var knownRuleEvents = map[string]bool{
	"intake.advance": true, "doc.upload": true, "doc.analyzed": true,
	"claims.imported": true, "invoice.settled": true, "sweep.intake": true,
	"sweep.case": true,
}
var knownRuleOps = map[string]bool{
	"eq": true, "neq": true, "in": true, "contains": true,
	"gt": true, "gte": true, "lt": true, "lte": true,
	"is_null": true, "not_null": true, "days_older_than": true,
}
var knownRuleActions = map[string]bool{
	"set_status": true, "set_detail": true, "notify": true,
	"log_activity": true, "block_request": true, "flag_review": true,
}

// validateRules rejects structurally unsound rules with a precise error per
// rule — the UI surfaces these verbatim so admins can fix without guessing.
func validateRules(rules []Rule) error {
	names := map[string]bool{}
	for i, ru := range rules {
		where := fmt.Sprintf("rule #%d", i+1)
		if ru.Name == "" {
			return fmt.Errorf("%s: name is required", where)
		}
		where = fmt.Sprintf("rule %q", ru.Name)
		if names[ru.Name] {
			return fmt.Errorf("%s: duplicate name", where)
		}
		names[ru.Name] = true
		if !knownRuleEvents[ru.Event] {
			return fmt.Errorf("%s: unknown event %q (known: intake.advance, doc.upload, doc.analyzed, claims.imported, invoice.settled, sweep.intake, sweep.case)", where, ru.Event)
		}
		for _, c := range ru.Conditions {
			if c.Field == "" || !knownRuleOps[c.Op] {
				return fmt.Errorf("%s: bad condition (field %q, op %q)", where, c.Field, c.Op)
			}
		}
		for _, a := range ru.Actions {
			if !knownRuleActions[a.Type] {
				return fmt.Errorf("%s: unknown action %q", where, a.Type)
			}
		}
	}
	return nil
}

func (s *server) rulesAdminGuard(w http.ResponseWriter, r *http.Request) (principal, bool) {
	p := r.Context().Value(ctxPrincipal{}).(principal)
	if !hasAnyRole(p, "FEDERAL_ADMIN", "PLATFORM_ADMIN") {
		http.Error(w, `{"error":"rules are admin-managed (FEDERAL_ADMIN or PLATFORM_ADMIN required)"}`, http.StatusForbidden)
		return p, false
	}
	// Second layer: Permify object-level permission (program_rules.edit on the
	// tenant entity). Fail-CLOSED when Permify is configured — an authorization
	// outage must not become authorization for live policy edits. When Permify
	// isn't deployed (dev), allow() returns true and Keycloak RBAC stands alone.
	tenant := r.Context().Value(ctxTenant{}).(string)
	if !s.allow(r.Context(), "program_rules", tenant, "edit", p.Subject) {
		http.Error(w, `{"error":"rules edit denied by fine-grained authorization (program_rules.edit)"}`, http.StatusForbidden)
		return p, false
	}
	return p, true
}

// listRules: GET /v1/tenants/{tenant}/rules
func (s *server) listRules(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.rulesAdminGuard(w, r); !ok {
		return
	}
	tenant := r.Context().Value(ctxTenant{}).(string)
	var raw []byte
	err := s.db.QueryRow(r.Context(),
		`SELECT coalesce(config->'rules','[]'::jsonb) FROM public.program_rules WHERE tenant=$1`, tenant).Scan(&raw)
	if err != nil {
		writeJSON(w, http.StatusOK, map[string]any{"rules": []any{}})
		return
	}
	var rules []Rule
	_ = json.Unmarshal(raw, &rules)
	if rules == nil {
		rules = []Rule{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"rules": rules})
}

// putRules: PUT /v1/tenants/{tenant}/rules — full replacement, validated,
// audited. Full-replacement (not per-rule PATCH) keeps the change set
// reviewable as one unit and the audit diff meaningful.
func (s *server) putRules(w http.ResponseWriter, r *http.Request) {
	p, ok := s.rulesAdminGuard(w, r)
	if !ok {
		return
	}
	tenant := r.Context().Value(ctxTenant{}).(string)
	var in struct {
		Rules []Rule `json:"rules"`
		Note  string `json:"note"` // change rationale, stored in the audit row
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		http.Error(w, `{"error":"rules array required"}`, http.StatusBadRequest)
		return
	}
	if err := validateRules(in.Rules); err != nil {
		http.Error(w, fmt.Sprintf(`{"error":%q}`, err.Error()), http.StatusUnprocessableEntity)
		return
	}

	var before []byte
	_ = s.db.QueryRow(r.Context(),
		`SELECT coalesce(config->'rules','[]'::jsonb) FROM public.program_rules WHERE tenant=$1`, tenant).Scan(&before)
	after, _ := json.Marshal(in.Rules)

	// Config write + audit append in one transaction — the trail can never
	// drift from the actual config state.
	tx, err := s.db.Begin(r.Context())
	if err != nil {
		http.Error(w, `{"error":"db"}`, http.StatusInternalServerError)
		return
	}
	defer tx.Rollback(r.Context())
	res, err := tx.Exec(r.Context(),
		`UPDATE public.program_rules SET config = jsonb_set(config, '{rules}', $2::jsonb), updated_at=now()
		 WHERE tenant=$1`, tenant, string(after))
	if err != nil {
		http.Error(w, `{"error":"db"}`, http.StatusInternalServerError)
		return
	}
	if res.RowsAffected() == 0 {
		http.Error(w, `{"error":"no program config for this tenant — rules apply to program tenants only"}`, http.StatusNotFound)
		return
	}
	if _, err := tx.Exec(r.Context(), `
		INSERT INTO public.rule_changes (tenant, changed_by, note, before, after)
		VALUES ($1,$2,$3,$4,$5)`, tenant, p.Subject, in.Note, string(before), string(after)); err != nil {
		http.Error(w, `{"error":"db"}`, http.StatusInternalServerError)
		return
	}
	if err := tx.Commit(r.Context()); err != nil {
		http.Error(w, `{"error":"db"}`, http.StatusInternalServerError)
		return
	}
	s.logActivity(r.Context(), tenant, "", "RULES_UPDATED",
		fmt.Sprintf("Program rules updated by %s (%d rules)%s", p.Subject, len(in.Rules), orDash(" — "+in.Note)))
	writeJSON(w, http.StatusOK, map[string]any{"rules": in.Rules, "saved": len(in.Rules)})
}

// rulesAudit: GET /v1/tenants/{tenant}/rules/audit — newest first.
func (s *server) rulesAudit(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.rulesAdminGuard(w, r); !ok {
		return
	}
	tenant := r.Context().Value(ctxTenant{}).(string)
	rows, err := s.db.Query(r.Context(), `
		SELECT id, changed_by, coalesce(note,''), before, after, changed_at
		FROM public.rule_changes WHERE tenant=$1 ORDER BY id DESC LIMIT 100`, tenant)
	if err != nil {
		http.Error(w, `{"error":"db"}`, http.StatusInternalServerError)
		return
	}
	defer rows.Close()
	type ch struct {
		ID        int64     `json:"id"`
		ChangedBy string    `json:"changed_by"`
		Note      string    `json:"note"`
		Before    []Rule    `json:"before"`
		After     []Rule    `json:"after"`
		ChangedAt time.Time `json:"changed_at"`
	}
	out := []ch{}
	for rows.Next() {
		var c ch
		var b, a []byte
		if rows.Scan(&c.ID, &c.ChangedBy, &c.Note, &b, &a, &c.ChangedAt) != nil {
			continue
		}
		_ = json.Unmarshal(b, &c.Before)
		_ = json.Unmarshal(a, &c.After)
		out = append(out, c)
	}
	writeJSON(w, http.StatusOK, map[string]any{"changes": out})
}

var _ = chi.URLParam // keep chi import stable if routes move
