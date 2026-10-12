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

// The author calls a finding intentional and PRism settles the verdict; the
// next push rewords the same finding. The reword is recognised as the
// dismissed defect and stays silent, the verdict's thread is resolved, and
// the conceded cache fix counts as fixed in the next summary.
func TestE2E_PushbackThenRewordStaysSilent(t *testing.T) {
	run, s := replayFixture(t, "pushback_reword")

	assertPosted(t, run, 0, "svc/retry.go:4:a1a1a1a1a1a1", "svc/cache.go:1:b2b2b2b2b2b2")
	assertReply(t, run, 201, ActionConcede)
	assertRowState(t, run, "svc/retry.go:4:a1a1a1a1a1a1", db.PublishedStateDismissed)
	assertReply(t, run, 202, ActionConcede)
	assertRowState(t, run, "svc/cache.go:1:b2b2b2b2b2b2", db.PublishedStateFixed)

	assertPosted(t, run, 1)
	assertSince(t, run, 1, "0 new · 0 still open · 1 fixed")
	if len(run.Rec.Resolved) != 2 {
		t.Fatalf("both threads are resolved, one by the verdict and one by the conceded fix: %v", run.Rec.Resolved)
	}
	assertGaps(t, s, nil)
}

// One finding is fixed by a commit the author reports; another is simply
// missing from the next review although its file never changed, and the
// agent raises it again a round later. Only the conceded fixes count, each
// once, in the summary after the concession; the untouched finding stays
// open and is never posted twice.
func TestE2E_FixCommitCountsAndAbsenceDoesNot(t *testing.T) {
	run, s := replayFixture(t, "fix_and_false_fixed")

	assertPosted(t, run, 0, "api/handler.go:2:d4d4d4d4d4d4", "api/limits.go:6:e5e5e5e5e5e5")
	assertReply(t, run, 211, ActionConcede)
	assertPosted(t, run, 1, "api/handler_test.go:1:f6f6f6f6f6f6")
	assertSince(t, run, 1, "1 new · 1 still open · 1 fixed")
	assertReply(t, run, 212, ActionConcede)

	assertPosted(t, run, 2)
	assertSince(t, run, 2, "0 new · 1 still open · 1 fixed")
	assertRowState(t, run, "api/limits.go:6:e5e5e5e5e5e5", db.PublishedStateOpen)
	assertRowState(t, run, "api/handler.go:2:d4d4d4d4d4d4", db.PublishedStateFixed)
	if s.Fixed != 0 || s.WrongFixed != 0 {
		t.Fatalf("fixed %d, wrong %d; the fixes come from concessions between rounds and the untouched finding stays open", s.Fixed, s.WrongFixed)
	}
	assertGaps(t, s, nil)
}

// Greptile raised a point two minutes before PRism; the review then runs
// again on the same commit, and a fix lands. Greptile's point gets no second
// root: an external row, one delta reply in Greptile's thread and a summary
// link. The same-commit round is silent and the fix is counted once.
func TestE2E_GreptileDuplicateAndSameCommitRerun(t *testing.T) {
	run, s := replayFixture(t, "greptile_duplicate")

	assertPosted(t, run, 0, "web/form.tsx:12:b8b8b8b8b8b8")
	assertRowState(t, run, "web/form.tsx:8:a7a7a7a7a7a7", db.PublishedStateExternal)
	if row := run.FinalRows["web/form.tsx:8:a7a7a7a7a7a7"]; row.ExternalCommentID != 301 || row.CommentID != 0 {
		t.Fatalf("the external row names Greptile's root and owns no comment: %+v", row)
	}
	deltas := 0
	for _, rp := range run.Rec.Replies {
		if rp.RootCommentID == 301 {
			deltas++
			if !strings.Contains(rp.Body, publisher.ExternalReplyMarker("web/form.tsx:8:a7a7a7a7a7a7")) {
				t.Fatalf("delta reply lacks its marker: %q", rp.Body)
			}
		}
	}
	if deltas != 1 {
		t.Fatalf("one delta reply under Greptile's root over three rounds, got %d", deltas)
	}
	if !strings.Contains(run.Rounds[0].Summary, "#discussion_r301") || !strings.Contains(run.Rounds[0].Summary, "already raised in another reviewer's thread") {
		t.Fatalf("summary links to Greptile's thread:\n%s", run.Rounds[0].Summary)
	}
	assertPosted(t, run, 1)
	if run.Rounds[1].Summary != run.Rounds[0].Summary || !run.Rounds[1].Skipped {
		t.Fatalf("a same-commit round leaves the summary untouched:\n%s", run.Rounds[1].Summary)
	}
	assertPosted(t, run, 2)
	assertSince(t, run, 2, "0 new · 0 still open · 1 fixed")
	assertReply(t, run, 221, ActionConcede)
	if _, ok := run.Replies[321]; ok {
		t.Fatal("a reviewer who is not the PR author got a reply step")
	}
	assertGaps(t, s, nil)
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
	want := "cases=3 post=6/3 precision=1.000 recall=1.000 suppression=1.000 reposts=0 fixed=1 wrong=0 summary=1.000 replies=1.000 resolution=1.000"
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
	pass := Thresholds{MinPostPrecision: 0.8, MinSuppressionRecall: 0.6, MaxReposts: 0, MaxWrongFixed: 0, MinSummaryCorrect: -1, MinReplyAccuracy: 1, MinResolutionRecall: 0.8}
	if f := pass.check(res.Tiers[TierGold]); len(f) != 0 {
		t.Fatalf("thresholds met but failed: %v", f)
	}
	fail := Thresholds{MinPostPrecision: 0.9, MinSuppressionRecall: 0.7, MaxReposts: 0, MaxWrongFixed: -1, MinSummaryCorrect: 0.5, MinReplyAccuracy: -1, MinResolutionRecall: 0.9}
	short := TierMetrics{PostPrecision: ratio(1, 2), SuppressionRecall: ratio(1, 2), SummaryCorrect: ratio(0, 1), ResolutionRecall: ratio(1, 2), Reposts: 1}
	if f := fail.check(short); len(f) != 5 {
		t.Fatalf("want five failures, got %v", f)
	}
	if f := fail.check(res.Tiers[TierGold]); len(f) != 0 {
		t.Fatalf("the fixtures meet the stricter thresholds too: %v", f)
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
	if len(res.Cases) != 1 || len(res.Cases[0].Diffs) != 0 {
		t.Fatalf("the fixture replays without differences: %+v", res.Cases)
	}
	if md := renderMarkdown(res); strings.Contains(md, "### acme/example#13") || !strings.Contains(md, "| post precision | 1.000 |") {
		t.Fatalf("a clean case has no per-case section:\n%s", md)
	}
	res.Cases[0].Diffs = []Diff{{Kind: DiffPostedShouldNot, Round: 0, CommentID: 121, Detail: "external first raised by greptile"}}
	md := renderMarkdown(res)
	for _, want := range []string{"### acme/example#13 (gold)", "posted_should_not (round 1, comment 121): external first raised by greptile", "| fix_claim | 1 / 1 |"} {
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
