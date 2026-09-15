package poller

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"pr-review-server/db"
	"pr-review-server/github"
	"pr-review-server/pkg/reviewer/service"
)

const (
	autoReviewOldHead = "1111111111111111111111111111111111111111"
	autoReviewNewHead = "2222222222222222222222222222222222222222"
)

type autoReviewFixture struct {
	p         *Poller
	db        *MockDatabase
	gh        *MockGitHubClient
	storage   *MockReviewStorage
	generator *MockReviewGenerator
}

func newAutoReviewFixture(t *testing.T, switchOn bool, authors string) autoReviewFixture {
	t.Helper()
	mockDB := NewMockDatabase()
	mockDB.AutoReviewEnabled = false
	require.NoError(t, mockDB.SetSetting(settingPublishEnabledAuthors, authors))
	if switchOn {
		require.NoError(t, mockDB.SetSetting(settingAutoReviewReadyPRs, "true"))
	}
	mockGH := NewMockGitHubClient()
	mockGH.PRsRequestingReview = []github.PullRequest{}
	mockGH.MyOpenPRs = []github.PullRequest{}
	storage := NewMockReviewStorage()
	generator := NewMockReviewGenerator()
	p := newTestPollerFull(mockGH, mockDB, storage, generator)
	return autoReviewFixture{p: p, db: mockDB, gh: mockGH, storage: storage, generator: generator}
}

func readyDelivery(action, head string, draft bool) db.WebhookDelivery {
	return db.WebhookDelivery{
		DeliveryID: "delivery-" + action + "-" + head[:4], Event: "pull_request", Action: action,
		RepoOwner: "acme", RepoName: "example", PRNumber: 7, HeadSHA: head, Author: "Alice",
		Title: "Ready PR", Draft: draft, State: "open",
	}
}

func (f autoReviewFixture) intents(t *testing.T) []db.AutoReviewIntent {
	t.Helper()
	intents, err := f.db.ListAutoReviewIntents(db.AutoReviewIntentFilter{})
	require.NoError(t, err)
	return intents
}

func (f autoReviewFixture) runs() []*db.ReviewRun {
	f.db.mu.RLock()
	defer f.db.mu.RUnlock()
	runs := make([]*db.ReviewRun, 0, len(f.db.ReviewRuns))
	for _, run := range f.db.ReviewRuns {
		runs = append(runs, run)
	}
	return runs
}

func TestWebhookReadyForReviewQueuesOneForcedPublishedReview(t *testing.T) {
	f := newAutoReviewFixture(t, true, "alice,bob")
	require.NoError(t, f.p.HandleWebhookDelivery(context.Background(), readyDelivery("ready_for_review", autoReviewOldHead, false)))
	waitForDetachedReviews(t, f.p)

	intents := f.intents(t)
	require.Len(t, intents, 1)
	assert.Equal(t, "ready_for_review", intents[0].Trigger)
	assert.Equal(t, "delivery-ready_for_review-1111", intents[0].DeliveryID)
	runs := f.runs()
	require.Len(t, runs, 1)
	assert.Equal(t, intents[0].RunID, runs[0].RunID)
	assert.Equal(t, autoReviewTriggerSource, runs[0].TriggerSource)
	assert.Equal(t, autoReviewOldHead, runs[0].CommitSHA)
	waitForReviewRunStatus(t, f.db, runs[0].RunID, db.ReviewRunStatusCompleted)
	require.Len(t, f.generator.GenerateReviewCalls, 1)
	assert.Equal(t, publicationUnavailable, f.intents(t)[0].Publication, "the worker records how publication ended on the intent")

	require.NoError(t, f.db.SetAutoReviewIntentPublicationByRun(runs[0].RunID, publicationPosted))
	f.p.settleAutoReviewIntents()
	assert.Equal(t, db.AutoReviewIntentDone, f.intents(t)[0].Status)

	require.NoError(t, f.p.HandleWebhookDelivery(context.Background(), readyDelivery("ready_for_review", autoReviewOldHead, false)))
	assert.Len(t, f.intents(t), 1, "the same head is not reviewed twice")
	assert.Len(t, f.runs(), 1)
}

func TestSettlementFollowsThePublicationOutcome(t *testing.T) {
	cases := []struct {
		publication string
		want        string
	}{
		{publicationPosted, db.AutoReviewIntentDone},
		{publicationSkippedPrefix + "pull request is a draft", db.AutoReviewIntentSuperseded},
		{publicationSkippedPrefix + "pull request head moved past the reviewed commit", db.AutoReviewIntentSuperseded},
		{publicationFailedPrefix + "publish", db.AutoReviewIntentFailed},
		{publicationUnavailable, db.AutoReviewIntentFailed},
	}
	for _, tc := range cases {
		t.Run(tc.publication, func(t *testing.T) {
			f := newAutoReviewFixture(t, true, "*")
			require.NoError(t, f.p.HandleWebhookDelivery(context.Background(), readyDelivery("ready_for_review", autoReviewOldHead, false)))
			waitForDetachedReviews(t, f.p)
			intent := f.intents(t)[0]
			waitForReviewRunStatus(t, f.db, intent.RunID, db.ReviewRunStatusCompleted)
			require.NoError(t, f.db.SetAutoReviewIntentPublicationByRun(intent.RunID, tc.publication))

			f.p.settleAutoReviewIntents()
			settled := f.intents(t)[0]
			assert.Equal(t, tc.want, settled.Status)
			assert.Equal(t, intent.RunID, settled.RunID, "settling keeps the run link")
		})
	}
}

func TestHeadFinishedWhileDraftIsReviewedAgainWhenReady(t *testing.T) {
	f := newAutoReviewFixture(t, true, "*")
	require.NoError(t, f.p.HandleWebhookDelivery(context.Background(), readyDelivery("opened", autoReviewOldHead, false)))
	waitForDetachedReviews(t, f.p)
	first := f.intents(t)[0]
	waitForReviewRunStatus(t, f.db, first.RunID, db.ReviewRunStatusCompleted)
	require.NoError(t, f.db.SetAutoReviewIntentPublicationByRun(first.RunID, publicationSkippedPrefix+"pull request is a draft"))
	f.p.settleAutoReviewIntents()
	require.Equal(t, db.AutoReviewIntentSuperseded, f.intents(t)[0].Status)

	require.NoError(t, f.p.HandleWebhookDelivery(context.Background(), readyDelivery("ready_for_review", autoReviewOldHead, false)))
	waitForDetachedReviews(t, f.p)
	intents := f.intents(t)
	require.Len(t, intents, 1, "the same head reuses its intent row")
	assert.NotEqual(t, first.RunID, intents[0].RunID, "the ready transition gets a fresh run")
	assert.Len(t, f.generator.GenerateReviewCalls, 2)
}

func TestSettlementWaitsForThePublicationOutcomeThenConsultsTheLedger(t *testing.T) {
	f := newAutoReviewFixture(t, true, "*")
	require.NoError(t, f.p.HandleWebhookDelivery(context.Background(), readyDelivery("ready_for_review", autoReviewOldHead, false)))
	waitForDetachedReviews(t, f.p)
	intent := f.intents(t)[0]
	waitForReviewRunStatus(t, f.db, intent.RunID, db.ReviewRunStatusCompleted)
	require.NoError(t, f.db.SetAutoReviewIntentPublicationByRun(intent.RunID, ""))

	f.p.settleAutoReviewIntents()
	assert.Equal(t, db.AutoReviewIntentRunning, f.intents(t)[0].Status, "a just-completed run may still be publishing")

	stale := time.Now().Add(-autoReviewPublicationGrace - time.Minute)
	require.NoError(t, f.db.PatchReviewRun(intent.RunID, db.ReviewRunPatch{CompletedAt: &stale}))
	f.p.settleAutoReviewIntents()
	settled := f.intents(t)[0]
	assert.Equal(t, db.AutoReviewIntentFailed, settled.Status, "the comments may have reached GitHub before the ledger, so the head is not reviewed again automatically")
	assert.Equal(t, intent.RunID, settled.RunID)
}

func TestAdmissionFailureLeavesTheIntentQueued(t *testing.T) {
	f := newAutoReviewFixture(t, true, "*")
	f.p.cfg.AgenticReviews = true
	f.p.cfg.AgentBackend = service.AgentBackendOpenRouter
	f.p.cfg.AgentModel = service.DefaultOpenRouterAgentModel
	f.p.cfg.AgentEffort = "high"
	f.p.cfg.AgentWallClockSec = 30
	f.p.cfg.AgentMaxTurns = 5
	f.p.cfg.OpenRouterAPIKey = ""

	require.NoError(t, f.p.HandleWebhookDelivery(context.Background(), readyDelivery("ready_for_review", autoReviewOldHead, false)))
	intents := f.intents(t)
	require.Len(t, intents, 1)
	assert.Equal(t, db.AutoReviewIntentQueued, intents[0].Status, "an operator-fixable admission failure keeps the head owed")
	assert.Equal(t, "", intents[0].RunID)
	assert.Empty(t, f.runs())
}

func TestDispatchAdmitsWithinThePollBudgetAndLeavesTheRestQueued(t *testing.T) {
	f := newAutoReviewFixture(t, true, "*")
	f.p.firstPassSlots = make(chan struct{}, 1)
	f.gh.BatchGetPRStateResults = map[string]*github.PRState{}
	var prs []db.PR
	for n := 1; n <= 6; n++ {
		pr := db.PR{RepoOwner: "acme", RepoName: "example", PRNumber: n, LastCommitSHA: autoReviewOldHead, Status: "completed", Author: "alice", PRState: "open"}
		f.db.PRs[fmt.Sprintf("acme/example/%d", n)] = &pr
		prs = append(prs, pr)
		f.gh.BatchGetPRStateResults[fmt.Sprintf("acme/example/%d", n)] = &github.PRState{Owner: "acme", Repo: "example", Number: n, State: "OPEN", HeadRefOid: autoReviewOldHead}
		intent := db.AutoReviewIntent{RepoOwner: "acme", RepoName: "example", PRNumber: n, HeadSHA: autoReviewOldHead, Trigger: "opened"}
		_, err := f.db.EnsureAutoReviewIntent(&intent, nil)
		require.NoError(t, err)
	}

	f.p.dispatchAutoReviewIntents(context.Background(), prs, f.gh.BatchGetPRStateResults)
	queued, err := f.db.ListAutoReviewIntents(db.AutoReviewIntentFilter{Statuses: []string{db.AutoReviewIntentQueued}})
	require.NoError(t, err)
	assert.Len(t, queued, 6-f.p.pollAdmissionLimit(), "intents beyond the budget wait for the next cycle")
	waitForDetachedReviews(t, f.p)
}

func TestSettlementGivesAClaimedIntentTimeForItsRunRowThenRequeuesIt(t *testing.T) {
	f := newAutoReviewFixture(t, true, "*")
	claimed := db.AutoReviewIntent{RepoOwner: "acme", RepoName: "example", PRNumber: 7, HeadSHA: autoReviewOldHead, Trigger: "opened"}
	_, err := f.db.EnsureAutoReviewIntent(&claimed, nil)
	require.NoError(t, err)
	_, err = f.db.UpdateAutoReviewIntentStatus(claimed.ID, nil, db.AutoReviewIntentRunning, "run-not-yet-written")
	require.NoError(t, err)

	f.p.settleAutoReviewIntents()
	assert.Equal(t, db.AutoReviewIntentRunning, f.intents(t)[0].Status, "the run row may still be on its way")

	f.db.mu.Lock()
	f.db.AutoReviewIntents[0].UpdatedAt = time.Now().Add(-autoReviewClaimGrace - time.Minute)
	f.db.mu.Unlock()
	f.p.settleAutoReviewIntents()
	requeued := f.intents(t)[0]
	assert.Equal(t, db.AutoReviewIntentQueued, requeued.Status, "nothing executed, so the head is owed again")
	assert.Equal(t, "", requeued.RunID)
}

func TestWebhookLeavesTheIntentQueuedWhenTheResidentQueueIsFull(t *testing.T) {
	f := newAutoReviewFixture(t, true, "*")
	f.p.firstPassSlots = make(chan struct{}, 1)
	for n := 1; n <= f.p.pollAdmissionLimit(); n++ {
		f.p.activeReviews[prKey("acme", "busy", n)] = ProcessInfo{RunID: fmt.Sprintf("run-busy-%d", n)}
	}

	require.NoError(t, f.p.HandleWebhookDelivery(context.Background(), readyDelivery("ready_for_review", autoReviewOldHead, false)))
	intents := f.intents(t)
	require.Len(t, intents, 1)
	assert.Equal(t, db.AutoReviewIntentQueued, intents[0].Status)
	assert.Empty(t, f.runs(), "the dispatcher admits it once capacity frees up")
}

func TestIntentIsClaimedBeforeTheRunLaunches(t *testing.T) {
	f := newAutoReviewFixture(t, true, "*")
	intent := db.AutoReviewIntent{RepoOwner: "acme", RepoName: "example", PRNumber: 7, HeadSHA: autoReviewOldHead, Trigger: "opened"}
	_, err := f.db.EnsureAutoReviewIntent(&intent, nil)
	require.NoError(t, err)
	_, err = f.db.UpdateAutoReviewIntentStatus(intent.ID, nil, db.AutoReviewIntentSuperseded, "")
	require.NoError(t, err)

	f.p.admitAutoReviewIntent(context.Background(), intent, github.PullRequest{Owner: "acme", Repo: "example", Number: 7, CommitSHA: autoReviewOldHead, Author: "alice"})
	assert.Empty(t, f.runs(), "an intent that is no longer queued must not launch a review")
	assert.Equal(t, db.AutoReviewIntentSuperseded, f.intents(t)[0].Status)
}

func TestIntentTargetsAreCaseInsensitive(t *testing.T) {
	f := newAutoReviewFixture(t, true, "*")
	upper := readyDelivery("ready_for_review", autoReviewOldHead, false)
	upper.RepoOwner, upper.RepoName = "ACME", "Example"
	require.NoError(t, f.p.HandleWebhookDelivery(context.Background(), upper))
	waitForDetachedReviews(t, f.p)
	require.NoError(t, f.p.HandleWebhookDelivery(context.Background(), readyDelivery("ready_for_review", autoReviewOldHead, false)))
	assert.Len(t, f.intents(t), 1, "casing variants of the same PR share one intent")
	assert.Len(t, f.runs(), 1)
}

func TestWebhookIgnoresAuthorsOutsideTheAllowlist(t *testing.T) {
	f := newAutoReviewFixture(t, true, "bob")
	require.NoError(t, f.p.HandleWebhookDelivery(context.Background(), readyDelivery("ready_for_review", autoReviewOldHead, false)))
	assert.Empty(t, f.intents(t))
	assert.Empty(t, f.runs())
	assert.Empty(t, f.generator.GenerateReviewCalls)
}

func TestWebhookDoesNothingWhileTheSwitchIsOff(t *testing.T) {
	f := newAutoReviewFixture(t, false, "*")
	require.NoError(t, f.p.HandleWebhookDelivery(context.Background(), readyDelivery("opened", autoReviewOldHead, false)))
	assert.Empty(t, f.intents(t))
	assert.Empty(t, f.runs())
}

func TestWebhookOpenedDraftIsIgnoredAndOpenedReadyIsReviewed(t *testing.T) {
	f := newAutoReviewFixture(t, true, "*")
	require.NoError(t, f.p.HandleWebhookDelivery(context.Background(), readyDelivery("opened", autoReviewOldHead, true)))
	assert.Empty(t, f.intents(t))
	assert.Empty(t, f.runs())

	require.NoError(t, f.p.HandleWebhookDelivery(context.Background(), readyDelivery("opened", autoReviewNewHead, false)))
	waitForDetachedReviews(t, f.p)
	intents := f.intents(t)
	require.Len(t, intents, 1)
	assert.Equal(t, autoReviewNewHead, intents[0].HeadSHA)
	assert.Equal(t, "opened", intents[0].Trigger)
	require.Len(t, f.generator.GenerateReviewCalls, 1)
	assert.Equal(t, autoReviewNewHead, f.generator.GenerateReviewCalls[0].CommitSHA)
}

func TestWebhookSynchronizeSupersedesQueuedOlderHead(t *testing.T) {
	f := newAutoReviewFixture(t, true, "*")
	older := db.AutoReviewIntent{RepoOwner: "acme", RepoName: "example", PRNumber: 7, HeadSHA: autoReviewOldHead, Trigger: "ready_for_review"}
	_, err := f.db.EnsureAutoReviewIntent(&older, nil)
	require.NoError(t, err)

	require.NoError(t, f.p.HandleWebhookDelivery(context.Background(), readyDelivery("synchronize", autoReviewNewHead, false)))
	waitForDetachedReviews(t, f.p)

	byHead := map[string]db.AutoReviewIntent{}
	for _, intent := range f.intents(t) {
		byHead[intent.HeadSHA] = intent
	}
	require.Len(t, byHead, 2)
	assert.Equal(t, db.AutoReviewIntentSuperseded, byHead[autoReviewOldHead].Status)
	assert.Equal(t, db.AutoReviewIntentRunning, byHead[autoReviewNewHead].Status)
	require.Len(t, f.generator.GenerateReviewCalls, 1)
	assert.Equal(t, autoReviewNewHead, f.generator.GenerateReviewCalls[0].CommitSHA)
}

func TestWebhookConvertedToDraftAndClosedSupersedeQueuedIntents(t *testing.T) {
	for _, action := range []string{"converted_to_draft", "closed"} {
		t.Run(action, func(t *testing.T) {
			f := newAutoReviewFixture(t, true, "*")
			queued := db.AutoReviewIntent{RepoOwner: "acme", RepoName: "example", PRNumber: 7, HeadSHA: autoReviewOldHead, Trigger: "ready_for_review"}
			_, err := f.db.EnsureAutoReviewIntent(&queued, nil)
			require.NoError(t, err)

			d := readyDelivery(action, autoReviewOldHead, action == "converted_to_draft")
			require.NoError(t, f.p.HandleWebhookDelivery(context.Background(), d))

			intents := f.intents(t)
			require.Len(t, intents, 1)
			assert.Equal(t, db.AutoReviewIntentSuperseded, intents[0].Status)
			assert.Empty(t, f.runs())
		})
	}
}

func TestWebhookLeavesIntentQueuedWhileAnotherReviewIsActive(t *testing.T) {
	f := newAutoReviewFixture(t, true, "*")
	rival := customReviewJob(t, "run-77700000000000000000000000000001")
	rival.PR = github.PullRequest{Owner: "acme", Repo: "example", Number: 7, CommitSHA: autoReviewOldHead}
	_, tracked := f.p.tryTrackReviewJob(context.Background(), rival)
	require.True(t, tracked)
	defer f.p.untrackReviewRun("acme", "example", 7, rival.RunID)

	require.NoError(t, f.p.HandleWebhookDelivery(context.Background(), readyDelivery("ready_for_review", autoReviewNewHead, false)))
	intents := f.intents(t)
	require.Len(t, intents, 1)
	assert.Equal(t, db.AutoReviewIntentQueued, intents[0].Status)
	assert.Equal(t, "", intents[0].RunID)
	assert.Empty(t, f.generator.GenerateReviewCalls)
}

func TestReadyReviewIsForcedPastAReviewCachedWhileDraft(t *testing.T) {
	f := newAutoReviewFixture(t, true, "*")
	f.db.PRs["acme/example/7"] = &db.PR{
		RepoOwner: "acme", RepoName: "example", PRNumber: 7, LastCommitSHA: autoReviewOldHead,
		Status: "completed", Draft: false, Title: "Ready PR", Author: "alice",
		ReviewHTMLPath: "review-acme-example-7-1111111.html",
	}
	f.storage.ExistingReviews["acme/example/7/"+autoReviewOldHead] = true

	require.NoError(t, f.p.HandleWebhookDelivery(context.Background(), readyDelivery("ready_for_review", autoReviewOldHead, false)))
	waitForDetachedReviews(t, f.p)

	require.Len(t, f.generator.GenerateReviewCalls, 1, "the cached draft-time review must not satisfy the ready review")
	runs := f.runs()
	require.Len(t, runs, 1)
	waitForReviewRunStatus(t, f.db, runs[0].RunID, db.ReviewRunStatusCompleted)
}

func trackedReadyPR(f autoReviewFixture, head string, draft bool) {
	f.db.PRs["acme/example/7"] = &db.PR{
		RepoOwner: "acme", RepoName: "example", PRNumber: 7, LastCommitSHA: head,
		Status: "completed", Draft: draft, Title: "Ready PR", Author: "alice", PRState: "open",
	}
	f.gh.BatchGetPRStateResults = map[string]*github.PRState{
		"acme/example/7": {Owner: "acme", Repo: "example", Number: 7, State: "OPEN", HeadRefOid: head, IsDraft: draft},
	}
}

func TestPollFallbackQueuesAndRunsMissingIntent(t *testing.T) {
	f := newAutoReviewFixture(t, true, "alice")
	trackedReadyPR(f, autoReviewOldHead, false)

	_, _, err := f.p.cleanupAndDetectOutdated(context.Background())
	require.NoError(t, err)
	waitForDetachedReviews(t, f.p)

	intents := f.intents(t)
	require.Len(t, intents, 1)
	assert.Equal(t, db.AutoReviewTriggerPollFallback, intents[0].Trigger)
	assert.Equal(t, autoReviewOldHead, intents[0].HeadSHA)
	assert.Equal(t, db.AutoReviewIntentRunning, intents[0].Status)
	assert.NotEmpty(t, intents[0].RunID)
	require.Len(t, f.generator.GenerateReviewCalls, 1)

	waitForReviewRunStatus(t, f.db, intents[0].RunID, db.ReviewRunStatusCompleted)
	require.NoError(t, f.db.SetAutoReviewIntentPublicationByRun(intents[0].RunID, publicationPosted))
	_, _, err = f.p.cleanupAndDetectOutdated(context.Background())
	require.NoError(t, err)
	assert.Equal(t, db.AutoReviewIntentDone, f.intents(t)[0].Status, "the next poll settles the finished run")
	assert.Len(t, f.generator.GenerateReviewCalls, 1)
}

func TestPollFallbackMarksAQueuedIntentDoneOnceAnotherRunPublishedTheHead(t *testing.T) {
	database, err := db.NewGormSQLite(":memory:")
	require.NoError(t, err)
	defer database.Close()
	require.NoError(t, database.SetSetting(settingPublishEnabledAuthors, "alice"))
	require.NoError(t, database.SetSetting(settingAutoReviewReadyPRs, "true"))
	require.NoError(t, database.UpsertPR(&db.PR{
		RepoOwner: "acme", RepoName: "example", PRNumber: 7, LastCommitSHA: autoReviewOldHead,
		Status: "completed", Title: "Ready PR", Author: "alice", PRState: "open",
	}))
	waiting := db.AutoReviewIntent{RepoOwner: "acme", RepoName: "example", PRNumber: 7, HeadSHA: autoReviewOldHead, Trigger: "ready_for_review"}
	_, err = database.EnsureAutoReviewIntent(&waiting, nil)
	require.NoError(t, err)
	require.NoError(t, database.UpsertPublishedFinding(&db.PublishedFinding{
		RepoOwner: "acme", RepoName: "example", PRNumber: 7, Kind: db.PublishedKindSummary, Fingerprint: db.PublishedKindSummary,
		ReviewedSHA: autoReviewOldHead, LastSeenSHA: autoReviewOldHead, State: db.PublishedStateOpen, Rounds: 1,
	}))
	mockGH := NewMockGitHubClient()
	mockGH.BatchGetPRStateResults = map[string]*github.PRState{
		"acme/example/7": {Owner: "acme", Repo: "example", Number: 7, State: "OPEN", HeadRefOid: autoReviewOldHead},
	}
	generator := NewMockReviewGenerator()
	p := newTestPollerFull(mockGH, database, NewMockReviewStorage(), generator)

	_, _, err = p.cleanupAndDetectOutdated(context.Background())
	require.NoError(t, err)
	intents, err := database.ListAutoReviewIntents(db.AutoReviewIntentFilter{})
	require.NoError(t, err)
	require.Len(t, intents, 1)
	assert.Equal(t, db.AutoReviewIntentDone, intents[0].Status)
	assert.Empty(t, generator.GenerateReviewCalls, "a head another run already commented on is not reviewed again")
}

func TestWebhookRecordsAnAlreadyPublishedHeadAsDone(t *testing.T) {
	database, err := db.NewGormSQLite(":memory:")
	require.NoError(t, err)
	defer database.Close()
	require.NoError(t, database.SetSetting(settingPublishEnabledAuthors, "alice"))
	require.NoError(t, database.SetSetting(settingAutoReviewReadyPRs, "true"))
	waiting := db.AutoReviewIntent{RepoOwner: "acme", RepoName: "example", PRNumber: 7, HeadSHA: autoReviewOldHead, Trigger: db.AutoReviewTriggerPollFallback}
	_, err = database.EnsureAutoReviewIntent(&waiting, nil)
	require.NoError(t, err)
	require.NoError(t, database.UpsertPublishedFinding(&db.PublishedFinding{
		RepoOwner: "acme", RepoName: "example", PRNumber: 7, Kind: db.PublishedKindSummary, Fingerprint: db.PublishedKindSummary,
		ReviewedSHA: autoReviewOldHead, LastSeenSHA: autoReviewOldHead, State: db.PublishedStateOpen, Rounds: 1,
	}))
	generator := NewMockReviewGenerator()
	p := newTestPollerFull(NewMockGitHubClient(), database, NewMockReviewStorage(), generator)

	require.NoError(t, p.HandleWebhookDelivery(context.Background(), readyDelivery("opened", autoReviewOldHead, false)))
	intents, err := database.ListAutoReviewIntents(db.AutoReviewIntentFilter{})
	require.NoError(t, err)
	require.Len(t, intents, 1)
	assert.Equal(t, db.AutoReviewIntentDone, intents[0].Status)
	assert.Equal(t, publicationPosted, intents[0].Publication)
	assert.Empty(t, generator.GenerateReviewCalls, "a delivery that arrives after another run commented on the head starts nothing")

	require.NoError(t, p.HandleWebhookDelivery(context.Background(), readyDelivery("synchronize", autoReviewNewHead, false)))
	intents, err = database.ListAutoReviewIntents(db.AutoReviewIntentFilter{HeadSHA: autoReviewNewHead})
	require.NoError(t, err)
	require.Len(t, intents, 1)
	assert.NotEqual(t, db.AutoReviewIntentDone, intents[0].Status, "a new head is still owed a review")
	waitForDetachedReviews(t, p)
}

func TestPollFallbackRecordsAnAlreadyPublishedHeadAsDone(t *testing.T) {
	database, err := db.NewGormSQLite(":memory:")
	require.NoError(t, err)
	defer database.Close()
	require.NoError(t, database.SetSetting(settingPublishEnabledAuthors, "alice"))
	require.NoError(t, database.SetSetting(settingAutoReviewReadyPRs, "true"))
	require.NoError(t, database.UpsertPR(&db.PR{
		RepoOwner: "acme", RepoName: "example", PRNumber: 7, LastCommitSHA: autoReviewOldHead,
		Status: "completed", Title: "Ready PR", Author: "alice", PRState: "open",
	}))
	require.NoError(t, database.UpsertPublishedFinding(&db.PublishedFinding{
		RepoOwner: "acme", RepoName: "example", PRNumber: 7, Kind: db.PublishedKindSummary, Fingerprint: db.PublishedKindSummary,
		ReviewedSHA: autoReviewOldHead, LastSeenSHA: autoReviewOldHead, State: db.PublishedStateOpen, Rounds: 1,
	}))
	mockGH := NewMockGitHubClient()
	mockGH.BatchGetPRStateResults = map[string]*github.PRState{
		"acme/example/7": {Owner: "acme", Repo: "example", Number: 7, State: "OPEN", HeadRefOid: autoReviewOldHead},
	}
	generator := NewMockReviewGenerator()
	p := newTestPollerFull(mockGH, database, NewMockReviewStorage(), generator)

	_, _, err = p.cleanupAndDetectOutdated(context.Background())
	require.NoError(t, err)
	intents, err := database.ListAutoReviewIntents(db.AutoReviewIntentFilter{})
	require.NoError(t, err)
	require.Len(t, intents, 1)
	assert.Equal(t, db.AutoReviewIntentDone, intents[0].Status)
	assert.Equal(t, publicationPosted, intents[0].Publication)
	assert.Empty(t, generator.GenerateReviewCalls, "a head PRism already commented on is not reviewed again on enablement")
}

func TestPollFallbackSkipsHeadsWithADoneIntentDraftsAndOtherAuthors(t *testing.T) {
	f := newAutoReviewFixture(t, true, "alice")
	trackedReadyPR(f, autoReviewOldHead, false)
	done := db.AutoReviewIntent{RepoOwner: "acme", RepoName: "example", PRNumber: 7, HeadSHA: autoReviewOldHead, Trigger: "ready_for_review"}
	_, err := f.db.EnsureAutoReviewIntent(&done, nil)
	require.NoError(t, err)
	_, err = f.db.UpdateAutoReviewIntentStatus(done.ID, nil, db.AutoReviewIntentDone, "run-done")
	require.NoError(t, err)

	_, _, err = f.p.cleanupAndDetectOutdated(context.Background())
	require.NoError(t, err)
	intents := f.intents(t)
	require.Len(t, intents, 1)
	assert.Equal(t, db.AutoReviewIntentDone, intents[0].Status)
	assert.Empty(t, f.generator.GenerateReviewCalls)

	f.db.AutoReviewIntents = nil
	trackedReadyPR(f, autoReviewOldHead, true)
	_, _, err = f.p.cleanupAndDetectOutdated(context.Background())
	require.NoError(t, err)
	assert.Empty(t, f.intents(t), "a draft never gets a fallback intent")

	trackedReadyPR(f, autoReviewOldHead, false)
	f.db.PRs["acme/example/7"].Author = "mallory"
	_, _, err = f.p.cleanupAndDetectOutdated(context.Background())
	require.NoError(t, err)
	assert.Empty(t, f.intents(t), "an author outside the allowlist never gets a fallback intent")
}

func TestPollFallbackNewHeadSupersedesQueuedOlderHead(t *testing.T) {
	f := newAutoReviewFixture(t, true, "alice")
	trackedReadyPR(f, autoReviewNewHead, false)
	older := db.AutoReviewIntent{RepoOwner: "acme", RepoName: "example", PRNumber: 7, HeadSHA: autoReviewOldHead, Trigger: "synchronize"}
	_, err := f.db.EnsureAutoReviewIntent(&older, nil)
	require.NoError(t, err)

	_, _, err = f.p.cleanupAndDetectOutdated(context.Background())
	require.NoError(t, err)
	waitForDetachedReviews(t, f.p)

	byHead := map[string]db.AutoReviewIntent{}
	for _, intent := range f.intents(t) {
		byHead[intent.HeadSHA] = intent
	}
	require.Len(t, byHead, 2)
	assert.Equal(t, db.AutoReviewIntentSuperseded, byHead[autoReviewOldHead].Status)
	assert.Equal(t, db.AutoReviewIntentRunning, byHead[autoReviewNewHead].Status)
}

func TestPollFallbackDoesNothingWhileTheSwitchIsOff(t *testing.T) {
	f := newAutoReviewFixture(t, false, "*")
	trackedReadyPR(f, autoReviewOldHead, false)
	_, _, err := f.p.cleanupAndDetectOutdated(context.Background())
	require.NoError(t, err)
	assert.Empty(t, f.intents(t))
	assert.Empty(t, f.runs())
}

func TestPublishGateStillDeniesWhenTheAllowlistIsEmpty(t *testing.T) {
	f := newAutoReviewFixture(t, true, "")
	allowed, err := f.p.publishAllowedFor("alice")
	require.NoError(t, err)
	assert.False(t, allowed)
	eligible, err := f.p.autoReviewEligible("alice")
	require.NoError(t, err)
	assert.False(t, eligible)

	require.NoError(t, f.db.SetSetting(settingPublishEnabledAuthors, "ALICE"))
	eligible, err = f.p.autoReviewEligible("alice")
	require.NoError(t, err)
	assert.True(t, eligible)
}
