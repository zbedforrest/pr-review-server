package poller

import (
	"bytes"
	"context"
	"log"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"pr-review-server/db"
	"pr-review-server/github"
)

func ciSelectionFixture() ([]github.PullRequest, map[string]*db.PR, map[string]bool, ciSelectOptions) {
	allPRs := []github.PullRequest{
		{Owner: "acme", Repo: "example", Number: 1, CommitSHA: "head1", Author: "alice"},
		{Owner: "acme", Repo: "example", Number: 2, CommitSHA: "head2"},
		{Owner: "acme", Repo: "example", Number: 3, CommitSHA: "head3"},
		{Owner: "acme", Repo: "example", Number: 4, CommitSHA: "head4", Author: "bob"},
		{Owner: "acme", Repo: "example", Number: 5, CommitSHA: "head5"},
		{Owner: "acme", Repo: "example", Number: 6, CommitSHA: "head6"},
		{Owner: "acme", Repo: "example", Number: 7, CommitSHA: "head7"},
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
	opts := ciSelectOptions{
		watched:         map[int]bool{1: true, 4: true},
		excludedAuthors: db.ParseLoginCSV(db.DefaultCIStatusExcludeAuthors),
		marks:           map[string]ciMergeMark{},
		cycle:           1,
	}
	return allPRs, dbPRMap, ghKeys, opts
}

func mergeStateByNumber(prs []github.PRInfo) map[int]bool {
	got := map[int]bool{}
	for _, pr := range prs {
		got[pr.Number] = pr.IncludeMergeState
	}
	return got
}

func TestSelectCIStatusPRs_OnlyWatchedOpenPRsGetMergeState(t *testing.T) {
	allPRs, dbPRMap, ghKeys, opts := ciSelectionFixture()

	sel := selectCIStatusPRs(allPRs, dbPRMap, ghKeys, opts)

	want := map[int]bool{1: true, 4: true, 5: false, 7: false}
	if got := mergeStateByNumber(sel.prs); !reflect.DeepEqual(got, want) {
		t.Errorf("selected %v, want %v (watched open rows with merge state, empty-CI unknown row and unknown-to-DB row checks-only)", got, want)
	}
	if got, want := sel.String(), "open=4 watched=2 (visible=2 bot-excluded=0 due=2) unwatched-skipped=1 closed-skipped=2 checks-only=2"; got != want {
		t.Errorf("summary = %q, want %q", got, want)
	}
}

func TestSelectCIStatusPRs_FullRefreshQueriesClosedRowsChecksOnly(t *testing.T) {
	allPRs, dbPRMap, ghKeys, opts := ciSelectionFixture()
	opts.fullRefresh = true

	sel := selectCIStatusPRs(allPRs, dbPRMap, ghKeys, opts)

	want := map[int]bool{1: true, 2: false, 3: false, 4: true, 5: false, 7: false}
	if got := mergeStateByNumber(sel.prs); !reflect.DeepEqual(got, want) {
		t.Errorf("full refresh selected %v, want %v", got, want)
	}
	if sel.closedSkipped != 0 || sel.checksOnly != 4 || sel.unwatched != 1 {
		t.Errorf("full refresh counts = %s, want closed-skipped=0 checks-only=4 unwatched-skipped=1", sel)
	}
}

func TestSelectCIStatusPRs_WatchedLookupFailureFallsBackToEveryOpenPR(t *testing.T) {
	allPRs, dbPRMap, ghKeys, opts := ciSelectionFixture()
	opts.watched = nil

	sel := selectCIStatusPRs(allPRs, dbPRMap, ghKeys, opts)

	if got := mergeStateByNumber(sel.prs); !got[6] || got[7] || sel.unwatched != 0 || sel.watched != 3 || sel.due != 3 {
		t.Errorf("with no watched set every known open PR must be queried with merge state, got %v counts %s", got, sel)
	}
}

func TestSelectCIStatusPRs_ExcludedAuthorsNeverGetMergeState(t *testing.T) {
	allPRs, dbPRMap, ghKeys, opts := ciSelectionFixture()
	allPRs[0].Author = "renovate"
	allPRs[3].Author = "dependabot[bot]"
	dbPRMap["acme/example/4"].CIState = ""

	sel := selectCIStatusPRs(allPRs, dbPRMap, ghKeys, opts)

	want := map[int]bool{4: false, 5: false, 7: false}
	if got := mergeStateByNumber(sel.prs); !reflect.DeepEqual(got, want) {
		t.Errorf("selected %v, want %v (bot PR with CI state skipped, bot PR without CI state checks-only)", got, want)
	}
	if got, want := sel.String(), "open=4 watched=0 (visible=2 bot-excluded=2 due=0) unwatched-skipped=1 closed-skipped=2 checks-only=3"; got != want {
		t.Errorf("summary = %q, want %q", got, want)
	}

	opts.fullRefresh = true
	sel = selectCIStatusPRs(allPRs, dbPRMap, ghKeys, opts)
	if v, ok := mergeStateByNumber(sel.prs)[1]; !ok || v {
		t.Errorf("full refresh must include bot PR 1 checks-only, got %v", mergeStateByNumber(sel.prs))
	}
}

func TestIsExcludedCIAuthor(t *testing.T) {
	excluded := db.ParseLoginCSV(db.DefaultCIStatusExcludeAuthors)
	cases := map[string]bool{
		"renovate": true, "Renovate": true, "mm-renovate-bot": true, "dependabot[bot]": true,
		"github-actions[bot]": true, "alice": false, "": false, "renovate-fan": false,
	}
	for login, want := range cases {
		if got := isExcludedCIAuthor(login, excluded); got != want {
			t.Errorf("isExcludedCIAuthor(%q) = %v, want %v", login, got, want)
		}
	}
	if isExcludedCIAuthor("renovate", db.ParseLoginCSV("")) {
		t.Errorf("a cleared list must only exclude [bot] logins")
	}
}

func TestMergeStateDue_UnchangedPRWaitsFiveCycles(t *testing.T) {
	mark := ciMergeMark{cycle: 1, head: "head1", ciState: "success"}
	for cycle := 2; cycle <= 5; cycle++ {
		if mergeStateDue(mark, true, cycle, "head1", "success", false) {
			t.Errorf("cycle %d: unchanged PR must not be due", cycle)
		}
	}
	if !mergeStateDue(mark, true, 6, "head1", "success", false) {
		t.Errorf("cycle 6: unchanged PR must be due after %d cycles", ciMergeStateEveryCycles)
	}
	if !mergeStateDue(mark, true, 2, "head2", "success", false) {
		t.Errorf("a head change must make the PR due immediately")
	}
	if !mergeStateDue(mark, true, 2, "head1", "failure", false) {
		t.Errorf("a CI state change must make the PR due immediately")
	}
	if !mergeStateDue(mark, true, 2, "head1", "success", true) {
		t.Errorf("GitHub activity (updatedAt moved) must make the PR due immediately")
	}
	if !mergeStateDue(ciMergeMark{}, false, 2, "head1", "success", false) {
		t.Errorf("a PR never fetched must be due")
	}
}

func TestSelectCIStatusPRs_CadenceSendsUnchangedWatchedPRsChecksOnly(t *testing.T) {
	allPRs, dbPRMap, ghKeys, opts := ciSelectionFixture()
	opts.marks["acme/example/1"] = ciMergeMark{cycle: 1, head: "head1", ciState: "success"}
	opts.marks["acme/example/4"] = ciMergeMark{cycle: 1, head: "old4", ciState: "pending"}
	opts.cycle = 3

	sel := selectCIStatusPRs(allPRs, dbPRMap, ghKeys, opts)

	want := map[int]bool{1: false, 4: true, 5: false, 7: false}
	if got := mergeStateByNumber(sel.prs); !reflect.DeepEqual(got, want) {
		t.Errorf("selected %v, want %v (unchanged PR 1 checks-only, PR 4 with a new head due)", got, want)
	}
	if sel.watched != 2 || sel.due != 1 || sel.checksOnly != 3 {
		t.Errorf("counts = %s, want watched=2 due=1 checks-only=3", sel)
	}

	opts.changed = map[string]bool{"acme/example/1": true}
	sel = selectCIStatusPRs(allPRs, dbPRMap, ghKeys, opts)
	if got := mergeStateByNumber(sel.prs); !got[1] || sel.due != 2 {
		t.Errorf("a PR with GitHub activity must be due: selected %v counts %s", got, sel)
	}
}

func TestRecordMergeStateFetches_MarksFetchedAndForgetsUntracked(t *testing.T) {
	allPRs := []github.PullRequest{{Owner: "acme", Repo: "example", Number: 1, CommitSHA: "head1"}}
	marks := map[string]ciMergeMark{"acme/example/9": {cycle: 1}}
	requested := []github.PRInfo{{Owner: "acme", Repo: "example", Number: 1, IncludeMergeState: true}}
	results := map[string]*github.CIStatus{
		"acme/example/1": {State: "failure", MergeStateRequested: true},
	}

	recordMergeStateFetches(marks, allPRs, requested, results, 7)

	if got, want := marks["acme/example/1"], (ciMergeMark{cycle: 7, head: "head1", ciState: "failure"}); got != want {
		t.Errorf("mark = %+v, want %+v", got, want)
	}
	if _, kept := marks["acme/example/9"]; kept {
		t.Errorf("mark for an untracked PR must be dropped")
	}
	requested[0].IncludeMergeState = false
	recordMergeStateFetches(marks, allPRs, requested, results, 8)
	if marks["acme/example/1"].cycle != 7 {
		t.Errorf("a checks-only request must not refresh the merge-state mark")
	}
	delete(results, "acme/example/1")
	requested[0].IncludeMergeState = true
	recordMergeStateFetches(marks, allPRs, requested, results, 9)
	if marks["acme/example/1"].cycle != 7 {
		t.Errorf("an unanswered request (skipped batch) must not refresh the merge-state mark")
	}
	results["acme/example/1"] = &github.CIStatus{State: "unknown", MergeStateRequested: true, Missing: true}
	recordMergeStateFetches(marks, allPRs, requested, results, 10)
	if marks["acme/example/1"].cycle != 7 {
		t.Errorf("a placeholder for a missing pullRequest node must not refresh the merge-state mark")
	}
}

func TestPoll_CIStatusGitHubActivityMakesMergeStateDue(t *testing.T) {
	mockDB, poller, _ := mergeStatePollFixture(t, "CLEAN", "CLEAN")
	mockGH := poller.ghClient.(*MockGitHubClient)
	poller.cfg.GitHubOrgName = "myorg"
	first := time.Date(2026, 9, 18, 12, 0, 0, 0, time.UTC)
	mockGH.SearchOpenPRsResults = []github.PRInfo{{Owner: "owner", Repo: "repo", Number: 1, UpdatedAt: &first}}

	poller.poll(context.Background(), false)
	waitForDetachedReviews(t, poller)
	poller.poll(context.Background(), false)
	waitForDetachedReviews(t, poller)
	if got := mockDB.PRs["owner/repo/1"].GitHubUpdatedAt; got == nil || !got.Equal(first) {
		t.Fatalf("search timestamp not persisted after cycle 1, got %v", got)
	}
	reviewed := first.Add(time.Hour)
	mockGH.SearchOpenPRsResults[0].UpdatedAt = &reviewed
	poller.poll(context.Background(), false)
	waitForDetachedReviews(t, poller)

	calls := mockGH.BatchGetCIStatusCalls
	if len(calls) != 3 {
		t.Fatalf("BatchGetCIStatus calls = %d, want 3", len(calls))
	}
	wantMerge := []bool{true, false, true}
	for i, call := range calls {
		if len(call) != 1 || call[0].IncludeMergeState != wantMerge[i] {
			t.Errorf("cycle %d: CI query = %+v, want one PR with IncludeMergeState=%v", i+1, call, wantMerge[i])
		}
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

	poller.poll(context.Background(), false)
	waitForDetachedReviews(t, poller)

	if len(mockGH.BatchGetCIStatusCalls) != 1 {
		t.Fatalf("BatchGetCIStatus calls = %d, want 1", len(mockGH.BatchGetCIStatusCalls))
	}
	want := map[int]bool{1: true, 4: true, 5: false}
	if got := mergeStateByNumber(mockGH.BatchGetCIStatusCalls[0]); !reflect.DeepEqual(got, want) {
		t.Errorf("CI query PRs = %v, want %v", got, want)
	}
	wantLine := "[POLL] CI status selection: open=3 watched=2 (visible=2 bot-excluded=0 due=2) unwatched-skipped=1 closed-skipped=2 checks-only=1"
	if !strings.Contains(logs.String(), wantLine) {
		t.Errorf("poll log missing %q", wantLine)
	}
}

func TestPoll_CIStatusHiddenViewAndBotAuthorAreNotWatched(t *testing.T) {
	mockDB, poller, _ := mergeStatePollFixture(t, "CLEAN", "CLEAN")
	mockGH := poller.ghClient.(*MockGitHubClient)
	mockDB.UserPRViews["1/1"].Hidden = true
	mockDB.PRs["owner/repo/2"] = &db.PR{ID: 2, RepoOwner: "owner", RepoName: "repo", PRNumber: 2, Status: "completed", PRState: "open", CIState: "success", Author: "renovate"}
	mockDB.UserPRViews["1/2"] = &db.UserPRView{UserID: 1, PRID: 2}
	_ = mockDB.SetSetting(db.SettingCIStatusExcludeAuthors, db.DefaultCIStatusExcludeAuthors)
	mockGH.PRsRequestingReview = append(mockGH.PRsRequestingReview,
		github.PullRequest{Owner: "owner", Repo: "repo", Number: 2, CommitSHA: "def", Title: "Bot bump", Author: "renovate"})

	var logs bytes.Buffer
	log.SetOutput(&logs)
	defer log.SetOutput(os.Stderr)

	poller.poll(context.Background(), false)
	waitForDetachedReviews(t, poller)

	if len(mockGH.BatchGetCIStatusCalls) != 1 {
		t.Fatalf("BatchGetCIStatus calls = %d, want 1", len(mockGH.BatchGetCIStatusCalls))
	}
	if got := mockGH.BatchGetCIStatusCalls[0]; len(got) != 0 {
		t.Errorf("CI query PRs = %+v, want none (hidden view unwatched, bot PR with CI state skipped)", got)
	}
	wantLine := "[POLL] CI status selection: open=2 watched=0 (visible=1 bot-excluded=1 due=0) unwatched-skipped=1 closed-skipped=0 checks-only=0"
	if !strings.Contains(logs.String(), wantLine) {
		t.Errorf("poll log missing %q in:\n%s", wantLine, logs.String())
	}
}

func TestPoll_CIStatusMergeStateFollowsCadenceAcrossCycles(t *testing.T) {
	_, poller, _ := mergeStatePollFixture(t, "CLEAN", "CLEAN")
	mockGH := poller.ghClient.(*MockGitHubClient)

	for cycle := 1; cycle <= 6; cycle++ {
		poller.poll(context.Background(), false)
		waitForDetachedReviews(t, poller)
	}

	calls := mockGH.BatchGetCIStatusCalls
	if len(calls) != 6 {
		t.Fatalf("BatchGetCIStatus calls = %d, want 6", len(calls))
	}
	wantMerge := []bool{true, false, false, false, false, true}
	for i, call := range calls {
		if len(call) != 1 || call[0].Number != 1 {
			t.Fatalf("cycle %d: CI query PRs = %+v, want the open PR", i+1, call)
		}
		if call[0].IncludeMergeState != wantMerge[i] {
			t.Errorf("cycle %d: IncludeMergeState = %v, want %v", i+1, call[0].IncludeMergeState, wantMerge[i])
		}
	}
}

func TestPoll_CIStatusHeadChangeMakesMergeStateDue(t *testing.T) {
	mockDB, poller, _ := mergeStatePollFixture(t, "CLEAN", "CLEAN")
	mockGH := poller.ghClient.(*MockGitHubClient)

	poller.poll(context.Background(), false)
	waitForDetachedReviews(t, poller)
	const newSHA = "fedcba9876543210fedcba9876543210fedcba98"
	mockGH.PRsRequestingReview[0].CommitSHA = newSHA
	mockGH.GetPRHeadSHAResults["owner/repo/1"] = struct {
		SHA string
		Err error
	}{newSHA, nil}
	mockDB.PRs["owner/repo/1"].LastCommitSHA = newSHA
	poller.poll(context.Background(), false)
	waitForDetachedReviews(t, poller)

	calls := mockGH.BatchGetCIStatusCalls
	if len(calls) != 2 || len(calls[1]) != 1 {
		t.Fatalf("BatchGetCIStatus calls = %+v, want two single-PR calls", calls)
	}
	if !calls[1][0].IncludeMergeState {
		t.Errorf("a new head must make merge state due on the next cycle")
	}
}

func TestPoll_CIStatusQueryRequestsMergeStateForOpenPRs(t *testing.T) {
	_, poller, _ := mergeStatePollFixture(t, "CLEAN", "CLEAN")
	mockGH := poller.ghClient.(*MockGitHubClient)

	poller.poll(context.Background(), false)
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

	poller.poll(context.Background(), false)
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

func TestPoll_CIStatusMissingNodeKeepsStoredValues(t *testing.T) {
	mockDB, poller, events := mergeStatePollFixture(t, "CLEAN", "")
	mockGH := poller.ghClient.(*MockGitHubClient)
	mockGH.BatchGetCIStatusResults["owner/repo/1"] = &github.CIStatus{
		State: "unknown", FailedChecks: []string{}, MergeStateRequested: true, Missing: true,
	}

	poller.poll(context.Background(), false)
	waitForDetachedReviews(t, poller)

	pr := mockDB.PRs["owner/repo/1"]
	if pr.CIState != "success" || pr.MergeStateStatus != "CLEAN" || pr.ReviewDecision != "APPROVED" {
		t.Errorf("stored CI/merge fields overwritten by a missing node: ci=%q merge=%q decision=%q", pr.CIState, pr.MergeStateStatus, pr.ReviewDecision)
	}
	if n := countEvents(*events, "pr_updated"); n != 0 {
		t.Errorf("a missing node must not broadcast an update, got %d", n)
	}
	if _, marked := poller.ciMergeMarks["owner/repo/1"]; marked {
		t.Errorf("a missing node must leave the PR due next cycle")
	}
}

func TestPoll_CIStatusDevModeTreatsEveryOpenPRAsVisible(t *testing.T) {
	stale := time.Now().Add(-60 * 24 * time.Hour)
	for _, devMode := range []bool{false, true} {
		mockDB, poller, _ := mergeStatePollFixture(t, "CLEAN", "CLEAN")
		mockGH := poller.ghClient.(*MockGitHubClient)
		mockDB.Users = []db.User{{ID: 1, GitHubUsername: "dev", LastLoginAt: &stale}}
		if devMode {
			poller.SetDevUser(&mockDB.Users[0])
		}

		poller.poll(context.Background(), false)
		waitForDetachedReviews(t, poller)

		if len(mockGH.BatchGetCIStatusCalls) != 1 {
			t.Fatalf("devMode=%v: BatchGetCIStatus calls = %d, want 1", devMode, len(mockGH.BatchGetCIStatusCalls))
		}
		got := mergeStateByNumber(mockGH.BatchGetCIStatusCalls[0])
		if devMode && !got[1] {
			t.Errorf("dev mode: the dev user never logs in, so its PR must still get merge state; got %v", got)
		}
		if !devMode && len(got) != 0 {
			t.Errorf("prod mode: a user inactive for 60 days must not keep PRs watched; got %v", got)
		}
	}
}
