package poller

import (
	"bytes"
	"context"
	"log"
	"os"
	"reflect"
	"strings"
	"testing"

	"pr-review-server/db"
	"pr-review-server/github"
)

func ciSelectionFixture() ([]github.PullRequest, map[string]*db.PR, map[string]bool, map[int]bool) {
	allPRs := []github.PullRequest{
		{Owner: "acme", Repo: "example", Number: 1},
		{Owner: "acme", Repo: "example", Number: 2},
		{Owner: "acme", Repo: "example", Number: 3},
		{Owner: "acme", Repo: "example", Number: 4},
		{Owner: "acme", Repo: "example", Number: 5},
		{Owner: "acme", Repo: "example", Number: 6},
		{Owner: "acme", Repo: "example", Number: 7},
	}
	dbPRMap := map[string]*db.PR{
		"acme/example/1": {ID: 1, PRState: "open", CIState: "success"},
		"acme/example/2": {ID: 2, PRState: "closed", CIState: "success"},
		"acme/example/3": {ID: 3, PRState: "merged", CIState: "failure"},
		"acme/example/4": {ID: 4, PRState: "", CIState: "pending"},
		"acme/example/5": {ID: 5, PRState: "", CIState: ""},
		"acme/example/6": {ID: 6, PRState: "open", CIState: "success"},
	}
	ghKeys := map[string]bool{"acme/example/1": true, "acme/example/4": true}
	watched := map[int]bool{1: true, 4: true}
	return allPRs, dbPRMap, ghKeys, watched
}

func mergeStateByNumber(prs []github.PRInfo) map[int]bool {
	got := map[int]bool{}
	for _, pr := range prs {
		got[pr.Number] = pr.IncludeMergeState
	}
	return got
}

func TestSelectCIStatusPRs_OnlyWatchedOpenPRsGetMergeState(t *testing.T) {
	allPRs, dbPRMap, ghKeys, watched := ciSelectionFixture()

	sel := selectCIStatusPRs(allPRs, dbPRMap, ghKeys, watched, false)

	want := map[int]bool{1: true, 4: true, 5: false, 7: true}
	if got := mergeStateByNumber(sel.prs); !reflect.DeepEqual(got, want) {
		t.Errorf("selected %v, want %v (open+search rows with merge state, empty-CI unknown row checks-only, unknown-to-DB row with merge state)", got, want)
	}
	if got, want := sel.String(), "open=4 (watched=3 unwatched-skipped=1) closed-skipped=2 checks-only=1 unknown-state=1"; got != want {
		t.Errorf("summary = %q, want %q", got, want)
	}
}

func TestSelectCIStatusPRs_FullRefreshQueriesClosedRowsChecksOnly(t *testing.T) {
	allPRs, dbPRMap, ghKeys, watched := ciSelectionFixture()

	sel := selectCIStatusPRs(allPRs, dbPRMap, ghKeys, watched, true)

	want := map[int]bool{1: true, 2: false, 3: false, 4: true, 5: false, 7: true}
	if got := mergeStateByNumber(sel.prs); !reflect.DeepEqual(got, want) {
		t.Errorf("full refresh selected %v, want %v", got, want)
	}
	if sel.closedSkipped != 0 || sel.checksOnly != 3 || sel.unwatched != 1 {
		t.Errorf("full refresh counts = %s, want closed-skipped=0 checks-only=3 unwatched-skipped=1", sel)
	}
}

func TestSelectCIStatusPRs_WatchedLookupFailureFallsBackToEveryOpenPR(t *testing.T) {
	allPRs, dbPRMap, ghKeys, _ := ciSelectionFixture()

	sel := selectCIStatusPRs(allPRs, dbPRMap, ghKeys, nil, false)

	if got := mergeStateByNumber(sel.prs); !got[6] || sel.unwatched != 0 || sel.watched != 4 {
		t.Errorf("with no watched set every open PR must be queried with merge state, got %v counts %s", got, sel)
	}
}

func TestPoll_CIStatusSelectionSkipsClosedAndUnwatchedRows(t *testing.T) {
	mockDB, poller, _ := mergeStatePollFixture(t, "CLEAN", "CLEAN")
	mockGH := poller.ghClient.(*MockGitHubClient)
	row := func(id, number int, state, ciState string) *db.PR {
		return &db.PR{ID: id, RepoOwner: "owner", RepoName: "repo", PRNumber: number, Status: "completed", PRState: state, CIState: ciState}
	}
	mockDB.PRs["owner/repo/2"] = row(2, 2, "closed", "success")
	mockDB.PRs["owner/repo/3"] = row(3, 3, "merged", "failure")
	mockDB.PRs["owner/repo/4"] = row(4, 4, "", "pending")
	mockDB.PRs["owner/repo/5"] = row(5, 5, "", "")
	mockDB.PRs["owner/repo/6"] = row(6, 6, "open", "success")
	mockDB.UserPRViews["1/4"] = &db.UserPRView{UserID: 1, PRID: 4}
	mockGH.PRsRequestingReview = append(mockGH.PRsRequestingReview,
		github.PullRequest{Owner: "owner", Repo: "repo", Number: 4, CommitSHA: "def", Title: "Legacy row", Author: "author"})

	var logs bytes.Buffer
	log.SetOutput(&logs)
	defer log.SetOutput(os.Stderr)

	poller.poll(context.Background())
	waitForDetachedReviews(t, poller)

	if len(mockGH.BatchGetCIStatusCalls) != 1 {
		t.Fatalf("BatchGetCIStatus calls = %d, want 1", len(mockGH.BatchGetCIStatusCalls))
	}
	want := map[int]bool{1: true, 4: true, 5: false}
	if got := mergeStateByNumber(mockGH.BatchGetCIStatusCalls[0]); !reflect.DeepEqual(got, want) {
		t.Errorf("CI query PRs = %v, want %v", got, want)
	}
	wantLine := "[POLL] CI status selection: open=3 (watched=2 unwatched-skipped=1) closed-skipped=2 checks-only=1 unknown-state=1"
	if !strings.Contains(logs.String(), wantLine) {
		t.Errorf("poll log missing %q", wantLine)
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
