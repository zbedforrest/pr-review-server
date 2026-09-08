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

	got := replyTargetsToScan(targets, lookup, last, false)
	if len(got) != 1 || got[0].PRNumber != 1 {
		t.Fatalf("incremental scan = %+v, want only PR 1 (updated since last scan, open, not draft)", got)
	}
	if !last["acme/example#1"].Equal(t2) {
		t.Errorf("last-scanned marker must advance to the PR's updated_at")
	}

	got = replyTargetsToScan(targets, lookup, last, true)
	if len(got) != 2 || got[0].PRNumber != 1 || got[1].PRNumber != 2 {
		t.Fatalf("full scan = %+v, want every open non-draft PR", got)
	}
}
