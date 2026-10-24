// Author: Patrick Munis at newwave
package main

// graph_age.go — Apache AGE backend: openCypher inside the same Postgres
// cluster. One disposable graph per tenant (e360_<tenant>), projected from
// the relational tables by the sync projector (graph_sync.go); the relational
// data stays authoritative.
//
// Query strategy: the tenant graphs are small (thousands of nodes), so the
// ego query is deliberately simple — reachability via a variable-length MATCH
// for the node ring, plus one full edge scan filtered in Go to the ring.
// That keeps every RETURN clause scalar-only (no agtype vertex/edge decoding)
// and makes the result shaping identical across backends.
//
// AGE requires LOAD 'age' per session, so every method pins a pool connection
// and LOADs before issuing cypher.

import (
	"context"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

type ageGraphProvider struct{ db *pgxpool.Pool }

func (p *ageGraphProvider) Name() string { return "age" }

func ageGraphName(tenant string) string { return "e360_" + sanitizeTenant(tenant) }

// withConn runs fn on a pinned connection with age loaded.
func (p *ageGraphProvider) withConn(ctx context.Context, fn func(*pgx.Conn) error) error {
	conn, err := p.db.Acquire(ctx)
	if err != nil {
		return err
	}
	defer conn.Release()
	if _, err := conn.Exec(ctx, "LOAD 'age'"); err != nil {
		return fmt.Errorf("age: extension not available: %w", err)
	}
	return fn(conn.Conn())
}

func (p *ageGraphProvider) cypherQuery(ctx context.Context, conn *pgx.Conn, graph, cypher, cols string) (pgx.Rows, error) {
	return conn.Query(ctx, fmt.Sprintf(
		`SELECT * FROM ag_catalog.cypher(%s, $$%s$$) AS (%s)`,
		cypherQuote(graph), cypher, cols))
}

func (p *ageGraphProvider) cypherExec(ctx context.Context, conn *pgx.Conn, graph, cypher string) error {
	_, err := conn.Exec(ctx, fmt.Sprintf(
		`SELECT * FROM ag_catalog.cypher(%s, $$%s$$) AS (a ag_catalog.agtype)`,
		cypherQuote(graph), cypher))
	return err
}

// Neighbors implements graphProvider. Errors when the extension or the
// tenant graph is missing so the chain falls through to sql.
func (p *ageGraphProvider) Neighbors(ctx context.Context, tenant, entityID string, depth int) ([]map[string]any, []map[string]any, error) {
	graph := ageGraphName(tenant)
	var nodes []map[string]any
	var edges []map[string]any
	err := p.withConn(ctx, func(conn *pgx.Conn) error {
		// Seed presence — absent means "not synced yet": fall through.
		seedRows, err := p.cypherQuery(ctx, conn, graph,
			fmt.Sprintf(`MATCH (a:Entity {id: %s}) RETURN a.id, a.name, a.typ`, cypherQuote(entityID)),
			`id ag_catalog.agtype, name ag_catalog.agtype, typ ag_catalog.agtype`)
		if err != nil {
			return err
		}
		seedFound := false
		for seedRows.Next() {
			var id, name, typ string
			if seedRows.Scan(&id, &name, &typ) == nil {
				seedFound = true
				nodes = append(nodes, map[string]any{"id": agtypeString(parseAgtypeScalar(id)),
					"name": agtypeString(parseAgtypeScalar(name)), "type": agtypeString(parseAgtypeScalar(typ)), "seed": true})
			}
		}
		seedRows.Close()
		if !seedFound {
			return errGraphSeedMissing
		}
		// Node ring: everything within `depth` hops.
		ring := map[string]bool{entityID: true}
		nodeRows, err := p.cypherQuery(ctx, conn, graph,
			fmt.Sprintf(`MATCH (a:Entity {id: %s})-[*1..%d]-(b:Entity) RETURN DISTINCT b.id, b.name, b.typ`,
				cypherQuote(entityID), depth),
			`id ag_catalog.agtype, name ag_catalog.agtype, typ ag_catalog.agtype`)
		if err != nil {
			return err
		}
		for nodeRows.Next() {
			var id, name, typ string
			if nodeRows.Scan(&id, &name, &typ) == nil {
				nid := agtypeString(parseAgtypeScalar(id))
				ring[nid] = true
				if len(nodes) <= 60 {
					nodes = append(nodes, map[string]any{"id": nid,
						"name": agtypeString(parseAgtypeScalar(name)), "type": agtypeString(parseAgtypeScalar(typ))})
				}
			}
		}
		nodeRows.Close()
		// Full edge scan, filtered in Go to the ring (tenant graphs are small;
		// avoids path-object decoding entirely).
		edgeRows, err := p.cypherQuery(ctx, conn, graph,
			`MATCH (a:Entity)-[r]->(b:Entity) RETURN a.id, b.id, r.kind, r.cases`,
			`f ag_catalog.agtype, t ag_catalog.agtype, kind ag_catalog.agtype, cases ag_catalog.agtype`)
		if err != nil {
			return err
		}
		edges = []map[string]any{}
		seen := map[string]bool{}
		for edgeRows.Next() {
			var f, t, kind, cases string
			if edgeRows.Scan(&f, &t, &kind, &cases) != nil {
				continue
			}
			fid, tid := agtypeString(parseAgtypeScalar(f)), agtypeString(parseAgtypeScalar(t))
			if !ring[fid] || !ring[tid] || fid == tid {
				continue
			}
			k := agtypeString(parseAgtypeScalar(kind))
			key := fid + "|" + tid + "|" + k
			if seen[key] {
				continue
			}
			seen[key] = true
			e := map[string]any{"from": fid, "to": tid, "kind": k}
			if n := agtypeInt(parseAgtypeScalar(cases)); n > 0 {
				e["cases"] = n
			}
			edges = append(edges, e)
		}
		edgeRows.Close()
		return nil
	})
	if err != nil {
		return nil, nil, err
	}
	if edges == nil {
		edges = []map[string]any{}
	}
	return nodes, edges, nil
}

// SyncTenant rebuilds the tenant graph from the projection. Batched CREATEs
// with per-statement fallback so one malformed row can't sink the batch.
func (p *ageGraphProvider) SyncTenant(ctx context.Context, tenant string, nodes []graphNodeRow, edges []graphEdgeRow) error {
	graph := ageGraphName(tenant)
	return p.withConn(ctx, func(conn *pgx.Conn) error {
		_, _ = conn.Exec(ctx, `SELECT ag_catalog.drop_graph($1, true)`, graph) // ignore: first sync
		if _, err := conn.Exec(ctx, `SELECT ag_catalog.create_graph($1)`, graph); err != nil {
			return err
		}
		const batch = 100
		for i := 0; i < len(nodes); i += batch {
			parts := []string{}
			for _, n := range nodes[i:min(i+batch, len(nodes))] {
				parts = append(parts, fmt.Sprintf(`(:Entity {id: %s, name: %s, typ: %s})`,
					cypherQuote(n.ID), cypherQuote(n.Name), cypherQuote(n.Typ)))
			}
			if err := p.cypherExec(ctx, conn, graph, "CREATE "+strings.Join(parts, ",")); err != nil {
				for _, n := range nodes[i:min(i+batch, len(nodes))] { // per-row fallback
					_ = p.cypherExec(ctx, conn, graph, fmt.Sprintf(`CREATE (:Entity {id: %s, name: %s, typ: %s})`,
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
			if err := p.cypherExec(ctx, conn, graph, strings.Join(clauses, " ")); err != nil {
				for _, e := range edges[i:min(i+batch, len(edges))] {
					_ = p.cypherExec(ctx, conn, graph, fmt.Sprintf(
						`MATCH (a:Entity {id: %s}),(b:Entity {id: %s}) CREATE (a)-[:%s {kind: %s, cases: %d}]->(b)`,
						cypherQuote(e.From), cypherQuote(e.To), e.Kind, cypherQuote(e.Kind), e.Cases))
				}
			}
		}
		return nil
	})
}
