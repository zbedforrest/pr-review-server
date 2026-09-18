package poller

import (
	"context"
	"testing"

	"pr-review-server/db"
	"pr-review-server/github"
)

func TestSelectCIStatusPRs(t *testing.T) {
	allPRs := []github.PullRequest{
		{Owner: "acme", Repo: "example", Number: 1},
		{Owner: "acme", Repo: "example", Number: 2},
		{Owner: "acme", Repo: "example", Number: 3},
		{Owner: "acme", Repo: "example", Number: 4},
		{Owner: "acme", Repo: "example", Number: 5},
	}
	dbPRMap := map[string]*db.PR{
		"acme/example/1": {PRState: "open", CIState: "success"},
		"acme/example/2": {PRState: "closed", CIState: "success"},
		"acme/example/3": {PRState: "merged", CIState: ""},
		"acme/example/4": {PRState: "", CIState: "pending"},
	}

	selected := selectCIStatusPRs(allPRs, dbPRMap, false)
	got := map[int]bool{}
	for _, pr := range selected {
		got[pr.Number] = pr.IncludeMergeState
	}
	want := map[int]bool{1: true, 3: false, 4: true, 5: true}
	if len(got) != len(want) {
		t.Fatalf("selected %v, want %v", got, want)
	}
	for number, mergeState := range want {
		if flag, ok := got[number]; !ok || flag != mergeState {
			t.Errorf("PR %d: selected=%v IncludeMergeState=%v, want selected with IncludeMergeState=%v", number, ok, flag, mergeState)
		}
	}

	full := selectCIStatusPRs(allPRs, dbPRMap, true)
	if len(full) != len(allPRs) {
		t.Fatalf("full refresh selected %d PRs, want all %d", len(full), len(allPRs))
	}
	for _, pr := range full {
		if pr.Number == 2 && pr.IncludeMergeState {
			t.Errorf("closed PR must never request merge state, even on a full refresh")
		}
	}
}

func TestPoll_CIStatusQueryRequestsMergeStateForOpenPRs(t *testing.T) {
	_, poller, _ := mergeStatePollFixture(t, "CLEAN", "CLEAN")
	mockGH := poller.ghClient.(*MockGitHubClient)

	poller.poll(context.Background())
	waitForDetachedReviews(t, poller)

	if len(mockGH.BatchGetCIStatusCalls) != 1 {
		t.Fatalf("BatchGetCIStatus calls = %d, want 1", len(mockGH.BatchGetCIStatusCalls))
	}
	call := mockGH.BatchGetCIStatusCalls[0]
	if len(call) != 1 || call[0].Number != 1 || !call[0].IncludeMergeState {
		t.Errorf("CI query PRs = %+v, want the open PR with IncludeMergeState", call)
	}
}

func TestPoll_CIStatusWithoutMergeStateKeepsStoredMergeFields(t *testing.T) {
	mockDB, poller, events := mergeStatePollFixture(t, "CLEAN", "")
	mockGH := poller.ghClient.(*MockGitHubClient)
	mockGH.BatchGetCIStatusResults["owner/repo/1"] = &github.CIStatus{
		State:        "failure",
		FailedChecks: []string{"build"},
	}

	poller.poll(context.Background())
	waitForDetachedReviews(t, poller)

	pr := mockDB.PRs["owner/repo/1"]
	if pr.CIState != "failure" {
		t.Errorf("CIState = %q, want failure", pr.CIState)
	}
	if pr.MergeStateStatus != "CLEAN" || pr.ReviewDecision != "APPROVED" {
		t.Errorf("stored merge fields changed to %q/%q although the query did not request them", pr.MergeStateStatus, pr.ReviewDecision)
	}
	if countEvents(*events, "pr_updated") != 1 {
		t.Errorf("expected one pr_updated for the CI change, got %d", countEvents(*events, "pr_updated"))
	}
}
