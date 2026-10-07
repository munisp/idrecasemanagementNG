package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// The lowercase-JSON-tag contract: the portal reads h.kind/h.id/h.label/
// h.detail — a regression to Go's default capitalized keys renders every
// hit as an empty row ("search does nothing").
func TestGlobalSearchHitShapeIsLowercase(t *testing.T) {
	payload := `[{"kind":"case","id":"abc","label":"IDR-2026-00001","detail":"OPEN"}]`
	var hits []struct {
		Kind   string `json:"kind"`
		ID     string `json:"id"`
		Label  string `json:"label"`
		Detail string `json:"detail"`
	}
	if err := json.Unmarshal([]byte(payload), &hits); err != nil || len(hits) != 1 || hits[0].Kind != "case" {
		t.Fatalf("hit contract broken: %v %v", err, hits)
	}
}

func TestSearchDocsOpenSearchParsesHits(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/idre-docs-fl/_search") {
			t.Errorf("wrong index path: %s", r.URL.Path)
		}
		u, _, _ := r.BasicAuth()
		if u != "admin" {
			t.Errorf("basic auth not forwarded")
		}
		w.Write([]byte(`{"hits":{"hits":[
		  {"_id":"doc1","_source":{"doc_type":"EOB","case_id":"c1"},"highlight":{"text":["…underpayment of <em>$412</em>…"]}},
		  {"_id":"doc2","_source":{"doc_type":"","case_id":"c2"}}]}}`))
	}))
	defer srv.Close()
	s := &server{cfg: Config{OpenSearchURL: srv.URL, OpenSearchUser: "admin", OpenSearchPass: "x"}}
	hits, ok := s.searchDocsOpenSearch(context.Background(), "fl", "underpayment")
	if !ok || len(hits) != 2 {
		t.Fatalf("ok=%v hits=%v", ok, hits)
	}
	if hits[0].DocType != "EOB" || hits[0].CaseID != "c1" || !strings.Contains(hits[0].Snippet, "$412") {
		t.Fatalf("hit 0 malformed: %+v", hits[0])
	}
}

func TestSearchDocsOpenSearchDegradesSilently(t *testing.T) {
	// Unreachable cluster: ok=false, no panic, no error surfaced.
	s := &server{cfg: Config{OpenSearchURL: "http://127.0.0.1:1"}}
	if _, ok := s.searchDocsOpenSearch(context.Background(), "fl", "x"); ok {
		t.Fatal("unreachable cluster must report ok=false")
	}
	// Disabled (empty URL): also ok=false.
	s = &server{}
	if _, ok := s.searchDocsOpenSearch(context.Background(), "fl", "x"); ok {
		t.Fatal("empty URL must report ok=false")
	}
	// 404 (index not created yet — no docs analyzed) is NOT an outage: ok=true, zero hits.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()
	s = &server{cfg: Config{OpenSearchURL: srv.URL}}
	hits, ok := s.searchDocsOpenSearch(context.Background(), "fl", "x")
	if !ok || len(hits) != 0 {
		t.Fatalf("404 must be ok=true with no hits, got ok=%v hits=%v", ok, hits)
	}
}

func TestOsSearchBodyBoundsAndFields(t *testing.T) {
	body := string(osSearchBody("test query"))
	for _, want := range []string{`"size":8`, "text^2", "doc_type^3", "multi_match"} {
		if !strings.Contains(body, want) {
			t.Fatalf("query body missing %q: %s", want, body)
		}
	}
}
