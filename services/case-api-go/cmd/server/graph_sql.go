// Author: Patrick Munis at newwave
package main

// graph_sql.go — the default graph backend: adjacency derived from relational
// tables on read. This is the system of record (tenants isolated by
// construction, nothing to sync, nothing to drift) and the guaranteed
// fallback when AGE or FalkorDB are unavailable or stale.

import (
	"context"
	"fmt"
)

type sqlGraphProvider struct{ s *server }

func (p *sqlGraphProvider) Name() string { return "sql" }

func (p *sqlGraphProvider) SyncTenant(context.Context, string, []graphNodeRow, []graphEdgeRow) error {
	return nil // derives on read — nothing to project
}

// Neighbors: the ego network of one entity — itself plus every entity it has
// faced in a dispute (OPPOSES, weighted by shared cases) or that sits in its
// org chart (REPRESENTS: TPA→client, plan→party, when both sides resolved to
// entities). depth=2 adds one hop beyond the seed's direct neighbors. Nodes
// capped at 60 by edge weight.
func (p *sqlGraphProvider) Neighbors(ctx context.Context, tenant, entityID string, depth int) ([]map[string]any, []map[string]any, error) {
	s := p.s
	seed, err := s.queryRowMap(ctx, `
		SELECT id::text, entity_type, canonical_name
		FROM public.entities WHERE tenant=$1 AND id=$2 AND status='ACTIVE'`, tenant, entityID)
	if err != nil {
		return nil, nil, errGraphSeedMissing
	}
	_, byTable := s.entityLinks(ctx, tenant, entityID)
	caseIDs := s.entityCaseIDs(ctx, tenant, byTable)
	neighbors, err := s.entityGraphNeighbors(ctx, tenant, entityID, caseIDs)
	if err != nil {
		return nil, nil, err
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
	nodes := []map[string]any{{"id": seed["id"], "name": seed["canonical_name"], "type": seed["entity_type"], "seed": true}}
	edges := []map[string]any{}
	seenNode := map[string]bool{entityID: true}
	seenEdge := map[string]bool{}
	addNode := func(id string) {
		if seenNode[id] || len(nodes) > 60 {
			return
		}
		if row, qerr := s.queryRowMap(ctx, `
			SELECT canonical_name, entity_type FROM public.entities
			WHERE tenant=$1 AND id=$2 AND status='ACTIVE'`, tenant, id); qerr == nil {
			nodes = append(nodes, map[string]any{"id": id, "name": row["canonical_name"], "type": row["entity_type"]})
			seenNode[id] = true
		}
	}
	addEdge := func(from, to, kind string, cases any) {
		key := from + "|" + to + "|" + kind
		if seenEdge[key] || from == to {
			return
		}
		seenEdge[key] = true
		e := map[string]any{"from": from, "to": to, "kind": kind}
		if cases != nil {
			e["cases"] = cases
		}
		edges = append(edges, e)
	}
	for _, nb := range neighbors {
		nid := nb["entity_id"].(string)
		addNode(nid)
		if seenNode[nid] {
			addEdge(entityID, nid, "OPPOSES", nb["shared_cases"])
		}
	}
	// Org-chart edges: TPA↔client and plan↔party pairs where both sides
	// resolved to entities.
	for _, pair := range s.graphRepresentsPairs(ctx, tenant, entityID) {
		addNode(pair[0])
		addNode(pair[1])
		addEdge(pair[0], pair[1], "REPRESENTS", nil)
	}
	// depth=2: one hop beyond the direct neighbors (bounded fan-out).
	if depth == 2 {
		ring := []string{}
		for _, nb := range neighbors {
			if len(ring) < 12 {
				ring = append(ring, nb["entity_id"].(string))
			}
		}
		for _, nid := range ring {
			_, nbt := s.entityLinks(ctx, tenant, nid)
			nCases := s.entityCaseIDs(ctx, tenant, nbt)
			second, qerr := s.entityGraphNeighbors(ctx, tenant, nid, nCases)
			if qerr != nil {
				continue
			}
			for _, nb := range second {
				sid := nb["entity_id"].(string)
				if sid == entityID {
					continue
				}
				addNode(sid)
				if seenNode[sid] {
					addEdge(nid, sid, "OPPOSES", nb["shared_cases"])
				}
			}
		}
	}
	return nodes, edges, nil
}

// graphRepresentsPairs: org-chart edges touching entityID (empty string = all
// pairs in the tenant, used by the sync projector).
func (s *server) graphRepresentsPairs(ctx context.Context, tenant, entityID string) [][2]string {
	filter := ""
	args := []any{tenant}
	if entityID != "" {
		filter = " AND (l1.entity_id=$2 OR l2.entity_id=$2)"
		args = append(args, entityID)
	}
	out := [][2]string{}
	for _, q := range []string{
		`SELECT l1.entity_id::text, l2.entity_id::text FROM public.tpa_clients tc
		 JOIN public.entity_links l1 ON l1.tenant=tc.tenant AND l1.source_table='tpas' AND l1.source_id=tc.tpa_id::text
		 JOIN public.entity_links l2 ON l2.tenant=tc.tenant AND l2.source_table='tpa_clients' AND l2.source_id=tc.id::text
		 WHERE tc.tenant=$1` + filter + ` AND l1.entity_id<>l2.entity_id`,
		`SELECT l1.entity_id::text, l2.entity_id::text FROM public.hp_parties hp
		 JOIN public.entity_links l1 ON l1.tenant=hp.tenant AND l1.source_table='health_plans' AND l1.source_id=hp.plan_id::text
		 JOIN public.entity_links l2 ON l2.tenant=hp.tenant AND l2.source_table='hp_parties' AND l2.source_id=hp.id::text
		 WHERE hp.tenant=$1` + filter + ` AND l1.entity_id<>l2.entity_id`,
	} {
		rows, qerr := s.db.Query(ctx, q, args...)
		if qerr != nil {
			continue
		}
		for rows.Next() {
			var a, b string
			if rows.Scan(&a, &b) == nil {
				out = append(out, [2]string{a, b})
			}
		}
		rows.Close()
	}
	return out
}

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
