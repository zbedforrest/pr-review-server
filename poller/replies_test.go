package poller

import (
	"testing"
	"time"

	"pr-review-server/db"
	"pr-review-server/pkg/publisher"
)

func TestReplyTargetsToScanSkipsUnchangedPRsExceptOnFullScans(t *testing.T) {
	t1 := time.Date(2026, 9, 8, 12, 0, 0, 0, time.UTC)
	t2 := t1.Add(time.Minute)
	targets := []db.PublishedReplyTarget{
		{RepoOwner: "acme", RepoName: "example", PRNumber: 1},
		{RepoOwner: "acme", RepoName: "example", PRNumber: 2},
		{RepoOwner: "acme", RepoName: "example", PRNumber: 3},
		{RepoOwner: "acme", RepoName: "example", PRNumber: 4},
	}
	prs := map[string]*db.PR{
		"acme/example#1": {PRState: "open", GitHubUpdatedAt: &t2},
		"acme/example#2": {PRState: "open", GitHubUpdatedAt: &t1},
		"acme/example#3": {PRState: "closed", GitHubUpdatedAt: &t2},
		"acme/example#4": {PRState: "open", Draft: true, GitHubUpdatedAt: &t2},
	}
	lookup := func(owner, repo string, n int) *db.PR { return prs[replyKey(owner, repo, n)] }
	last := map[string]time.Time{"acme/example#1": t1, "acme/example#2": t1}

	got, marks := replyTargetsToScan(targets, lookup, last, false)
	if len(got) != 1 || got[0].PRNumber != 1 {
		t.Fatalf("incremental scan = %+v, want only PR 1 (updated since last scan, open, not draft)", got)
	}
	if !marks["acme/example#1"].Equal(t2) || last["acme/example#1"].Equal(t2) {
		t.Errorf("the candidate watermark is returned, not committed, until the scan succeeds")
	}

	got, _ = replyTargetsToScan(targets, lookup, last, true)
	if len(got) != 2 || got[0].PRNumber != 1 || got[1].PRNumber != 2 {
		t.Fatalf("full scan = %+v, want every open non-draft PR", got)
	}
}

func TestCommitReplyWatermarksSkipsFailedTargets(t *testing.T) {
	t2 := time.Date(2026, 9, 8, 12, 1, 0, 0, time.UTC)
	last := map[string]time.Time{}
	marks := map[string]time.Time{"acme/example#1": t2, "acme/example#2": t2}
	commitReplyWatermarks(last, marks, []string{"acme/example#2: list review comments: boom"})
	if _, ok := last["acme/example#1"]; !ok {
		t.Errorf("successful target must advance")
	}
	if _, ok := last["acme/example#2"]; ok {
		t.Errorf("failed target must be retried next cycle")
	}
}

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
}
