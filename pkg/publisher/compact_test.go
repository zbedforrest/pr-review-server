package publisher

import (
	"strings"
	"testing"

	"pr-review-server/pkg/reviewer/payload"
	"pr-review-server/pkg/reviewer/types"
)

func withContract(x payload.Finding, kind, materiality, impact, uncertainty string) payload.Finding {
	x.FindingContract = &types.FindingContract{
		SchemaVersion: 1, FindingKind: kind, Materiality: materiality,
		CurrentImpact: impact, Falsifiability: "unknown", Uncertainty: uncertainty,
	}
	x.FindingContractStatus = "valid"
	return x
}

func noContract(id, sev, file string, line int, comment string) payload.Finding {
	x := f(id, sev, file, line, comment)
	x.FindingContract, x.FindingContractStatus = nil, ""
	x.Provenance = "agent"
	return x
}

func TestSelect_InlineRequiresCurrentImpactOnBehaviorOrSecurity(t *testing.T) {
	commentable := map[string]map[int]bool{"a.go": {1: true, 2: true, 3: true, 4: true, 5: true}}
	findings := []payload.Finding{
		withContract(fp("prod", "medium", "a.go", 1, "Breaks now.", "agent"), "production_behavior", "current_impact", "Users see a 500.", ""),
		withContract(fp("sec", "critical", "a.go", 2, "Leaks token.", "agent"), "security_risk", "current_impact", "Tokens logged.", ""),
		withContract(fp("unk", "medium", "a.go", 3, "Might break.", "agent"), "production_behavior", "unknown", "Maybe.", "probably fine"),
		withContract(fp("latent", "critical", "a.go", 4, "Breaks if X.", "agent"), "latent_hazard", "future_condition_only", "Only if X.", ""),
		noContract("nocontract", "critical", "a.go", 5, "No contract."),
	}
	sel := Select(findings, nil, commentable, DefaultPolicy())
	if got := ids(sel.Inline); len(got) != 2 || got[0] != "sec" || got[1] != "prod" {
		t.Fatalf("inline = %v, want [sec prod] (critical first)", got)
	}
	if len(sel.Annotations) != 3 {
		t.Fatalf("everything else must fold into the summary: %v", ids(sel.Annotations))
	}
}

func TestDefaultPolicy_CapIsThree(t *testing.T) {
	if DefaultPolicy().InlineCap != 3 {
		t.Fatalf("default inline cap = %d, want 3", DefaultPolicy().InlineCap)
	}
}

func TestRenderInline_CompactHeadlineFromContract(t *testing.T) {
	fd := withContract(f("x", "medium", "purr/arbiter.py", 50,
		"On master the legacy path built its own client with verify=False. Now it shares the pooled client.\n\nThat is probably right, but if the bundle lacks the root every journey is skipped.\n\n```suggestion\nverify=get_ssl_verification(),\n```"),
		"production_behavior", "current_impact",
		"Legacy audience-match now verifies TLS; if the bundle lacks MMLLC ROOT every psychographic journey silently stops firing.",
		"Most likely a documentation gap rather than an outage.")
	fd.FindingContract.Falsifiability = "falsifiable"
	fd.FindingContract.FalsifiableCondition = strp("POST to the arbiter endpoint with verify enabled")
	fd.FindingContract.ExpectedObservable = strp("a 2xx if wrong, ConnectError if right")

	out := RenderInline(fd, "prism-only", "https://prism.example/go/agent?o=a&r=b&n=1")
	visible := out
	if i := strings.Index(out, "<details>"); i >= 0 {
		visible = out[:i]
	}

	mustContain := []string{
		"**[MEDIUM] Behavior change · Legacy audience-match now verifies TLS; if the bundle lacks MMLLC ROOT every psychographic journey silently stops firing**",
		"Most likely a documentation gap rather than an outage.",
		"```suggestion\nverify=get_ssl_verification(),\n```",
	}
	for _, s := range mustContain {
		if !strings.Contains(visible, s) {
			t.Errorf("visible part missing %q:\n%s", s, out)
		}
	}
	if strings.Contains(visible, "On master the legacy path") {
		t.Errorf("full reasoning must be folded, not visible:\n%s", out)
	}
	if !strings.Contains(out, "<details><summary>Reasoning and how to verify</summary>") || !strings.Contains(out, "On master the legacy path") || !strings.Contains(out, "**How to verify:**") {
		t.Errorf("details block must hold the reasoning and verify steps:\n%s", out)
	}
	if !strings.Contains(out, "Fix with agent") {
		t.Errorf("footer link missing")
	}
}

func TestRenderInline_WithoutContractFallsBackToFirstSentence(t *testing.T) {
	out := RenderInline(noContract("x", "critical", "a.go", 3, "Nil deref when cfg is missing. Details."), "prism-only", "")
	if !strings.Contains(out, "**[CRITICAL] Nil deref when cfg is missing.**") {
		t.Fatalf("fallback headline wrong:\n%s", out)
	}
}

func TestSummaryRows_UseImpactWhenAvailable(t *testing.T) {
	r := Round{Owner: "a", Repo: "b", Number: 1, HeadSHA: "abc1234", RoundNumber: 1,
		Findings: []payload.Finding{withContract(f("x", "medium", "a.go", 3, "Long narrative first sentence that rambles."), "production_behavior", "current_impact", "Users see a 500.", "")}}
	out := RenderSummary(r, Select(r.Findings, nil, nil, DefaultPolicy()))
	if !strings.Contains(out, "| medium | `a.go:3` | Users see a 500. | PRism |") {
		t.Fatalf("table row must use the impact sentence:\n%s", out)
	}
}
