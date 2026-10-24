// Author: Patrick Munis at newwave
package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestDeidBlockReason(t *testing.T) {
	if got := deidBlockReason(&deidScanResp{Risk: 0.4, Findings: []deidFinding{{Type: "DATE"}}}); got != "" {
		t.Fatalf("low risk should release, got %q", got)
	}
	got := deidBlockReason(&deidScanResp{Risk: 1.0, Findings: []deidFinding{{Type: "SSN"}, {Type: "SSN"}, {Type: "EMAIL"}}})
	if !strings.Contains(got, "SSN") || !strings.Contains(got, "EMAIL") {
		t.Fatalf("block reason should list distinct types, got %q", got)
	}
}

func TestDeidScanRoundTrip(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/scan" {
			t.Errorf("unexpected path %s", r.URL.Path)
		}
		var in map[string]any
		_ = json.NewDecoder(r.Body).Decode(&in)
		if in["tenant"] != "fl" || in["artifact"] != "letter" {
			t.Errorf("bad payload: %v", in)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"findings": []map[string]any{{"type": "SSN", "start": 4, "end": 15, "score": 0.99, "text": "x", "detector": "safeharbor-regex"}},
			"risk":     1.0,
			"audit":    map[string]any{"policy": "safe-harbor-18/v1", "detectors": []string{"safeharbor-regex/safe-harbor-18/v1"}, "counts": map[string]int{"SSN": 1}, "risk": 1.0, "text_hash": "abc"},
		})
	}))
	defer srv.Close()
	s := &server{cfg: Config{DeidURL: srv.URL}}
	res, err := s.deidScan(context.Background(), "fl", "c1", "letter", "body")
	if err != nil {
		t.Fatal(err)
	}
	if res.Risk != 1.0 || len(res.Findings) != 1 || res.Findings[0].Type != "SSN" {
		t.Fatalf("unexpected scan result: %+v", res)
	}
	if deidBlockReason(res) == "" {
		t.Fatal("SSN finding must block")
	}
	if res.Audit.Counts["SSN"] != 1 || res.Audit.Policy == "" {
		t.Fatalf("audit mapping broken: %+v", res.Audit)
	}
}

func TestDeidScanUnavailable(t *testing.T) {
	s := &server{cfg: Config{DeidURL: "http://127.0.0.1:1"}}
	if _, err := s.deidScan(context.Background(), "fl", "c1", "letter", "body"); err == nil {
		t.Fatal("expected connection error")
	}
}
