package main

// Document full-text search via the shared OpenSearch cluster.
//
// doc-intel already indexes every analyzed document into idre-docs-{tenant}
// (full text + trusted normalized fields) as a best-effort sidecar to
// Postgres. Until now nothing ever READ that index — the portal's global
// search was Postgres ILIKE only. This file is the read path.
//
// Posture mirrors the writer: Postgres is the system of record, OpenSearch
// is an accelerator. Any cluster error (down, auth, timeout) degrades to
// "no document hits" — the SQL hits still return, the endpoint never 5xxes
// because a search sidecar sneezed.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// docSearchHit is one OpenSearch document match, shaped for the global
// search response (Kind is always "document").
type docSearchHit struct {
	DocID   string `json:"doc_id"`
	CaseID  string `json:"case_id,omitempty"`
	DocType string `json:"doc_type"`
	Snippet string `json:"snippet"`
}

// osSearchBody keeps the payload small: no text _source (documents can be
// 100k chars), just the metadata + a highlighted fragment.
func osSearchBody(query string) []byte {
	body := map[string]any{
		"size":    8,
		"_source": []string{"doc_type", "case_id", "status"},
		"query": map[string]any{
			"multi_match": map[string]any{
				"query":  query,
				"type":   "best_fields",
				"fields": []string{"text^2", "doc_type^3", "normalized.*", "extracted.*"},
			},
		},
		"highlight": map[string]any{
			"fields":    map[string]any{"text": map[string]any{"fragment_size": 160, "number_of_fragments": 1}},
			"pre_tags":  []string{""},
			"post_tags": []string{""},
		},
	}
	b, _ := json.Marshal(body)
	return b
}

// searchDocsOpenSearch queries idre-docs-{tenant} for full-text matches.
// ok=false means "cluster unavailable" — callers silently skip doc hits.
func (s *server) searchDocsOpenSearch(ctx context.Context, tenant, query string) (hits []docSearchHit, ok bool) {
	if s.cfg.OpenSearchURL == "" || strings.TrimSpace(query) == "" {
		return nil, false
	}
	// Index name carries the tenant code doc-intel wrote with; sanitizeTenant
	// guarantees [a-z0-9], which is also a legal index suffix.
	url := fmt.Sprintf("%s/idre-docs-%s/_search", strings.TrimRight(s.cfg.OpenSearchURL, "/"), sanitizeTenant(tenant))
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(osSearchBody(query)))
	if err != nil {
		return nil, false
	}
	req.Header.Set("Content-Type", "application/json")
	if s.cfg.OpenSearchUser != "" {
		req.SetBasicAuth(s.cfg.OpenSearchUser, s.cfg.OpenSearchPass)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, false
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return nil, true // index simply doesn't exist yet (no docs analyzed)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, false
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, false
	}
	var parsed struct {
		Hits struct {
			Hits []struct {
				ID     string `json:"_id"`
				Source struct {
					DocType string `json:"doc_type"`
					CaseID  string `json:"case_id"`
				} `json:"_source"`
				Highlight map[string][]string `json:"highlight"`
			} `json:"hits"`
		} `json:"hits"`
	}
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return nil, false
	}
	for _, h := range parsed.Hits.Hits {
		snippet := ""
		if frags := h.Highlight["text"]; len(frags) > 0 {
			snippet = frags[0]
		}
		hits = append(hits, docSearchHit{
			DocID: h.ID, CaseID: h.Source.CaseID,
			DocType: h.Source.DocType, Snippet: snippet,
		})
	}
	return hits, true
}
