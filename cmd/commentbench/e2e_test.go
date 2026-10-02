package main

import (
	"context"
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"pr-review-server/db"
	"pr-review-server/pkg/publisher"
)

// knownGap is a difference from the intended behaviour that master still
// has, with the program item expected to remove it. A publisher change that
// removes a gap must delete its entry; one that adds a difference fails.
type knownGap struct {
	diff  string
	owner string
}

func replayFixture(t *testing.T, name string) (*Run, CaseScore) {
	t.Helper()
	c, err := loadCase(filepath.Join("testdata", "cases", name+".json"))
	if err != nil {
		t.Fatal(err)
	}
	run, err := replayCase(context.Background(), c, publisher.DefaultPolicy(), stubResponder)
	if err != nil {
		t.Fatal(err)
	}
	return run, scoreCase(run)
}

func diffKey(d Diff) string {
	if d.Round < 0 {
		return fmt.Sprintf("%s reply %d", d.Kind, d.CommentID)
	}
	if d.CommentID != 0 {
		return fmt.Sprintf("%s round %d comment %d", d.Kind, d.Round+1, d.CommentID)
	}
	return fmt.Sprintf("%s round %d", d.Kind, d.Round+1)
}

func assertGaps(t *testing.T, s CaseScore, gaps []knownGap) {
	t.Helper()
	var got, want []string
	for _, d := range s.Diffs {
		got = append(got, diffKey(d))
	}
	for _, g := range gaps {
		if g.owner == "" {
			t.Fatalf("known gap %q names no item to remove it", g.diff)
		}
		want = append(want, g.diff)
	}
	sort.Strings(got)
	sort.Strings(want)
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("differences from the intended behaviour changed.\n got:\n  %s\nwant (known gaps):\n  %s\nfull diffs: %+v",
			strings.Join(got, "\n  "), strings.Join(want, "\n  "), s.Diffs)
	}
}

func postedIDs(run *Run, round int) []string {
	var out []string
	for _, rr := range run.Rounds {
		if rr.Index != round {
			continue
		}
		for _, p := range rr.Posts {
			out = append(out, p.FindingID)
		}
	}
	sort.Strings(out)
	return out
}

func assertPosted(t *testing.T, run *Run, round int, want ...string) {
	t.Helper()
	sort.Strings(want)
	if got := postedIDs(run, round); strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("round %d posted %v, want %v", round+1, got, want)
	}
}

func assertReply(t *testing.T, run *Run, authorComment int64, action string) {
	t.Helper()
	row, ok := run.Replies[authorComment]
	if got := replyAction(row, ok); got != action {
		t.Fatalf("reply to %d: got %s (outcome %q), want %s", authorComment, got, row.Outcome, action)
	}
}

func assertRowState(t *testing.T, run *Run, fingerprint, state string) {
	t.Helper()
	if got := run.FinalRows[fingerprint].State; got != state {
		t.Fatalf("ledger row %s is %q, want %q", fingerprint, got, state)
	}
}

func assertSince(t *testing.T, run *Run, round int, want string) {
	t.Helper()
	for _, rr := range run.Rounds {
		if rr.Index == round {
			if !strings.Contains(rr.Summary, want) {
				t.Fatalf("round %d summary lacks %q:\n%s", round+1, want, rr.Summary)
			}
			return
		}
	}
	t.Fatalf("round %d not replayed", round+1)
}

// The author calls a finding intentional and PRism concedes; the next push
// rewords the same finding. Intended: the reword is recognised as the
// dismissed defect and stays silent, and the cache fix counts as fixed.
func TestE2E_PushbackThenRewordStaysSilent(t *testing.T) {
	run, s := replayFixture(t, "pushback_reword")

	assertPosted(t, run, 0, "svc/retry.go:4:a1a1a1a1a1a1", "svc/cache.go:1:b2b2b2b2b2b2")
	assertReply(t, run, 201, ActionConcede)
	assertRowState(t, run, "svc/retry.go:4:a1a1a1a1a1a1", db.PublishedStateDismissed)
	assertReply(t, run, 202, ActionConcede)

	assertPosted(t, run, 1, "svc/retry.go:4:c3c3c3c3c3c3")
	assertSince(t, run, 1, "1 new · 0 still open · 0 fixed")
	assertGaps(t, s, []knownGap{
		{"posted_should_not round 2 comment 103", "W1-1: a dismissed row suppresses every later wording"},
		{"repost round 2", "W1-1"},
		{"summary_mismatch round 2", "W1-1 and W1-4: the reword is not new; a verified fix claim is fixed, not dismissed"},
	})
}

// One finding is fixed by a commit; another is simply missing from the next
// review although its file never changed, and the agent raises it again a
// round later. Intended: only the first counts as fixed, the second stays
// open and is never posted twice.
func TestE2E_FixCommitCountsAndAbsenceDoesNot(t *testing.T) {
	run, s := replayFixture(t, "fix_and_false_fixed")

	assertPosted(t, run, 0, "api/handler.go:2:d4d4d4d4d4d4", "api/limits.go:6:e5e5e5e5e5e5")
	assertReply(t, run, 211, ActionConcede)
	assertPosted(t, run, 1, "api/handler_test.go:1:f6f6f6f6f6f6")
	assertSince(t, run, 1, "1 new · 0 still open · 1 fixed")
	assertReply(t, run, 212, ActionConcede)

	assertPosted(t, run, 2, "api/limits.go:6:e5e5e5e5e5e5")
	if s.Fixed != 1 || s.WrongFixed != 1 {
		t.Fatalf("fixed %d, wrong %d; want the one false fix only", s.Fixed, s.WrongFixed)
	}
	assertGaps(t, s, []knownGap{
		{"wrong_fixed round 2 comment 112", "W1-1: absence is not fixed when the cited file did not change"},
		{"summary_mismatch round 2", "W1-1: the unchanged finding is still open, not fixed"},
		{"posted_should_not round 3 comment 114", "W1-1: every row with a comment id counts as already published"},
		{"repost round 3", "W1-1"},
		{"summary_mismatch round 3", "W1-1 and W1-4"},
	})
}

// Greptile raised a point two minutes before PRism; the review then runs
// again on the same commit, and a fix lands. Intended: no second root for
// Greptile's point, a silent same-commit round, and the fix counted once.
func TestE2E_GreptileDuplicateAndSameCommitRerun(t *testing.T) {
	run, s := replayFixture(t, "greptile_duplicate")

	assertPosted(t, run, 0, "web/form.tsx:8:a7a7a7a7a7a7", "web/form.tsx:12:b8b8b8b8b8b8")
	assertPosted(t, run, 1)
	assertSince(t, run, 1, "0 new · 2 still open · 0 fixed")
	assertPosted(t, run, 2)
	assertSince(t, run, 2, "0 new · 1 still open · 1 fixed")
	assertReply(t, run, 221, ActionConcede)
	if _, ok := run.Replies[321]; ok {
		t.Fatal("a reviewer who is not the PR author got a reply step")
	}
	assertGaps(t, s, []knownGap{
		{"posted_should_not round 1 comment 121", "W3-1: a match to another bot's root becomes an external row"},
		{"summary_mismatch round 2", "W3-1: an external row is not counted as a PRism finding"},
		{"summary_mismatch round 3", "W3-1"},
	})
}

func TestFixtureAggregates(t *testing.T) {
	cases, err := loadCases("testdata")
	if err != nil {
		t.Fatal(err)
	}
	res, err := runCases(context.Background(), cases, "", publisher.DefaultPolicy(), stubResponder)
	if err != nil {
		t.Fatal(err)
	}
	g := res.Tiers[TierGold]
	got := fmt.Sprintf("cases=%d post=%d/%d precision=%s recall=%s suppression=%s reposts=%d fixed=%d wrong=%d summary=%s replies=%s resolution=%s",
		g.Cases, g.ShouldPost, g.ShouldSuppress, fmtRatio(g.PostPrecision), fmtRatio(g.PostRecall), fmtRatio(g.SuppressionRecall),
		g.Reposts, g.Fixed, g.WrongFixed, fmtRatio(g.SummaryCorrect), fmtRatio(g.ReplyAccuracy), fmtRatio(g.ResolutionRecall))
	want := "cases=3 post=6/3 precision=0.667 recall=1.000 suppression=0.000 reposts=2 fixed=2 wrong=1 summary=0.000 replies=1.000 resolution=n/a"
	if got != want {
		t.Fatalf("gold aggregate\n got %s\nwant %s", got, want)
	}
	if res.Tiers[TierAccepted].Cases != 0 {
		t.Fatalf("accepted cases = %d", res.Tiers[TierAccepted].Cases)
	}
}

func TestThresholdsGateOnGold(t *testing.T) {
	cases, err := loadCases("testdata")
	if err != nil {
		t.Fatal(err)
	}
	res, err := runCases(context.Background(), cases, "", publisher.DefaultPolicy(), stubResponder)
	if err != nil {
		t.Fatal(err)
	}
	pass := Thresholds{MinPostPrecision: 0.6, MinSuppressionRecall: -1, MaxReposts: 2, MaxWrongFixed: -1, MinSummaryCorrect: -1, MinReplyAccuracy: 1, MinResolutionRecall: 0.9}
	if f := pass.check(res.Tiers[TierGold]); len(f) != 0 {
		t.Fatalf("thresholds met but failed: %v", f)
	}
	fail := Thresholds{MinPostPrecision: 0.9, MinSuppressionRecall: 0.5, MaxReposts: 0, MaxWrongFixed: 0, MinSummaryCorrect: -1, MinReplyAccuracy: -1, MinResolutionRecall: -1}
	if f := fail.check(res.Tiers[TierGold]); len(f) != 4 {
		t.Fatalf("want four failures, got %v", f)
	}
	if f := (Thresholds{MinPostPrecision: 0.5, MinSuppressionRecall: -1, MaxReposts: -1, MaxWrongFixed: -1, MinSummaryCorrect: -1, MinReplyAccuracy: -1, MinResolutionRecall: -1}).check(TierMetrics{}); len(f) != 1 {
		t.Fatalf("an unmeasured gated metric must fail, got %v", f)
	}
}

func TestMarkdownReportListsDiffs(t *testing.T) {
	cases, err := loadCases("testdata")
	if err != nil {
		t.Fatal(err)
	}
	res, err := runCases(context.Background(), cases, "acme/example#13", publisher.DefaultPolicy(), stubResponder)
	if err != nil {
		t.Fatal(err)
	}
	md := renderMarkdown(res)
	for _, want := range []string{"| post precision | 0.500 |", "### acme/example#13 (gold)", "posted_should_not (round 1, comment 121): external first raised by greptile", "| fix_claim | 1 / 1 |"} {
		if !strings.Contains(md, want) {
			t.Fatalf("report lacks %q:\n%s", want, md)
		}
	}
}

func TestUnknownCaseIDIsAnError(t *testing.T) {
	cases, err := loadCases("testdata")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := runCases(context.Background(), cases, "acme/example#999", publisher.DefaultPolicy(), stubResponder); err == nil {
		t.Fatal("a --case that matches nothing must fail")
	}
}
