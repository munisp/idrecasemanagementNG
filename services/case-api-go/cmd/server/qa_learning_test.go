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
