package publisher

import (
	"fmt"
	"strings"
	"testing"

	"pr-review-server/db"
	"pr-review-server/pkg/reviewer/payload"
)

func TestConfidence(t *testing.T) {
	crit := f("c1", "critical", "a.go", 1, "Critical.")
	med := func(id string) payload.Finding { return f(id, "medium", "b.go", 2, "Medium.") }
	threeMediums := []payload.Finding{med("m1"), med("m2"), med("m3")}
	requestChanges := f("sum", "unknown", "SUMMARY", 0, "**Verdict: request changes.** Fix first.")

	cases := []struct {
		name          string
		findings      []payload.Finding
		checkViolated bool
		want          int
	}{
		{"no findings", nil, false, 5},
		{"one shown medium", []payload.Finding{med("m1")}, false, 4},
		{"three shown mediums", threeMediums, false, 3},
		{"one critical", []payload.Finding{crit}, false, 3},
		{"critical and medium", []payload.Finding{crit, med("m1")}, false, 2},
		{"critical and three mediums", append([]payload.Finding{crit}, threeMediums...), false, 1},
		{"critical, three mediums and a violated check", append([]payload.Finding{crit}, threeMediums...), true, 0},
		{"request changes caps a clean review at 3", []payload.Finding{requestChanges}, false, 3},
		{"request changes leaves a lower score alone", []payload.Finding{requestChanges, crit, med("m1")}, false, 2},
		{"first-pass provenance is ignored", []payload.Finding{fp("x", "critical", "a.go", 1, "Claim.", "first-pass")}, false, 5},
		{"unverified state is ignored", []payload.Finding{unverified("x", "critical", "a.go", 1, "Claim.")}, false, 5},
		{"inactive record is ignored", []payload.Finding{inactive("x", "critical", "a.go", 1, "Claim.", "rejected")}, false, 5},
		{"latent medium is ignored", []payload.Finding{withContract(f("x", "medium", "a.go", 1, "Later."), "latent_hazard", "future_condition_only", "Only if X.", "")}, false, 5},
		{"summary and check rows are ignored", []payload.Finding{f("sum", "unknown", "SUMMARY", 0, "Narrative."), f("chk", "critical", "CHECK", 0, "VIOLATED.")}, false, 5},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := Confidence(tc.findings, tc.checkViolated); got != tc.want {
				t.Fatalf("Confidence = %d, want %d", got, tc.want)
			}
		})
	}
}

func TestRenderSummary_ConfidenceLineMatchesConfidence(t *testing.T) {
	requestChanges := Round{Owner: "acme", Repo: "example", Number: 1, HeadSHA: "abc1234", RoundNumber: 1,
		Findings: []payload.Finding{
			f("sum", "unknown", "SUMMARY", 0, "**Verdict: request changes.** Two mediums need attention."),
			f("x", "medium", "a.go", 3, "Something."),
		}}
	checkViolated := roundOne()
	checkViolated.RequiredCheckViolated = true
	rounds := map[string]Round{
		"round one":       roundOne(),
		"shown round":     shownRound(),
		"unverified":      unverifiedRound(),
		"request changes": requestChanges,
		"check violated":  checkViolated,
	}
	for name, r := range rounds {
		out := RenderSummary(r, Select(r.Findings, nil, r.Commentable, DefaultPolicy()))
		want := fmt.Sprintf("merge confidence %d/5", Confidence(r.Findings, r.RequiredCheckViolated))
		if !strings.Contains(out, want) {
			t.Errorf("%s: summary missing %q:\n%s", name, want, out)
		}
	}
}

func TestWithoutDismissed(t *testing.T) {
	findings := []payload.Finding{f("c1", "critical", "a.go", 1, "Critical."), f("m1", "medium", "b.go", 2, "Medium.")}
	previous := []db.PublishedFinding{
		{Kind: db.PublishedKindFinding, Fingerprint: "c1", State: db.PublishedStateDismissed},
		{Kind: db.PublishedKindAnnotation, Fingerprint: "m1", State: db.PublishedStateOpen},
		{Kind: db.PublishedKindSummary, Fingerprint: "summary", State: db.PublishedStateDismissed},
	}
	kept := WithoutDismissed(findings, previous)
	if len(kept) != 1 || kept[0].ID != "m1" {
		t.Fatalf("only the dismissed finding is dropped, got %+v", kept)
	}
	if got := WithoutDismissed(findings, nil); len(got) != 2 {
		t.Fatalf("no ledger keeps every finding, got %d", len(got))
	}
}
