package poller

import (
	"testing"
	"time"

	"pr-review-server/db"
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
