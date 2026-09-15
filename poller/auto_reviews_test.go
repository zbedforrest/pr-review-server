package poller

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"pr-review-server/db"
	"pr-review-server/github"
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
	f.p.HandleWebhookDelivery(context.Background(), readyDelivery("ready_for_review", autoReviewOldHead, false))
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

	f.p.settleAutoReviewIntents()
	assert.Equal(t, db.AutoReviewIntentDone, f.intents(t)[0].Status)

	f.p.HandleWebhookDelivery(context.Background(), readyDelivery("ready_for_review", autoReviewOldHead, false))
	assert.Len(t, f.intents(t), 1, "the same head is not reviewed twice")
	assert.Len(t, f.runs(), 1)
}

func TestWebhookIgnoresAuthorsOutsideTheAllowlist(t *testing.T) {
	f := newAutoReviewFixture(t, true, "bob")
	f.p.HandleWebhookDelivery(context.Background(), readyDelivery("ready_for_review", autoReviewOldHead, false))
	assert.Empty(t, f.intents(t))
	assert.Empty(t, f.runs())
	assert.Empty(t, f.generator.GenerateReviewCalls)
}

func TestWebhookDoesNothingWhileTheSwitchIsOff(t *testing.T) {
	f := newAutoReviewFixture(t, false, "*")
	f.p.HandleWebhookDelivery(context.Background(), readyDelivery("opened", autoReviewOldHead, false))
	assert.Empty(t, f.intents(t))
	assert.Empty(t, f.runs())
}

func TestWebhookOpenedDraftIsIgnoredAndOpenedReadyIsReviewed(t *testing.T) {
	f := newAutoReviewFixture(t, true, "*")
	f.p.HandleWebhookDelivery(context.Background(), readyDelivery("opened", autoReviewOldHead, true))
	assert.Empty(t, f.intents(t))
	assert.Empty(t, f.runs())

	f.p.HandleWebhookDelivery(context.Background(), readyDelivery("opened", autoReviewNewHead, false))
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

	f.p.HandleWebhookDelivery(context.Background(), readyDelivery("synchronize", autoReviewNewHead, false))
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
			f.p.HandleWebhookDelivery(context.Background(), d)

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

	f.p.HandleWebhookDelivery(context.Background(), readyDelivery("ready_for_review", autoReviewNewHead, false))
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

	f.p.HandleWebhookDelivery(context.Background(), readyDelivery("ready_for_review", autoReviewOldHead, false))
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

	_, _, err = f.p.cleanupAndDetectOutdated(context.Background())
	require.NoError(t, err)
	assert.Equal(t, db.AutoReviewIntentDone, f.intents(t)[0].Status, "the next poll settles the finished run")
	assert.Len(t, f.generator.GenerateReviewCalls, 1)
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
