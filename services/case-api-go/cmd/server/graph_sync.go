// Author: Patrick Munis at newwave
package main

// graph_sync.go — the projector: derives the tenant-wide node/edge projection
// from the relational system of record and fans it out to every configured
// non-sql graph backend (graph_provider.go). Triggered by
// POST /internal/entities/graph/sync and opportunistically after entity
// backfill. Graphs are disposable: sync drops and rebuilds the tenant graph
// wholesale, which keeps drift impossible by construction.

import (
	"context"
	"fmt"
	"net/http"
)

// graphProjection builds the full tenant subgraph: ACTIVE entities as nodes;
// OPPOSES edges from shared disputes (case provider↔payer accounts, plus
// cross-org pairs on the filing rail); REPRESENTS edges from the org charts.
func (s *server) graphProjection(ctx context.Context, tenant string) ([]graphNodeRow, []graphEdgeRow, error) {
	nodeRows, err := s.db.Query(ctx, `
		SELECT id::text, canonical_name, entity_type
		FROM public.entities WHERE tenant=$1 AND status='ACTIVE'`, tenant)
	if err != nil {
		return nil, nil, err
	}
	nodes := []graphNodeRow{}
	known := map[string]bool{}
	for nodeRows.Next() {
		var n graphNodeRow
		if nodeRows.Scan(&n.ID, &n.Name, &n.Typ) == nil {
			nodes = append(nodes, n)
			known[n.ID] = true
		}
	}
	nodeRows.Close()

	type edgeKey struct{ a, b, kind string }
	weights := map[edgeKey]int64{}
	addOpposes := func(a, b string, n int64) {
		if a == b || !known[a] || !known[b] {
			return
		}
		if b < a {
			a, b = b, a // canonical order — OPPOSES is undirected
		}
		weights[edgeKey{a, b, "OPPOSES"}] += n
	}

	// OPPOSES via case provider↔payer accounts.
	tcase := fmt.Sprintf("tenant_%s.cases", sanitizeTenant(tenant))
	if rows, qerr := s.db.Query(ctx, fmt.Sprintf(`
		SELECT lp.entity_id::text, lr.entity_id::text, count(DISTINCT c.id)
		FROM %s c
		JOIN public.entity_links lp ON lp.tenant=$1 AND lp.source_table='accounts' AND lp.source_id=c.provider_id
		JOIN public.entity_links lr ON lr.tenant=$1 AND lr.source_table='accounts' AND lr.source_id=c.payer_id
		GROUP BY 1, 2`, tcase), tenant); qerr == nil {
		for rows.Next() {
			var a, b string
			var n int64
			if rows.Scan(&a, &b, &n) == nil {
				addOpposes(a, b, n)
			}
		}
		rows.Close()
	} // else: tenant cases table absent — filing-rail pairs still project

	// OPPOSES via cross-org pairs sharing a filing-rail case (case_origin).
	if rows, qerr := s.db.Query(ctx, `
		WITH orgs AS (
			SELECT co.case_id::text AS case_id, l.entity_id
			FROM public.case_origin co
			JOIN public.entity_links l ON l.tenant=co.tenant AND (
				(l.source_table='tpas' AND co.tpa_id::text=l.source_id) OR
				(l.source_table='tpa_clients' AND co.client_id::text=l.source_id) OR
				(l.source_table='health_plans' AND co.health_plan_id::text=l.source_id) OR
				(l.source_table='hp_parties' AND co.hp_party_id::text=l.source_id))
			WHERE co.tenant=$1)
		SELECT o1.entity_id::text, o2.entity_id::text, count(DISTINCT o1.case_id)
		FROM orgs o1 JOIN orgs o2 ON o1.case_id=o2.case_id AND o1.entity_id < o2.entity_id
		GROUP BY 1, 2`, tenant); qerr == nil {
		for rows.Next() {
			var a, b string
			var n int64
			if rows.Scan(&a, &b, &n) == nil {
				addOpposes(a, b, n)
			}
		}
		rows.Close()
	}

	edges := []graphEdgeRow{}
	for k, n := range weights {
		edges = append(edges, graphEdgeRow{From: k.a, To: k.b, Kind: k.kind, Cases: n})
	}
	// REPRESENTS: org-chart pairs (all rows in the tenant).
	for _, pair := range s.graphRepresentsPairs(ctx, tenant, "") {
		if known[pair[0]] && known[pair[1]] {
			edges = append(edges, graphEdgeRow{From: pair[0], To: pair[1], Kind: "REPRESENTS"})
		}
	}
	return nodes, edges, nil
}

// syncGraphBackends projects and pushes to every configured non-sql backend.
// Best-effort per backend: one failure never blocks the others. Returns a
// per-backend status map for the sync endpoint's response.
func (s *server) syncGraphBackends(ctx context.Context, tenant string) map[string]string {
	out := map[string]string{}
	targets := []graphProvider{}
	for _, p := range s.graphBackends {
		if p.Name() != "sql" {
			targets = append(targets, p)
		}
	}
	if len(targets) == 0 {
		return out
	}
	nodes, edges, err := s.graphProjection(ctx, tenant)
	if err != nil {
		for _, p := range targets {
			out[p.Name()] = "projection failed: " + err.Error()
		}
		return out
	}
	for _, p := range targets {
		if err := p.SyncTenant(ctx, tenant, nodes, edges); err != nil {
			out[p.Name()] = "error: " + err.Error()
		} else {
			out[p.Name()] = fmt.Sprintf("ok (%d nodes, %d edges)", len(nodes), len(edges))
		}
	}
	return out
}

// entityGraphSync: POST /internal/entities/graph/sync — rebuild the graph
// projections for this tenant. Same admin gate as the entity backfill.
// (Distinct from graph.go's graphSync/graphSyncNow, which drive the
// case-level dispute graph in the graph-intel service.)
func (s *server) entityGraphSync(w http.ResponseWriter, r *http.Request) {
	tenant := r.Context().Value(ctxTenant{}).(string)
	p := r.Context().Value(ctxPrincipal{}).(principal)
	if !hasAnyRole(p, "FEDERAL_ADMIN", "PLATFORM_ADMIN", serviceRole) {
		http.Error(w, `{"error":"forbidden"}`, http.StatusForbidden)
		return
	}
	status := s.syncGraphBackends(r.Context(), tenant)
	if len(status) == 0 {
		writeJSON(w, http.StatusOK, map[string]any{"status": "no graph backends configured (GRAPH_BACKENDS=sql)"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"status": status})
}
