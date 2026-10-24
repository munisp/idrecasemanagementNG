// Author: Patrick Munis at newwave
package main

// Graph-intelligence middleware: tenant-scoped proxy from the authenticated
// case-api surface to the graph-intel service (FalkorDB dispute graph, numpy
// GraphSAGE link prediction, EPR-KGQA over the local ollama model, lakehouse
// sync). Auth, tenant extraction, and Permify checks happen here — the Python
// service never faces the portal directly.
//
// Also nudges a graph resync after case lifecycle mutations (initiate /
// signal) so FalkorDB converges on Postgres within seconds, with the
// lakehouse silver zone written by the same sync (db -> graph -> lakehouse).

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"time"

	"github.com/go-chi/chi/v5"
)

var graphHTTP = &http.Client{Timeout: 90 * time.Second}

// graphSync is a best-effort, fire-and-forget nudge; failures never block the
// transactional path (Postgres remains the source of truth; the next manual
// or scheduled /graph/sync reconciles).
func (s *server) graphSync(tenant string) {
	if s.cfg.GraphIntelURL == "" {
		return
	}
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		req, _ := http.NewRequestWithContext(ctx, http.MethodPost,
			s.cfg.GraphIntelURL+"/sync/from-db?tenant="+url.QueryEscape(tenant), nil)
		resp, err := graphHTTP.Do(req)
		if err == nil {
			_ = resp.Body.Close()
		}
	}()
}

func (s *server) proxyGraph(w http.ResponseWriter, r *http.Request, method, path string, body []byte) {
	if s.cfg.GraphIntelURL == "" {
		http.Error(w, `{"error":"graph intelligence not configured (GRAPH_INTEL_URL unset)"}`, http.StatusServiceUnavailable)
		return
	}
	tenant := r.Context().Value(ctxTenant{}).(string)
	sep := "?"
	if bytes.Contains([]byte(path), []byte("?")) {
		sep = "&"
	}
	u := s.cfg.GraphIntelURL + path + sep + "tenant=" + url.QueryEscape(tenant)
	req, err := http.NewRequestWithContext(r.Context(), method, u, bytes.NewReader(body))
	if err != nil {
		http.Error(w, `{"error":"proxy"}`, http.StatusInternalServerError)
		return
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := graphHTTP.Do(req)
	if err != nil {
		http.Error(w, `{"error":"graph-intel unreachable"}`, http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(resp.StatusCode)
	_, _ = io.Copy(w, resp.Body)
}

// graphAsk handles POST /graph/ask {question, k?} — EPR-KGQA: entity linking,
// path retrieval, GNN ranking, ollama answer with citations. The tenant is
// injected server-side so a caller can never query across the state line.
func (s *server) graphAsk(w http.ResponseWriter, r *http.Request) {
	tenant := r.Context().Value(ctxTenant{}).(string)
	var in struct {
		Question string `json:"question"`
		K        int    `json:"k"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil || in.Question == "" {
		http.Error(w, `{"error":"question required"}`, http.StatusBadRequest)
		return
	}
	if in.K <= 0 || in.K > 20 {
		in.K = 5
	}
	payload, _ := json.Marshal(map[string]any{"tenant": tenant, "question": in.Question, "k": in.K})
	s.logActivity(r.Context(), tenant, "", "KGQA_ASK", truncate("KGQA: "+in.Question, 400))
	s.proxyGraph(w, r, http.MethodPost, "/ask", payload)
}

// graphFeedback handles POST /graph/feedback {log_id, rating}.
func (s *server) graphFeedback(w http.ResponseWriter, r *http.Request) {
	tenant := r.Context().Value(ctxTenant{}).(string)
	var in struct {
		LogID  string `json:"log_id"`
		Rating int    `json:"rating"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil || in.LogID == "" {
		http.Error(w, `{"error":"log_id required"}`, http.StatusBadRequest)
		return
	}
	payload, _ := json.Marshal(map[string]any{"tenant": tenant, "log_id": in.LogID, "rating": in.Rating})
	s.proxyGraph(w, r, http.MethodPost, "/feedback", payload)
}

// caseRelated proxies GET /gnn/predict for the case workspace "Related
// disputes" panel. Scores are numpy-GraphSAGE link predictions, written back
// to FalkorDB as RELATED_TO edges by the graph service.
func (s *server) caseRelated(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "caseId")
	s.proxyGraph(w, r, http.MethodGet, "/gnn/predict?case_id="+url.QueryEscape(id)+"&k=5", nil)
}

// caseGraphNeighbors proxies GET /graph/neighbors (evidence neighborhood for
// the case graph panel).
func (s *server) caseGraphNeighbors(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "caseId")
	s.proxyGraph(w, r, http.MethodGet, "/graph/neighbors?case_id="+url.QueryEscape(id)+"&hops=2", nil)
}

// graphSyncNow handles POST /graph/sync — full Postgres -> FalkorDB ->
// lakehouse resync for this tenant (admin action, also runs on a schedule).
func (s *server) graphSyncNow(w http.ResponseWriter, r *http.Request) {
	s.proxyGraph(w, r, http.MethodPost, "/sync/from-db", nil)
}

// graphToLakehouse handles POST /graph/to-lakehouse — graph -> gold export.
func (s *server) graphToLakehouse(w http.ResponseWriter, r *http.Request) {
	s.proxyGraph(w, r, http.MethodPost, "/sync/to-lakehouse", nil)
}

// graphTrain handles POST /graph/train {epochs?}.
func (s *server) graphTrain(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Epochs int `json:"epochs"`
	}
	_ = json.NewDecoder(r.Body).Decode(&in)
	if in.Epochs <= 0 || in.Epochs > 2000 {
		in.Epochs = 120
	}
	s.proxyGraph(w, r, http.MethodPost, "/gnn/train?epochs="+url.QueryEscape(itoa(in.Epochs)), nil)
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var b [12]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		b[i] = '-'
	}
	return string(b[i:])
}
