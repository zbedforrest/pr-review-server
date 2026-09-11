package poller

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"pr-review-server/db"
	"pr-review-server/pkg/publisher"
)

func TestReplyLinkDueTriesEachPROnceThenOnlyOnFullScans(t *testing.T) {
	now := time.Date(2026, 9, 9, 12, 0, 0, 0, time.UTC)
	rows := []db.UnlinkedPublishedFinding{
		{ID: 1, RepoOwner: "acme", RepoName: "example", PRNumber: 1, ReviewID: 5, Fingerprint: "a"},
		{ID: 2, RepoOwner: "acme", RepoName: "example", PRNumber: 1, ReviewID: 5, Fingerprint: "b"},
		{ID: 3, RepoOwner: "acme", RepoName: "example", PRNumber: 2, ReviewID: 6, Fingerprint: "c"},
	}
	tried := map[string]time.Time{"acme/example#2": now.Add(-time.Minute)}

	due := replyLinkDue(rows, tried, false, now)
	if len(due) != 2 || due[0].PRNumber != 1 || due[1].PRNumber != 1 {
		t.Fatalf("incremental = %+v, want only the never-tried PR's rows", due)
	}
	if !tried["acme/example#1"].Equal(now) {
		t.Errorf("attempt must be stamped so a comment that never matches is not re-listed every cycle")
	}
	if len(replyLinkDue(rows, tried, false, now.Add(time.Minute))) != 0 {
		t.Errorf("nothing is due until the next full scan once every PR has been tried")
	}
	if len(replyLinkDue(rows, tried, true, now.Add(time.Minute))) != 3 {
		t.Errorf("a full scan retries every unlinked row")
	}
}

func TestUnstampFailedLinksRetriesErroredPRsNextCycle(t *testing.T) {
	now := time.Date(2026, 9, 9, 12, 0, 0, 0, time.UTC)
	tried := map[string]time.Time{"acme/example#1": now, "acme/example#2": now}
	unstampFailedLinks(tried, []string{"acme/example#2: list thread: 502"})
	if _, ok := tried["acme/example#1"]; !ok {
		t.Errorf("an unmatched PR stays stamped until the next full scan")
	}
	if _, ok := tried["acme/example#2"]; ok {
		t.Errorf("an errored PR must be retried next cycle")
	}
}

func TestReplyTelemetryEventsTruncatesLabelsToTheColumnWidth(t *testing.T) {
	long := strings.Repeat("d/", 200) + "f.go:1:abc"
	rep := publisher.ReplyReport{Handled: []db.PublishedReply{{Fingerprint: long, Class: "question", Action: "reacted"}}}
	events := replyTelemetryEvents(rep, publisher.LinkReport{}, 3)
	if len(events) != 1 || len([]rune(events[0].Label)) != 255 {
		t.Fatalf("label must be cut to 255 runes, got %d", len([]rune(events[0].Label)))
	}
}

func TestReplyTelemetryEventsOnePerHandledReplyPlusLinksAndErrors(t *testing.T) {
	rep := publisher.ReplyReport{
		Handled: []db.PublishedReply{
			{RepoOwner: "acme", RepoName: "example", PRNumber: 7, Fingerprint: "a.go:1:abc", Class: "question", Action: "reacted", AuthorCommentID: 101},
			{RepoOwner: "acme", RepoName: "example", PRNumber: 7, Fingerprint: "b.go:2:def", Class: "resolution", Action: "observed", AuthorCommentID: 102},
		},
		Errors: []string{"acme/example#9: list review comments: boom"},
	}
	link := publisher.LinkReport{Linked: 2, Unmatched: 1, Errors: []string{"acme/example#8: list thread: 502"}}
	events := replyTelemetryEvents(rep, link, 3)
	if len(events) != 5 {
		t.Fatalf("got %d events: %+v", len(events), events)
	}
	if events[0].UserID != 3 || events[0].Action != "reply_reacted" || events[0].PRNumber != 7 || events[0].Label != "class=question fp=a.go:1:abc comment=101" {
		t.Errorf("first event = %+v", events[0])
	}
	if events[1].Action != "reply_observed" {
		t.Errorf("second event = %+v", events[1])
	}
	if events[2].Action != "reply_scan_error" || events[2].PRNumber != 9 || events[2].PROwner != "acme" {
		t.Errorf("error event = %+v", events[2])
	}
	if events[3].Action != "reply_roots_linked" || events[3].Label != "linked=2 unmatched=1" {
		t.Errorf("link event = %+v", events[3])
	}
	if events[4].Action != "reply_link_error" || events[4].PRNumber != 8 {
		t.Errorf("link error event = %+v", events[4])
	}
	if got := replyTelemetryEvents(publisher.ReplyReport{}, publisher.LinkReport{Unmatched: 3}, 3); len(got) != 0 {
		t.Errorf("a pass that linked nothing is not link activity, got %+v", got)
	}
}

func TestReplyInputFromRequestMapsThreadRolesAndStripsNothingElse(t *testing.T) {
	t0 := time.Date(2026, 9, 9, 18, 0, 0, 0, time.UTC)
	req := publisher.ReplyRequest{
		Owner: "acme", Repo: "example", Number: 7, HeadSHA: "head1", BaseRef: "main", Fingerprint: "a.go:1:abc",
		Root: publisher.ThreadComment{ID: 100, AuthorID: 1, Author: "prism[bot]", Body: "finding"},
		Thread: []publisher.ThreadComment{
			{ID: 100, AuthorID: 1, Author: "prism[bot]", Body: "finding", CreatedAt: t0},
			{ID: 101, InReplyToID: 100, AuthorID: 42, Author: "pilot", Body: "pushback", CreatedAt: t0.Add(time.Minute)},
		},
		Reply: publisher.AuthorReply{CommentID: 101, Body: "pushback", Class: publisher.ReplyPushback},
	}
	in := replyInputFromRequest(req, 1)
	if in.Owner != "acme" || in.PRNumber != 7 || in.HeadSHA != "head1" || in.DefaultBranch != "main" || in.Fingerprint != "a.go:1:abc" || in.FindingBody != "finding" || in.AuthorReply != "pushback" || in.Class != "pushback" {
		t.Fatalf("input = %+v", in)
	}
	if len(in.Thread) != 2 || !in.Thread[0].Ours || in.Thread[1].Ours || in.Thread[1].Author != "pilot" {
		t.Fatalf("thread = %+v", in.Thread)
	}
}

func TestReplyOutcomeEventDistinguishesFailuresFromDecisions(t *testing.T) {
	o := publisher.ReplyOutcome{RepoOwner: "acme", RepoName: "example", PRNumber: 7, AuthorCommentID: 101, Decision: "hold", Outcome: "posted", Posted: true, Action: "observed", Model: "m", DurationMS: 1200}
	ev := replyOutcomeEvent(o, nil, 3)
	if ev.Action != "reply_decision" || ev.Label != "outcome=posted decision=hold posted=true action=observed model=m ms=1200 comment=101" || ev.PRNumber != 7 || ev.UserID != 3 {
		t.Errorf("event = %+v", ev)
	}
	ev = replyOutcomeEvent(o, fmt.Errorf("wall clock"), 3)
	if ev.Action != "reply_text_error" || ev.Label != "comment=101: wall clock" {
		t.Errorf("error event = %+v", ev)
	}
	for _, outcome := range []string{"skipped:thread_moved", "ineligible:thread_cap"} {
		o.Outcome = outcome
		if ev := replyOutcomeEvent(o, nil, 3); ev.Action != "reply_text_skipped" {
			t.Errorf("%s: action = %s", outcome, ev.Action)
		}
	}
}

func TestReplyLiveCandidatesLimitsLiveReadsToActiveOrMovedPRs(t *testing.T) {
	now := time.Date(2026, 9, 9, 19, 0, 0, 0, time.UTC)
	recent, old, moved := now.Add(-time.Hour), now.Add(-3*time.Hour), now.Add(-4*time.Hour)
	targets := []db.PublishedReplyTarget{
		{RepoOwner: "acme", RepoName: "example", PRNumber: 1},
		{RepoOwner: "acme", RepoName: "example", PRNumber: 2},
		{RepoOwner: "acme", RepoName: "example", PRNumber: 3},
		{RepoOwner: "acme", RepoName: "example", PRNumber: 4},
		{RepoOwner: "acme", RepoName: "example", PRNumber: 5},
		{RepoOwner: "acme", RepoName: "example", PRNumber: 6},
	}
	prs := map[string]*db.PR{
		"acme/example#1": {PRState: "open", GitHubUpdatedAt: &recent},
		"acme/example#2": {PRState: "open", GitHubUpdatedAt: &old},
		"acme/example#3": {PRState: "open", GitHubUpdatedAt: &moved},
		"acme/example#5": {PRState: "closed", GitHubUpdatedAt: &recent},
		"acme/example#6": {PRState: "open", Draft: true, GitHubUpdatedAt: &recent},
	}
	lookup := func(owner, repo string, n int) *db.PR { return prs[replyKey(owner, repo, n)] }
	last := map[string]time.Time{"acme/example#2": old, "acme/example#3": moved.Add(-time.Hour)}

	got := replyLiveCandidates(targets, lookup, last, false, now, 2*time.Hour)
	if len(got) != 3 || got[0].PRNumber != 1 || got[1].PRNumber != 3 || got[2].PRNumber != 4 {
		t.Fatalf("incremental = %+v, want recent (1), moved-since-settled (3) and uncached (4); cached closed (5) and draft (6) cost no live read", got)
	}
	if got := replyLiveCandidates(targets, lookup, last, true, now, 2*time.Hour); len(got) != 6 {
		t.Fatalf("full scan checks every target, got %d", len(got))
	}
}

func TestReplyClaimLeaseOutlastsTheWallClock(t *testing.T) {
	if got := replyClaimLease(180 * time.Second); got != 11*time.Minute {
		t.Errorf("180s wall clock -> %s, want 11m", got)
	}
	if got := replyClaimLease(30 * time.Second); got != 10*time.Minute {
		t.Errorf("short wall clock keeps the 10m floor, got %s", got)
	}
	if got := replyClaimLease(15 * time.Minute); got != 35*time.Minute {
		t.Errorf("long wall clock -> %s, want 35m", got)
	}
}

func TestReplyOutcomeEventsAlsoEmitTheSettledReaction(t *testing.T) {
	o := publisher.ReplyOutcome{RepoOwner: "acme", RepoName: "example", PRNumber: 7, AuthorCommentID: 101, Decision: "concede", Outcome: "posted", Posted: true, Action: "reacted"}
	events := replyOutcomeEvents(o, nil, 3)
	if len(events) != 2 || events[1].Action != "reply_reacted" || events[1].PRNumber != 7 {
		t.Fatalf("events = %+v", events)
	}
	if events := replyOutcomeEvents(o, fmt.Errorf("boom"), 3); len(events) != 2 {
		t.Fatalf("a reaction settled before the failure is still recorded: %+v", events)
	}
	o.Action = ""
	if events := replyOutcomeEvents(o, fmt.Errorf("boom"), 3); len(events) != 1 {
		t.Fatalf("no settled reaction, no reaction event: %+v", events)
	}
}

func TestReplyTelemetryEventsSkipPendingRowsUntilSettled(t *testing.T) {
	rep := publisher.ReplyReport{Handled: []db.PublishedReply{
		{RepoOwner: "acme", RepoName: "example", PRNumber: 7, Fingerprint: "a", Class: "pushback", Action: "pending", AuthorCommentID: 101},
		{RepoOwner: "acme", RepoName: "example", PRNumber: 7, Fingerprint: "b", Class: "resolution", Action: "reacted", AuthorCommentID: 102},
	}}
	events := replyTelemetryEvents(rep, publisher.LinkReport{}, 3)
	if len(events) != 1 || events[0].Action != "reply_reacted" {
		t.Fatalf("events = %+v", events)
	}
}
