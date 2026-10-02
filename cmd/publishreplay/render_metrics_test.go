package main

import "testing"

func TestNilImpactBullets_CountsNilImpactPhrasesNotSentencesAboutNone(t *testing.T) {
	cases := []struct {
		text string
		want int
	}{
		{"None today; the fallback masks the error", 1},
		{"None.", 1},
		{"No user impact; output ordering is unstable", 1},
		{"Not demonstrated in this PR", 1},
		{"None of the six cases contains the literal flag", 0},
		{"Nonexistent file is reported as empty", 0},
		{"Retry loop never backs off", 0},
	}
	for _, tc := range cases {
		summary := "- **[LOW]** " + tc.text + " — [`a.go:1`](https://example.invalid)\n"
		if got := nilImpactBullets(summary); got != tc.want {
			t.Errorf("%q: nilImpactBullets = %d, want %d", tc.text, got, tc.want)
		}
	}
}
