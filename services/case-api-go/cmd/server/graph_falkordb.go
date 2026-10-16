package main

// graph_falkordb.go — FalkorDB backend over the Redis protocol. Uses the
// stdlib-only RESP client (redis.go): GRAPH.QUERY with --compact results,
// decoded via cmdAny. Same node/edge model and ego-query strategy as the AGE
// backend (graph_age.go), so both engines stay byte-for-byte interchangeable
// on the wire contract the portal sees.
//
// Compact scalar value types (RESP2): 1=NULL 2=STRING 3=INTEGER 4=BOOLEAN
// 5=DOUBLE. Queries RETURN scalars only — node/edge/path decoding is avoided
// by design (see graph_age.go for the rationale).

import (
	"context"
	"fmt"
	"strings"
)

type falkordbGraphProvider struct{ rds *redisClient }

func (p *falkordbGraphProvider) Name() string { return "falkordb" }

// Graph keys mirror the graph-intel service's convention (idre_<st> there,
// e360_<tenant> here): one FalkorDB instance can host both workloads because
// the key namespaces don't overlap.
func falkordbGraphKey(tenant string) string { return "e360_" + sanitizeTenant(tenant) }

// graphQuery runs a read or write cypher and returns the result rows as
// typed scalars. Reply shape (RESP2, --compact):
//
//	[ [ [colName, colType], ... ],        // header
//	  [ [ [vtype, value], ... ], ... ],   // rows
//	  [ "Labels added: 2", ... ] ]        // stats
func (p *falkordbGraphProvider) graphQuery(key, cypher string) ([][]any, error) {
	res, err := p.rds.cmdAny("GRAPH.QUERY", key, cypher, "--compact")
	if err != nil {
		return nil, err
	}
	arr, ok := res.([]any)
	if !ok || len(arr) < 2 {
		return nil, fmt.Errorf("falkordb: unexpected reply shape")
	}
	rows, _ := arr[1].([]any)
	out := make([][]any, 0, len(rows))
	for _, r := range rows {
		row, ok := r.([]any)
		if !ok {
			continue
		}
		vals := make([]any, 0, len(row))
		for _, cell := range row {
			vals = append(vals, falkorScalar(cell))
		}
		out = append(out, vals)
	}
	return out, nil
}

// falkorScalar unwraps a compact [vtype, value] cell.
func falkorScalar(cell any) any {
	pair, ok := cell.([]any)
	if !ok || len(pair) != 2 {
		return nil
	}
	vt, _ := pair[0].(int64)
	switch vt {
	case 1: // NULL
		return nil
	case 2: // STRING
		s, _ := pair[1].(string)
		return s
	case 3: // INTEGER
		n, _ := pair[1].(int64)
		return n
	case 4: // BOOLEAN — RESP2 "t"/"f" markers
		s, _ := pair[1].(string)
		return s == "t" || s == "true"
	case 5: // DOUBLE — arrives as string in RESP2
		s, _ := pair[1].(string)
		var f float64
		fmt.Sscanf(s, "%g", &f)
		return f
	default:
		return nil
	}
}

// Neighbors implements graphProvider.
func (p *falkordbGraphProvider) Neighbors(ctx context.Context, tenant, entityID string, depth int) ([]map[string]any, []map[string]any, error) {
	key := falkordbGraphKey(tenant)
	seed, err := p.graphQuery(key, fmt.Sprintf(`MATCH (a:Entity {id: %s}) RETURN a.id, a.name, a.typ`, cypherQuote(entityID)))
	if err != nil {
		return nil, nil, err
	}
	if len(seed) == 0 {
		return nil, nil, errGraphSeedMissing
	}
	nodes := []map[string]any{{"id": agtypeString(seed[0][0]), "name": agtypeString(seed[0][1]), "type": agtypeString(seed[0][2]), "seed": true}}
	ring := map[string]bool{entityID: true}
	nodeRows, err := p.graphQuery(key, fmt.Sprintf(
		`MATCH (a:Entity {id: %s})-[*1..%d]-(b:Entity) RETURN DISTINCT b.id, b.name, b.typ`, cypherQuote(entityID), depth))
	if err != nil {
		return nil, nil, err
	}
	for _, r := range nodeRows {
		nid := agtypeString(r[0])
		ring[nid] = true
		if len(nodes) <= 60 {
			nodes = append(nodes, map[string]any{"id": nid, "name": agtypeString(r[1]), "type": agtypeString(r[2])})
		}
	}
	edgeRows, err := p.graphQuery(key, `MATCH (a:Entity)-[r]->(b:Entity) RETURN a.id, b.id, r.kind, r.cases`)
	if err != nil {
		return nil, nil, err
	}
	edges := []map[string]any{}
	seen := map[string]bool{}
	for _, r := range edgeRows {
		if len(r) < 4 {
			continue
		}
		fid, tid := agtypeString(r[0]), agtypeString(r[1])
		if !ring[fid] || !ring[tid] || fid == tid {
			continue
		}
		k := agtypeString(r[2])
		ek := fid + "|" + tid + "|" + k
		if seen[ek] {
			continue
		}
		seen[ek] = true
		e := map[string]any{"from": fid, "to": tid, "kind": k}
		if n := agtypeInt(r[3]); n > 0 {
			e["cases"] = n
		}
		edges = append(edges, e)
	}
	return nodes, edges, nil
}

// SyncTenant rebuilds the tenant graph key from the projection.
func (p *falkordbGraphProvider) SyncTenant(ctx context.Context, tenant string, nodes []graphNodeRow, edges []graphEdgeRow) error {
	key := falkordbGraphKey(tenant)
	_, _ = p.rds.cmdAny("GRAPH.DELETE", key) // ignore: first sync
	const batch = 100
	for i := 0; i < len(nodes); i += batch {
		parts := []string{}
		for _, n := range nodes[i:min(i+batch, len(nodes))] {
			parts = append(parts, fmt.Sprintf(`(:Entity {id: %s, name: %s, typ: %s})`,
				cypherQuote(n.ID), cypherQuote(n.Name), cypherQuote(n.Typ)))
		}
		if _, err := p.graphQuery(key, "CREATE "+strings.Join(parts, ",")); err != nil {
			for _, n := range nodes[i:min(i+batch, len(nodes))] {
				_, _ = p.graphQuery(key, fmt.Sprintf(`CREATE (:Entity {id: %s, name: %s, typ: %s})`,
					cypherQuote(n.ID), cypherQuote(n.Name), cypherQuote(n.Typ)))
			}
		}
	}
	for i := 0; i < len(edges); i += batch {
		clauses := []string{}
		for j, e := range edges[i:min(i+batch, len(edges))] {
			clauses = append(clauses, fmt.Sprintf(
				`MATCH (a%d:Entity {id: %s}),(b%d:Entity {id: %s}) CREATE (a%d)-[:%s {kind: %s, cases: %d}]->(b%d)`,
				j, cypherQuote(e.From), j, cypherQuote(e.To), j, e.Kind, cypherQuote(e.Kind), e.Cases, j))
		}
		if _, err := p.graphQuery(key, strings.Join(clauses, " ")); err != nil {
			for _, e := range edges[i:min(i+batch, len(edges))] {
				_, _ = p.graphQuery(key, fmt.Sprintf(
					`MATCH (a:Entity {id: %s}),(b:Entity {id: %s}) CREATE (a)-[:%s {kind: %s, cases: %d}]->(b)`,
					cypherQuote(e.From), cypherQuote(e.To), e.Kind, cypherQuote(e.Kind), e.Cases))
			}
		}
	}
	return nil
}
