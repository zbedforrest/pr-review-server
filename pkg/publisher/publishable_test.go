package publisher

import (
	"strings"
	"testing"

	"pr-review-server/pkg/reviewer/payload"
)

func fp(id, sev, file string, line int, comment, provenance string) payload.Finding {
	x := f(id, sev, file, line, comment)
	x.Provenance = provenance
	return x
}

func TestPublishable_OnlyAgentConfirmedProvenance(t *testing.T) {
	cases := map[string]bool{
		"agent": true, "required-check": true, "carried": true, "": true,
		"first-pass": false, "mechanical": false,
	}
	for prov, want := range cases {
		if got := Publishable(fp("x", "medium", "a.go", 1, "c", prov)); got != want {
			t.Errorf("Publishable(provenance=%q) = %v, want %v", prov, got, want)
		}
	}
}

func TestSelect_SkipsUnconfirmedFindings(t *testing.T) {
	findings := []payload.Finding{
		fp("fp", "critical", "a.go", 10, "First-pass claim.", "first-pass"),
		fp("gate", "medium", "a.go", 0, "Gate fired.", "mechanical"),
		fp("ag", "medium", "a.go", 11, "Agent finding.", "agent"),
	}
	sel := Select(findings, nil, map[string]map[int]bool{"a.go": {10: true, 11: true}}, DefaultPolicy())
	if len(sel.Inline) != 1 || sel.Inline[0].ID != "ag" || len(sel.Annotations) != 0 {
		t.Fatalf("unconfirmed findings must be neither inline nor annotations: inline=%v annotations=%v", ids(sel.Inline), ids(sel.Annotations))
	}
}

func TestRenderSummary_ExcludesUnconfirmedFindingsEverywhere(t *testing.T) {
	r := Round{Owner: "acme", Repo: "example", Number: 1, HeadSHA: "abc1234", RoundNumber: 1,
		Findings: []payload.Finding{
			fp("fp", "critical", "a.go", 10, "First-pass claim.", "first-pass"),
			fp("ag", "medium", "a.go", 11, "Agent finding.", "agent"),
		}}
	out := RenderSummary(r, Select(r.Findings, nil, nil, DefaultPolicy()))
	if strings.Contains(out, "First-pass claim") || !strings.Contains(out, "Findings (1)") {
		t.Fatalf("summary must not list unconfirmed findings:\n%s", out)
	}
	if !strings.Contains(out, "merge confidence 5/5") {
		t.Fatalf("an unconfirmed critical must not lower confidence:\n%s", out)
	}
}

func TestRenderSummary_FooterHasNoRuleExplainer(t *testing.T) {
	r := roundOne()
	out := RenderSummary(r, Select(r.Findings, nil, r.Commentable, DefaultPolicy()))
	if strings.Contains(out, "rule:") || strings.Contains(out, "minus") {
		t.Fatalf("footer must stay compact:\n%s", out)
	}
	if !strings.Contains(out, "Reviews (1) · reviewed sha-rou") {
		t.Fatalf("footer must keep the round and sha:\n%s", out)
	}
}
