package db

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestAutoReviewReadySettingSeededOff(t *testing.T) {
	database := newTestDB(t)
	defer database.Close()
	value, err := database.GetSetting("auto_review_ready_prs")
	require.NoError(t, err)
	assert.Equal(t, "false", value)
}

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

func TestDeleteWebhookDeliveriesBeforePrunesOnlyOldRows(t *testing.T) {
	database := newTestDB(t)
	defer database.Close()
	old := &WebhookDelivery{DeliveryID: "old", Event: "pull_request", Action: "opened", RepoOwner: "acme", RepoName: "example", PRNumber: 1, ReceivedAt: time.Now().Add(-48 * time.Hour)}
	fresh := &WebhookDelivery{DeliveryID: "fresh", Event: "pull_request", Action: "opened", RepoOwner: "acme", RepoName: "example", PRNumber: 2}
	for _, d := range []*WebhookDelivery{old, fresh} {
		_, err := database.CreateWebhookDelivery(d)
		require.NoError(t, err)
	}
	n, err := database.DeleteWebhookDeliveriesBefore(time.Now().Add(-24 * time.Hour))
	require.NoError(t, err)
	assert.Equal(t, int64(1), n)
	inserted, err := database.CreateWebhookDelivery(old)
	require.NoError(t, err)
	assert.True(t, inserted, "a pruned delivery id is accepted again")
	inserted, err = database.CreateWebhookDelivery(fresh)
	require.NoError(t, err)
	assert.False(t, inserted)
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

func TestEnsureAutoReviewIntentLowercasesTargetsAndSeedsStatus(t *testing.T) {
	database := newTestDB(t)
	defer database.Close()
	seeded := AutoReviewIntent{RepoOwner: "ACME", RepoName: "Example", PRNumber: 7, HeadSHA: "aaa", Trigger: "poll_fallback", Status: AutoReviewIntentDone, Publication: "posted"}
	created, err := database.EnsureAutoReviewIntent(&seeded, nil)
	require.NoError(t, err)
	assert.True(t, created)
	assert.Equal(t, "acme", seeded.RepoOwner)
	assert.Equal(t, "example", seeded.RepoName)
	assert.Equal(t, AutoReviewIntentDone, seeded.Status)
	assert.Equal(t, "posted", seeded.Publication)

	variant := AutoReviewIntent{RepoOwner: "acme", RepoName: "EXAMPLE", PRNumber: 7, HeadSHA: "aaa", Trigger: "ready_for_review"}
	created, err = database.EnsureAutoReviewIntent(&variant, []string{AutoReviewIntentSuperseded})
	require.NoError(t, err)
	assert.False(t, created)
	assert.Equal(t, seeded.ID, variant.ID)

	n, err := database.SupersedeQueuedAutoReviewIntents("Acme", "example", 7, "")
	require.NoError(t, err)
	assert.Equal(t, 0, n, "a done intent is left alone")
	listed, err := database.ListAutoReviewIntents(AutoReviewIntentFilter{RepoOwner: "ACME", RepoName: "example"})
	require.NoError(t, err)
	assert.Len(t, listed, 1)

	require.NoError(t, database.SetAutoReviewIntentPublicationByRun("run-none", "posted"))
	moved, err := database.UpdateAutoReviewIntentStatus(seeded.ID, nil, AutoReviewIntentRunning, "run-9")
	require.NoError(t, err)
	assert.True(t, moved)
	require.NoError(t, database.SetAutoReviewIntentPublicationByRun("run-9", "skipped: pull request is a draft"))
	listed, err = database.ListAutoReviewIntents(AutoReviewIntentFilter{PRNumber: 7})
	require.NoError(t, err)
	assert.Equal(t, "skipped: pull request is a draft", listed[0].Publication)
	assert.Equal(t, "run-9", listed[0].RunID)
}

func TestEnsureAutoReviewIntentHonorsTheSeededStatusOverASupersededRow(t *testing.T) {
	database := newTestDB(t)
	defer database.Close()
	stale := AutoReviewIntent{RepoOwner: "acme", RepoName: "example", PRNumber: 7, HeadSHA: "aaa", Trigger: "ready_for_review"}
	_, err := database.EnsureAutoReviewIntent(&stale, nil)
	require.NoError(t, err)
	_, err = database.UpdateAutoReviewIntentStatus(stale.ID, nil, AutoReviewIntentSuperseded, "")
	require.NoError(t, err)

	seeded := AutoReviewIntent{RepoOwner: "acme", RepoName: "example", PRNumber: 7, HeadSHA: "aaa", Trigger: "poll_fallback", Status: AutoReviewIntentDone, Publication: "posted"}
	created, err := database.EnsureAutoReviewIntent(&seeded, []string{AutoReviewIntentSuperseded})
	require.NoError(t, err)
	assert.True(t, created)
	assert.Equal(t, stale.ID, seeded.ID)
	assert.Equal(t, AutoReviewIntentDone, seeded.Status, "a published head must not be re-queued over a superseded row")
	assert.Equal(t, "posted", seeded.Publication)

	old := time.Now().Add(-48 * time.Hour)
	require.NoError(t, database.db.Model(&AutoReviewIntentModel{}).Where("id = ?", seeded.ID).Update("updated_at", old).Error)
	n, err := database.DeleteTerminalAutoReviewIntentsBefore(time.Now().Add(-24 * time.Hour))
	require.NoError(t, err)
	assert.Equal(t, int64(1), n)
	left, err := database.ListAutoReviewIntents(AutoReviewIntentFilter{})
	require.NoError(t, err)
	assert.Empty(t, left)
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
