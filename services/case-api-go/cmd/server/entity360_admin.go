package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"
)

// entity360_admin.go — the entity registry's housekeeping surfaces:
// merge (dedupe), backfill (resolve everything that predates the registry),
// unlinked-source search (the portal's "link a record" picker), and the CRM
// outreach list (entities with dispute activity but no recent engagement).

// ---- Merge (#1) -------------------------------------------------------------

// entityMerge: POST /entities/{entityId}/merge {into_id}. The loser is
// tombstoned (status MERGED, merged_into set); its links, CRM account
// pointer and any identity keys the survivor lacks move to the survivor.
func (s *server) entityMerge(w http.ResponseWriter, r *http.Request) {
	tenant := r.Context().Value(ctxTenant{}).(string)
	_, ok := s.entity360Gate(w, r, tenant, entity360WriteRoles)
	if !ok {
		return
	}
	loser := chi.URLParam(r, "entityId")
	var in struct {
		IntoID string `json:"into_id"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16<<10)).Decode(&in); err != nil || in.IntoID == "" {
		http.Error(w, `{"error":"into_id is required"}`, http.StatusBadRequest)
		return
	}
	if in.IntoID == loser {
		http.Error(w, `{"error":"an entity cannot merge into itself"}`, http.StatusBadRequest)
		return
	}
	// Both must be live entities of this tenant.
	var loserName, intoName string
	if err := s.db.QueryRow(r.Context(),
		`SELECT canonical_name FROM public.entities WHERE tenant=$1 AND id=$2 AND status='ACTIVE'`,
		tenant, loser).Scan(&loserName); err != nil {
		http.Error(w, `{"error":"entity not found"}`, http.StatusNotFound)
		return
	}
	if err := s.db.QueryRow(r.Context(),
		`SELECT canonical_name FROM public.entities WHERE tenant=$1 AND id=$2 AND status='ACTIVE'`,
		tenant, in.IntoID).Scan(&intoName); err != nil {
		http.Error(w, `{"error":"target entity not found"}`, http.StatusNotFound)
		return
	}
	tx, err := s.db.Begin(r.Context())
	if err != nil {
		http.Error(w, `{"error":"db"}`, http.StatusInternalServerError)
		return
	}
	defer tx.Rollback(r.Context())
	// Links move (the source-row unique key can't collide: a source row is
	// linked to at most one entity, and the loser's links are all to it).
	if _, err := tx.Exec(r.Context(),
		`UPDATE public.entity_links SET entity_id=$1 WHERE tenant=$2 AND entity_id=$3`,
		in.IntoID, tenant, loser); err != nil {
		http.Error(w, `{"error":"merge failed moving links"}`, http.StatusInternalServerError)
		return
	}
	if _, err := tx.Exec(r.Context(),
		`UPDATE public.accounts SET entity_id=$1, updated_at=now() WHERE tenant=$2 AND entity_id=$3`,
		in.IntoID, tenant, loser); err != nil {
		http.Error(w, `{"error":"merge failed moving accounts"}`, http.StatusInternalServerError)
		return
	}
	// Identity keys: fill gaps on the survivor from the loser (unique
	// indexes can't collide — the loser's keys matched nothing else, or the
	// resolver would already have merged them).
	if _, err := tx.Exec(r.Context(), `
		UPDATE public.entities e SET
			npi  = CASE WHEN e.npi  = '' THEN l.npi  ELSE e.npi  END,
			tin  = CASE WHEN e.tin  = '' THEN l.tin  ELSE e.tin  END,
			naic = CASE WHEN e.naic = '' THEN l.naic ELSE e.naic END,
			fein = CASE WHEN e.fein = '' THEN l.fein ELSE e.fein END,
			updated_at = now()
		FROM public.entities l
		WHERE e.tenant=$1 AND e.id=$2 AND l.id=$3`, tenant, in.IntoID, loser); err != nil {
		http.Error(w, `{"error":"merge failed moving identity keys"}`, http.StatusInternalServerError)
		return
	}
	if _, err := tx.Exec(r.Context(),
		`UPDATE public.entities SET status='MERGED', merged_into=$1, updated_at=now() WHERE tenant=$2 AND id=$3`,
		in.IntoID, tenant, loser); err != nil {
		http.Error(w, `{"error":"merge failed tombstoning"}`, http.StatusInternalServerError)
		return
	}
	if err := tx.Commit(r.Context()); err != nil {
		http.Error(w, `{"error":"db"}`, http.StatusInternalServerError)
		return
	}
	// NG: no audit_log — the entity_links rows moved and the loser's tombstone
	// (status MERGED, merged_into) are the record of the merge.
	writeJSON(w, http.StatusOK, map[string]any{"merged": loser, "into": in.IntoID})
}

// ---- Backfill (#2) ----------------------------------------------------------

// entityBackfill: POST /internal/entities/backfill — worker-token job that
// resolves every pre-existing source row that has no entity link yet.
// Idempotent: ensureEntity is a no-op for already-linked rows. ?limit=
// bounds one run; the job is re-runnable until "remaining" returns 0.
func (s *server) entityBackfill(w http.ResponseWriter, r *http.Request) {
	// Same gate as ledgerBackfill: admins and worker tokens only — it writes
	// entity/link rows tenant-wide.
	if p := r.Context().Value(ctxPrincipal{}).(principal); !hasAnyRole(p, "FEDERAL_ADMIN", "PLATFORM_ADMIN", serviceRole) {
		http.Error(w, `{"error":"forbidden"}`, http.StatusForbidden)
		return
	}
	tenant := r.Context().Value(ctxTenant{}).(string)
	limit := 500
	if n := r.URL.Query().Get("limit"); n != "" {
		fmt.Sscanf(n, "%d", &limit)
		if limit <= 0 || limit > 5000 {
			limit = 500
		}
	}
	type src struct {
		table, query string
	}
	// Each query returns unlinked rows oldest-first: id, name, type, npi, tin, naic, fein.
	sources := []src{
		{"accounts", `SELECT a.id::text, a.legal_name, a.type, coalesce(a.npi,''), '', '', ''
			FROM public.accounts a WHERE a.tenant=$1
			AND NOT EXISTS (SELECT 1 FROM public.entity_links l WHERE l.tenant=a.tenant AND l.source_table='accounts' AND l.source_id=a.id::text)
			ORDER BY a.created_at LIMIT $2`},
		{"tpas", `SELECT t.id::text, t.name, 'TPA', '', '', '', ''
			FROM public.tpas t WHERE t.tenant=$1
			AND NOT EXISTS (SELECT 1 FROM public.entity_links l WHERE l.tenant=t.tenant AND l.source_table='tpas' AND l.source_id=t.id::text)
			ORDER BY t.created_at LIMIT $2`},
		{"tpa_clients", `SELECT c.id::text, c.party_name, c.party_type, coalesce(c.npi,''), coalesce(c.tin,''), '', ''
			FROM public.tpa_clients c WHERE c.tenant=$1
			AND NOT EXISTS (SELECT 1 FROM public.entity_links l WHERE l.tenant=c.tenant AND l.source_table='tpa_clients' AND l.source_id=c.id::text)
			ORDER BY c.created_at LIMIT $2`},
		{"health_plans", `SELECT h.id::text, h.name, 'HEALTH_PLAN', '', '', coalesce(h.naic,''), coalesce(h.fein,'')
			FROM public.health_plans h WHERE h.tenant=$1
			AND NOT EXISTS (SELECT 1 FROM public.entity_links l WHERE l.tenant=h.tenant AND l.source_table='health_plans' AND l.source_id=h.id::text)
			ORDER BY h.created_at LIMIT $2`},
		{"hp_parties", `SELECT p.id::text, p.party_name, p.party_type, coalesce(p.npi,''), coalesce(p.tin,''), '', ''
			FROM public.hp_parties p WHERE p.tenant=$1
			AND NOT EXISTS (SELECT 1 FROM public.entity_links l WHERE l.tenant=p.tenant AND l.source_table='hp_parties' AND l.source_id=p.id::text)
			ORDER BY p.created_at LIMIT $2`},
	}
	linked, failed, remaining := 0, 0, 0
	for _, src := range sources {
		rows, err := s.db.Query(r.Context(), src.query, tenant, limit)
		if err != nil {
			continue // a rail the tenant never migrated is skipped, not fatal
		}
		type row struct{ id, name, typ, npi, tin, naic, fein string }
		var batch []row
		for rows.Next() {
			var rw row
			if rows.Scan(&rw.id, &rw.name, &rw.typ, &rw.npi, &rw.tin, &rw.naic, &rw.fein) == nil {
				batch = append(batch, rw)
			}
		}
		rows.Close()
		for _, rw := range batch {
			if _, err := s.ensureEntity(r.Context(), tenant, src.table, rw.id, rw.typ, rw.name, rw.npi, rw.tin, rw.naic, rw.fein, ""); err != nil {
				failed++
			} else {
				linked++
			}
		}
		// Any rows still unlinked after this pass (beyond the limit, or
		// failed) tell the caller whether to schedule another run.
		var left int
		_ = s.db.QueryRow(r.Context(), `
			SELECT count(*) FROM `+map[string]string{
			"accounts":     `public.accounts x WHERE x.tenant=$1 AND NOT EXISTS (SELECT 1 FROM public.entity_links l WHERE l.tenant=x.tenant AND l.source_table='accounts' AND l.source_id=x.id::text)`,
			"tpas":         `public.tpas x WHERE x.tenant=$1 AND NOT EXISTS (SELECT 1 FROM public.entity_links l WHERE l.tenant=x.tenant AND l.source_table='tpas' AND l.source_id=x.id::text)`,
			"tpa_clients":  `public.tpa_clients x WHERE x.tenant=$1 AND NOT EXISTS (SELECT 1 FROM public.entity_links l WHERE l.tenant=x.tenant AND l.source_table='tpa_clients' AND l.source_id=x.id::text)`,
			"health_plans": `public.health_plans x WHERE x.tenant=$1 AND NOT EXISTS (SELECT 1 FROM public.entity_links l WHERE l.tenant=x.tenant AND l.source_table='health_plans' AND l.source_id=x.id::text)`,
			"hp_parties":   `public.hp_parties x WHERE x.tenant=$1 AND NOT EXISTS (SELECT 1 FROM public.entity_links l WHERE l.tenant=x.tenant AND l.source_table='hp_parties' AND l.source_id=x.id::text)`,
		}[src.table], tenant).Scan(&left)
		remaining += left
	}
	s.refreshTenantCaseStats(r.Context(), tenant)
	writeJSON(w, http.StatusOK, map[string]any{"linked": linked, "failed": failed, "remaining": remaining})
}

// ---- Unlinked-source search (#4 support) ------------------------------------

// entitySourceSearch: GET /entities/sources/search?q=&source_table= — the
// portal's "link a record" picker: source rows matching a name, whether or
// not linked yet (the link state is shown so the picker can say "already
// linked elsewhere").
func (s *server) entitySourceSearch(w http.ResponseWriter, r *http.Request) {
	tenant := r.Context().Value(ctxTenant{}).(string)
	if _, ok := s.entity360Gate(w, r, tenant, entity360WriteRoles); !ok {
		return
	}
	q := strings.TrimSpace(r.URL.Query().Get("q"))
	if len(q) < 2 {
		writeJSON(w, http.StatusOK, map[string]any{"sources": []any{}})
		return
	}
	like := "%" + q + "%"
	queries := []struct {
		table, sql string
	}{
		{"accounts", `SELECT a.id::text, a.legal_name, a.type, coalesce(a.npi,'') AS key,
			(SELECT l.entity_id::text FROM public.entity_links l WHERE l.tenant=a.tenant AND l.source_table='accounts' AND l.source_id=a.id::text)
			FROM public.accounts a WHERE a.tenant=$1 AND a.legal_name ILIKE $2 LIMIT 8`},
		{"tpas", `SELECT t.id::text, t.name, 'TPA', '',
			(SELECT l.entity_id::text FROM public.entity_links l WHERE l.tenant=t.tenant AND l.source_table='tpas' AND l.source_id=t.id::text)
			FROM public.tpas t WHERE t.tenant=$1 AND t.name ILIKE $2 LIMIT 8`},
		{"tpa_clients", `SELECT c.id::text, c.party_name, c.party_type, coalesce(c.npi,''),
			(SELECT l.entity_id::text FROM public.entity_links l WHERE l.tenant=c.tenant AND l.source_table='tpa_clients' AND l.source_id=c.id::text)
			FROM public.tpa_clients c WHERE c.tenant=$1 AND c.party_name ILIKE $2 LIMIT 8`},
		{"health_plans", `SELECT h.id::text, h.name, 'HEALTH_PLAN', coalesce(h.naic,''),
			(SELECT l.entity_id::text FROM public.entity_links l WHERE l.tenant=h.tenant AND l.source_table='health_plans' AND l.source_id=h.id::text)
			FROM public.health_plans h WHERE h.tenant=$1 AND h.name ILIKE $2 LIMIT 8`},
		{"hp_parties", `SELECT p.id::text, p.party_name, p.party_type, coalesce(p.npi,''),
			(SELECT l.entity_id::text FROM public.entity_links l WHERE l.tenant=p.tenant AND l.source_table='hp_parties' AND l.source_id=p.id::text)
			FROM public.hp_parties p WHERE p.tenant=$1 AND p.party_name ILIKE $2 LIMIT 8`},
	}
	out := []map[string]any{}
	for _, q2 := range queries {
		rows, err := s.db.Query(r.Context(), q2.sql, tenant, like)
		if err != nil {
			continue
		}
		for rows.Next() {
			var id, name, typ, key string
			var linkedTo *string
			if rows.Scan(&id, &name, &typ, &key, &linkedTo) == nil {
				m := map[string]any{"source_table": q2.table, "source_id": id, "name": name,
					"kind": typ, "source_label": entity360Sources[q2.table]}
				if key != "" {
					m["identity_key"] = key
				}
				if linkedTo != nil {
					m["linked_to"] = *linkedTo
				}
				out = append(out, m)
			}
		}
		rows.Close()
	}
	writeJSON(w, http.StatusOK, map[string]any{"sources": out})
}

// ---- CRM outreach list (#14) ------------------------------------------------

// entityOutreach: GET /entity360/outreach?min_cases=&quiet_days= — entities
// with real dispute volume but no notes/correspondence in the quiet window:
// the platform team's "who should we be talking to" list.
func (s *server) entityOutreach(w http.ResponseWriter, r *http.Request) {
	tenant := r.Context().Value(ctxTenant{}).(string)
	if _, ok := s.entity360Gate(w, r, tenant, entity360ReadRoles); !ok {
		return
	}
	minCases, quietDays := 3, 90
	fmt.Sscanf(r.URL.Query().Get("min_cases"), "%d", &minCases)
	fmt.Sscanf(r.URL.Query().Get("quiet_days"), "%d", &quietDays)
	if minCases < 1 {
		minCases = 3
	}
	if quietDays < 7 {
		quietDays = 7
	}
	rows, err := s.db.Query(r.Context(), fmt.Sprintf(`
		WITH ent_cases AS (
			SELECT l.entity_id, co.case_id::text AS case_id
			FROM public.entity_links l
			JOIN public.case_origin co ON co.tenant=l.tenant AND (
				(l.source_table='tpas' AND co.tpa_id::text=l.source_id) OR
				(l.source_table='tpa_clients' AND co.client_id::text=l.source_id) OR
				(l.source_table='health_plans' AND co.health_plan_id::text=l.source_id) OR
				(l.source_table='hp_parties' AND co.hp_party_id::text=l.source_id))
			WHERE l.tenant=$1 AND co.case_id IS NOT NULL
			UNION
			SELECT l.entity_id, c.id::text
			FROM public.entity_links l
			JOIN tenant_%s.cases c ON c.provider_id=l.source_id OR c.payer_id=l.source_id
			WHERE l.tenant=$1 AND l.source_table='accounts'
		),
		stats AS (
			SELECT entity_id, count(DISTINCT case_id) AS cases
			FROM ent_cases GROUP BY entity_id HAVING count(DISTINCT case_id) >= $2
		),
		last_touch AS (
			SELECT l.entity_id, MAX(x.at) AS at FROM public.entity_links l
			JOIN LATERAL (
				SELECT n.created_at AS at FROM public.notes n
				WHERE n.tenant=l.tenant AND n.record_type='ACCOUNT'
				  AND l.source_table='accounts' AND n.record_id=l.source_id
				UNION ALL
				SELECT cl.created_at FROM public.correspondence_log cl
				JOIN ent_cases ec ON ec.entity_id=l.entity_id AND ec.case_id=cl.case_id
				WHERE cl.tenant=l.tenant
				UNION ALL
				SELECT pm.created_at FROM public.payments pm
				JOIN ent_cases ec ON ec.entity_id=l.entity_id AND ec.case_id=pm.case_id
				WHERE pm.tenant=l.tenant AND pm.status='PAID'
				UNION ALL
				SELECT ca.created_at FROM public.case_activities ca
				JOIN ent_cases ec ON ec.entity_id=l.entity_id AND ec.case_id=ca.case_id
				WHERE ca.tenant=l.tenant
			) x ON true
			WHERE l.tenant=$1 GROUP BY l.entity_id
		)
		SELECT e.id::text, e.canonical_name, e.entity_type, st.cases,
		       coalesce(to_char(lt.at, 'YYYY-MM-DD"T"HH24:MI'), '')
		FROM stats st
		JOIN public.entities e ON e.id=st.entity_id AND e.tenant=$1 AND e.status='ACTIVE'
		LEFT JOIN last_touch lt ON lt.entity_id=st.entity_id
		WHERE lt.at IS NULL OR lt.at < now() - make_interval(days => $3)
		ORDER BY st.cases DESC LIMIT 50`, sanitizeTenant(tenant)), tenant, minCases, quietDays)
	if err != nil {
		http.Error(w, `{"error":"query failed"}`, http.StatusInternalServerError)
		return
	}
	defer rows.Close()
	out := []map[string]any{}
	for rows.Next() {
		var id, name, typ, lastTouch string
		var cases int
		if rows.Scan(&id, &name, &typ, &cases, &lastTouch) == nil {
			out = append(out, map[string]any{"entity_id": id, "canonical_name": name,
				"entity_type": typ, "cases": cases, "last_engagement": lastTouch})
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"outreach": out, "min_cases": minCases, "quiet_days": quietDays})
}

// ensureEntityQuiet: the write-time hook — resolve a freshly created source
// row to its entity, best-effort (errors logged nowhere by design: the
// caller's real work already committed, and lookup re-resolves lazily).
func (s *server) ensureEntityQuiet(ctx context.Context, tenant, sourceTable, sourceID, entityType, name, npi, tin, naic, fein string) {
	_, _ = s.ensureEntity(ctx, tenant, sourceTable, sourceID, entityType, name, npi, tin, naic, fein, "")
}

// createLead: POST /leads — the CRM outreach list (#14) needs a write path:
// entity360 outreach rows become leads staff can work through the normal
// pipeline (WORKING → CONVERTED/DISQUALIFIED).
func (s *server) createLead(w http.ResponseWriter, r *http.Request) {
	tenant := r.Context().Value(ctxTenant{}).(string)
	_, ok := s.entity360Gate(w, r, tenant, entity360WriteRoles)
	if !ok {
		return
	}
	var in struct {
		Name         string `json:"name"`
		Organization string `json:"organization"`
		Email        string `json:"email"`
		Phone        string `json:"phone"`
		Summary      string `json:"summary"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil ||
		(strings.TrimSpace(in.Name) == "" && strings.TrimSpace(in.Organization) == "") {
		http.Error(w, `{"error":"name or organization required"}`, http.StatusBadRequest)
		return
	}
	var id string
	if err := s.db.QueryRow(r.Context(), `
		INSERT INTO public.leads (tenant, source, name, organization, email, phone, summary, status)
		VALUES ($1,'OUTREACH', NULLIF($2,''), NULLIF($3,''), NULLIF($4,''), NULLIF($5,''), NULLIF($6,''), 'NEW')
		RETURNING id`, tenant, in.Name, in.Organization, in.Email, in.Phone, in.Summary).Scan(&id); err != nil {
		http.Error(w, `{"error":"save failed"}`, http.StatusInternalServerError)
		return
	}
	// NG: no audit_log — the lead row itself (source OUTREACH, created_at) is the record.
	writeJSON(w, http.StatusCreated, map[string]any{"lead_id": id})
}

// refreshTenantCaseStats: recompute the tenant-wide benchmark row the 360
// performance panel reads (#9 follow-up). Best-effort: the panel falls back
// to a live compute when no fresh row exists. Called by the backfill job
// and opportunistically after a live compute.
func (s *server) refreshTenantCaseStats(ctx context.Context, tenant string) {
	tcase := fmt.Sprintf("tenant_%s.cases", sanitizeTenant(tenant))
	_, _ = s.db.Exec(ctx, fmt.Sprintf(`
		INSERT INTO public.tenant_case_stats (tenant, closed_cases, avg_days, median_days, computed_at)
		SELECT $1, count(*), round(avg(days),1),
		       round(percentile_cont(0.5) WITHIN GROUP (ORDER BY days)::numeric,1), now()
		FROM (SELECT extract(epoch FROM (updated_at - opened_at))/86400.0 AS days
		      FROM %s
		      WHERE status LIKE 'CLOSED%%%%' OR status IN
		      ('SETTLED_IN_NEGOTIATION','Decided - Invoice Paid','Invoice Paid','Dismissed','Withdrawn','Ineligible','Plan Opt-Out')) closed
		ON CONFLICT (tenant) DO UPDATE SET closed_cases=excluded.closed_cases,
		  avg_days=excluded.avg_days, median_days=excluded.median_days, computed_at=now()`, tcase), tenant)
	_, _ = s.db.Exec(ctx, fmt.Sprintf(`
		UPDATE public.tenant_case_stats st SET above_qpa_pct = (
			SELECT round(100.0 * count(*) FILTER (WHERE d.amount_cents > c.qpa_cents)
			             / nullif(count(*) FILTER (WHERE c.qpa_cents IS NOT NULL),0), 1)
			FROM public.determinations d JOIN %s c ON c.id::text = d.case_id
			WHERE d.tenant=$1)
		WHERE st.tenant=$1`, tcase), tenant)
}
