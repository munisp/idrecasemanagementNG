package main

// graph_provider.go — pluggable relationship-graph backends for Entity 360.
//
// Three engines behind one interface:
//
//	sql       — adjacency derived from relational tables on read (default;
//	            always the last-resort fallback and the system of record)
//	age       — Apache AGE: openCypher inside the same Postgres cluster; a
//	            per-tenant graph (e360_<tenant>) projected from relational data
//	falkordb  — FalkorDB over the Redis protocol (GRAPH.QUERY, RESP2) via the
//	            stdlib-only RESP client in redis.go — zero new dependencies
//
// Selection: GRAPH_BACKENDS="age,falkordb" (comma-separated, in preference
// order; "sql" is appended automatically when absent). Reads walk the chain
// and fall through on error, so a missing extension or a down FalkorDB never
// blanks the portal. Writes (the sync projector) fan out to every configured
// non-sql backend best-effort; the relational tables stay authoritative and
// the graphs are disposable projections.

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5/pgxpool"
)

var errGraphSeedMissing = errors.New("graph: seed entity not present in backend")

// graphNodeRow / graphEdgeRow: the projection unit handed to backends by the
// sync projector. cases is the OPPOSES edge weight (0 for REPRESENTS).
type graphNodeRow struct{ ID, Name, Typ string }
type graphEdgeRow struct {
	From, To, Kind string
	Cases          int64
}

type graphProvider interface {
	Name() string
	// Neighbors returns the ego network of entityID as portal-shaped
	// nodes/edges (same JSON contract as the legacy SQL implementation).
	Neighbors(ctx context.Context, tenant, entityID string, depth int) ([]map[string]any, []map[string]any, error)
	// SyncTenant replaces the backend's tenant subgraph with the projection.
	// sql is a no-op (it derives on read).
	SyncTenant(ctx context.Context, tenant string, nodes []graphNodeRow, edges []graphEdgeRow) error
}

// buildGraphBackends resolves GRAPH_BACKENDS into a provider chain. sql is
// always present (appended when unlisted) so the chain can never be empty.
func buildGraphBackends(db *pgxpool.Pool, s *server) []graphProvider {
	spec := strings.ToLower(strings.TrimSpace(os.Getenv("GRAPH_BACKENDS")))
	if spec == "" {
		spec = "sql"
	}
	out := []graphProvider{}
	seen := map[string]bool{}
	for _, name := range strings.Split(spec, ",") {
		name = strings.TrimSpace(name)
		if name == "" || seen[name] {
			continue
		}
		seen[name] = true
		switch name {
		case "sql":
			out = append(out, &sqlGraphProvider{s: s})
		case "age":
			out = append(out, &ageGraphProvider{db: db})
		case "falkordb":
			addr := envOr("FALKORDB_ADDR", "")
			if addr == "" {
				continue // requested but unconfigured — skip
			}
			out = append(out, &falkordbGraphProvider{rds: newRedis(addr, os.Getenv("FALKORDB_PASSWORD"))})
		}
	}
	if !seen["sql"] {
		out = append(out, &sqlGraphProvider{s: s})
	}
	return out
}

// cypherQuote renders a Go string as a single-quoted Cypher literal.
func cypherQuote(s string) string {
	s = strings.ReplaceAll(s, `\`, `\\`)
	s = strings.ReplaceAll(s, `'`, `\'`)
	return "'" + s + "'"
}

// parseAgtypeScalar unwraps an AGE agtype value scanned as text: JSON-ish
// strings ("abc"), integers (3), floats (3.5), booleans, null. Composite
// annotations (::vertex, ::edge) are not expected — queries RETURN scalars
// only, by design.
func parseAgtypeScalar(v string) any {
	if i := strings.Index(v, "::"); i >= 0 {
		v = v[:i]
	}
	v = strings.TrimSpace(v)
	if v == "" || v == "null" {
		return nil
	}
	if strings.HasPrefix(v, `"`) {
		// agtype strings are JSON strings: trim the quotes and unescape.
		t := v[1 : len(v)-1]
		t = strings.ReplaceAll(t, `\"`, `"`)
		t = strings.ReplaceAll(t, `\\`, `\`)
		return t
	}
	if n, err := strconv.ParseInt(v, 10, 64); err == nil {
		return n
	}
	if f, err := strconv.ParseFloat(v, 64); err == nil {
		return f
	}
	if v == "true" {
		return true
	}
	if v == "false" {
		return false
	}
	return v
}

// agtypeString / agtypeInt: typed accessors over parseAgtypeScalar.
func agtypeString(v any) string {
	switch t := v.(type) {
	case string:
		return t
	case nil:
		return ""
	default:
		return fmt.Sprintf("%v", t)
	}
}

func agtypeInt(v any) int64 {
	switch t := v.(type) {
	case int64:
		return t
	case float64:
		return int64(t)
	case string:
		n, _ := strconv.ParseInt(t, 10, 64)
		return n
	default:
		return 0
	}
}

// graphBFSPath finds a shortest connection between two entities over whatever
// backend answered the ego query (maxDepth hops, visited-cap 200). Returns
// the id path, or nil when unreachable. Names are resolved by the handler
// from the relational system of record.
func graphBFSPath(ctx context.Context, p graphProvider, tenant, from, to string, maxDepth int) []string {
	if from == to {
		return []string{from}
	}
	type item struct {
		id   string
		path []string
	}
	queue := []item{{from, []string{from}}}
	visited := map[string]bool{from: true}
	for len(queue) > 0 && len(visited) < 200 {
		cur := queue[0]
		queue = queue[1:]
		if len(cur.path)-1 >= maxDepth {
			continue
		}
		_, edges, err := p.Neighbors(ctx, tenant, cur.id, 1)
		if err != nil {
			continue
		}
		for _, e := range edges {
			for _, end := range [2]string{agtypeString(e["from"]), agtypeString(e["to"])} {
				if end == "" || visited[end] {
					continue
				}
				visited[end] = true
				np := append(append([]string{}, cur.path...), end)
				if end == to {
					return np
				}
				queue = append(queue, item{end, np})
			}
		}
	}
	return nil
}
