package publisher

import (
	"context"
	"errors"
	"strings"
	"testing"

	"pr-review-server/db"
	"pr-review-server/pkg/reviewer/payload"
)

// Round counts must stabilize: a finding that disappears is fixed exactly once,
// and a persistent annotation-only finding is new exactly once.
func TestPublish_RoundCountsStabilizeAcrossThreeRounds(t *testing.T) {
	gh, ledger := newFakeGitHub(), newFakeLedger()
	publishRound(t, gh, ledger, roundOne()) // c1, m1 inline; l1, m2 annotations

	r2 := roundOne()
	r2.HeadSHA, r2.RoundNumber = "sha-2", 0
	r2.Findings = []payload.Finding{
		f("sum", "unknown", "SUMMARY", 0, "Narrative."),
		f("m1", "medium", "b.go", 20, "Medium thing."),
		f("l1", "low", "a.go", 11, "Low thing."),
		f("m2", "medium", "a.go", 99, "Medium outside hunk."),
	}
	rep2 := publishRound(t, gh, ledger, r2)
	if rep2.Fixed != 1 || rep2.StillOpen != 3 {
		t.Fatalf("round 2 report = %+v, want fixed=1 (c1) still_open=3 (m1, l1, m2)", rep2)
	}
	if !strings.Contains(gh.issueEdits[501], "**Since last review:** 0 new · 3 still open · 1 fixed") {
		t.Fatalf("round 2 summary:\n%s", gh.issueEdits[501])
	}
	if c1 := ledger.get(db.PublishedKindFinding, "c1"); c1 == nil || c1.State != db.PublishedStateResolved {
		t.Fatalf("disappeared inline finding must be marked resolved in the ledger: %+v", c1)
	}
	if l1 := ledger.get(db.PublishedKindAnnotation, "l1"); l1 == nil || l1.LastSeenSHA != "sha-2" {
		t.Fatalf("annotation-only finding must be tracked in the ledger: %+v", l1)
	}

	r3 := r2
	r3.HeadSHA = "sha-3"
	rep3 := publishRound(t, gh, ledger, r3)
	if rep3.Fixed != 0 || rep3.StillOpen != 3 {
		t.Fatalf("round 3 report = %+v, want fixed=0 still_open=3", rep3)
	}
	if !strings.Contains(gh.issueEdits[501], "**Since last review:** 0 new · 3 still open · 0 fixed") {
		t.Fatalf("round 3 summary:\n%s", gh.issueEdits[501])
	}
	if len(gh.reviews) != 1 {
		t.Fatalf("rounds 2 and 3 introduced nothing new inline; reviews=%d", len(gh.reviews))
	}
}

func TestSelect_CapZeroDisablesInline(t *testing.T) {
	r := roundOne()
	sel := Select(r.Findings, nil, r.Commentable, Policy{InlineCap: 0, InlineMinSeverity: "medium"})
	if len(sel.Inline) != 0 || len(sel.Annotations) != 4 {
		t.Fatalf("cap 0 must route everything to annotations: inline=%d annotations=%d", len(sel.Inline), len(sel.Annotations))
	}
}

func TestSelect_NegativeCapFallsBackToDefault(t *testing.T) {
	r := roundOne()
	sel := Select(r.Findings, nil, r.Commentable, Policy{InlineCap: -1, InlineMinSeverity: "medium"})
	if len(sel.Inline) != 2 {
		t.Fatalf("negative cap must use the default cap: inline=%d", len(sel.Inline))
	}
}

func TestTruncate_IsRuneSafe(t *testing.T) {
	got := truncate("héllo wörld — ünïcode", 10)
	if !strings.HasSuffix(got, "...") || strings.ContainsRune(got, '�') || len([]rune(got)) != 10 {
		t.Fatalf("truncate must cut on rune boundaries: %q", got)
	}
}

// If the ledger lost the summary row (or the comment was deleted on GitHub),
// the marker in the comment body is the recovery path.
func TestPublish_RecoversSummaryByMarkerWhenLedgerHasNoRow(t *testing.T) {
	gh, ledger := newFakeGitHub(), newFakeLedger()
	gh.existingIssueComments = []IssueComment{{ID: 77, Body: SummaryMarker + "\nold summary"}}
	rep := publishRound(t, gh, ledger, roundOne())
	if len(gh.issueCreates) != 0 || rep.SummaryCommentID != 77 || gh.issueEdits[77] == "" {
		t.Fatalf("must edit the marked comment instead of creating a second summary: creates=%d id=%d", len(gh.issueCreates), rep.SummaryCommentID)
	}
}

func TestPublish_RecreatesSummaryWhenEditFinds404(t *testing.T) {
	gh, ledger := newFakeGitHub(), newFakeLedger()
	publishRound(t, gh, ledger, roundOne())
	gh.editErr = errors.New("404 Not Found")
	r2 := roundOne()
	r2.HeadSHA = "sha-2"
	rep := publishRound(t, gh, ledger, r2)
	if len(gh.issueCreates) != 2 || rep.SummaryCommentID != 502 {
		t.Fatalf("a deleted summary must be recreated: creates=%d id=%d", len(gh.issueCreates), rep.SummaryCommentID)
	}
	if sum := ledger.get(db.PublishedKindSummary, "summary"); sum == nil || sum.CommentID != 502 {
		t.Fatalf("ledger must point at the recreated comment: %+v", sum)
	}
}

var _ = context.Background

func TestPublish_PromotedAnnotationKeepsFindingKind(t *testing.T) {
	gh, ledger := newFakeGitHub(), newFakeLedger()
	r1 := roundOne() // l1 is annotation-only in round 1 (low severity)
	publishRound(t, gh, ledger, r1)

	r2 := roundOne()
	r2.HeadSHA, r2.RoundNumber = "sha-2", 0
	p := &Publisher{GH: gh, Ledger: ledger, Policy: Policy{InlineCap: DefaultInlineCap, InlineMinSeverity: "low"}}
	if _, err := p.Publish(context.Background(), r2); err != nil {
		t.Fatal(err)
	}
	row := ledger.get(db.PublishedKindFinding, "l1")
	if row == nil || row.Kind != db.PublishedKindFinding || row.CommentID == 0 {
		t.Fatalf("a promoted annotation must end the round as an inline finding row: %+v", row)
	}
	if ledger.get(db.PublishedKindAnnotation, "l1") != nil {
		t.Fatalf("stale annotation snapshot must not be written back")
	}
	if _, err := p.Publish(context.Background(), r2); err != nil {
		t.Fatal(err)
	}
	if len(gh.reviews) != 2 {
		t.Fatalf("a promoted finding must not be posted inline again: reviews=%d", len(gh.reviews))
	}
}

func TestPublish_ReappearingResolvedFindingIsReopenedAndReposted(t *testing.T) {
	gh, ledger := newFakeGitHub(), newFakeLedger()
	publishRound(t, gh, ledger, roundOne())

	gone := roundOne()
	gone.HeadSHA, gone.RoundNumber = "sha-2", 0
	gone.Findings = []payload.Finding{f("sum", "unknown", "SUMMARY", 0, "Narrative.")}
	publishRound(t, gh, ledger, gone)
	if row := ledger.get(db.PublishedKindFinding, "c1"); row == nil || row.State != db.PublishedStateResolved {
		t.Fatalf("precondition: c1 resolved, got %+v", row)
	}

	back := roundOne()
	back.HeadSHA, back.RoundNumber = "sha-3", 0
	rep := publishRound(t, gh, ledger, back)
	if rep.InlinePosted != 2 || len(gh.reviews) != 2 {
		t.Fatalf("reappearing findings must be posted again: %+v reviews=%d", rep, len(gh.reviews))
	}
	if row := ledger.get(db.PublishedKindFinding, "c1"); row == nil || row.State != db.PublishedStateOpen || row.CommentID == 1001 {
		t.Fatalf("reopened row must be open with the new comment id: %+v", row)
	}
	if !strings.Contains(gh.issueEdits[501], "**Since last review:** 4 new · 0 still open · 0 fixed") {
		t.Fatalf("reappearing findings count as new:\n%s", gh.issueEdits[501])
	}
}
