package main

// entity360_graph.go — the cross-entity relationship graph (#10): entities as
// nodes, disputes as edges. The handler is backend-agnostic: it walks the
// configured provider chain (GRAPH_BACKENDS — sql | age | falkordb, always
// ending in sql) and falls through on error, so a down graph store or a
// missing AGE extension never blanks the portal. ?path_to=<entityId> adds a
// shortest-connection BFS over whichever backend answered. See
// graph_provider.go for the architecture.

import (
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"
)

// entityGraph: GET /entity360/{entityId}/graph?depth=1|2&path_to=<entityId>.
func (s *server) entityGraph(w http.ResponseWriter, r *http.Request) {
	tenant := r.Context().Value(ctxTenant{}).(string)
	if _, ok := s.entity360Gate(w, r, tenant, entity360ReadRoles); !ok {
		return
	}
	entityID := chi.URLParam(r, "entityId")
	ctx := r.Context()
	depth := 1
	if r.URL.Query().Get("depth") == "2" {
		depth = 2
	}
	pathTo := strings.TrimSpace(r.URL.Query().Get("path_to"))
	// Existence + 404 semantics stay with the relational system of record,
	// independent of whether a graph backend has been synced yet.
	if _, err := s.queryRowMap(ctx, `
		SELECT id::text FROM public.entities
		WHERE tenant=$1 AND id=$2 AND status='ACTIVE'`, tenant, entityID); err != nil {
		http.Error(w, `{"error":"entity not found"}`, http.StatusNotFound)
		return
	}
	for _, p := range s.graphBackends {
		nodes, edges, err := p.Neighbors(ctx, tenant, entityID, depth)
		if err != nil {
			continue // fall through the chain
		}
		resp := map[string]any{"nodes": nodes, "edges": edges, "depth": depth, "backend": p.Name()}
		if pathTo != "" {
			path := []any{}
			if ids := graphBFSPath(ctx, p, tenant, entityID, pathTo, 4); ids != nil {
				for _, id := range ids {
					name := id
					if row, qerr := s.queryRowMap(ctx, `
						SELECT canonical_name FROM public.entities
						WHERE tenant=$1 AND id=$2`, tenant, id); qerr == nil {
						name = agtypeString(row["canonical_name"])
					}
					path = append(path, map[string]any{"id": id, "name": name})
				}
			}
			resp["path"] = path
		}
		writeJSON(w, http.StatusOK, resp)
		return
	}
	http.Error(w, `{"error":"graph query failed"}`, http.StatusInternalServerError)
}
