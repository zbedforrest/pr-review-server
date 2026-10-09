package publisher

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"pr-review-server/db"
	"pr-review-server/pkg/reviewer/payload"
	"pr-review-server/pkg/reviewer/reconcile"
)

// fakeListingGitHub is fakeGitHub plus the review-comment listing the ledger
// policy refreshes right before it posts.
type fakeListingGitHub struct {
	*fakeGitHub
	comments []ReviewComment
	listings int
	listErr  error
}

func (g *fakeListingGitHub) ListReviewComments(context.Context, string, string, int) ([]ReviewComment, error) {
	g.listings++
	if g.listErr != nil {
		return nil, g.listErr
	}
	return g.comments, nil
}

const retryText = "The retry loop in fetchUser never backs off, so a 429 from upstream is retried immediately and the rate limiter keeps rejecting every call."

var retryID = fid("api/users.go", 140, retryText)

func retryFinding() payload.Finding {
	x := f(retryID, "medium", "api/users.go", 140, retryText+"\n\n```suggestion\n\ttime.Sleep(backoff(i))\n```")
	x.FindingContract.CurrentImpact = "Every retry after a 429 is rejected, so the call fails for users today."
	x.FindingContract.Headline = "Retry loop never backs off"
	return x
}

func greptileRetryRoot(id int64) reconcile.ExternalFinding {
	return reconcile.ExternalFinding{Source: reconcile.SourceGreptile, CommentID: id, Severity: "medium", File: "api/users.go", StartLine: 138, EndLine: 141,
		Title: "Retry never backs off", Body: "fetchUser retries a 429 at once: the loop never backs off, so the rate limiter rejects every retry and the call fails."}
}

func externalRound(head string, roots ...reconcile.ExternalFinding) Round {
	return Round{
		Owner: "acme", Repo: "example", Number: 7, HeadSHA: head,
		Findings:    []payload.Finding{f("sum", "unknown", "SUMMARY", 0, "Narrative."), retryFinding(), f(m1ID, "medium", "b.go", 20, "Medium thing.")},
		Commentable: map[string]map[int]bool{"api/users.go": {140: true}, "b.go": {20: true}},
		External:    roots,
	}
}

func TestExternal_MatchBecomesARowALinkAndOneDeltaReply(t *testing.T) {
	gh, ledger := newFakeGitHub(), newFakeLedger()
	rep := publishRound(t, gh, ledger, externalRound("sha-1", greptileRetryRoot(900)))

	if rep.InlinePosted != 1 || rep.ExternalMatched != 1 || rep.ExternalReplies != 1 || rep.ThreadReplies != 0 {
		t.Fatalf("report = %+v", rep)
	}
	if len(gh.reviews) != 1 || len(gh.reviews[0].comments) != 1 || gh.reviews[0].comments[0].Path != "b.go" {
		t.Fatalf("only the unmatched finding gets a root: %+v", gh.reviews)
	}
	row := ledger.get(db.PublishedKindAnnotation, retryID)
	if row == nil || row.State != db.PublishedStateExternal || row.ExternalCommentID != 900 || row.CommentID != 0 || row.SourceTag != "external:greptile" || row.CommentText == "" {
		t.Fatalf("external row = %+v", row)
	}
	replies := gh.repliesTo(900)
	if len(replies) != 1 || !strings.HasPrefix(replies[0], ExternalReplyMarker(retryID)) {
		t.Fatalf("delta reply = %q", replies)
	}
	if !strings.Contains(replies[0], "```suggestion") || !strings.Contains(replies[0], "**How to verify:**") {
		t.Fatalf("the delta carries the suggestion and the experiment Greptile lacks: %q", replies[0])
	}
	if strings.Contains(replies[0], "fails for users today") {
		t.Fatalf("an impact Greptile already states is not repeated: %q", replies[0])
	}
	summary := gh.issueCreates[0]
	if !strings.Contains(summary, "1 finding already raised in another reviewer's thread") || !strings.Contains(summary, "#discussion_r900") || !strings.Contains(summary, "· Greptile") {
		t.Fatalf("summary lacks the external link:\n%s", summary)
	}
	if i := strings.Index(summary, "Retry loop never backs off"); i < 0 || i < strings.Index(summary, "<details>") {
		t.Fatalf("a matched finding is listed only under the external fold:\n%s", summary)
	}
	if rep.Confidence != 4 {
		t.Fatalf("the matched finding leaves the score to PRism's own: %d", rep.Confidence)
	}
}

func TestExternal_LaterRoundsStaySilentAndKeepTheLink(t *testing.T) {
	gh, ledger := newFakeGitHub(), newFakeLedger()
	publishRound(t, gh, ledger, externalRound("sha-1", greptileRetryRoot(900)))

	r2 := externalRound("sha-2", greptileRetryRoot(900))
	r2.Findings[1].Comment = "fetchUser retries a 429 response at once with no backoff, so the rate limiter rejects each retry and nothing ever succeeds."
	r2.Findings[1].ID = fid("api/users.go", 141, r2.Findings[1].Comment)
	r2.Findings[1].Line = 141
	rep := publishRound(t, gh, ledger, r2)
	if rep.InlinePosted != 0 || rep.ExternalMatched != 0 || rep.ExternalReplies != 0 || len(gh.repliesTo(900)) != 1 {
		t.Fatalf("a reworded match aliases to the external row and posts nothing: %+v replies=%v", rep, gh.repliesTo(900))
	}
	if len(gh.reviews) != 1 {
		t.Fatalf("no new review: %d", len(gh.reviews))
	}
	summary := gh.issueEdits[501]
	if !strings.Contains(summary, "#discussion_r900") || !strings.Contains(summary, "0 new · 1 still open · 0 fixed") {
		t.Fatalf("summary keeps the link and counts no new finding:\n%s", summary)
	}
	if rows := ledger.rows; len(rows) != 3 {
		t.Fatalf("no second row for the point: %d rows", len(rows))
	}
}

func TestExternal_ClaimedRootGetsNoSecondReply(t *testing.T) {
	gh, ledger := newFakeGitHub(), newFakeLedger()
	publishRound(t, gh, ledger, externalRound("sha-1", greptileRetryRoot(900)))

	// A terser finding that shares too little with the first row's prose to
	// alias to it, but enough with Greptile's root to match it.
	r2 := externalRound("sha-2", greptileRetryRoot(900))
	other := f(fid("api/users.go", 139, "x"), "medium", "api/users.go", 139, "Rejected by the rate limiter: the loop retries at once.")
	if reconcile.Similarity(other.Comment, retryText) >= 0.2 {
		t.Fatal("fixture must not alias to the first row")
	}
	r2.Findings = []payload.Finding{f("sum", "unknown", "SUMMARY", 0, "Narrative."), other}
	r2.Commentable = map[string]map[int]bool{"api/users.go": {139: true}}
	rep := publishRound(t, gh, ledger, r2)
	if rep.InlinePosted != 0 || len(gh.repliesTo(900)) != 1 {
		t.Fatalf("the root is claimed, so no root and no second delta: %+v replies=%d", rep, len(gh.repliesTo(900)))
	}
	if row := ledger.get(db.PublishedKindAnnotation, other.ID); row == nil || row.State != db.PublishedStateExternal || row.ExternalCommentID != 900 {
		t.Fatalf("the second finding still gets its memory: %+v", row)
	}
}

func TestExternal_RelistsRightBeforePostingWhenGitHubAllows(t *testing.T) {
	gh := &fakeListingGitHub{fakeGitHub: newFakeGitHub(), comments: []ReviewComment{
		{ID: 901, Author: "Copilot", Path: "api/users.go", Line: 141, Body: "fetchUser retries a 429 at once: the loop never backs off, so the rate limiter rejects every retry and the call fails."},
		{ID: 902, Author: "Copilot", Path: "api/users.go", Line: 141, InReplyToID: 901, Body: "same again"},
	}}
	ledger := newFakeLedger()
	p := &Publisher{GH: gh, Ledger: ledger, Policy: testPolicy()}
	rep, err := p.Publish(context.Background(), externalRound("sha-1"))
	if err != nil {
		t.Fatal(err)
	}
	if gh.listings != 1 || rep.ExternalMatched != 1 || rep.InlinePosted != 1 {
		t.Fatalf("the fresh listing is reconciled against: listings=%d rep=%+v", gh.listings, rep)
	}
	if row := ledger.get(db.PublishedKindAnnotation, retryID); row == nil || row.ExternalCommentID != 901 || row.SourceTag != "external:copilot" {
		t.Fatalf("row = %+v", row)
	}

	gh2 := &fakeListingGitHub{fakeGitHub: newFakeGitHub(), listErr: fmt.Errorf("502")}
	p = &Publisher{GH: gh2, Ledger: newFakeLedger(), Policy: testPolicy()}
	rep, err = p.Publish(context.Background(), externalRound("sha-1", greptileRetryRoot(900)))
	if err != nil || rep.ExternalMatched != 1 {
		t.Fatalf("a failed listing falls back to the round's roots: %+v %v", rep, err)
	}
}

func TestExternal_OwnRootsAndKnownRowsAreNotMatched(t *testing.T) {
	gh, ledger := newFakeGitHub(), newFakeLedger()
	publishRound(t, gh, ledger, externalRound("sha-1"))
	if len(gh.reviews) != 1 || len(gh.reviews[0].comments) != 2 {
		t.Fatalf("round one posts both roots: %+v", gh.reviews)
	}
	// Greptile restates PRism's point after PRism posted it: the open row
	// keeps its thread and the later root is not a match.
	rep := publishRound(t, gh, ledger, externalRound("sha-2", greptileRetryRoot(900)))
	if rep.ExternalMatched != 0 || rep.InlinePosted != 0 || len(gh.repliesTo(900)) != 0 {
		t.Fatalf("report = %+v", rep)
	}
	if row := ledger.get(db.PublishedKindFinding, retryID); row == nil || row.State != db.PublishedStateOpen {
		t.Fatalf("row = %+v", row)
	}
}

func TestExternal_OffKeepsTheTagOnlyBehaviour(t *testing.T) {
	gh, ledger := newFakeGitHub(), newFakeLedger()
	pol := testPolicy()
	pol.ExternalReconcile = false
	rep := publishWith(t, gh, ledger, externalRound("sha-1", greptileRetryRoot(900)), pol)
	if rep.InlinePosted != 2 || rep.ExternalMatched != 0 || len(gh.repliesTo(900)) != 0 {
		t.Fatalf("report = %+v", rep)
	}
}

func TestExternal_NoDeltaWhenTheRootSaysItAll(t *testing.T) {
	gh, ledger := newFakeGitHub(), newFakeLedger()
	root := greptileRetryRoot(900)
	root.Suggestion = "time.Sleep(backoff(i))"
	root.Body += " Send the request with the field unset. The handler returns 500 instead of 400."
	rep := publishRound(t, gh, ledger, externalRound("sha-1", root))
	if rep.ExternalMatched != 1 || rep.ExternalReplies != 0 || len(gh.repliesTo(900)) != 0 {
		t.Fatalf("report = %+v replies=%v", rep, gh.repliesTo(900))
	}
	if row := ledger.get(db.PublishedKindAnnotation, retryID); row == nil || row.ExternalCommentID != 900 {
		t.Fatalf("the row is still written: %+v", row)
	}
}
