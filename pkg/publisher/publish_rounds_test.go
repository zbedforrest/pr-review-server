package publisher

import (
	"context"
	"errors"
	"strings"
	"testing"

	"pr-review-server/db"
	"pr-review-server/pkg/reviewer/payload"
)

// Round counts must stabilize: a finding that disappears from a changed file
// is fixed exactly once, and a persistent annotation-only finding is new
// exactly once.
func TestPublish_RoundCountsStabilizeAcrossThreeRounds(t *testing.T) {
	gh, ledger := newFakeGitHub(), newFakeLedger()
	publishRound(t, gh, ledger, ledgerRoundOne()) // c1, m1 inline; m2 annotation; l1 is low and never shown

	r2 := ledgerRoundOne()
	r2.HeadSHA, r2.RoundNumber = "sha-2", 0
	r2.Changes = changed("a.go")
	r2.Findings = []payload.Finding{
		f("sum", "unknown", "SUMMARY", 0, "Narrative."),
		f(m1ID, "medium", "b.go", 20, "Medium thing."),
		f("l1", "low", "a.go", 11, "Low thing."),
		f(m2ID, "medium", "a.go", 99, "Medium outside hunk."),
	}
	rep2 := publishRound(t, gh, ledger, r2)
	if rep2.Fixed != 1 || rep2.StillOpen != 2 {
		t.Fatalf("round 2 report = %+v, want fixed=1 (c1) still_open=2 (m1, m2)", rep2)
	}
	if !strings.Contains(gh.issueEdits[501], "**Since last review:** 0 new · 2 still open · 1 fixed") {
		t.Fatalf("round 2 summary:\n%s", gh.issueEdits[501])
	}
	if c1 := ledger.get(db.PublishedKindFinding, c1ID); c1 == nil || c1.State != db.PublishedStateFixed {
		t.Fatalf("a finding absent from a changed file must be marked fixed in the ledger: %+v", c1)
	}
	if m2 := ledger.get(db.PublishedKindAnnotation, m2ID); m2 == nil || m2.LastSeenSHA != "sha-2" {
		t.Fatalf("summary-only finding must be tracked in the ledger: %+v", m2)
	}
	if ledger.get(db.PublishedKindAnnotation, "l1") != nil {
		t.Fatalf("a low finding must leave no ledger row")
	}

	r3 := r2
	r3.HeadSHA = "sha-3"
	r3.Changes = changed("a.go", "b.go")
	rep3 := publishRound(t, gh, ledger, r3)
	if rep3.Fixed != 0 || rep3.StillOpen != 2 {
		t.Fatalf("round 3 report = %+v, want fixed=0 still_open=2", rep3)
	}
	if !strings.Contains(gh.issueEdits[501], "**Since last review:** 0 new · 2 still open · 0 fixed") {
		t.Fatalf("round 3 summary:\n%s", gh.issueEdits[501])
	}
	if len(gh.reviews) != 1 {
		t.Fatalf("rounds 2 and 3 introduced nothing new inline; reviews=%d", len(gh.reviews))
	}
}

func TestSelect_CapZeroDisablesInline(t *testing.T) {
	r := roundOne()
	sel := Select(r.Findings, nil, r.Commentable, Policy{InlineCap: 0, InlineMinSeverity: "medium"})
	if len(sel.Inline) != 0 || len(sel.Annotations) != 3 {
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
	r1 := roundOne() // m2 is summary-only in round 1 (its line is outside the diff)
	publishRound(t, gh, ledger, r1)

	r2 := roundOne()
	r2.HeadSHA, r2.RoundNumber = "sha-2", 0
	r2.Commentable["a.go"][99] = true
	p := &Publisher{GH: gh, Ledger: ledger, Policy: DefaultPolicy()}
	if _, err := p.Publish(context.Background(), r2); err != nil {
		t.Fatal(err)
	}
	row := ledger.get(db.PublishedKindFinding, "m2")
	if row == nil || row.Kind != db.PublishedKindFinding || row.CommentID == 0 {
		t.Fatalf("a promoted summary-only finding must end the round as an inline finding row: %+v", row)
	}
	if ledger.get(db.PublishedKindAnnotation, "m2") != nil {
		t.Fatalf("stale annotation snapshot must not be written back")
	}
	if _, err := p.Publish(context.Background(), r2); !errors.Is(err, ErrHeadAlreadyPublished) {
		t.Fatalf("a second round for the same head is refused, got %v", err)
	}
	p.Policy.RepublishSameCommit = true
	if _, err := p.Publish(context.Background(), r2); err != nil {
		t.Fatal(err)
	}
	if len(gh.reviews) != 2 {
		t.Fatalf("a promoted finding must not be posted inline again: reviews=%d", len(gh.reviews))
	}
}

func TestPublish_ReappearingFixedFindingIsReopenedInItsThread(t *testing.T) {
	gh, ledger := newFakeGitHub(), newFakeLedger()
	publishRound(t, gh, ledger, ledgerRoundOne())

	gone := ledgerRoundOne()
	gone.HeadSHA, gone.RoundNumber = "sha-2", 0
	gone.Changes = changed("a.go", "b.go")
	gone.Findings = []payload.Finding{f("sum", "unknown", "SUMMARY", 0, "Narrative.")}
	publishRound(t, gh, ledger, gone)
	if row := ledger.get(db.PublishedKindFinding, c1ID); row == nil || row.State != db.PublishedStateFixed {
		t.Fatalf("precondition: c1 fixed, got %+v", row)
	}

	back := ledgerRoundOne()
	back.HeadSHA, back.RoundNumber = "sha-3", 0
	back.Changes = changed("a.go", "b.go")
	rep := publishRound(t, gh, ledger, back)
	if rep.InlinePosted != 0 || len(gh.reviews) != 1 {
		t.Fatalf("a finding that comes back must not get a second root: %+v reviews=%d", rep, len(gh.reviews))
	}
	if rep.Reopened != 3 || rep.ThreadReplies != 2 {
		t.Fatalf("c1, m1 and the m2 annotation reopen; only the two threads get a note: %+v", rep)
	}
	if got := gh.repliesTo(1001); len(got) != 1 || got[0] != "Back at sha-3." {
		t.Fatalf("c1 thread replies = %q", got)
	}
	if row := ledger.get(db.PublishedKindFinding, c1ID); row == nil || row.State != db.PublishedStateOpen || row.CommentID != 1001 || row.LastSeenSHA != "sha-3" {
		t.Fatalf("reopened row must be open under its original comment: %+v", row)
	}
	if row := ledger.get(db.PublishedKindAnnotation, m2ID); row == nil || row.State != db.PublishedStateOpen {
		t.Fatalf("reopened annotation row = %+v", row)
	}
	if !strings.Contains(gh.issueEdits[501], "**Since last review:** 0 new · 3 still open · 0 fixed") {
		t.Fatalf("reopened findings count as still open, not new:\n%s", gh.issueEdits[501])
	}
}
