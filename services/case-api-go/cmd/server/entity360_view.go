package main

import (
	"context"
	"fmt"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"
)

// entity360_view.go — GET /entity360/{entityId}: the six-panel aggregate.
// Every panel is computed independently and tolerates its own query failing
// (a tenant that has not run one of the later migrations still gets the
// other five panels, with the failed panel's "error" noted) — one missing
// column must never blank the whole 360.

// entityLinks loads the entity's links, then the display row for each
// linked source (name/type/status from the source's own table).
func (s *server) entityLinks(ctx context.Context, tenant, entityID string) ([]map[string]any, map[string][]string) {
	rows, err := s.db.Query(ctx, `
		SELECT id, source_table, source_id, link_kind, linked_by, created_at
		FROM public.entity_links WHERE tenant=$1 AND entity_id=$2
		ORDER BY source_table, created_at`, tenant, entityID)
	if err != nil {
		return []map[string]any{}, map[string][]string{}
	}
	defer rows.Close()
	type link struct {
		id     int64
		table  string
		srcID  string
		kind   string
		by     string
		at     any
	}
	var links []link
	byTable := map[string][]string{}
	for rows.Next() {
		var l link
		if rows.Scan(&l.id, &l.table, &l.srcID, &l.kind, &l.by, &l.at) == nil {
			links = append(links, l)
			byTable[l.table] = append(byTable[l.table], l.srcID)
		}
	}
	// Display info per source row. Errors on any one table leave its links
	// with just the raw reference — the link list itself still renders.
	detail := map[string]map[string]any{}
	load := func(table, query string) {
		for _, srcID := range byTable[table] {
			r, qerr := s.queryRowMap(ctx, query, tenant, srcID)
			if qerr == nil {
				detail[table+":"+srcID] = r
			}
		}
	}
	load("accounts", `SELECT legal_name AS name, type AS kind, '' AS status FROM public.accounts WHERE tenant=$1 AND id=$2`)
	load("tpas", `SELECT name, 'TPA' AS kind, status FROM public.tpas WHERE tenant=$1 AND id=$2`)
	load("tpa_clients", `SELECT party_name AS name, party_type AS kind,
		coalesce(onboarding_status, status) AS status, coalesce(npi_status,'') AS npi_status
		FROM public.tpa_clients WHERE tenant=$1 AND id=$2`)
	load("health_plans", `SELECT name, coalesce(line_of_business,'HEALTH_PLAN') AS kind, status
		FROM public.health_plans WHERE tenant=$1 AND id=$2`)
	load("hp_parties", `SELECT party_name AS name, party_type AS kind,
		coalesce(onboarding_status, status) AS status, coalesce(npi_status,'') AS npi_status
		FROM public.hp_parties WHERE tenant=$1 AND id=$2`)

	out := []map[string]any{}
	for _, l := range links {
		m := map[string]any{
			"link_id": l.id, "source_table": l.table, "source_id": l.srcID,
			"source_label": entity360Sources[l.table], "link_kind": l.kind,
			"linked_by": l.by, "created_at": l.at,
		}
		if d, ok := detail[l.table+":"+l.srcID]; ok {
			m["detail"] = d
		}
		out = append(out, m)
	}
	return out, byTable
}

// queryRowMap: one row as a map (queryRows' single-row sibling).
func (s *server) queryRowMap(ctx context.Context, query string, args ...any) (map[string]any, error) {
	rows, err := s.db.Query(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	cols := rows.FieldDescriptions()
	if !rows.Next() {
		return nil, fmt.Errorf("no rows")
	}
	vals, err := rows.Values()
	if err != nil {
		return nil, err
	}
	out := map[string]any{}
	for i, c := range cols {
		out[string(c.Name)] = vals[i]
	}
	return out, nil
}

// entityCaseIDs: every case touching any of the entity's source rows — via
// case_origin (who filed / who it belongs to) and via CRM account ids on the
// case itself (provider_id / payer_id).
func (s *server) entityCaseIDs(ctx context.Context, tenant string, byTable map[string][]string) []string {
	set := map[string]bool{}
	cond := []string{}
	args := []any{tenant}
	addIn := func(col string, ids []string) {
		if len(ids) == 0 {
			return
		}
		args = append(args, ids)
		cond = append(cond, fmt.Sprintf("%s = ANY($%d)", col, len(args)))
	}
	addIn("tpa_id", byTable["tpas"])
	addIn("client_id", byTable["tpa_clients"])
	addIn("health_plan_id", byTable["health_plans"])
	addIn("hp_party_id", byTable["hp_parties"])
	if len(cond) > 0 {
		rows, err := s.db.Query(ctx, fmt.Sprintf(
			`SELECT case_id::text FROM public.case_origin WHERE tenant=$1 AND case_id IS NOT NULL AND (%s)`,
			strings.Join(cond, " OR ")), args...)
		if err == nil {
			defer rows.Close()
			for rows.Next() {
				var id string
				if rows.Scan(&id) == nil {
					set[id] = true
				}
			}
		}
	}
	if accts := byTable["accounts"]; len(accts) > 0 {
		rows, err := s.db.Query(ctx, fmt.Sprintf(`
			SELECT id::text FROM tenant_%s.cases
			WHERE provider_id = ANY($1) OR payer_id = ANY($1)`, sanitizeTenant(tenant)), accts)
		if err == nil {
			defer rows.Close()
			for rows.Next() {
				var id string
				if rows.Scan(&id) == nil {
					set[id] = true
				}
			}
		}
	}
	out := make([]string, 0, len(set))
	for id := range set {
		out = append(out, id)
	}
	return out
}

// entityCaseSides: the same case universe split by which side of the dispute
// the entity sat on. Initiating = filed through one of its org rows
// (case_origin) or as the CRM provider account; responding = named as the
// payer account. A case can be on both sides for a self-funded TPA entity.
func (s *server) entityCaseSides(ctx context.Context, tenant string, byTable map[string][]string) (initiating, responding []string) {
	iSet, rSet := map[string]bool{}, map[string]bool{}
	cond := []string{}
	args := []any{tenant}
	addIn := func(col string, ids []string) {
		if len(ids) == 0 {
			return
		}
		args = append(args, ids)
		cond = append(cond, fmt.Sprintf("%s = ANY($%d)", col, len(args)))
	}
	addIn("tpa_id", byTable["tpas"])
	addIn("client_id", byTable["tpa_clients"])
	addIn("health_plan_id", byTable["health_plans"])
	addIn("hp_party_id", byTable["hp_parties"])
	if len(cond) > 0 {
		rows, err := s.db.Query(ctx, fmt.Sprintf(
			`SELECT case_id::text FROM public.case_origin WHERE tenant=$1 AND case_id IS NOT NULL AND (%s)`,
			strings.Join(cond, " OR ")), args...)
		if err == nil {
			defer rows.Close()
			for rows.Next() {
				var id string
				if rows.Scan(&id) == nil {
					iSet[id] = true
				}
			}
		}
	}
	if accts := byTable["accounts"]; len(accts) > 0 {
		tcase := fmt.Sprintf("tenant_%s.cases", sanitizeTenant(tenant))
		rows, err := s.db.Query(ctx, fmt.Sprintf(
			`SELECT id::text, (payer_id = ANY($1)) AS responding FROM %s
			 WHERE provider_id = ANY($1) OR payer_id = ANY($1)`, tcase), accts)
		if err == nil {
			defer rows.Close()
			for rows.Next() {
				var id string
				var resp bool
				if rows.Scan(&id, &resp) == nil {
					if resp {
						rSet[id] = true
					} else {
						iSet[id] = true
					}
				}
			}
		}
	}
	for id := range iSet {
		initiating = append(initiating, id)
	}
	for id := range rSet {
		responding = append(responding, id)
	}
	return initiating, responding
}

// caseStage: ordered pipeline stage for any status vocabulary the platform
// carries — federal IDRE, FL AHCA CDR, and the CRM lifecycle. Stages:
// 0 FILED, 1 NEGOTIATION, 2 IDRE, 3 DETERMINED, 4 PAID, 5 CLOSED_OTHER.
// Terminal-but-unpaid outcomes (Dismissed/Withdrawn/Ineligible/…) sit in
// CLOSED_OTHER so the funnel's conversion to PAID stays honest.
func caseStage(status string) int {
	s := strings.ToUpper(status)
	switch {
	case strings.Contains(s, "PAID"):
		return 4
	case strings.HasPrefix(s, "DECIDED") || strings.Contains(s, "DETERMIN"):
		return 3
	case strings.Contains(s, "IDRE") || strings.Contains(s, "SELECTED") ||
		strings.Contains(s, "DISPUTE_REVIEW") || strings.Contains(s, "UNDER_REVIEW"):
		return 2
	case strings.Contains(s, "NEGOTIATION") || strings.Contains(s, "SETTLED"):
		return 1
	case strings.HasPrefix(s, "CLOSED") || strings.Contains(s, "DISMISS") ||
		strings.Contains(s, "WITHDRAW") || strings.Contains(s, "INELIGIBLE") ||
		strings.Contains(s, "OPT-OUT") || strings.Contains(s, "OPT_OUT"):
		return 5
	default:
		return 0
	}
}

var caseStageNames = []string{"FILED", "NEGOTIATION", "IDRE", "DETERMINED", "PAID", "CLOSED_OTHER"}

// caseFunnel: ordered stages with counts currently at each stage, the number
// that reached each stage (this stage or beyond, CLOSED_OTHER excluded from
// "beyond PAID" so terminal unpaid outcomes don't inflate conversion), and
// stage-to-stage conversion percentages.
func caseFunnel(byStatus []map[string]any) []map[string]any {
	at := make([]int, 6)
	for _, row := range byStatus {
		st, _ := row["status"].(string)
		n := 0
		switch v := row["count"].(type) {
		case int:
			n = v
		case int64:
			n = int(v)
		}
		at[caseStage(st)] += n
	}
	reached := make([]int, 6)
	reached[5] = at[5] // terminal-unpaid: its own stage only, never "beyond PAID"
	run := 0
	for i := 4; i >= 0; i-- {
		run += at[i]
		reached[i] = run
	}
	funnel := make([]map[string]any, 0, 6)
	for i, name := range caseStageNames {
		stage := map[string]any{"stage": name, "count": at[i], "reached": reached[i]}
		if i > 0 && reached[i-1] > 0 {
			stage["conversion_pct"] = float64(reached[i]) * 100.0 / float64(reached[i-1])
		}
		funnel = append(funnel, stage)
	}
	return funnel
}

func (s *server) entity360View(w http.ResponseWriter, r *http.Request) {
	tenant := r.Context().Value(ctxTenant{}).(string)
	if _, ok := s.entity360Gate(w, r, tenant, entity360ReadRoles); !ok {
		return
	}
	entityID := chi.URLParam(r, "entityId")
	ctx := r.Context()

	entity, err := s.queryRowMap(ctx, `
		SELECT id::text, entity_type, canonical_name, npi, tin, naic, fein, status, created_at
		FROM public.entities WHERE tenant=$1 AND id=$2 AND status='ACTIVE'`, tenant, entityID)
	if err != nil {
		http.Error(w, `{"error":"entity not found"}`, http.StatusNotFound)
		return
	}
	links, byTable := s.entityLinks(ctx, tenant, entityID)
	caseIDs := s.entityCaseIDs(ctx, tenant, byTable)
	initiating, responding := s.entityCaseSides(ctx, tenant, byTable)
	tcase := fmt.Sprintf("tenant_%s.cases", sanitizeTenant(tenant))
	hasCases := len(caseIDs) > 0
	resp := map[string]any{"entity": entity, "links": links}

	// ── Panel: pipeline funnel ──────────────────────────────────────────
	pipeline := map[string]any{"total": 0, "open": 0, "closed": 0, "by_status": []map[string]any{}, "recent": []map[string]any{}}
	if hasCases {
		if rows, qerr := s.db.Query(ctx, fmt.Sprintf(`
			SELECT status, count(*) FROM %s WHERE id = ANY($1) GROUP BY status ORDER BY 2 DESC`, tcase), caseIDs); qerr == nil {
			defer rows.Close()
			for rows.Next() {
				var st string
				var n int
				if rows.Scan(&st, &n) == nil {
					pipeline["by_status"] = append(pipeline["by_status"].([]map[string]any), map[string]any{"status": st, "count": n})
					pipeline["total"] = pipeline["total"].(int) + n
					if strings.HasPrefix(st, "CLOSED") || st == "SETTLED_IN_NEGOTIATION" ||
						st == "Decided - Invoice Paid" || st == "Invoice Paid" || st == "Ineligible" ||
						st == "Dismissed" || st == "Withdrawn" || st == "Plan Opt-Out" {
						pipeline["closed"] = pipeline["closed"].(int) + n
					} else {
						pipeline["open"] = pipeline["open"].(int) + n
					}
				}
			}
		} else {
			pipeline["error"] = "unavailable"
		}
		if recent, qerr := s.queryRows(r, fmt.Sprintf(`
			SELECT id::text, case_number, status, coalesce(service_line,''), opened_at
			FROM %s WHERE id = ANY($1) ORDER BY opened_at DESC LIMIT 25`, tcase), caseIDs); qerr == nil {
			pipeline["recent"] = recent
		}
		pipeline["funnel"] = caseFunnel(pipeline["by_status"].([]map[string]any))
	}
	// Initiator vs respondent split (#11): counts per side, so the portal can
	// render "filed N / answered M" and flip panels per side.
	pipeline["initiating"] = len(initiating)
	pipeline["responding"] = len(responding)
	resp["pipeline"] = pipeline

	// ── Panel: financials ───────────────────────────────────────────────
	financials := map[string]any{}
	if hasCases {
		if m, qerr := s.queryRowMap(ctx, `
			SELECT count(*) AS invoices,
			       coalesce(sum(amount_cents) FILTER (WHERE status='PAID'),0) AS collected_cents,
			       coalesce(sum(amount_cents) FILTER (WHERE status='OPEN'),0) AS open_cents,
			       coalesce(sum(amount_cents),0) AS invoiced_cents
			FROM public.invoices WHERE tenant=$1 AND case_id = ANY($2)`, tenant, caseIDs); qerr == nil {
			for k, v := range m {
				financials["fee_"+k] = v
			}
		}
		if m, qerr := s.queryRowMap(ctx, `
			SELECT count(*) FILTER (WHERE status='PAID') AS payments,
			       coalesce(sum(amount_cents) FILTER (WHERE status='PAID'),0) AS paid_cents,
			       coalesce(sum(amount_cents) FILTER (WHERE status='REFUNDED'),0) AS refunded_cents
			FROM public.payments WHERE tenant=$1 AND case_id = ANY($2)`, tenant, caseIDs); qerr == nil {
			for k, v := range m {
				financials[k] = v
			}
		}
		if m, qerr := s.queryRowMap(ctx, `
			SELECT count(*) AS invoices,
			       coalesce(sum(total_cents) FILTER (WHERE status='PAID'),0) AS collected_cents,
			       coalesce(sum(total_cents) FILTER (WHERE status IN ('ISSUED','APPROVED')),0) AS outstanding_cents
			FROM public.billing_invoices WHERE tenant=$1 AND case_id = ANY($2)`, tenant, caseIDs); qerr == nil {
			for k, v := range m {
				financials["billing_"+k] = v
			}
		}
		// Aging buckets on OPEN fee invoices (#8): what is owed, aged from
		// the invoice date, plus the oldest unpaid invoice in days.
		if m, qerr := s.queryRowMap(ctx, `
			SELECT coalesce(sum(amount_cents) FILTER (WHERE age <= 30),0) AS bucket_0_30,
			       coalesce(sum(amount_cents) FILTER (WHERE age BETWEEN 31 AND 60),0) AS bucket_31_60,
			       coalesce(sum(amount_cents) FILTER (WHERE age BETWEEN 61 AND 90),0) AS bucket_61_90,
			       coalesce(sum(amount_cents) FILTER (WHERE age > 90),0) AS bucket_90_plus,
			       coalesce(max(age),0)::bigint AS oldest_open_days
			FROM (SELECT amount_cents, extract(day FROM now() - created_at)::int AS age
			      FROM public.invoices
			      WHERE tenant=$1 AND status='OPEN' AND case_id = ANY($2)) open_inv`, tenant, caseIDs); qerr == nil {
			financials["aging"] = m
		}
	}
	// Checkout history scoped to the org itself (not just its cases): a
	// TPA's client-level and plan-level fee checkouts.
	pgCond, pgArgs := []string{}, []any{tenant}
	for _, kv := range [][2]string{{"client_id", ""}, {"plan_id", ""}} {
		var ids []string
		if kv[0] == "client_id" {
			ids = byTable["tpa_clients"]
		} else {
			ids = byTable["health_plans"]
		}
		if len(ids) > 0 {
			pgArgs = append(pgArgs, ids)
			pgCond = append(pgCond, fmt.Sprintf("%s = ANY($%d)", kv[0], len(pgArgs)))
		}
	}
	if len(pgCond) > 0 {
		if m, qerr := s.queryRowMap(ctx, fmt.Sprintf(`
			SELECT count(*) AS checkouts,
			       coalesce(sum(total_cents) FILTER (WHERE status='PAID'),0) AS paid_cents
			FROM public.payment_groups WHERE tenant=$1 AND (%s)`, strings.Join(pgCond, " OR ")), pgArgs...); qerr == nil {
			for k, v := range m {
				financials["checkout_"+k] = v
			}
		}
	}
	resp["financials"] = financials

	// ── Panel: performance ──────────────────────────────────────────────
	performance := map[string]any{}
	if hasCases {
		if m, qerr := s.queryRowMap(ctx, fmt.Sprintf(`
			SELECT count(*) FILTER (WHERE status LIKE 'CLOSED%%' OR status IN
			         ('SETTLED_IN_NEGOTIATION','Decided - Invoice Paid','Invoice Paid','Dismissed','Withdrawn','Ineligible','Plan Opt-Out')) AS closed,
			       round(avg(extract(epoch FROM (updated_at - opened_at))/86400.0)
			         FILTER (WHERE status LIKE 'CLOSED%%' OR status IN
			         ('SETTLED_IN_NEGOTIATION','Decided - Invoice Paid','Invoice Paid','Dismissed','Withdrawn','Ineligible','Plan Opt-Out')), 1) AS avg_days_to_close
			FROM %s WHERE id = ANY($1)`, tcase), caseIDs); qerr == nil {
			for k, v := range m {
				performance[k] = v
			}
		}
		if m, qerr := s.queryRowMap(ctx, fmt.Sprintf(`
			SELECT count(*) AS determinations,
			       coalesce(round(avg(d.amount_cents)),0) AS avg_award_cents,
			       count(*) FILTER (WHERE c.qpa_cents IS NOT NULL AND d.amount_cents > c.qpa_cents) AS above_qpa
			FROM public.determinations d
			JOIN %s c ON c.id::text = d.case_id
			WHERE d.tenant=$1 AND d.case_id = ANY($2)`, tcase), tenant, caseIDs); qerr == nil {
			for k, v := range m {
				performance[k] = v
			}
		}
	}
	if m, qerr := s.queryRowMap(ctx, `
		SELECT count(*) AS sla_breaches, count(DISTINCT clock) AS breach_clocks
		FROM public.sla_breaches WHERE tenant=$1 AND case_id = ANY($2)`, tenant, caseIDs); qerr == nil {
		for k, v := range m {
			performance[k] = v
		}
	}
	// Tenant-wide benchmark (#9): the entity's numbers against the whole
	// program — tenant avg/median days to close, above-QPA rate, and the
	// share of tenant cases that closed slower than this entity's average
	// (a percentile-style comparison without a cross-entity scan).
	benchmark := map[string]any{}
	if m, qerr := s.queryRowMap(ctx, fmt.Sprintf(`
		SELECT round(avg(days),1) AS tenant_avg_days_to_close,
		       round(percentile_cont(0.5) WITHIN GROUP (ORDER BY days)::numeric,1) AS tenant_median_days_to_close,
		       count(*) AS tenant_closed_cases
		FROM (SELECT extract(epoch FROM (updated_at - opened_at))/86400.0 AS days
		      FROM %s
		      WHERE status LIKE 'CLOSED%%%%' OR status IN
		      ('SETTLED_IN_NEGOTIATION','Decided - Invoice Paid','Invoice Paid','Dismissed','Withdrawn','Ineligible','Plan Opt-Out')) closed`, tcase)); qerr == nil {
		for k, v := range m {
			benchmark[k] = v
		}
	}
	if m, qerr := s.queryRowMap(ctx, fmt.Sprintf(`
		SELECT round(100.0 * count(*) FILTER (WHERE d.amount_cents > c.qpa_cents)
		             / nullif(count(*) FILTER (WHERE c.qpa_cents IS NOT NULL),0), 1) AS tenant_above_qpa_pct
		FROM public.determinations d JOIN %s c ON c.id::text = d.case_id
		WHERE d.tenant=$1`, tcase), tenant); qerr == nil {
		for k, v := range m {
			benchmark[k] = v
		}
	}
	if avgDays, ok := performance["avg_days_to_close"]; ok && avgDays != nil {
		if m, qerr := s.queryRowMap(ctx, fmt.Sprintf(`
			SELECT round(100.0 * count(*) FILTER (WHERE days > $1)
			             / nullif(count(*),0), 1) AS faster_than_pct
			FROM (SELECT extract(epoch FROM (updated_at - opened_at))/86400.0 AS days
			      FROM %s
			      WHERE status LIKE 'CLOSED%%%%' OR status IN
			      ('SETTLED_IN_NEGOTIATION','Decided - Invoice Paid','Invoice Paid','Dismissed','Withdrawn','Ineligible','Plan Opt-Out')) closed`, tcase), avgDays); qerr == nil {
			for k, v := range m {
				benchmark[k] = v
			}
		}
	}
	if dets, ok := performance["determinations"]; ok {
		if n, isNum := dets.(int64); isNum && n > 0 {
			if above, ok2 := performance["above_qpa"].(int64); ok2 {
				benchmark["entity_above_qpa_pct"] = float64(above) * 100.0 / float64(n)
			}
		}
	}
	performance["benchmark"] = benchmark
	resp["performance"] = performance

	// ── Panel: compliance & risk ────────────────────────────────────────
	compliance := map[string]any{}
	if rows, qerr := s.db.Query(ctx, `
		SELECT clock, count(*) FROM public.sla_breaches
		WHERE tenant=$1 AND case_id = ANY($2) GROUP BY clock ORDER BY 2 DESC`, tenant, caseIDs); qerr == nil {
		defer rows.Close()
		clocks := []map[string]any{}
		for rows.Next() {
			var clock string
			var n int
			if rows.Scan(&clock, &n) == nil {
				clocks = append(clocks, map[string]any{"clock": clock, "count": n})
			}
		}
		compliance["breach_clocks"] = clocks
	}
	// Onboarding package document verdicts for every org row this entity
	// links to (both rails share the six-slot validation pipeline).
	docVerdicts := map[string]int{}
	addVerdicts := func(query string, ids []string) {
		if len(ids) == 0 {
			return
		}
		rows, qerr := s.db.Query(ctx, query, tenant, ids)
		if qerr != nil {
			return
		}
		defer rows.Close()
		for rows.Next() {
			var st string
			var n int
			if rows.Scan(&st, &n) == nil {
				docVerdicts[st] += n
			}
		}
	}
	addVerdicts(`SELECT d.status, count(*) FROM public.tpa_package_docs d
		JOIN public.tpa_packages pk ON pk.id = d.package_id
		WHERE pk.tenant=$1 AND pk.tpa_id = ANY($2) GROUP BY d.status`, byTable["tpas"])
	addVerdicts(`SELECT d.status, count(*) FROM public.tpa_package_docs d
		JOIN public.tpa_packages pk ON pk.id = d.package_id
		WHERE pk.tenant=$1 AND pk.client_id = ANY($2) GROUP BY d.status`, byTable["tpa_clients"])
	addVerdicts(`SELECT d.status, count(*) FROM public.hp_package_docs d
		JOIN public.hp_packages pk ON pk.id = d.package_id
		WHERE pk.tenant=$1 AND pk.plan_id = ANY($2) GROUP BY d.status`, byTable["health_plans"])
	addVerdicts(`SELECT d.status, count(*) FROM public.hp_package_docs d
		JOIN public.hp_packages pk ON pk.id = d.package_id
		WHERE pk.tenant=$1 AND pk.party_id = ANY($2) GROUP BY d.status`, byTable["hp_parties"])
	compliance["doc_verdicts"] = docVerdicts
	// NPI registry flags on linked clients/parties (NEEDS_REVIEW / REJECTED
	// are the risk signal; VERIFIED/UNVERIFIED are informational).
	npiFlags := []map[string]any{}
	for _, q := range []struct {
		table, label string
	}{{"tpa_clients", "TPA client"}, {"hp_parties", "health plan party"}} {
		for _, srcID := range byTable[q.table] {
			if r, qerr := s.queryRowMap(ctx, fmt.Sprintf(`
				SELECT party_name AS name, npi_status, npi_registry_name FROM public.%s
				WHERE tenant=$1 AND id=$2 AND npi_status IN ('NEEDS_REVIEW','REJECTED')`, q.table), tenant, srcID); qerr == nil {
				r["source"] = q.label
				npiFlags = append(npiFlags, r)
			}
		}
	}
	compliance["npi_flags"] = npiFlags
	resp["compliance"] = compliance

	// ── Panel: engagement timeline ──────────────────────────────────────
	engagement := map[string]any{"notes": []map[string]any{}, "correspondence": []map[string]any{}, "audit": []map[string]any{}}
	if accts := byTable["accounts"]; len(accts) > 0 {
		if notes, qerr := s.queryRows(r, `
			SELECT stream, body, author, created_at FROM public.notes
			WHERE tenant=$1 AND record_type='ACCOUNT' AND record_id = ANY($2)
			ORDER BY created_at DESC LIMIT 25`, tenant, accts); qerr == nil {
			engagement["notes"] = notes
		}
	}
	if hasCases {
		if corr, qerr := s.queryRows(r, `
			SELECT case_id, direction, channel, subject, sent_by, created_at
			FROM public.correspondence_log
			WHERE tenant=$1 AND case_id = ANY($2) ORDER BY created_at DESC LIMIT 25`, tenant, caseIDs); qerr == nil {
			engagement["correspondence"] = corr
		}
		// NG has no public.audit_log — the case's own activity stream is the record.
		if acts, qerr := s.queryRows(r, `
			SELECT case_id, type, body, created_at FROM public.case_activities
			WHERE tenant=$1 AND case_id = ANY($2) ORDER BY created_at DESC LIMIT 30`, tenant, caseIDs); qerr == nil {
			engagement["audit"] = acts
		}
	}
	// Unified timeline (#12): one reverse-chronological stream across notes,
	// correspondence, case audit, and payments — the portal renders a single
	// feed instead of four tabs. Each leg is optional (a tenant missing one
	// table still gets the rest).
	tlParts := []string{}
	tlArgs := []any{}
	if accts := byTable["accounts"]; len(accts) > 0 {
		tlArgs = append(tlArgs, accts)
		tlParts = append(tlParts, fmt.Sprintf(`
			SELECT created_at AS at, 'NOTE' AS kind, left(body,160) AS summary, record_id AS case_id
			FROM public.notes WHERE tenant='%s' AND record_type='ACCOUNT' AND record_id = ANY($%d)`,
			sanitizeTenant(tenant), len(tlArgs)))
	}
	if hasCases {
		tlArgs = append(tlArgs, caseIDs)
		n := len(tlArgs)
		tlParts = append(tlParts, fmt.Sprintf(`
			SELECT created_at, 'CORRESPONDENCE', direction||' '||channel||': '||coalesce(subject,''), case_id
			FROM public.correspondence_log WHERE tenant='%s' AND case_id = ANY($%d)`, sanitizeTenant(tenant), n))
		// NG: the activity stream stands in for an audit log in the timeline too.
		tlParts = append(tlParts, fmt.Sprintf(`
			SELECT created_at, 'ACTIVITY', type||': '||left(coalesce(body,''),80), case_id
			FROM public.case_activities WHERE tenant='%s' AND case_id = ANY($%d)`, sanitizeTenant(tenant), n))
		tlParts = append(tlParts, fmt.Sprintf(`
			SELECT created_at, 'PAYMENT', status||' '||(amount_cents/100.0)::text, case_id
			FROM public.payments WHERE tenant='%s' AND case_id = ANY($%d)`, sanitizeTenant(tenant), n))
	}
	if len(tlParts) > 0 {
		if tl, qerr := s.queryRows(r, `
			SELECT at, kind, summary, case_id FROM (`+strings.Join(tlParts, " UNION ALL ")+`) t
			ORDER BY at DESC LIMIT 50`, tlArgs...); qerr == nil {
			engagement["timeline"] = tl
		}
	}
	resp["engagement"] = engagement

	// ── Panel: composite risk score (#13) ─────────────────────────────────
	// A 0–100 rollup of the signals already gathered above, so staff can
	// triage entities without reading every panel: NPI registry flags (20
	// each), rejected/needs-review package documents (10 each), SLA breach
	// rate against the entity's cases (up to 15), and a fee invoice unpaid
	// for 90+ days (10). Bands: <25 clear, <60 watch, otherwise review.
	riskFactors := []map[string]any{}
	risk := 0
	if n := len(npiFlags); n > 0 {
		riskFactors = append(riskFactors, map[string]any{"factor": "npi_registry_flags", "count": n, "points": n * 20})
		risk += n * 20
	}
	badDocs := docVerdicts["REJECTED"] + docVerdicts["NEEDS_REVIEW"]
	if badDocs > 0 {
		riskFactors = append(riskFactors, map[string]any{"factor": "failed_package_documents", "count": badDocs, "points": badDocs * 10})
		risk += badDocs * 10
	}
	if breaches, ok := performance["sla_breaches"].(int64); ok && breaches > 0 && len(caseIDs) > 0 {
		rate := float64(breaches) / float64(len(caseIDs))
		pts := int(rate * 15)
		if pts > 15 {
			pts = 15
		}
		if pts > 0 {
			riskFactors = append(riskFactors, map[string]any{"factor": "sla_breach_rate", "breaches": breaches, "points": pts})
			risk += pts
		}
	}
	if aging, ok := financials["aging"].(map[string]any); ok {
		if oldest, ok2 := aging["oldest_open_days"].(int64); ok2 && oldest > 90 {
			riskFactors = append(riskFactors, map[string]any{"factor": "fee_invoice_unpaid_90_plus", "days": oldest, "points": 10})
			risk += 10
		}
	}
	if risk > 100 {
		risk = 100
	}
	band := "clear"
	if risk >= 60 {
		band = "review"
	} else if risk >= 25 {
		band = "watch"
	}
	resp["risk"] = map[string]any{"score": risk, "band": band, "factors": riskFactors}

	writeJSON(w, http.StatusOK, resp)
}
