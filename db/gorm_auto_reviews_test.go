package db

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCreateWebhookDeliveryIsIdempotentPerDeliveryID(t *testing.T) {
	database := newTestDB(t)
	defer database.Close()
	first := &WebhookDelivery{DeliveryID: "d-1", Event: "pull_request", Action: "opened", RepoOwner: "acme", RepoName: "example", PRNumber: 7, HeadSHA: "abc"}
	inserted, err := database.CreateWebhookDelivery(first)
	require.NoError(t, err)
	assert.True(t, inserted)

	again, err := database.CreateWebhookDelivery(&WebhookDelivery{DeliveryID: "d-1", Event: "pull_request", Action: "synchronize", RepoOwner: "acme", RepoName: "example", PRNumber: 7})
	require.NoError(t, err)
	assert.False(t, again)

	status, err := database.GetWebhookStatus(time.Now().Add(-time.Hour))
	require.NoError(t, err)
	assert.Equal(t, 1, status.Deliveries)
	require.NotNil(t, status.LastDeliveryAt)
	assert.WithinDuration(t, time.Now(), *status.LastDeliveryAt, time.Minute)

	old, err := database.GetWebhookStatus(time.Now().Add(time.Hour))
	require.NoError(t, err)
	assert.Equal(t, 0, old.Deliveries)
}

func TestEnsureAutoReviewIntentDedupsPerHeadAndRequeuesOnlyListedStatuses(t *testing.T) {
	database := newTestDB(t)
	defer database.Close()
	intent := AutoReviewIntent{RepoOwner: "acme", RepoName: "example", PRNumber: 7, HeadSHA: "aaa", Trigger: "ready_for_review", DeliveryID: "d-1"}
	created, err := database.EnsureAutoReviewIntent(&intent, nil)
	require.NoError(t, err)
	assert.True(t, created)
	assert.NotZero(t, intent.ID)
	assert.Equal(t, AutoReviewIntentQueued, intent.Status)

	dup := AutoReviewIntent{RepoOwner: "acme", RepoName: "example", PRNumber: 7, HeadSHA: "aaa", Trigger: "poll_fallback"}
	created, err = database.EnsureAutoReviewIntent(&dup, []string{AutoReviewIntentSuperseded})
	require.NoError(t, err)
	assert.False(t, created)
	assert.Equal(t, intent.ID, dup.ID)
	assert.Equal(t, "ready_for_review", dup.Trigger, "a queued intent keeps its original trigger")

	moved, err := database.UpdateAutoReviewIntentStatus(intent.ID, []string{AutoReviewIntentQueued}, AutoReviewIntentRunning, "run-1")
	require.NoError(t, err)
	assert.True(t, moved)
	moved, err = database.UpdateAutoReviewIntentStatus(intent.ID, []string{AutoReviewIntentQueued}, AutoReviewIntentFailed, "")
	require.NoError(t, err)
	assert.False(t, moved, "the status guard must refuse a stale transition")

	moved, err = database.UpdateAutoReviewIntentStatus(intent.ID, []string{AutoReviewIntentRunning}, AutoReviewIntentSuperseded, "")
	require.NoError(t, err)
	assert.True(t, moved)

	requeue := AutoReviewIntent{RepoOwner: "acme", RepoName: "example", PRNumber: 7, HeadSHA: "aaa", Trigger: "ready_for_review", DeliveryID: "d-2"}
	created, err = database.EnsureAutoReviewIntent(&requeue, []string{AutoReviewIntentSuperseded, AutoReviewIntentFailed})
	require.NoError(t, err)
	assert.True(t, created)
	assert.Equal(t, intent.ID, requeue.ID)
	assert.Equal(t, AutoReviewIntentQueued, requeue.Status)
	assert.Equal(t, "d-2", requeue.DeliveryID)
	assert.Equal(t, "", requeue.RunID)

	all, err := database.ListAutoReviewIntents(AutoReviewIntentFilter{RepoOwner: "acme", RepoName: "example", PRNumber: 7})
	require.NoError(t, err)
	assert.Len(t, all, 1)
}

func TestSupersedeQueuedAutoReviewIntentsKeepsCurrentHeadAndRunningWork(t *testing.T) {
	database := newTestDB(t)
	defer database.Close()
	seed := func(head, status string) AutoReviewIntent {
		intent := AutoReviewIntent{RepoOwner: "acme", RepoName: "example", PRNumber: 7, HeadSHA: head, Trigger: "synchronize"}
		_, err := database.EnsureAutoReviewIntent(&intent, nil)
		require.NoError(t, err)
		if status != AutoReviewIntentQueued {
			_, err = database.UpdateAutoReviewIntentStatus(intent.ID, nil, status, "run-x")
			require.NoError(t, err)
		}
		return intent
	}
	older := seed("aaa", AutoReviewIntentQueued)
	running := seed("bbb", AutoReviewIntentRunning)
	current := seed("ccc", AutoReviewIntentQueued)

	n, err := database.SupersedeQueuedAutoReviewIntents("acme", "example", 7, "ccc")
	require.NoError(t, err)
	assert.Equal(t, 1, n)
	byID := map[uint]string{}
	all, err := database.ListAutoReviewIntents(AutoReviewIntentFilter{PRNumber: 7})
	require.NoError(t, err)
	for _, intent := range all {
		byID[intent.ID] = intent.Status
	}
	assert.Equal(t, AutoReviewIntentSuperseded, byID[older.ID])
	assert.Equal(t, AutoReviewIntentRunning, byID[running.ID])
	assert.Equal(t, AutoReviewIntentQueued, byID[current.ID])

	n, err = database.SupersedeQueuedAutoReviewIntents("acme", "example", 7, "")
	require.NoError(t, err)
	assert.Equal(t, 1, n)
	status, err := database.GetWebhookStatus(time.Now())
	require.NoError(t, err)
	assert.Equal(t, 0, status.IntentsQueued)
}
