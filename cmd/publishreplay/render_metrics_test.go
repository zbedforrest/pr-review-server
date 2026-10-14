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
		summary := "- **[LOW]** " + tc.text + " \u2014 [`a.go:1`](https://example.invalid)\n"
		if got := nilImpactBullets(summary); got != tc.want {
			t.Errorf("%q: nilImpactBullets = %d, want %d", tc.text, got, tc.want)
		}
	}
}

func TestLeadingMarkers_CountTitlesAndBulletsThatOpenWithACutOrConnective(t *testing.T) {
	for title, want := range map[string]bool{
		"Behavior change · […] but differ by target_audience or assign_countries": true,
		"Behavior change · The higher-priority one now holds the slot":            false,
		"[…] but the cache is never invalidated":                                  true,
		"however, the job re-runs":                                                true,
		"The job re-runs":                                                         false,
		"`leave_room` responses can arrive":                                       false,
	} {
		for _, inline := range []string{"<!-- m -->\n**[MEDIUM] " + title + "**\n\nbody", "<!-- m -->\n<img alt=\"MEDIUM\" src=\"x\"> **" + title + "**\n\nbody"} {
			if got := leadingMarkerTitle(inline); got != want {
				t.Errorf("%q: leadingMarkerTitle = %v, want %v", title, got, want)
			}
		}
	}
	summary := "- **[LOW]** […] but the row is served \u2014 [`a.go:1`](https://example.invalid)\n" +
		"- **[LOW]** The row is served \u2014 [`a.go:2`](https://example.invalid)\n" +
		"- <img alt=\"MEDIUM\" src=\"x\"> when the flag is off the row is served \u2014 [`a.go:3`](https://example.invalid)\n"
	if got := leadingMarkerBullets(summary); got != 2 {
		t.Errorf("leadingMarkerBullets = %d, want 2", got)
	}
}
