package main

import (
	"context"
	"fmt"
	"net/http"

	"github.com/go-chi/chi/v5"
)

// entity360_graph.go — the cross-entity relationship graph (#10): entities as
// nodes, disputes as edges. An edge means "these two organizations sat on
// opposite sides of the same dispute(s)" — extracted from the case universe
// each entity's linked records touch (case_origin on the filing rail, CRM
// account ids on the case itself). No external graph store: the adjacency is
// derived from relational data on read, which keeps tenants isolated by
// construction and the deployment footprint unchanged; a dedicated store can
// be fed from this same extraction later.

// entityGraphNeighbors: entities sharing at least one case with the seed
// entity, with the shared-case count as the edge weight.
func (s *server) entityGraphNeighbors(ctx context.Context, tenant, entityID string, caseIDs []string) ([]map[string]any, error) {
	if len(caseIDs) == 0 {
		return []map[string]any{}, nil
	}
	tcase := fmt.Sprintf("tenant_%s.cases", sanitizeTenant(tenant))
	// Counterpart accounts on the seed's cases → their entities.
	accRows, err := s.db.Query(ctx, fmt.Sprintf(`
		SELECT l.entity_id::text, count(DISTINCT c.id) AS shared
		FROM %s c
		JOIN public.entity_links l ON l.tenant=$1 AND l.source_table='accounts'
		  AND (l.source_id = c.provider_id OR l.source_id = c.payer_id)
		WHERE c.id = ANY($2) AND l.entity_id <> $3
		GROUP BY l.entity_id`, tcase), tenant, caseIDs, entityID)
	if err != nil {
		return nil, err
	}
	defer accRows.Close()
	weight := map[string]int{}
	for accRows.Next() {
		var id string
		var n int
		if accRows.Scan(&id, &n) == nil {
			weight[id] += n
		}
	}
	// Counterpart org rows on the filing rail (case_origin) → their entities.
	// A case the seed filed names a health plan / hp party; a case against
	// the seed names a tpa / client.
	coRows, err := s.db.Query(ctx, `
		SELECT l.entity_id::text, count(DISTINCT co.case_id) AS shared
		FROM public.case_origin co
		JOIN public.entity_links l ON l.tenant=co.tenant AND (
			(l.source_table='tpas' AND co.tpa_id::text=l.source_id) OR
			(l.source_table='tpa_clients' AND co.client_id::text=l.source_id) OR
			(l.source_table='health_plans' AND co.health_plan_id::text=l.source_id) OR
			(l.source_table='hp_parties' AND co.hp_party_id::text=l.source_id))
		WHERE co.tenant=$1 AND co.case_id::text = ANY($2) AND l.entity_id <> $3
		GROUP BY l.entity_id`, tenant, caseIDs, entityID)
	if err == nil {
		defer coRows.Close()
		for coRows.Next() {
			var id string
			var n int
			if coRows.Scan(&id, &n) == nil {
				weight[id] += n
			}
		}
	}
	out := make([]map[string]any, 0, len(weight))
	for id, n := range weight {
		out = append(out, map[string]any{"entity_id": id, "shared_cases": n})
	}
	return out, nil
}

// entityGraph: GET /entity360/{entityId}/graph — the ego network of one
// entity: itself plus every entity it has faced in a dispute, edges weighted
// by shared cases. Capped at 60 neighbors by weight.
func (s *server) entityGraph(w http.ResponseWriter, r *http.Request) {
	tenant := r.Context().Value(ctxTenant{}).(string)
	if _, ok := s.entity360Gate(w, r, tenant, entity360ReadRoles); !ok {
		return
	}
	entityID := chi.URLParam(r, "entityId")
	ctx := r.Context()
	seed, err := s.queryRowMap(ctx, `
		SELECT id::text, entity_type, canonical_name
		FROM public.entities WHERE tenant=$1 AND id=$2 AND status='ACTIVE'`, tenant, entityID)
	if err != nil {
		http.Error(w, `{"error":"entity not found"}`, http.StatusNotFound)
		return
	}
	_, byTable := s.entityLinks(ctx, tenant, entityID)
	caseIDs := s.entityCaseIDs(ctx, tenant, byTable)
	neighbors, err := s.entityGraphNeighbors(ctx, tenant, entityID, caseIDs)
	if err != nil {
		http.Error(w, `{"error":"graph query failed"}`, http.StatusInternalServerError)
		return
	}
	// Sort by weight desc, cap.
	for i := 0; i < len(neighbors); i++ {
		for j := i + 1; j < len(neighbors); j++ {
			if neighbors[j]["shared_cases"].(int) > neighbors[i]["shared_cases"].(int) {
				neighbors[i], neighbors[j] = neighbors[j], neighbors[i]
			}
		}
	}
	if len(neighbors) > 60 {
		neighbors = neighbors[:60]
	}
	// Node details for the neighbors.
	nodes := []map[string]any{{"id": seed["id"], "name": seed["canonical_name"], "type": seed["entity_type"], "seed": true}}
	edges := []map[string]any{}
	for _, nb := range neighbors {
		nid := nb["entity_id"].(string)
		if row, qerr := s.queryRowMap(ctx, `
			SELECT canonical_name, entity_type FROM public.entities
			WHERE tenant=$1 AND id=$2 AND status='ACTIVE'`, tenant, nid); qerr == nil {
			nodes = append(nodes, map[string]any{"id": nid, "name": row["canonical_name"], "type": row["entity_type"]})
			edges = append(edges, map[string]any{
				"from": entityID, "to": nid, "kind": "OPPOSES", "cases": nb["shared_cases"]})
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"nodes": nodes, "edges": edges})
}
