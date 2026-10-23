package main

import (
	"strings"
	"testing"
)

func TestTokenEditDistance(t *testing.T) {
	cases := []struct {
		name     string
		a, b     string
		wantDist int
		wantTok  int
	}{
		{"identical", "the quick brown fox", "the quick brown fox", 0, 4},
		{"one substitution", "the quick brown fox", "the quick red fox", 1, 4},
		{"one insertion", "the quick fox", "the quick brown fox", 1, 3},
		{"one deletion", "the quick brown fox", "the quick fox", 1, 4},
		{"empty original", "", "a b c", 3, 0},
		{"empty edit", "a b c", "", 3, 3},
		{"both empty", "", "", 0, 0},
		{"full rewrite", "a b c", "x y z", 3, 3},
		{"whitespace-only difference is no edit at capture level but DP still counts tokens equally",
			"  padded   text here ", "padded text here", 0, 3},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d, n := tokenEditDistance(tc.a, tc.b)
			if d != tc.wantDist || n != tc.wantTok {
				t.Errorf("got dist=%d tokens=%d, want dist=%d tokens=%d", d, n, tc.wantDist, tc.wantTok)
			}
		})
	}
}

func TestTokenEditDistanceCaps(t *testing.T) {
	big := strings.Repeat("word ", 10000)
	d, n := tokenEditDistance(big, big)
	if d != 0 || n != 2000 {
		t.Errorf("cap not applied: dist=%d tokens=%d", d, n)
	}
}

func TestTemplateHealthVerdict(t *testing.T) {
	f := func(v float64) *float64 { return &v }
	cases := []struct {
		name        string
		drafts, rej int64
		ratio       *float64
		want        string
	}{
		{"below minN stays silent even when terrible", 19, 19, f(0.9), "insufficient_data"},
		{"healthy at minN", 20, 1, f(0.05), "healthy"},
		{"nil ratio treated as zero", 25, 0, nil, "healthy"},
		{"watch by rejection rate", 20, 6, f(0.1), "watch"},
		{"watch by edit ratio", 40, 2, f(0.25), "watch"},
		{"retire by rejection rate", 20, 9, f(0.1), "retire_candidate"},
		{"retire by edit ratio", 30, 3, f(0.4), "retire_candidate"},
		{"boundary: exactly 40% rejection is watch not retire", 20, 8, f(0.1), "watch"},
		{"boundary: exactly 0.35 ratio is watch not retire", 20, 0, f(0.35), "watch"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := templateHealthVerdict(tc.drafts, tc.rej, tc.ratio); got != tc.want {
				t.Errorf("got %q want %q", got, tc.want)
			}
		})
	}
}
