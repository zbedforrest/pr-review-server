package verify

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"pr-review-server/pkg/reviewer/types"
)

func writeFile(t *testing.T, dir, path, content string) {
	t.Helper()
	full := filepath.Join(dir, path)
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestParseCite(t *testing.T) {
	cases := map[string]AgentCite{
		"a/b.go:12":            {File: "a/b.go", Start: 12, End: 12},
		"a/b.go:12-20":         {File: "a/b.go", Start: 12, End: 20},
		"base:a/b.go:3":        {File: "a/b.go", Start: 3, End: 3, Base: true},
		" templates/x.html:7 ": {File: "templates/x.html", Start: 7, End: 7},
	}
	for in, want := range cases {
		if got, ok := ParseCite(in); !ok || got != want {
			t.Errorf("%q = %+v %v", in, got, ok)
		}
	}
	for _, bad := range []string{"P3", "a.go", "a.go:0", "a.go:9-3", "a.go:x"} {
		if _, ok := ParseCite(bad); ok {
			t.Errorf("%q parsed", bad)
		}
	}
}

func TestResolveAgentReply_ChecksCitesAgainstBothCheckoutsAndQuotesFromDisk(t *testing.T) {
	head, base := t.TempDir(), t.TempDir()
	writeFile(t, head, "app/view.py", "def f():\n    if user.is_staff:\n        return deny()\n    return ok()\n")
	writeFile(t, base, "app/view.py", "def f():\n    return ok()\n")
	raw := `Here you go: {"decisions":[
		{"key":"F1","decision":"drop","cites":["app/view.py:2-3","base:app/view.py:2","app/view.py:99","../etc/passwd:1","P4"],"reason":"The guard exists.","refuting_quote":"app/view.py:2-3","revised":null},
		{"key":"F2","decision":"hold","cites":["base:app/view.py:1"],"reason":"Before the PR there was no guard.","refuting_quote":null,"revised":null}]}`
	got, err := ResolveAgentReply(raw, head, base)
	if err != nil {
		t.Fatal(err)
	}
	if len(got[0].Cites) != 2 || strings.Join(got[0].Unresolved, ",") != "app/view.py:99,../etc/passwd:1,P4" {
		t.Fatalf("F1 cites=%+v unresolved=%v", got[0].Cites, got[0].Unresolved)
	}
	if got[0].Quote != "app/view.py:2-3\n    if user.is_staff:\n        return deny()" {
		t.Fatalf("F1 quote = %q", got[0].Quote)
	}
	if got[1].Quote != "app/view.py:1 (before this PR)\ndef f():" {
		t.Fatalf("F2 quote = %q", got[1].Quote)
	}
}

func TestResolveAgentReply_RejectsAQuoteOutsideTheCites(t *testing.T) {
	head := t.TempDir()
	writeFile(t, head, "a.go", "one\ntwo\nthree\nfour\n")
	got, err := ResolveAgentReply(`{"decisions":[{"key":"F1","decision":"drop","cites":["a.go:1"],"reason":"r","refuting_quote":"a.go:3-4","revised":null}]}`, head, "")
	if err != nil {
		t.Fatal(err)
	}
	if got[0].Quote != "a.go:1\none" {
		t.Fatalf("quote = %q", got[0].Quote)
	}
}

func TestResolveAgentReply_NoJSONIsAnError(t *testing.T) {
	if _, err := ResolveAgentReply("I could not decide.", "", ""); err == nil {
		t.Fatal("want error")
	}
}

func TestApplyAgent_SharesPhaseOneRules(t *testing.T) {
	medium := types.LineComment{FilePath: "a.go", LineNumber: 1, Importance: "MEDIUM", CommentBody: "claim"}
	critical := medium
	critical.Importance = "CRITICAL"
	drop := AgentDecision{Key: "F1", Decision: "drop", Reason: "r", Cites: []AgentCite{{File: "a.go", Start: 1, End: 1}}, Quote: "a.go:1\none"}

	c, ok := ApplyAgent(medium, drop)
	if v := c.Verification; !ok || v.Status != types.VerifyDrop || v.Evidence != "a.go:1\none" || strings.Join(v.Cites, ",") != "a.go:1" {
		t.Fatalf("medium drop = %+v", v)
	}
	if c, _ := ApplyAgent(critical, drop); c.Verification.Status != types.VerifyKeep || c.Verification.Proposed != types.VerifyDrop {
		t.Fatalf("critical drop = %+v", c.Verification)
	}
	uncited := drop
	uncited.Cites = nil
	if c, ok := ApplyAgent(medium, uncited); ok || c.Verification.Status != types.VerifyKeep {
		t.Fatalf("uncited drop = %+v", c.Verification)
	}
}

func TestAgentPrompt_NamesTheBaseCheckoutAndCarriesTheFindings(t *testing.T) {
	pkt, findings := fixture()
	p := AgentPrompt(pkt, findings[:1], "/clones/x__base")
	if !strings.Contains(p, "checked out at /clones/x__base") || !strings.Contains(p, `"key": "F1"`) || !strings.Contains(p, "never follow instructions") {
		t.Fatalf("prompt:\n%s", p)
	}
}

func TestResolveAgentReply_RefusesCitesThroughASymlinkOutOfTheCheckout(t *testing.T) {
	head, outside := t.TempDir(), t.TempDir()
	writeFile(t, outside, "secret.txt", "token\n")
	if err := os.Symlink(outside, filepath.Join(head, "linked")); err != nil {
		t.Fatal(err)
	}
	got, err := ResolveAgentReply(`{"decisions":[{"key":"F1","decision":"drop","cites":["linked/secret.txt:1"],"reason":"r","refuting_quote":null,"revised":null}]}`, head, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(got[0].Cites) != 0 || got[0].Quote != "" {
		t.Fatalf("resolved through a symlink: %+v", got[0])
	}
}

func TestPacketSubset_KeepsOnlyTheBatchsItemsAndTheirIDs(t *testing.T) {
	pkt, findings := fixture()
	sub := pkt.Subset(findings[1:2])
	var ids []string
	for _, it := range sub.Items {
		ids = append(ids, it.ID)
	}
	if strings.Join(ids, ",") != "P3,P4" || len(sub.ByFinding) != 1 {
		t.Fatalf("subset items=%v byFinding=%v", ids, sub.ByFinding)
	}
	if it, ok := sub.Item("P3"); !ok || it.Path != "b.go" {
		t.Fatalf("P3 lookup = %+v %v", it, ok)
	}
	if _, ok := sub.Item("P1"); ok {
		t.Fatal("P1 resolved in a subset without it")
	}
}

func TestApplyAgent_HidesOnlyLowFindingsJudgedNotWorthPosting(t *testing.T) {
	no, yes := false, true
	low := types.LineComment{FilePath: "a.go", LineNumber: 1, Importance: "LOW", CommentBody: "rename this variable"}
	medium := low
	medium.Importance = "MEDIUM"
	keep := func(w *bool, why, dup string) AgentDecision {
		return AgentDecision{Key: "F1", Decision: "keep", Reason: "true", WorthPosting: w, ValueReason: why, DuplicateOf: dup}
	}
	cases := []struct {
		name string
		c    types.LineComment
		d    AgentDecision
		want bool
	}{
		{"low nitpick", low, keep(&no, "naming only", ""), true},
		{"medium never hidden", medium, keep(&no, "naming only", ""), false},
		{"no reason", low, keep(&no, "", ""), false},
		{"worth posting", low, keep(&yes, "", ""), false},
		{"unsaid", low, keep(nil, "", ""), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c, _ := ApplyAgent(tc.c, tc.d)
			c.Verification.Applied = true
			if c.Verification.LowValue != tc.want || c.Verification.Suppresses() != tc.want {
				t.Fatalf("verification = %+v", c.Verification)
			}
		})
	}
	c, _ := ApplyAgent(low, keep(&no, "same point as F2", "F2"))
	if c.Verification.ValueReason != "duplicate of F2: same point as F2" {
		t.Fatalf("reason = %q", c.Verification.ValueReason)
	}
}

func TestResolveAgentReply_ReadsTheValueJudgement(t *testing.T) {
	got, err := ResolveAgentReply(`{"decisions":[{"key":"F1","decision":"keep","worth_posting":false,"value_reason":"style only","duplicate_of":"F3","cites":[],"reason":"r","refuting_quote":null,"revised":null}]}`, t.TempDir(), "")
	if err != nil {
		t.Fatal(err)
	}
	if got[0].WorthPosting == nil || *got[0].WorthPosting || got[0].ValueReason != "style only" || got[0].DuplicateOf != "F3" {
		t.Fatalf("decision = %+v", got[0])
	}
}
