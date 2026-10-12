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
	*fakeThreadGitHub
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
	gh, ledger := newFakeThreadGitHub(), newFakeLedger()
	r := externalRound("sha-1", greptileRetryRoot(900))
	r.Findings[1].Severity = "critical"
	rep := publishThreads(t, gh, ledger, r)

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
	if !strings.Contains(replies[0], "Suggested change for line 140:\n```\n\ttime.Sleep(backoff(i))\n```") || !strings.Contains(replies[0], "**How to verify:**") {
		t.Fatalf("the delta carries the suggested code and the experiment Greptile lacks: %q", replies[0])
	}
	if strings.Contains(replies[0], "```suggestion") {
		t.Fatalf("a suggestion block in another reviewer's thread would apply to that thread's lines: %q", replies[0])
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
	if rep.Confidence != 2 || !strings.Contains(summary, "merge confidence 2/5") {
		t.Fatalf("the matched critical still scores the PR: %d\n%s", rep.Confidence, summary)
	}
}

func TestExternal_LaterRoundsStaySilentAndKeepTheLink(t *testing.T) {
	gh, ledger := newFakeThreadGitHub(), newFakeLedger()
	publishThreads(t, gh, ledger, externalRound("sha-1", greptileRetryRoot(900)))

	r2 := externalRound("sha-2", greptileRetryRoot(900))
	r2.Findings[1].Comment = "fetchUser retries a 429 response at once with no backoff, so the rate limiter rejects each retry and nothing ever succeeds."
	r2.Findings[1].ID = fid("api/users.go", 141, r2.Findings[1].Comment)
	r2.Findings[1].Line = 141
	rep := publishThreads(t, gh, ledger, r2)
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

func TestExternal_SummaryRefreshKeepsTheLinkAndTheScore(t *testing.T) {
	gh, ledger := newFakeThreadGitHub(), newFakeLedger()
	r := externalRound("sha-1", greptileRetryRoot(900))
	r.Findings[1].Severity = "critical"
	publishThreads(t, gh, ledger, r)
	gh.existingIssueComments = []IssueComment{{ID: 501, Body: gh.issueCreates[0]}}
	ledger.rows[m1ID].State = db.PublishedStateDismissed

	refresh := externalRound("", greptileRetryRoot(900))
	refresh.Findings[1].Severity = "critical"
	p := &Publisher{GH: gh, Ledger: ledger, Policy: DefaultPolicy()}
	if err := p.RefreshSummary(context.Background(), refresh); err != nil {
		t.Fatal(err)
	}
	out := gh.issueEdits[501]
	if strings.Contains(out, "Medium thing") {
		t.Fatalf("the conceded finding must leave the summary:\n%s", out)
	}
	for _, want := range []string{"1 finding already raised in another reviewer's thread", "#discussion_r900", "merge confidence 3/5"} {
		if !strings.Contains(out, want) {
			t.Errorf("refreshed summary missing %q:\n%s", want, out)
		}
	}
}

func TestDeltaReply_SuggestedCodeIsNeverASuggestionBlock(t *testing.T) {
	root := greptileRetryRoot(900)
	root.StartLine, root.EndLine = 138, 141
	body := deltaReply(retryFinding(), root)
	if strings.Contains(body, "```suggestion") || !strings.Contains(body, "Suggested change for line 140:\n```\n\ttime.Sleep(backoff(i))\n```") {
		t.Fatalf("delta = %q", body)
	}
	root.Suggestion = "time.Sleep(backoff(i))"
	if body := deltaReply(retryFinding(), root); strings.Contains(body, "Suggested change") {
		t.Fatalf("a root with its own suggestion gets no code: %q", body)
	}
}

func TestExternal_ResolvedRootIsNotACandidate(t *testing.T) {
	gh, ledger := newFakeThreadGitHub(), newFakeLedger()
	gh.extra = []ReviewThread{{NodeID: "T900", RootCommentID: 900, Resolved: true}}
	rep := publishThreads(t, gh, ledger, externalRound("sha-1", greptileRetryRoot(900)))
	if rep.ExternalMatched != 0 || rep.InlinePosted != 2 || len(gh.repliesTo(900)) != 0 {
		t.Fatalf("a settled thread gets neither a match nor a reply: %+v replies=%v", rep, gh.repliesTo(900))
	}
}

func TestExternal_SummaryRefreshKeepsTheLinkForARewordedFinding(t *testing.T) {
	gh, ledger := newFakeThreadGitHub(), newFakeLedger()
	publishThreads(t, gh, ledger, externalRound("sha-1", greptileRetryRoot(900)))
	gh.existingIssueComments = []IssueComment{{ID: 501, Body: gh.issueCreates[0]}}

	refresh := externalRound("", greptileRetryRoot(900))
	refresh.Findings[1].Comment = "fetchUser retries a 429 response at once with no backoff, so the rate limiter rejects each retry and nothing ever succeeds."
	refresh.Findings[1].ID = fid("api/users.go", 141, refresh.Findings[1].Comment)
	refresh.Findings[1].Line = 141
	p := &Publisher{GH: gh, Ledger: ledger, Policy: DefaultPolicy()}
	if err := p.RefreshSummary(context.Background(), refresh); err != nil {
		t.Fatal(err)
	}
	out := gh.issueEdits[501]
	if !strings.Contains(out, "1 finding already raised in another reviewer's thread") || !strings.Contains(out, "#discussion_r900") {
		t.Fatalf("a reworded finding aliases to its external row on refresh:\n%s", out)
	}
	if strings.Count(out, "fetchUser retries") > 0 && strings.Index(out, "fetchUser retries") < strings.Index(out, "<details>") {
		t.Fatalf("the reworded point is listed only under the external fold:\n%s", out)
	}
}

func TestExternal_NoThreadListingDeclinesMatching(t *testing.T) {
	gh, ledger := newFakeGitHub(), newFakeLedger()
	rep := publishRound(t, gh, ledger, externalRound("sha-1", greptileRetryRoot(900)))
	if rep.ExternalMatched != 0 || rep.InlinePosted != 2 || len(gh.repliesTo(900)) != 0 {
		t.Fatalf("without a listing nothing is deferred or replied into: %+v", rep)
	}
}

func TestExternal_ListingFailureDeclinesMatching(t *testing.T) {
	gh, ledger := newFakeThreadGitHub(), newFakeLedger()
	gh.listErr = fmt.Errorf("502")
	rep := publishThreads(t, gh, ledger, externalRound("sha-1", greptileRetryRoot(900)))
	if rep.ExternalMatched != 0 || rep.InlinePosted != 2 || len(gh.repliesTo(900)) != 0 {
		t.Fatalf("a failed listing cannot tell a settled root from an open one: %+v", rep)
	}
}

func TestForgetShown(t *testing.T) {
	kept := retryFinding()
	r := Round{Findings: []payload.Finding{kept}, ExternalMatches: []ExternalMatch{{Finding: kept, CommentID: 900}, {Finding: f(m1ID, "medium", "b.go", 20, "Medium thing."), CommentID: 901}}}
	forgetShown(&r)
	if len(r.ExternalMatches) != 1 || r.ExternalMatches[0].CommentID != 901 {
		t.Fatalf("a finding the round still shows keeps no external link: %+v", r.ExternalMatches)
	}
}

func TestWithoutClaimedRoots(t *testing.T) {
	refs := []GreptileOnlyRef{{CommentID: 900, Title: "claimed earlier"}, {CommentID: 901, Title: "claimed now"}, {CommentID: 902, Title: "still only Greptile's"}}
	rows := map[string]*db.PublishedFinding{"a": {ExternalCommentID: 900}, "b": {}}
	kept := withoutClaimedRoots(refs, rows, []*db.PublishedFinding{{ExternalCommentID: 901}})
	if len(kept) != 1 || kept[0].CommentID != 902 {
		t.Fatalf("kept = %+v", kept)
	}
}

func TestExternal_ClaimedRootGetsNoSecondReply(t *testing.T) {
	gh, ledger := newFakeThreadGitHub(), newFakeLedger()
	publishThreads(t, gh, ledger, externalRound("sha-1", greptileRetryRoot(900)))

	// A terser finding that shares too little with the first row's prose to
	// alias to it, but enough with Greptile's root to match it.
	r2 := externalRound("sha-2", greptileRetryRoot(900))
	other := f(fid("api/users.go", 139, "x"), "medium", "api/users.go", 139, "Rejected by the rate limiter: the loop retries at once.")
	if reconcile.Similarity(other.Comment, retryText) >= 0.2 {
		t.Fatal("fixture must not alias to the first row")
	}
	r2.Findings = []payload.Finding{f("sum", "unknown", "SUMMARY", 0, "Narrative."), other}
	r2.Commentable = map[string]map[int]bool{"api/users.go": {139: true}}
	rep := publishThreads(t, gh, ledger, r2)
	if rep.InlinePosted != 0 || len(gh.repliesTo(900)) != 1 {
		t.Fatalf("the root is claimed, so no root and no second delta: %+v replies=%d", rep, len(gh.repliesTo(900)))
	}
	if row := ledger.get(db.PublishedKindAnnotation, other.ID); row == nil || row.State != db.PublishedStateExternal || row.ExternalCommentID != 900 {
		t.Fatalf("the second finding still gets its memory: %+v", row)
	}
}

func TestExternal_RelistsRightBeforePostingWhenGitHubAllows(t *testing.T) {
	gh := &fakeListingGitHub{fakeThreadGitHub: newFakeThreadGitHub(), comments: []ReviewComment{
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

	gh2 := &fakeListingGitHub{fakeThreadGitHub: newFakeThreadGitHub(), listErr: fmt.Errorf("502")}
	p = &Publisher{GH: gh2, Ledger: newFakeLedger(), Policy: testPolicy()}
	rep, err = p.Publish(context.Background(), externalRound("sha-1", greptileRetryRoot(900)))
	if err != nil || rep.ExternalMatched != 1 {
		t.Fatalf("a failed listing falls back to the round's roots: %+v %v", rep, err)
	}
}

func TestExternal_OwnRootsAndKnownRowsAreNotMatched(t *testing.T) {
	gh, ledger := newFakeThreadGitHub(), newFakeLedger()
	publishThreads(t, gh, ledger, externalRound("sha-1"))
	if len(gh.reviews) != 1 || len(gh.reviews[0].comments) != 2 {
		t.Fatalf("round one posts both roots: %+v", gh.reviews)
	}
	// Greptile restates PRism's point after PRism posted it: the open row
	// keeps its thread and the later root is not a match.
	rep := publishThreads(t, gh, ledger, externalRound("sha-2", greptileRetryRoot(900)))
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
	gh, ledger := newFakeThreadGitHub(), newFakeLedger()
	root := greptileRetryRoot(900)
	root.Suggestion = "time.Sleep(backoff(i))"
	root.Body += " Send the request with the field unset. The handler returns 500 instead of 400."
	rep := publishThreads(t, gh, ledger, externalRound("sha-1", root))
	if rep.ExternalMatched != 1 || rep.ExternalReplies != 0 || len(gh.repliesTo(900)) != 0 {
		t.Fatalf("report = %+v replies=%v", rep, gh.repliesTo(900))
	}
	if row := ledger.get(db.PublishedKindAnnotation, retryID); row == nil || row.ExternalCommentID != 900 {
		t.Fatalf("the row is still written: %+v", row)
	}
}
