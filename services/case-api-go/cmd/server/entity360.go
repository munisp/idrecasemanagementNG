package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"sort"
	"strings"

	"github.com/go-chi/chi/v5"
)

// entity360.go — TPA 360 / provider 360 / health plan 360: one aggregate
// view per real-world organization across every rail that collects data on
// it (CRM accounts, TPA registry + clients, health plan registry + parties,
// cases, invoices, payments, correspondence, audit). Feature-flagged:
// entity360. Schema: scripts/entity360.sql.
//
// Resolution: an organization is identified by its identity keys (NPI, TIN,
// NAIC, FEIN). An exact identity-key match links automatically (ID_MATCH —
// two rows with the same NPI ARE the same organization). A name-only match
// is only ever a suggestion; a person confirms it (CONFIRMED) so a fuzzy
// string can never merge two different companies on its own.

// entity360Sources: the rails an entity can be met on. table → the label the
// portal shows, and the entity_type a link implies when the source row's own
// type column doesn't say.
var entity360Sources = map[string]string{
	"accounts":     "CRM account",
	"tpas":         "TPA organization",
	"tpa_clients":  "TPA client",
	"health_plans": "Health plan",
	"hp_parties":   "Health plan party",
}

// entity360ReadRoles: staff + auditor. External roles (TPA, HEALTH_PLAN,
// PARTY) get their own scoped pages and never the cross-org aggregate.
// NG role vocabulary: no PM/CODER/NURSE_PHYSICIAN/TENANT_ADMIN.
var entity360ReadRoles = []string{"CASE_MANAGER", "DOCTOR", "NURSE", "ATTORNEY", "ARBITRATOR",
	"FINANCE", "FEDERAL_ADMIN", "PLATFORM_ADMIN", "STATE_AUDITOR", serviceRole}

var entity360WriteRoles = []string{"CASE_MANAGER", "FEDERAL_ADMIN", "PLATFORM_ADMIN"}

// normName: lowercase alphanumeric tokens — "St. Mary's Hospital, LLC" and
// "St Marys Hospital" normalize to the same suggestion key.
var nonAlnum = regexp.MustCompile(`[^a-z0-9 ]+`)

var digitsOnly = regexp.MustCompile(`\D`)

// regexpDigits strips an EIN/SSN-style input to bare digits ("12-3456789" →
// "123456789") so it can serve as an identity key.
func regexpDigits(s string) string { return digitsOnly.ReplaceAllString(s, "") }

func normEntityName(s string) string {
	s = nonAlnum.ReplaceAllString(strings.ToLower(s), " ")
	return strings.Join(strings.Fields(s), " ")
}

// ---- Resolver ---------------------------------------------------------------

// findEntityByIdentity: exact identity-key match. Returns "" when no key
// matches. Only non-empty keys are compared (empty never matches anything).
func (s *server) findEntityByIdentity(ctx context.Context, tenant, npi, tin, naic, fein string) string {
	cond, args := []string{}, []any{tenant}
	for _, kv := range [][2]string{{"npi", npi}, {"tin", tin}, {"naic", naic}, {"fein", fein}} {
		if kv[1] != "" {
			args = append(args, kv[1])
			cond = append(cond, fmt.Sprintf("%s = $%d", kv[0], len(args)))
		}
	}
	if len(cond) == 0 {
		return ""
	}
	var id string
	_ = s.db.QueryRow(ctx, fmt.Sprintf(
		`SELECT id::text FROM public.entities WHERE tenant=$1 AND status='ACTIVE' AND (%s) LIMIT 1`,
		strings.Join(cond, " OR ")), args...).Scan(&id)
	return id
}

// entityNameNoise: words that say what kind of organization a name is, not
// which one — stripped before token comparison. (NG has no NPPES registry
// cache, so this lives here instead of an npi_registry.go.)
var entityNameNoise = map[string]bool{
	"THE": true, "OF": true, "AND": true, "DBA": true, "A": true,
	"LLC": true, "L": true, "C": true, "INC": true, "INCORPORATED": true, "CORP": true, "CORPORATION": true,
	"CO": true, "COMPANY": true, "LTD": true, "LIMITED": true, "LP": true, "LLP": true, "PLLC": true,
	"PA": true, "PC": true, "P": true, "MD": true, "DO": true, "PLC": true,
}

var entityNameTokenRe = regexp.MustCompile(`[A-Z0-9]+`)

func entityNameTokens(name string) map[string]bool {
	out := map[string]bool{}
	for _, t := range entityNameTokenRe.FindAllString(strings.ToUpper(strings.ReplaceAll(name, "&", " AND ")), -1) {
		if !entityNameNoise[t] {
			out[t] = true
		}
	}
	return out
}

// tokenJaccard: |A∩B| / |A∪B| over noise-stripped name token sets. 0 when
// either side has no usable tokens.
func tokenJaccard(a, b map[string]bool) float64 {
	if len(a) == 0 || len(b) == 0 {
		return 0
	}
	inter := 0
	for t := range a {
		if b[t] {
			inter++
		}
	}
	union := len(a) + len(b) - inter
	if union == 0 {
		return 0
	}
	return float64(inter) / float64(union)
}

// suggestEntities: name-normalized candidates for human confirmation —
// never auto-linked. Each candidate carries a strength score (token Jaccard
// against the queried name, 0–1) and results are sorted by it.
func (s *server) suggestEntities(ctx context.Context, tenant, name string) []map[string]any {
	norm := normEntityName(name)
	if norm == "" {
		return nil
	}
	rows, err := s.db.Query(ctx, `
		SELECT id::text, entity_type, canonical_name, npi, naic
		FROM public.entities
		WHERE tenant=$1 AND status='ACTIVE'
		  AND lower(regexp_replace(canonical_name, '[^a-zA-Z0-9 ]', '', 'g'))
		      LIKE '%' || $2 || '%'
		ORDER BY canonical_name LIMIT 10`, tenant, norm)
	if err != nil {
		return nil
	}
	defer rows.Close()
	qTokens := entityNameTokens(name)
	out := []map[string]any{}
	for rows.Next() {
		var id, typ, cname, npi, naic string
		if rows.Scan(&id, &typ, &cname, &npi, &naic) == nil {
			score := tokenJaccard(qTokens, entityNameTokens(cname))
			out = append(out, map[string]any{"id": id, "entity_type": typ, "canonical_name": cname,
				"npi": npi, "naic": naic, "score": float64(int(score*100+0.5)) / 100})
		}
	}
	sort.Slice(out, func(i, j int) bool {
		si, _ := out[i]["score"].(float64)
		sj, _ := out[j]["score"].(float64)
		return si > sj
	})
	return out
}

// ensureEntity: the entity for a source row — existing link, identity-key
// match, or a newly created entity. Returns the entity id. sourceTable must
// be one of entity360Sources (callers pass constants, never user input).
func (s *server) ensureEntity(ctx context.Context, tenant, sourceTable, sourceID, entityType, name, npi, tin, naic, fein, actor string) (string, error) {
	if _, ok := entity360Sources[sourceTable]; !ok {
		return "", fmt.Errorf("unknown source table %q", sourceTable)
	}
	var id string
	err := s.db.QueryRow(ctx,
		`SELECT entity_id::text FROM public.entity_links WHERE tenant=$1 AND source_table=$2 AND source_id=$3`,
		tenant, sourceTable, sourceID).Scan(&id)
	if err == nil {
		return id, nil
	}
	id = s.findEntityByIdentity(ctx, tenant, npi, tin, naic, fein)
	if id == "" {
		if entityType == "" {
			entityType = "OTHER"
		}
		if err := s.db.QueryRow(ctx, `
			INSERT INTO public.entities (tenant, entity_type, canonical_name, npi, tin, naic, fein, created_by)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8) RETURNING id::text`,
			tenant, entityType, name, npi, tin, naic, fein, actor).Scan(&id); err != nil {
			return "", err
		}
	}
	kind := "ID_MATCH"
	by := ""
	if actor != "" {
		kind, by = "CONFIRMED", actor
	}
	if _, err := s.db.Exec(ctx, `
		INSERT INTO public.entity_links (tenant, entity_id, source_table, source_id, link_kind, linked_by)
		VALUES ($1,$2,$3,$4,$5,$6) ON CONFLICT (tenant, source_table, source_id) DO NOTHING`,
		tenant, id, sourceTable, sourceID, kind, by); err != nil {
		return "", err
	}
	if sourceTable == "accounts" {
		_, _ = s.db.Exec(ctx, `UPDATE public.accounts SET entity_id=$1, updated_at=now() WHERE tenant=$2 AND id=$3 AND entity_id IS NULL`,
			id, tenant, sourceID)
	}
	return id, nil
}

// entity360Gate: feature on + staff/auditor role. External org roles never
// read the cross-organization aggregate.
func (s *server) entity360Gate(w http.ResponseWriter, r *http.Request, tenant string, roles []string) (principal, bool) {
	p := r.Context().Value(ctxPrincipal{}).(principal)
	if !s.featureGate(w, r, tenant, "entity360") {
		return p, false
	}
	if !hasAnyRole(p, roles...) {
		http.Error(w, `{"error":"forbidden"}`, http.StatusForbidden)
		return p, false
	}
	return p, true
}

// ---- Handlers ---------------------------------------------------------------

// entitySearch: GET /entities?q= — find entities by name or identity key.
func (s *server) entitySearch(w http.ResponseWriter, r *http.Request) {
	tenant := r.Context().Value(ctxTenant{}).(string)
	if _, ok := s.entity360Gate(w, r, tenant, entity360ReadRoles); !ok {
		return
	}
	q := strings.TrimSpace(r.URL.Query().Get("q"))
	if len(q) < 2 {
		writeJSON(w, http.StatusOK, map[string]any{"entities": []any{}})
		return
	}
	like := "%" + normEntityName(q) + "%"
	digits := regexp.MustCompile(`\D`).ReplaceAllString(q, "")
	rows, err := s.db.Query(r.Context(), `
		SELECT id::text, entity_type, canonical_name, npi, naic,
		       (SELECT count(*) FROM public.entity_links l WHERE l.tenant=e.tenant AND l.entity_id=e.id) AS links
		FROM public.entities e
		WHERE tenant=$1 AND status='ACTIVE' AND (
		      lower(regexp_replace(canonical_name, '[^a-zA-Z0-9 ]', '', 'g')) LIKE $2
		      OR ($3 <> '' AND (npi=$3 OR tin=$3 OR naic=$3 OR fein=$3)))
		ORDER BY canonical_name LIMIT 25`, tenant, like, digits)
	if err != nil {
		http.Error(w, `{"error":"query failed"}`, http.StatusInternalServerError)
		return
	}
	defer rows.Close()
	out := []map[string]any{}
	for rows.Next() {
		var id, typ, name, npi, naic string
		var links int
		if rows.Scan(&id, &typ, &name, &npi, &naic, &links) == nil {
			out = append(out, map[string]any{"id": id, "entity_type": typ, "canonical_name": name,
				"npi": npi, "naic": naic, "links": links})
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"entities": out})
}

// entityCreate: POST /entities — register an organization that has no source
// row yet (or the resolved home for a set of links).
func (s *server) entityCreate(w http.ResponseWriter, r *http.Request) {
	tenant := r.Context().Value(ctxTenant{}).(string)
	p, ok := s.entity360Gate(w, r, tenant, entity360WriteRoles)
	if !ok {
		return
	}
	var in struct {
		EntityType string `json:"entity_type"`
		Name       string `json:"name"`
		NPI        string `json:"npi"`
		TIN        string `json:"tin"`
		NAIC       string `json:"naic"`
		FEIN       string `json:"fein"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&in); err != nil || strings.TrimSpace(in.Name) == "" {
		http.Error(w, `{"error":"name is required"}`, http.StatusBadRequest)
		return
	}
	typ := strings.ToUpper(strings.TrimSpace(in.EntityType))
	if typ == "" {
		typ = "OTHER"
	}
	// Identity-key collision = the entity already exists; return it rather
	// than violate the unique index.
	if id := s.findEntityByIdentity(r.Context(), tenant, in.NPI, in.TIN, in.NAIC, in.FEIN); id != "" {
		writeJSON(w, http.StatusOK, map[string]any{"id": id, "existed": true})
		return
	}
	var id string
	if err := s.db.QueryRow(r.Context(), `
		INSERT INTO public.entities (tenant, entity_type, canonical_name, npi, tin, naic, fein, created_by)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8) RETURNING id::text`,
		tenant, typ, strings.TrimSpace(in.Name), in.NPI, in.TIN, in.NAIC, in.FEIN, p.Subject).Scan(&id); err != nil {
		http.Error(w, `{"error":"create failed"}`, http.StatusInternalServerError)
		return
	}
	// NG: no audit_log — the entity_links row (link_kind CONFIRMED, linked_by) is the record.
	writeJSON(w, http.StatusCreated, map[string]any{"id": id})
}

// entityConfirmLink: POST /entities/{entityId}/links — a person confirms a
// source row belongs to this entity (the fuzzy-name path).
func (s *server) entityConfirmLink(w http.ResponseWriter, r *http.Request) {
	tenant := r.Context().Value(ctxTenant{}).(string)
	p, ok := s.entity360Gate(w, r, tenant, entity360WriteRoles)
	if !ok {
		return
	}
	entityID := chi.URLParam(r, "entityId")
	var in struct {
		SourceTable string `json:"source_table"`
		SourceID    string `json:"source_id"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16<<10)).Decode(&in); err != nil {
		http.Error(w, `{"error":"invalid body"}`, http.StatusBadRequest)
		return
	}
	if _, ok := entity360Sources[in.SourceTable]; !ok || in.SourceID == "" {
		http.Error(w, `{"error":"unknown source"}`, http.StatusBadRequest)
		return
	}
	// The entity must exist, and the source row must not already be linked
	// elsewhere (re-linking is DELETE + POST, an auditable two-step).
	var live bool
	if err := s.db.QueryRow(r.Context(),
		`SELECT EXISTS(SELECT 1 FROM public.entities WHERE tenant=$1 AND id=$2 AND status='ACTIVE')`,
		tenant, entityID).Scan(&live); err != nil || !live {
		http.Error(w, `{"error":"entity not found"}`, http.StatusNotFound)
		return
	}
	var taken string
	_ = s.db.QueryRow(r.Context(),
		`SELECT entity_id::text FROM public.entity_links WHERE tenant=$1 AND source_table=$2 AND source_id=$3`,
		tenant, in.SourceTable, in.SourceID).Scan(&taken)
	if taken != "" && taken != entityID {
		http.Error(w, `{"error":"source row already linked to another entity — unlink it first"}`, http.StatusConflict)
		return
	}
	if _, err := s.db.Exec(r.Context(), `
		INSERT INTO public.entity_links (tenant, entity_id, source_table, source_id, link_kind, linked_by)
		VALUES ($1,$2,$3,$4,'CONFIRMED',$5)
		ON CONFLICT (tenant, source_table, source_id) DO NOTHING`,
		tenant, entityID, in.SourceTable, in.SourceID, p.Subject); err != nil {
		http.Error(w, `{"error":"link failed"}`, http.StatusInternalServerError)
		return
	}
	if in.SourceTable == "accounts" {
		_, _ = s.db.Exec(r.Context(), `UPDATE public.accounts SET entity_id=$1, updated_at=now() WHERE tenant=$2 AND id=$3`,
			entityID, tenant, in.SourceID)
	}
	// NG: no audit_log — the entity_links row (link_kind CONFIRMED, linked_by) is the record.
	writeJSON(w, http.StatusOK, map[string]any{"linked": true})
}

// entityUnlink: DELETE /entities/{entityId}/links/{linkId}.
func (s *server) entityUnlink(w http.ResponseWriter, r *http.Request) {
	tenant := r.Context().Value(ctxTenant{}).(string)
	_, ok := s.entity360Gate(w, r, tenant, entity360WriteRoles)
	if !ok {
		return
	}
	res, err := s.db.Exec(r.Context(),
		`DELETE FROM public.entity_links WHERE tenant=$1 AND entity_id=$2 AND id=$3`,
		tenant, chi.URLParam(r, "entityId"), chi.URLParam(r, "linkId"))
	if err != nil || res.RowsAffected() == 0 {
		http.Error(w, `{"error":"not found"}`, http.StatusNotFound)
		return
	}
	// NG: no audit_log — the entity_links row (link_kind CONFIRMED, linked_by) is the record.
	writeJSON(w, http.StatusOK, map[string]any{"unlinked": true})
}

// entitySourceRow: load a source row's identity fields for resolution.
// Returns name/type/npi/tin/naic/fein. table is a constant — never user input.
func (s *server) entitySourceRow(ctx context.Context, tenant, table, id string) (name, typ, npi, tin, naic, fein string, err error) {
	switch table {
	case "accounts":
		// ein_masked is deliberately NOT an identity key (masked), so an
		// account contributes its NPI only.
		err = s.db.QueryRow(ctx, `
			SELECT legal_name, type, coalesce(npi,''), '', '', ''
			FROM public.accounts WHERE tenant=$1 AND id=$2`, tenant, id).Scan(&name, &typ, &npi, &tin, &naic, &fein)
	case "tpas":
		err = s.db.QueryRow(ctx, `
			SELECT name, 'TPA', '', '', '', '' FROM public.tpas WHERE tenant=$1 AND id=$2`, tenant, id).
			Scan(&name, &typ, &npi, &tin, &naic, &fein)
	case "tpa_clients":
		err = s.db.QueryRow(ctx, `
			SELECT party_name, party_type, coalesce(npi,''), coalesce(tin,''), '', ''
			FROM public.tpa_clients WHERE tenant=$1 AND id=$2`, tenant, id).Scan(&name, &typ, &npi, &tin, &naic, &fein)
	case "health_plans":
		err = s.db.QueryRow(ctx, `
			SELECT name, 'HEALTH_PLAN', '', coalesce(fein,''), coalesce(naic,''), coalesce(fein,'')
			FROM public.health_plans WHERE tenant=$1 AND id=$2`, tenant, id).Scan(&name, &typ, &npi, &tin, &naic, &fein)
	case "hp_parties":
		err = s.db.QueryRow(ctx, `
			SELECT party_name, party_type, coalesce(npi,''), coalesce(tin,''), '', ''
			FROM public.hp_parties WHERE tenant=$1 AND id=$2`, tenant, id).Scan(&name, &typ, &npi, &tin, &naic, &fein)
	default:
		err = fmt.Errorf("unknown source table %q", table)
	}
	return
}

// entityLookupRef: GET /entity360/lookup?source_table=&source_id= — the
// portal's way in from any screen that knows a source row (account 360, TPA
// page, health plan page): resolves (auto-linking on identity keys) and
// answers the entity id plus any name-only suggestions needing confirmation.
func (s *server) entityLookupRef(w http.ResponseWriter, r *http.Request) {
	tenant := r.Context().Value(ctxTenant{}).(string)
	if _, ok := s.entity360Gate(w, r, tenant, entity360ReadRoles); !ok {
		return
	}
	table, id := r.URL.Query().Get("source_table"), r.URL.Query().Get("source_id")
	if _, ok := entity360Sources[table]; !ok || id == "" {
		http.Error(w, `{"error":"unknown source"}`, http.StatusBadRequest)
		return
	}
	name, typ, npi, tin, naic, fein, err := s.entitySourceRow(r.Context(), tenant, table, id)
	if err != nil {
		http.Error(w, `{"error":"source row not found"}`, http.StatusNotFound)
		return
	}
	entityID, err := s.ensureEntity(r.Context(), tenant, table, id, typ, name, npi, tin, naic, fein, "")
	if err != nil {
		http.Error(w, `{"error":"resolve failed"}`, http.StatusInternalServerError)
		return
	}
	suggestions := []map[string]any{}
	if npi == "" && tin == "" && naic == "" && fein == "" {
		// No identity keys: the auto-link rests on nothing but this row's own
		// creation — surface same-name entities so a person can merge them.
		for _, c := range s.suggestEntities(r.Context(), tenant, name) {
			if c["id"] != entityID {
				suggestions = append(suggestions, c)
			}
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"entity_id": entityID, "suggestions": suggestions})
}
