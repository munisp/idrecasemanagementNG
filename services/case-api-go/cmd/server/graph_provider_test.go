package main

import (
	"context"
	"errors"
	"reflect"
	"testing"
)

func TestCypherQuote(t *testing.T) {
	if got := cypherQuote(`o'brien\path`); got != `'o\'brien\\path'` {
		t.Fatalf("cypherQuote = %q", got)
	}
	if got := cypherQuote("plain"); got != `'plain'` {
		t.Fatalf("cypherQuote = %q", got)
	}
}

func TestParseAgtypeScalar(t *testing.T) {
	cases := []struct {
		in   string
		want any
	}{
		{`"Sunshine Health Plan"`, "Sunshine Health Plan"},
		{`"say \"hi\""`, `say "hi"`},
		{`3`, int64(3)},
		{`3.5`, 3.5},
		{`true`, true},
		{`false`, false},
		{`null`, nil},
		{`"abc"::text`, "abc"},
	}
	for _, c := range cases {
		if got := parseAgtypeScalar(c.in); !reflect.DeepEqual(got, c.want) {
			t.Errorf("parseAgtypeScalar(%q) = %#v, want %#v", c.in, got, c.want)
		}
	}
}

func TestFalkorScalar(t *testing.T) {
	cases := []struct {
		in   any
		want any
	}{
		{[]any{int64(1), nil}, nil},
		{[]any{int64(2), "abc"}, "abc"},
		{[]any{int64(3), int64(7)}, int64(7)},
		{[]any{int64(4), "t"}, true},
		{[]any{int64(5), "2.5"}, 2.5},
		{"garbage", nil},
	}
	for _, c := range cases {
		if got := falkorScalar(c.in); !reflect.DeepEqual(got, c.want) {
			t.Errorf("falkorScalar(%#v) = %#v, want %#v", c.in, got, c.want)
		}
	}
}

func TestBuildGraphBackendsAlwaysEndsInSQL(t *testing.T) {
	s := &server{}
	t.Setenv("GRAPH_BACKENDS", "age,falkordb") // falkordb skipped: no FALKORDB_ADDR
	t.Setenv("FALKORDB_ADDR", "")
	chain := buildGraphBackends(nil, s)
	if len(chain) != 2 || chain[0].Name() != "age" || chain[1].Name() != "sql" {
		names := []string{}
		for _, p := range chain {
			names = append(names, p.Name())
		}
		t.Fatalf("chain = %v, want [age sql]", names)
	}
	t.Setenv("GRAPH_BACKENDS", "")
	chain = buildGraphBackends(nil, s)
	if len(chain) != 1 || chain[0].Name() != "sql" {
		t.Fatalf("default chain should be [sql]")
	}
	t.Setenv("FALKORDB_ADDR", "localhost:6390")
	t.Setenv("GRAPH_BACKENDS", "falkordb,sql")
	chain = buildGraphBackends(nil, s)
	if len(chain) != 2 || chain[0].Name() != "falkordb" {
		t.Fatalf("chain with addr = %v", chain)
	}
}

// fakeGraphProvider: scripted responses for chain/BFS tests.
type fakeGraphProvider struct {
	name  string
	fail  bool
	edges map[string][]map[string]any // entityID -> edges
}

func (f *fakeGraphProvider) Name() string { return f.name }
func (f *fakeGraphProvider) Neighbors(_ context.Context, _ string, entityID string, _ int) ([]map[string]any, []map[string]any, error) {
	if f.fail {
		return nil, nil, errors.New("boom")
	}
	return []map[string]any{{"id": entityID}}, f.edges[entityID], nil
}
func (f *fakeGraphProvider) SyncTenant(context.Context, string, []graphNodeRow, []graphEdgeRow) error {
	return nil
}

func TestGraphBFSPath(t *testing.T) {
	// A — B — C — D; A — D direct edge absent: shortest is 3 hops.
	fp := &fakeGraphProvider{name: "fake", edges: map[string][]map[string]any{
		"A": {{"from": "A", "to": "B", "kind": "OPPOSES"}},
		"B": {{"from": "A", "to": "B", "kind": "OPPOSES"}, {"from": "B", "to": "C", "kind": "OPPOSES"}},
		"C": {{"from": "B", "to": "C", "kind": "OPPOSES"}, {"from": "C", "to": "D", "kind": "REPRESENTS"}},
		"D": {{"from": "C", "to": "D", "kind": "REPRESENTS"}},
	}}
	got := graphBFSPath(context.Background(), fp, "t", "A", "D", 4)
	if !reflect.DeepEqual(got, []string{"A", "B", "C", "D"}) {
		t.Fatalf("BFS path = %v", got)
	}
	if p := graphBFSPath(context.Background(), fp, "t", "A", "ZZZ", 4); p != nil {
		t.Fatalf("unreachable should be nil, got %v", p)
	}
	if p := graphBFSPath(context.Background(), fp, "t", "A", "D", 2); p != nil {
		t.Fatalf("maxDepth respected: got %v", p)
	}
}

func TestChainFallsThroughOnError(t *testing.T) {
	// Simulates the handler's provider loop: a failing first backend must not
	// sink the request when a later backend can answer.
	ok := false
	for _, p := range []graphProvider{&fakeGraphProvider{name: "age", fail: true},
		&fakeGraphProvider{name: "sql", edges: map[string][]map[string]any{"E1": {}}}} {
		nodes, _, err := p.Neighbors(context.Background(), "t", "E1", 1)
		if err != nil {
			continue
		}
		ok = nodes[0]["id"] == "E1"
		break
	}
	if !ok {
		t.Fatal("chain did not fall through to sql")
	}
}
