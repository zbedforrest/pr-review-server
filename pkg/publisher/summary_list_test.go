package publisher

import (
	"strings"
	"testing"

	"pr-review-server/pkg/reviewer/payload"
)

func shownRound() Round {
	crit := withContract(f("c1", "critical", "frontend/typescripts/src/common/broadcastlib/f2fViewerClient.ts", 579,
		"Viewers are never unmuted. Long reasoning follows."),
		"production_behavior", "current_impact", "Viewers who start a fresh call are not unmuted when the broadcaster accepts.", "")
	med := withContract(f("m1", "medium", "frontend/react/src/components/room/PremiumPrivateRequest/PremiumPrivateRequest.tsx", 380,
		"Broadcaster copy is wrong."), "production_behavior", "current_impact", "Broadcasters see the viewer's copy of the prompt.", "")
	softMed := withContract(f("m2", "medium", "a.go", 5, "Might flicker."), "production_behavior", "unknown", "Maybe a flicker.", "")
	low := withContract(f("l1", "low", "b.go", 9, "Nit."), "test_quality", "no_user_impact", "No user impact.", "")
	return Round{Owner: "acme", Repo: "example", Number: 7, HeadSHA: "96a9b9b1234567", RoundNumber: 1,
		Findings:       []payload.Finding{f("sum", "unknown", "SUMMARY", 0, "Narrative."), crit, med, softMed, low},
		GreptileOnly:   []GreptileOnlyRef{{Title: "Stale state", File: "c.ts", Line: 3, Severity: "low", CommentID: 55}},
		Commentable:    map[string]map[int]bool{"frontend/typescripts/src/common/broadcastlib/f2fViewerClient.ts": {579: true}},
		InlineComments: map[string]int64{"c1": 3937000001},
		DashboardURL:   "https://prism.example/api/review/acme/example/7?format=html",
	}
}

func TestRenderSummary_ListsOnlyConfirmedFindingsAsBullets(t *testing.T) {
	r := shownRound()
	out := RenderSummary(r, Select(r.Findings, nil, r.Commentable, DefaultPolicy()))

	for _, banned := range []string{"| Sev |", "<details>", "Source", "Greptile", "PRism |", "Stale state", "Maybe a flicker", "No user impact", "a.go", "b.go"} {
		if strings.Contains(out, banned) {
			t.Errorf("summary must not contain %q:\n%s", banned, out)
		}
	}
	wantCrit := "- **[CRITICAL]** Viewers who start a fresh call are not unmuted when the broadcaster accepts — [`f2fViewerClient.ts:579`](https://github.com/acme/example/pull/7#discussion_r3937000001)"
	wantMed := "- **[MEDIUM]** Broadcasters see the viewer's copy of the prompt — [`PremiumPrivateRequest.tsx:380`](https://github.com/acme/example/blob/96a9b9b1234567/frontend/react/src/components/room/PremiumPrivateRequest/PremiumPrivateRequest.tsx#L380)"
	for _, want := range []string{wantCrit, wantMed, "merge confidence 3/5", "Reviews (1) · reviewed 96a9b9b"} {
		if !strings.Contains(out, want) {
			t.Errorf("summary missing %q:\n%s", want, out)
		}
	}
	if strings.Index(out, wantCrit) > strings.Index(out, wantMed) {
		t.Errorf("criticals must come first")
	}
}

func TestRenderSummary_NothingShownReadsClean(t *testing.T) {
	r := shownRound()
	r.Findings = r.Findings[:1] // narrative only
	r.Findings = append(r.Findings, withContract(f("l1", "low", "b.go", 9, "Nit."), "test_quality", "no_user_impact", "No user impact.", ""))
	out := RenderSummary(r, Select(r.Findings, nil, r.Commentable, DefaultPolicy()))
	if !strings.Contains(out, "merge confidence 5/5") || !strings.Contains(out, "No blocking findings.") || strings.Contains(out, "- **[") {
		t.Fatalf("a round with nothing above the bar must read clean:\n%s", out)
	}
}

func TestSelect_BelowBarFindingsAreDroppedEntirely(t *testing.T) {
	r := shownRound()
	sel := Select(r.Findings, nil, r.Commentable, DefaultPolicy())
	if got := ids(sel.Inline); len(got) != 1 || got[0] != "c1" {
		t.Fatalf("inline = %v, want [c1] (m1 is not on a commentable line)", got)
	}
	if got := ids(sel.Annotations); len(got) != 1 || got[0] != "m1" {
		t.Fatalf("annotations = %v, want [m1] only; soft medium and low must not appear anywhere", got)
	}
}

func TestPublish_SummaryLinksInlineCommentsPostedThisRound(t *testing.T) {
	gh, ledger := newFakeGitHub(), newFakeLedger()
	r := shownRound()
	r.InlineComments = nil
	publishRound(t, gh, ledger, r)
	if len(gh.issueCreates) != 1 {
		t.Fatalf("creates=%d", len(gh.issueCreates))
	}
	if !strings.Contains(gh.issueCreates[0], "#discussion_r1001") {
		t.Fatalf("summary must link the inline comment created in the same round:\n%s", gh.issueCreates[0])
	}
}

func TestShown_LowSeverityNeverReachesGitHub(t *testing.T) {
	low := withContract(f("l", "low", "a.go", 5, "Unlabeled close button."), "production_behavior", "current_impact", "AT users get an unlabeled button.", "")
	if Shown(low) {
		t.Fatal("a low finding must not be shown even with current impact")
	}
	med := withContract(f("m", "medium", "a.go", 5, "x"), "production_behavior", "current_impact", "Users see a 500.", "")
	if !Shown(med) {
		t.Fatal("a current-impact medium is shown")
	}
}

func TestBullet_TruncatesOnWordBoundaryWithoutStrayPeriods(t *testing.T) {
	long := strings.Repeat("alpha beta gamma ", 20) + "delta."
	fd := withContract(f("x", "critical", "a.go", 1, "c"), "production_behavior", "current_impact", long, "")
	out := RenderSummary(Round{Owner: "a", Repo: "b", Number: 1, HeadSHA: "abc1234", RoundNumber: 1, Findings: []payload.Finding{fd}}, Selection{})
	line := out[strings.Index(out, "- **[CRITICAL]**"):]
	line = line[:strings.Index(line, " — ")]
	if !strings.HasSuffix(line, "gamma...") && !strings.HasSuffix(line, "beta...") && !strings.HasSuffix(line, "alpha...") {
		t.Fatalf("bullet must end on a whole word plus an ellipsis: %q", line)
	}
	if strings.Contains(line, "..") && !strings.Contains(line, "...") {
		t.Fatalf("no stray double periods: %q", line)
	}
}
