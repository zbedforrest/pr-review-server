package publisher

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"testing"

	"pr-review-server/db"
	"pr-review-server/pkg/reviewer/payload"
)

func TestRefreshSummary_DropsTheConcededBulletAndKeepsTheRoundCounts(t *testing.T) {
	gh, ledger := newFakeGitHub(), newFakeLedger()
	publishRound(t, gh, ledger, filedRound())

	r2 := filedRound()
	r2.HeadSHA, r2.RoundNumber = "sha-2", 0
	r2.Changes = changed("a.go", "b.go")
	r2.Commentable["a.go"][50] = true
	m1, n1 := payload.Fingerprint("b.go", 20, "Medium thing."), payload.Fingerprint("a.go", 50, "New critical.")
	r2.Findings = []payload.Finding{
		f("sum", "unknown", "SUMMARY", 0, "Narrative."),
		f(m1, "medium", "b.go", 20, "Medium thing."),
		f(payload.Fingerprint("a.go", 99, "Medium outside hunk."), "medium", "a.go", 99, "Medium outside hunk."),
		f(n1, "critical", "a.go", 50, "New critical."),
	}
	publishRound(t, gh, ledger, r2)
	if !strings.Contains(gh.issueEdits[501], "**Since last review:** 1 new · 2 still open · 1 fixed") {
		t.Fatalf("round 2 summary:\n%s", gh.issueEdits[501])
	}
	gh.existingIssueComments = []IssueComment{{ID: 501, Body: gh.issueEdits[501]}}
	ledger.rows[m1].State = db.PublishedStateDismissed
	reviewsBefore, editsBefore := len(gh.reviews), len(gh.issueEdits)

	refresh := r2
	refresh.RoundNumber, refresh.HeadSHA, refresh.Previous = 0, "", nil
	p := &Publisher{GH: gh, Ledger: ledger, Policy: DefaultPolicy()}
	if err := p.RefreshSummary(context.Background(), refresh); err != nil {
		t.Fatal(err)
	}
	out := gh.issueEdits[501]
	if strings.Contains(out, "Medium thing") {
		t.Fatalf("the conceded finding must leave the summary:\n%s", out)
	}
	for _, want := range []string{"New critical", "Medium outside hunk", "**Since last review:** 1 new · 1 still open · 1 fixed", "reviewed sha-2", "Reviews (2)"} {
		if !strings.Contains(out, want) {
			t.Errorf("refreshed summary missing %q:\n%s", want, out)
		}
	}
	if !strings.Contains(out, "#discussion_r"+itoa64(ledger.rows[n1].CommentID)) {
		t.Errorf("bullets keep linking to their inline comments:\n%s", out)
	}
	if len(gh.reviews) != reviewsBefore || len(gh.issueEdits) != editsBefore || len(gh.issueCreates) != 1 {
		t.Fatalf("a refresh edits the summary and posts nothing else: reviews=%d edits=%d creates=%d", len(gh.reviews), len(gh.issueEdits), len(gh.issueCreates))
	}
	if sum := ledger.get(db.PublishedKindSummary, "summary"); sum.Rounds != 2 || sum.LastSeenSHA != "sha-2" {
		t.Fatalf("a refresh is not a round: %+v", sum)
	}
}

func TestRefreshSummary_SkipsTheEditWhenANewerRoundRewroteTheSummary(t *testing.T) {
	gh, ledger := newFakeGitHub(), newFakeLedger()
	publishRound(t, gh, ledger, roundOne())
	gh.existingIssueComments = []IssueComment{{ID: 501, Body: gh.issueEdits[501]}}
	before := gh.issueEdits[501]
	reads := 0
	ledger.onRead = func() {
		reads++
		if reads == 2 {
			sum := ledger.get(db.PublishedKindSummary, "summary")
			sum.LastSeenSHA, sum.Rounds = "sha-2", 2
		}
	}
	refresh := roundOne()
	refresh.HeadSHA, refresh.Previous = "", nil
	p := &Publisher{GH: gh, Ledger: ledger, Policy: DefaultPolicy()}
	if err := p.RefreshSummary(context.Background(), refresh); !errors.Is(err, ErrSummaryMoved) {
		t.Fatalf("err = %v, want ErrSummaryMoved", err)
	}
	if gh.issueEdits[501] != before {
		t.Fatal("a stale refresh must not overwrite the newer summary")
	}
}

func TestRefreshSummary_AbortsWhenTheSummaryCannotBeRead(t *testing.T) {
	gh, ledger := newFakeGitHub(), newFakeLedger()
	publishRound(t, gh, ledger, roundOne())
	before := gh.issueEdits[501]
	p := &Publisher{GH: gh, Ledger: ledger, Policy: DefaultPolicy()}

	gh.listErr = errors.New("rate limited")
	if err := p.RefreshSummary(context.Background(), roundOne()); err == nil {
		t.Fatal("a failed read must abort the refresh instead of rendering 0 fixed")
	}
	gh.listErr, gh.existingIssueComments = nil, nil
	if err := p.RefreshSummary(context.Background(), roundOne()); err != nil {
		t.Fatal(err)
	}
	if gh.issueEdits[501] != before {
		t.Fatal("a summary that is no longer listed must not be edited")
	}
}

func TestRefreshSummary_WithoutASummaryCommentDoesNothing(t *testing.T) {
	gh, ledger := newFakeGitHub(), newFakeLedger()
	p := &Publisher{GH: gh, Ledger: ledger, Policy: DefaultPolicy()}
	if err := p.RefreshSummary(context.Background(), roundOne()); err != nil {
		t.Fatal(err)
	}
	if len(gh.issueEdits) != 0 || len(gh.issueCreates) != 0 {
		t.Fatal("nothing to refresh must write nothing")
	}
}

func TestParseSinceLastReview(t *testing.T) {
	d, ok := parseSinceLastReview("x\n**Since last review:** 2 new · 3 still open · 4 fixed\ny")
	if !ok || d != (roundDiff{New: 2, StillOpen: 3, Fixed: 4}) {
		t.Fatalf("parsed %+v ok=%t", d, ok)
	}
	if _, ok := parseSinceLastReview("no line"); ok {
		t.Fatal("a body without the line must not parse")
	}
}

func itoa64(n int64) string { return strconv.FormatInt(n, 10) }
