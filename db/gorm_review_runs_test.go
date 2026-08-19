package db

import (
	"errors"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm/schema"
)

func reviewRunFixture(runID string, acceptedAt time.Time) ReviewRun {
	return ReviewRun{
		RunID:               runID,
		RepoOwner:           "acme",
		RepoName:            "widgets",
		PRNumber:            42,
		CommitSHA:           "0123456789abcdef0123456789abcdef01234567",
		TriggerSource:       "api",
		Status:              ReviewRunStatusQueued,
		RequestedConfigJSON: `{"agent":{"model":"claude-fable-5"}}`,
		EffectiveConfigJSON: `{"schema_version":1,"agent":{"model":"claude-fable-5"}}`,
		ConfigSourcesJSON:   `{"agent.model":"request"}`,
		ConfigHash:          "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		ConfigSchemaVersion: 1,
		AgentBackend:        "claude",
		AgentModel:          "claude-fable-5",
		AgentEffort:         "medium",
		AgentWallClockSec:   900,
		AgentMaxTurns:       120,
		AcceptedAt:          acceptedAt,
		QueuedAt:            acceptedAt,
		IdempotencyScope:    "user:7",
		IdempotencyKeyHash:  runID + "-key",
		RequestHash:         runID + "-request",
	}
}

func TestGormDBReviewRunLifecycleAndHistory(t *testing.T) {
	database := newTestDB(t)
	defer database.Close()

	now := time.Now().UTC().Truncate(time.Millisecond)
	older := reviewRunFixture("run-00000000000000000000000000000001", now.Add(-time.Minute))
	newer := reviewRunFixture("run-00000000000000000000000000000002", now)
	require.NoError(t, database.CreateReviewRun(&older))
	olderCompleted := ReviewRunStatusCompleted
	require.NoError(t, database.PatchReviewRun(older.RunID, ReviewRunPatch{Status: &olderCompleted}))
	require.NoError(t, database.CreateReviewRun(&newer))

	fetched, err := database.GetReviewRun(newer.RunID)
	require.NoError(t, err)
	require.NotNil(t, fetched)
	assert.Equal(t, newer.AgentModel, fetched.AgentModel)
	assert.Equal(t, newer.EffectiveConfigJSON, fetched.EffectiveConfigJSON)

	running := ReviewRunStatusRunning
	started := now.Add(time.Second)
	attempt := 1
	require.NoError(t, database.PatchReviewRun(newer.RunID, ReviewRunPatch{
		Status:           &running,
		StartedAt:        &started,
		ExecutionAttempt: &attempt,
	}))
	fetched, err = database.GetReviewRun(newer.RunID)
	require.NoError(t, err)
	require.NotNil(t, fetched)
	assert.Equal(t, ReviewRunStatusRunning, fetched.Status)
	assert.Equal(t, 1, fetched.ExecutionAttempt)
	require.NotNil(t, fetched.StartedAt)
	assert.WithinDuration(t, started, *fetched.StartedAt, time.Millisecond)

	runs, err := database.ListReviewRuns(ReviewRunFilter{
		RepoOwner: "acme", RepoName: "widgets", PRNumber: 42,
	})
	require.NoError(t, err)
	require.Len(t, runs, 2)
	assert.Equal(t, newer.RunID, runs[0].RunID)
	assert.Equal(t, older.RunID, runs[1].RunID)

	err = database.PatchReviewRun(newer.RunID, ReviewRunPatch{})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "patch is empty")
}

func TestGormDBReviewProjectionFencesSupersededRuns(t *testing.T) {
	database := newTestDB(t)
	defer database.Close()

	const (
		owner = "acme"
		repo  = "widgets"
		prNum = 42
		runA  = "run-000000000000000000000000000000aa"
		runB  = "run-000000000000000000000000000000bb"
		sha   = "0123456789abcdef0123456789abcdef01234567"
	)
	require.NoError(t, database.SetPRGeneratingForReviewRun(owner, repo, prNum, sha, "Title", "alice", nil, false, runA))
	updated, err := database.SetPRAgentReviewingForReviewRun(owner, repo, prNum, runA)
	require.NoError(t, err)
	require.True(t, updated)
	require.NoError(t, database.SetPRGeneratingForReviewRun(owner, repo, prNum, sha, "Title", "alice", nil, false, runB))

	updated, err = database.SetPRErrorForReviewRun(owner, repo, prNum, runA, "stale failure")
	require.NoError(t, err)
	assert.False(t, updated)
	updated, err = database.MarkPRCompletedForReviewRun(owner, repo, prNum, runA, runA, sha, "stale.html", 1, 2, 3, "request_changes", false, `{}`)
	require.NoError(t, err)
	assert.False(t, updated)
	updated, err = database.MarkPRCompletedForReviewRun(owner, repo, prNum, runB, runB, sha, "winner.html", 0, 1, 0, "approve", false, `{"run_id":"`+runB+`"}`)
	require.NoError(t, err)
	assert.True(t, updated)

	pr, err := database.GetPR(owner, repo, prNum)
	require.NoError(t, err)
	require.NotNil(t, pr)
	assert.Equal(t, "completed", pr.Status)
	assert.Equal(t, "winner.html", pr.ReviewHTMLPath)
	assert.Equal(t, runB, pr.ReviewRunID)
}

func TestGormDBOutdatedResetInvalidatesReviewProjection(t *testing.T) {
	database := newTestDB(t)
	defer database.Close()

	const (
		owner  = "acme"
		repo   = "widgets"
		prNum  = 43
		runID  = "run-000000000000000000000000000000bc"
		oldSHA = "0123456789abcdef0123456789abcdef01234567"
		newSHA = "89abcdef0123456789abcdef0123456789abcdef"
	)
	require.NoError(t, database.SetPRGeneratingForReviewRun(owner, repo, prNum, oldSHA, "Title", "alice", nil, false, runID))
	require.NoError(t, database.db.Model(&PRModel{}).
		Where("repo_owner = ? AND repo_name = ? AND pr_number = ?", owner, repo, prNum).
		Updates(map[string]any{"status": "error", "error_message": "old failure", "error_retry_count": 1}).Error)
	require.NoError(t, database.ResetPRToOutdated(owner, repo, prNum, newSHA))

	projected, err := database.SetPRAgentReviewingForReviewRun(owner, repo, prNum, runID)
	require.NoError(t, err)
	assert.False(t, projected)
	projected, err = database.SetPRErrorForReviewRun(owner, repo, prNum, runID, "stale failure")
	require.NoError(t, err)
	assert.False(t, projected)
	projected, err = database.MarkPRCompletedForReviewRun(owner, repo, prNum, runID, runID, oldSHA, "stale.html", 1, 2, 3, "request_changes", false, `{}`)
	require.NoError(t, err)
	assert.False(t, projected)

	var model PRModel
	require.NoError(t, database.db.Where("repo_owner = ? AND repo_name = ? AND pr_number = ?", owner, repo, prNum).First(&model).Error)
	assert.Equal(t, "pending", model.Status)
	assert.Equal(t, newSHA, model.LastCommitSHA)
	assert.Empty(t, model.ProjectionRunID)
	assert.Empty(t, model.ErrorMessage)
	assert.Zero(t, model.ErrorRetryCount)
}

func TestGormDBRunAdmissionPreservesExistingCreatedAtWhenMissing(t *testing.T) {
	database := newTestDB(t)
	defer database.Close()

	createdAt := time.Now().UTC().Add(-24 * time.Hour).Truncate(time.Millisecond)
	const sha = "0123456789abcdef0123456789abcdef01234567"
	require.NoError(t, database.SetPRGeneratingForReviewRun(
		"acme", "created-at", 6, sha, "Title", "alice", &createdAt, false,
		"run-000000000000000000000000000000c1",
	))
	require.NoError(t, database.SetPRGeneratingForReviewRun(
		"acme", "created-at", 6, sha, "Updated", "alice", nil, false,
		"run-000000000000000000000000000000c2",
	))
	pr, err := database.GetPR("acme", "created-at", 6)
	require.NoError(t, err)
	require.NotNil(t, pr)
	require.NotNil(t, pr.CreatedAt)
	assert.WithinDuration(t, createdAt, *pr.CreatedAt, time.Millisecond)
}

func TestGormDBCachedProjectionCannotSupersedeLiveRun(t *testing.T) {
	database := newTestDB(t)
	defer database.Close()

	const (
		owner    = "acme"
		repo     = "widgets"
		prNum    = 42
		sha      = "0123456789abcdef0123456789abcdef01234567"
		liveRun  = "run-000000000000000000000000000000d1"
		cacheRun = "run-000000000000000000000000000000d2"
	)
	reviewRun := reviewRunFixture(liveRun, time.Now().UTC())
	require.NoError(t, database.CreateReviewRun(&reviewRun))
	claimed, err := database.ClaimReviewRun(liveRun, "worker-a", time.Now().UTC(), time.Now().UTC().Add(time.Minute))
	require.NoError(t, err)
	require.True(t, claimed)
	require.NoError(t, database.SetPRGeneratingForReviewRun(owner, repo, prNum, sha, "Title", "alice", nil, false, liveRun))
	// Model the stale-generating reset racing the still-live worker. The run
	// subquery, not just the visible status, must protect its projection.
	require.NoError(t, database.UpdatePRStatus(owner, repo, prNum, "pending"))
	projected, err := database.SetPRErrorIfNoLiveReview(owner, repo, prNum, "local dispatch failed")
	require.NoError(t, err)
	assert.False(t, projected)

	restored, err := database.RestorePRCompletedFromCacheForReviewRun(
		owner, repo, prNum, cacheRun, "old-success", sha, "cached.html", 1, 2, 3, "request_changes", false, `{}`, time.Now().UTC(),
	)
	require.NoError(t, err)
	assert.False(t, restored)
	var model PRModel
	require.NoError(t, database.db.Where("repo_owner = ? AND repo_name = ? AND pr_number = ?", owner, repo, prNum).First(&model).Error)
	assert.Equal(t, liveRun, model.ProjectionRunID)
	assert.Equal(t, "pending", model.Status)
}

func TestGormDBCachedProjectionRestoresIdlePendingPR(t *testing.T) {
	database := newTestDB(t)
	defer database.Close()

	const (
		owner    = "acme"
		repo     = "idle-cache"
		prNum    = 7
		sha      = "1123456789abcdef0123456789abcdef01234567"
		cacheRun = "run-000000000000000000000000000000d3"
	)
	require.NoError(t, database.UpsertPR(&PR{
		RepoOwner: owner, RepoName: repo, PRNumber: prNum, LastCommitSHA: sha, Status: "pending",
	}))
	projected, err := database.SetPRErrorIfNoLiveReview(owner, repo, prNum, "invalid local configuration")
	require.NoError(t, err)
	require.True(t, projected)
	restored, err := database.RestorePRCompletedFromCacheForReviewRun(
		owner, repo, prNum, cacheRun, "old-success", sha, "cached.html", 0, 1, 0, "approve_suggestions", false, `{}`, time.Now().UTC(),
	)
	require.NoError(t, err)
	require.True(t, restored)
	var model PRModel
	require.NoError(t, database.db.Where("repo_owner = ? AND repo_name = ? AND pr_number = ?", owner, repo, prNum).First(&model).Error)
	assert.Equal(t, cacheRun, model.ProjectionRunID)
	assert.Equal(t, "completed", model.Status)
	assert.Equal(t, "cached.html", model.ReviewPath)
}

func TestGormDBAdmissionErrorCannotClobberUnprojectedQueuedRun(t *testing.T) {
	database := newTestDB(t)
	defer database.Close()

	now := time.Now().UTC()
	terminal := reviewRunFixture("run-000000000000000000000000000000e1", now)
	queued := reviewRunFixture("run-000000000000000000000000000000e2", now)
	queued.RepoOwner, queued.RepoName, queued.PRNumber, queued.CommitSHA = terminal.RepoOwner, terminal.RepoName, terminal.PRNumber, terminal.CommitSHA
	require.NoError(t, database.CreateReviewRun(&terminal))
	require.NoError(t, database.SetPRGeneratingForReviewRun(terminal.RepoOwner, terminal.RepoName, terminal.PRNumber, terminal.CommitSHA, "Title", "alice", nil, false, terminal.RunID))
	failed := ReviewRunStatusFailed
	require.NoError(t, database.PatchReviewRun(terminal.RunID, ReviewRunPatch{Status: &failed}))
	require.NoError(t, database.CreateReviewRun(&queued))

	projected, err := database.SetPRErrorIfNoLiveReview(terminal.RepoOwner, terminal.RepoName, terminal.PRNumber, "dispatch failed")
	require.NoError(t, err)
	assert.False(t, projected)
	cancelled := ReviewRunStatusCancelled
	require.NoError(t, database.PatchReviewRun(queued.RunID, ReviewRunPatch{Status: &cancelled}))
	projected, err = database.SetPRErrorIfNoLiveReview(terminal.RepoOwner, terminal.RepoName, terminal.PRNumber, "dispatch failed")
	require.NoError(t, err)
	assert.True(t, projected)
}

func TestGormDBCachedProjectionRepairsGeneratingRowWithTerminalOwner(t *testing.T) {
	database := newTestDB(t)
	defer database.Close()

	const (
		owner       = "acme"
		repo        = "crash-cache"
		prNum       = 8
		sha         = "2123456789abcdef0123456789abcdef01234567"
		terminalRun = "run-000000000000000000000000000000d4"
		cacheRun    = "run-000000000000000000000000000000d5"
	)
	run := reviewRunFixture(terminalRun, time.Now().UTC())
	require.NoError(t, database.CreateReviewRun(&run))
	require.NoError(t, database.SetPRGeneratingForReviewRun(owner, repo, prNum, sha, "Title", "alice", nil, false, terminalRun))
	failed := ReviewRunStatusFailed
	require.NoError(t, database.PatchReviewRun(terminalRun, ReviewRunPatch{Status: &failed}))
	restored, err := database.RestorePRCompletedFromCacheForReviewRun(
		owner, repo, prNum, cacheRun, terminalRun, sha, "cached.html", 1, 0, 0, "request_changes", false, `{}`, time.Now().UTC().Add(-time.Minute),
	)
	require.NoError(t, err)
	require.False(t, restored, "a fresh manual admission window must not be mistaken for a crashed owner")
	queued := reviewRunFixture("run-000000000000000000000000000000d6", time.Now().UTC())
	queued.RepoOwner, queued.RepoName, queued.PRNumber, queued.CommitSHA = owner, repo, prNum, sha
	require.NoError(t, database.CreateReviewRun(&queued))

	restored, err = database.RestorePRCompletedFromCacheForReviewRun(
		owner, repo, prNum, cacheRun, terminalRun, sha, "cached.html", 1, 0, 0, "request_changes", false, `{}`, time.Now().UTC().Add(time.Second),
	)
	require.NoError(t, err)
	require.False(t, restored, "a separately queued live run must protect the PR before it claims the projection")
	cancelled := ReviewRunStatusCancelled
	require.NoError(t, database.PatchReviewRun(queued.RunID, ReviewRunPatch{Status: &cancelled}))
	restored, err = database.RestorePRCompletedFromCacheForReviewRun(
		owner, repo, prNum, cacheRun, terminalRun, sha, "cached.html", 1, 0, 0, "request_changes", false, `{}`, time.Now().UTC().Add(time.Second),
	)
	require.NoError(t, err)
	require.True(t, restored)
	var model PRModel
	require.NoError(t, database.db.Where("repo_owner = ? AND repo_name = ? AND pr_number = ?", owner, repo, prNum).First(&model).Error)
	assert.Equal(t, "completed", model.Status)
	assert.Equal(t, cacheRun, model.ProjectionRunID)
}

func TestGormDBAutomaticReviewAdmissionPreservesRetryCap(t *testing.T) {
	database := newTestDB(t)
	defer database.Close()

	const (
		owner = "acme"
		repo  = "retry-cap"
		prNum = 9
		sha   = "0123456789abcdef0123456789abcdef01234567"
		runA  = "run-000000000000000000000000000000ca"
		runB  = "run-000000000000000000000000000000cb"
	)
	require.NoError(t, database.SetPRGeneratingForReviewRun(owner, repo, prNum, sha, "Title", "alice", nil, false, runA))
	updated, err := database.SetPRErrorForReviewRun(owner, repo, prNum, runA, "timed out")
	require.NoError(t, err)
	require.True(t, updated)
	old := time.Now().UTC().Add(-time.Hour)
	require.NoError(t, database.db.Model(&PRModel{}).
		Where("repo_owner = ? AND repo_name = ? AND pr_number = ?", owner, repo, prNum).
		Update("last_reviewed_at", old).Error)
	count, err := database.ResetErrorPRs(5, 1)
	require.NoError(t, err)
	require.Equal(t, 1, count)

	require.NoError(t, database.SetPRGeneratingForReviewRun(owner, repo, prNum, sha, "Title", "alice", nil, false, runB))
	var model PRModel
	require.NoError(t, database.db.Where("repo_owner = ? AND repo_name = ? AND pr_number = ?", owner, repo, prNum).First(&model).Error)
	assert.Equal(t, 1, model.ErrorRetryCount, "automatic admission must not re-arm the retry counter")
	updated, err = database.SetPRErrorForReviewRun(owner, repo, prNum, runB, "timed out again")
	require.NoError(t, err)
	require.True(t, updated)
	require.NoError(t, database.db.Model(&PRModel{}).
		Where("repo_owner = ? AND repo_name = ? AND pr_number = ?", owner, repo, prNum).
		Update("last_reviewed_at", old).Error)
	count, err = database.ResetErrorPRs(5, 1)
	require.NoError(t, err)
	assert.Zero(t, count, "the second deterministic failure must remain capped")
}

func TestGormDBReviewRunIdempotencyLookup(t *testing.T) {
	database := newTestDB(t)
	defer database.Close()

	run := reviewRunFixture("run-00000000000000000000000000000003", time.Now().UTC())
	require.NoError(t, database.CreateReviewRun(&run))
	fetched, err := database.GetReviewRunByIdempotency(run.IdempotencyScope, run.IdempotencyKeyHash)
	require.NoError(t, err)
	require.NotNil(t, fetched)
	assert.Equal(t, run.RunID, fetched.RunID)

	missing, err := database.GetReviewRunByIdempotency("", "")
	require.NoError(t, err)
	assert.Nil(t, missing)

	conflict := reviewRunFixture("run-00000000000000000000000000000006", time.Now().UTC())
	conflict.IdempotencyScope = run.IdempotencyScope
	conflict.IdempotencyKeyHash = run.IdempotencyKeyHash
	err = database.CreateReviewRun(&conflict)
	if !errors.Is(err, ErrReviewRunConflict) {
		t.Fatalf("conflict err=%v", err)
	}

	mismatched := reviewRunFixture("run-00000000000000000000000000000011", time.Now().UTC())
	mismatched.IdempotencyScope = ""
	err = database.CreateReviewRun(&mismatched)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "must be set together")
}

func TestGormDBAllowsMultipleSequentialRunsWithoutIdempotencyKey(t *testing.T) {
	database := newTestDB(t)
	defer database.Close()

	ids := []string{
		"run-00000000000000000000000000000012",
		"run-00000000000000000000000000000013",
	}
	for index, id := range ids {
		run := reviewRunFixture(id, time.Now().UTC())
		run.IdempotencyScope = ""
		run.IdempotencyKeyHash = ""
		require.NoError(t, database.CreateReviewRun(&run))
		if index < len(ids)-1 {
			completed := ReviewRunStatusCompleted
			require.NoError(t, database.PatchReviewRun(run.RunID, ReviewRunPatch{Status: &completed}))
		}
	}
}

func TestGormDBRejectsSecondLiveRunForSamePR(t *testing.T) {
	database := newTestDB(t)
	defer database.Close()

	first := reviewRunFixture("run-00000000000000000000000000000014", time.Now().UTC())
	first.IdempotencyScope, first.IdempotencyKeyHash = "", ""
	require.NoError(t, database.CreateReviewRun(&first))
	second := reviewRunFixture("run-00000000000000000000000000000015", time.Now().UTC())
	second.IdempotencyScope, second.IdempotencyKeyHash = "", ""
	err := database.CreateReviewRun(&second)
	require.ErrorIs(t, err, ErrReviewRunActiveConflict)

	completed := ReviewRunStatusCompleted
	require.NoError(t, database.PatchReviewRun(first.RunID, ReviewRunPatch{Status: &completed}))
	require.NoError(t, database.CreateReviewRun(&second), "terminal history must not block the next run")
}

func TestGormDBCreateReviewRunRejectsIncompleteLedgerEntry(t *testing.T) {
	database := newTestDB(t)
	defer database.Close()

	err := database.CreateReviewRun(&ReviewRun{RunID: "run-00000000000000000000000000000005"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "complete PR target")
}

func TestGormDBPatchQueuedReviewRunIsFencedByStatus(t *testing.T) {
	database := newTestDB(t)
	defer database.Close()

	now := time.Now().UTC().Truncate(time.Millisecond)
	run := reviewRunFixture("run-00000000000000000000000000000019", now)
	require.NoError(t, database.CreateReviewRun(&run))
	failed := ReviewRunStatusFailed
	terminalCode := "dispatch_failed"
	updated, err := database.PatchQueuedReviewRun(run.RunID, ReviewRunPatch{Status: &failed, TerminalCode: &terminalCode})
	require.NoError(t, err)
	assert.True(t, updated)

	running := reviewRunFixture("run-00000000000000000000000000000020", now)
	running.IdempotencyScope = ""
	running.IdempotencyKeyHash = ""
	require.NoError(t, database.CreateReviewRun(&running))
	claimed, err := database.ClaimReviewRun(running.RunID, "worker-a", now, now.Add(time.Minute))
	require.NoError(t, err)
	require.True(t, claimed)
	updated, err = database.PatchQueuedReviewRun(running.RunID, ReviewRunPatch{Status: &failed, TerminalCode: &terminalCode})
	require.NoError(t, err)
	assert.False(t, updated)
	active, err := database.GetReviewRun(running.RunID)
	require.NoError(t, err)
	require.NotNil(t, active)
	assert.Equal(t, ReviewRunStatusRunning, active.Status)
}

func TestGormDBUpsertReviewStageAttempt(t *testing.T) {
	database := newTestDB(t)
	defer database.Close()

	now := time.Now().UTC().Truncate(time.Millisecond)
	run := reviewRunFixture("run-00000000000000000000000000000004", now)
	require.NoError(t, database.CreateReviewRun(&run))
	attempt := ReviewStageAttempt{
		RunID:                run.RunID,
		ExecutionAttempt:     1,
		Stage:                "agent",
		InvocationNumber:     1,
		AttemptNumber:        1,
		Provider:             "anthropic",
		Backend:              "claude",
		RequestedModel:       "claude-fable-5",
		ObservedServedModels: []string{"claude-fable-5", "claude-opus-4-8"},
		PrimaryServedModel:   "claude-opus-4-8",
		ServedModelSource:    "stream",
		ServingModelVerified: true,
		Fallback:             true,
		MatcherVersion:       "v1",
		Status:               "completed",
		AssistantTurns:       12,
		StartedAt:            &now,
	}
	require.NoError(t, database.UpsertReviewStageAttempt(&attempt))
	require.NotZero(t, attempt.ID)
	require.False(t, attempt.CreatedAt.IsZero())
	createdID := attempt.ID
	createdAt := attempt.CreatedAt

	attempt.AssistantTurns = 13
	attempt.StopReason = "complete"
	require.NoError(t, database.UpsertReviewStageAttempt(&attempt))
	assert.Equal(t, createdID, attempt.ID)
	assert.Equal(t, createdAt, attempt.CreatedAt)

	attempts, err := database.ListReviewStageAttempts(run.RunID)
	require.NoError(t, err)
	require.Len(t, attempts, 1)
	assert.Equal(t, 13, attempts[0].AssistantTurns)
	assert.Equal(t, []string{"claude-fable-5", "claude-opus-4-8"}, attempts[0].ObservedServedModels)
	assert.True(t, attempts[0].ServingModelVerified)

	retry := attempt
	retry.ID = 0
	retry.ExecutionAttempt = 2
	retry.AssistantTurns = 4
	require.NoError(t, database.UpsertReviewStageAttempt(&retry))
	attempts, err = database.ListReviewStageAttempts(run.RunID)
	require.NoError(t, err)
	require.Len(t, attempts, 2)
	assert.Equal(t, 1, attempts[0].ExecutionAttempt)
	assert.Equal(t, 2, attempts[1].ExecutionAttempt)
}

func TestGormDBReviewRunLeaseIsAtomicAndClearable(t *testing.T) {
	database := newTestDB(t)
	defer database.Close()

	now := time.Now().UTC().Truncate(time.Millisecond)
	run := reviewRunFixture("run-00000000000000000000000000000007", now)
	require.NoError(t, database.CreateReviewRun(&run))

	claimed, err := database.ClaimReviewRun(run.RunID, "worker-a", now, now.Add(time.Minute))
	require.NoError(t, err)
	assert.True(t, claimed)
	claimed, err = database.ClaimReviewRun(run.RunID, "worker-b", now, now.Add(time.Minute))
	require.NoError(t, err)
	assert.False(t, claimed)

	renewed, err := database.RenewReviewRunLease(run.RunID, "worker-b", now, now.Add(2*time.Minute))
	require.NoError(t, err)
	assert.False(t, renewed)
	renewed, err = database.RenewReviewRunLease(run.RunID, "worker-a", now, now.Add(2*time.Minute))
	require.NoError(t, err)
	assert.True(t, renewed)

	fetched, err := database.GetReviewRun(run.RunID)
	require.NoError(t, err)
	require.NotNil(t, fetched)
	assert.Equal(t, ReviewRunStatusRunning, fetched.Status)
	assert.Equal(t, "worker-a", fetched.LeaseHolder)
	assert.Equal(t, 1, fetched.ExecutionAttempt)
	require.NotNil(t, fetched.LeaseExpiresAt)

	zero := time.Time{}
	require.NoError(t, database.PatchReviewRun(run.RunID, ReviewRunPatch{LeaseExpiresAt: &zero}))
	fetched, err = database.GetReviewRun(run.RunID)
	require.NoError(t, err)
	require.NotNil(t, fetched)
	assert.Nil(t, fetched.LeaseExpiresAt)

	// A running run with no live lease can be reclaimed and increments the attempt.
	claimed, err = database.ClaimReviewRun(run.RunID, "worker-b", now, now.Add(time.Minute))
	require.NoError(t, err)
	assert.True(t, claimed)

	// An expired running lease can also be taken over.
	expired := now.Add(-time.Second)
	partialPath := "partial/review.json"
	oldError := "worker crashed"
	oldCriticalCount := 2
	require.NoError(t, database.PatchReviewRun(run.RunID, ReviewRunPatch{
		LeaseExpiresAt: &expired,
		JSONPath:       &partialPath,
		ErrorSummary:   &oldError,
		CriticalCount:  &oldCriticalCount,
	}))
	renewed, err = database.RenewReviewRunLease(run.RunID, "worker-b", now, now.Add(time.Minute))
	require.NoError(t, err)
	assert.False(t, renewed, "expired lease must not be resurrected by renewal")
	completed := ReviewRunStatusCompleted
	patched, err := database.PatchReviewRunAsHolder(run.RunID, "worker-b", now, ReviewRunPatch{Status: &completed})
	require.NoError(t, err)
	assert.False(t, patched, "holder with expired lease must not commit results")

	claimed, err = database.ClaimReviewRun(run.RunID, "worker-c", now, now.Add(time.Minute))
	require.NoError(t, err)
	assert.True(t, claimed)
	fetched, err = database.GetReviewRun(run.RunID)
	require.NoError(t, err)
	require.NotNil(t, fetched)
	assert.Equal(t, "worker-c", fetched.LeaseHolder)
	assert.Equal(t, 3, fetched.ExecutionAttempt)
	assert.Empty(t, fetched.JSONPath)
	assert.Empty(t, fetched.ErrorSummary)
	assert.Zero(t, fetched.CriticalCount)

	patched, err = database.PatchReviewRunAsHolder(run.RunID, "worker-a", now, ReviewRunPatch{Status: &completed})
	require.NoError(t, err)
	assert.False(t, patched, "stale worker must not commit a terminal result")
	patched, err = database.PatchReviewRunAsHolder(run.RunID, "worker-c", now, ReviewRunPatch{Status: &completed})
	require.NoError(t, err)
	assert.True(t, patched)
	fetched, err = database.GetReviewRun(run.RunID)
	require.NoError(t, err)
	require.NotNil(t, fetched)
	assert.Equal(t, ReviewRunStatusCompleted, fetched.Status)
	claimed, err = database.ClaimReviewRun(run.RunID, "worker-d", now, now.Add(time.Minute))
	require.NoError(t, err)
	assert.False(t, claimed, "a terminal run must not be claimable")
}

func TestGormDBAbandonsOnlyLeasesExpiredBeyondGrace(t *testing.T) {
	database := newTestDB(t)
	defer database.Close()

	now := time.Now().UTC().Truncate(time.Millisecond)
	expired := reviewRunFixture("run-00000000000000000000000000000014", now)
	withinGrace := reviewRunFixture("run-00000000000000000000000000000015", now)
	live := reviewRunFixture("run-00000000000000000000000000000016", now)
	withinGrace.PRNumber = 43
	live.PRNumber = 44
	for _, run := range []*ReviewRun{&expired, &withinGrace, &live} {
		run.IdempotencyScope = ""
		run.IdempotencyKeyHash = ""
		require.NoError(t, database.CreateReviewRun(run))
	}
	requireClaim := func(runID string, expiresAt time.Time) {
		claimed, err := database.ClaimReviewRun(runID, "worker-"+runID, now.Add(-time.Hour), expiresAt)
		require.NoError(t, err)
		require.True(t, claimed)
	}
	requireClaim(expired.RunID, now.Add(-10*time.Minute))
	requireClaim(withinGrace.RunID, now.Add(-time.Minute))
	requireClaim(live.RunID, now.Add(time.Minute))
	require.NoError(t, database.SetPRGeneratingForReviewRun(
		expired.RepoOwner, expired.RepoName, expired.PRNumber, expired.CommitSHA,
		"Expired run", "alice", nil, false, expired.RunID,
	))

	count, err := database.AbandonExpiredReviewRuns(now, 2*time.Minute, time.Hour)
	require.NoError(t, err)
	assert.Equal(t, 1, count)

	abandoned, err := database.GetReviewRun(expired.RunID)
	require.NoError(t, err)
	require.NotNil(t, abandoned)
	assert.Equal(t, ReviewRunStatusTimedOut, abandoned.Status)
	assert.Equal(t, "lease_abandoned", abandoned.TerminalCode)
	assert.Equal(t, "execution", abandoned.FailureStage)
	assert.Empty(t, abandoned.LeaseHolder)
	assert.Nil(t, abandoned.LeaseExpiresAt)
	require.NotNil(t, abandoned.CompletedAt)
	assert.WithinDuration(t, now, *abandoned.CompletedAt, time.Millisecond)
	pr, err := database.GetPR(expired.RepoOwner, expired.RepoName, expired.PRNumber)
	require.NoError(t, err)
	require.NotNil(t, pr)
	assert.Equal(t, "error", pr.Status)
	assert.Equal(t, "review run abandoned after lease expiry", pr.ErrorMessage)
	assert.Nil(t, pr.GeneratingSince)

	for _, runID := range []string{withinGrace.RunID, live.RunID} {
		run, getErr := database.GetReviewRun(runID)
		require.NoError(t, getErr)
		require.NotNil(t, run)
		assert.Equal(t, ReviewRunStatusRunning, run.Status)
	}

	count, err = database.AbandonExpiredReviewRuns(now.Add(time.Minute), -time.Second, time.Hour)
	require.Error(t, err)
	assert.Zero(t, count)
}

func TestGormDBAbandonsOnlyStaleQueuedRuns(t *testing.T) {
	database := newTestDB(t)
	defer database.Close()

	now := time.Now().UTC().Truncate(time.Millisecond)
	stale := reviewRunFixture("run-00000000000000000000000000000017", now.Add(-2*time.Hour))
	recent := reviewRunFixture("run-00000000000000000000000000000018", now.Add(-time.Minute))
	recent.PRNumber = 43
	for _, run := range []*ReviewRun{&stale, &recent} {
		run.IdempotencyScope = ""
		run.IdempotencyKeyHash = ""
		require.NoError(t, database.CreateReviewRun(run))
	}

	count, err := database.AbandonExpiredReviewRuns(now, 2*time.Minute, time.Hour)
	require.NoError(t, err)
	assert.Equal(t, 1, count)
	abandoned, err := database.GetReviewRun(stale.RunID)
	require.NoError(t, err)
	require.NotNil(t, abandoned)
	assert.Equal(t, ReviewRunStatusTimedOut, abandoned.Status)
	assert.Equal(t, "queue_abandoned", abandoned.TerminalCode)
	active, err := database.GetReviewRun(recent.RunID)
	require.NoError(t, err)
	require.NotNil(t, active)
	assert.Equal(t, ReviewRunStatusQueued, active.Status)
}

func TestGormDBAbandonedQueuedRunPreservesCompletedProjection(t *testing.T) {
	database := newTestDB(t)
	defer database.Close()

	now := time.Now().UTC().Truncate(time.Millisecond)
	run := reviewRunFixture("run-00000000000000000000000000000021", now.Add(-2*time.Hour))
	run.IdempotencyScope = ""
	run.IdempotencyKeyHash = ""
	require.NoError(t, database.CreateReviewRun(&run))
	require.NoError(t, database.SetPRGeneratingForReviewRun(
		run.RepoOwner, run.RepoName, run.PRNumber, run.CommitSHA,
		"Cached review", "alice", nil, false, run.RunID,
	))
	projected, err := database.MarkPRCompletedForReviewRun(
		run.RepoOwner, run.RepoName, run.PRNumber, run.RunID, run.RunID,
		run.CommitSHA, "cached.html", 0, 0, 1, "approve", false, `{}`,
	)
	require.NoError(t, err)
	require.True(t, projected)

	count, err := database.AbandonExpiredReviewRuns(now, 2*time.Minute, time.Hour)
	require.NoError(t, err)
	assert.Equal(t, 1, count)
	abandoned, err := database.GetReviewRun(run.RunID)
	require.NoError(t, err)
	require.NotNil(t, abandoned)
	assert.Equal(t, ReviewRunStatusTimedOut, abandoned.Status)
	assert.Equal(t, "queue_abandoned", abandoned.TerminalCode)

	pr, err := database.GetPR(run.RepoOwner, run.RepoName, run.PRNumber)
	require.NoError(t, err)
	require.NotNil(t, pr)
	assert.Equal(t, "completed", pr.Status)
	assert.Equal(t, "cached.html", pr.ReviewHTMLPath)
	assert.Empty(t, pr.ErrorMessage)
}

func TestGormDBAbandonedRunDoesNotErrorProjectionWithLiveReplacement(t *testing.T) {
	database := newTestDB(t)
	defer database.Close()
	require.NoError(t, database.db.Exec("DROP INDEX IF EXISTS idx_review_runs_one_live_per_pr").Error)

	now := time.Now().UTC().Truncate(time.Millisecond)
	abandoned := reviewRunFixture("run-00000000000000000000000000000022", now.Add(-2*time.Hour))
	replacement := reviewRunFixture("run-00000000000000000000000000000023", now)
	for _, run := range []*ReviewRun{&abandoned, &replacement} {
		run.IdempotencyScope = ""
		run.IdempotencyKeyHash = ""
		require.NoError(t, database.CreateReviewRun(run))
	}
	require.NoError(t, database.SetPRGeneratingForReviewRun(
		abandoned.RepoOwner, abandoned.RepoName, abandoned.PRNumber, abandoned.CommitSHA,
		"Superseded review", "alice", nil, false, abandoned.RunID,
	))

	count, err := database.AbandonExpiredReviewRuns(now, 2*time.Minute, time.Hour)
	require.NoError(t, err)
	assert.Equal(t, 1, count)
	stale, err := database.GetReviewRun(abandoned.RunID)
	require.NoError(t, err)
	require.NotNil(t, stale)
	assert.Equal(t, ReviewRunStatusTimedOut, stale.Status)
	live, err := database.GetReviewRun(replacement.RunID)
	require.NoError(t, err)
	require.NotNil(t, live)
	assert.Equal(t, ReviewRunStatusQueued, live.Status)

	pr, err := database.GetPR(abandoned.RepoOwner, abandoned.RepoName, abandoned.PRNumber)
	require.NoError(t, err)
	require.NotNil(t, pr)
	assert.Equal(t, "generating", pr.Status)
	assert.Empty(t, pr.ErrorMessage)
}

func TestGormDBQueuedDispatcherLeaseControlsAbandonment(t *testing.T) {
	database := newTestDB(t)
	defer database.Close()

	now := time.Now().UTC().Truncate(time.Millisecond)
	expired := reviewRunFixture("run-00000000000000000000000000000019", now)
	live := reviewRunFixture("run-00000000000000000000000000000020", now.Add(-2*time.Hour))
	live.PRNumber = 43
	for _, run := range []*ReviewRun{&expired, &live} {
		run.IdempotencyScope = ""
		run.IdempotencyKeyHash = ""
		require.NoError(t, database.CreateReviewRun(run))
	}
	claimed, err := database.ClaimOrRenewQueuedReviewRunLease(expired.RunID, "dispatcher-a", now.Add(-4*time.Minute), now.Add(-3*time.Minute))
	require.NoError(t, err)
	require.True(t, claimed)
	claimed, err = database.ClaimOrRenewQueuedReviewRunLease(live.RunID, "dispatcher-b", now, now.Add(time.Minute))
	require.NoError(t, err)
	require.True(t, claimed)
	claimed, err = database.ClaimOrRenewQueuedReviewRunLease(live.RunID, "dispatcher-c", now, now.Add(2*time.Minute))
	require.NoError(t, err)
	assert.False(t, claimed)

	count, err := database.AbandonExpiredReviewRuns(now, 2*time.Minute, time.Hour)
	require.NoError(t, err)
	assert.Equal(t, 1, count)
	expiredRun, err := database.GetReviewRun(expired.RunID)
	require.NoError(t, err)
	require.NotNil(t, expiredRun)
	assert.Equal(t, ReviewRunStatusTimedOut, expiredRun.Status)
	liveRun, err := database.GetReviewRun(live.RunID)
	require.NoError(t, err)
	require.NotNil(t, liveRun)
	assert.Equal(t, ReviewRunStatusQueued, liveRun.Status)
}

func TestGormDBReviewRunClaimHasSingleConcurrentWinner(t *testing.T) {
	database, err := NewGormSQLite("file:" + filepath.Join(t.TempDir(), "claims.db") + "?_busy_timeout=5000&_journal_mode=WAL")
	require.NoError(t, err)
	defer database.Close()

	now := time.Now().UTC().Truncate(time.Millisecond)
	run := reviewRunFixture("run-00000000000000000000000000000010", now)
	require.NoError(t, database.CreateReviewRun(&run))

	const workers = 12
	start := make(chan struct{})
	results := make(chan bool, workers)
	errors := make(chan error, workers)
	var group sync.WaitGroup
	for i := 0; i < workers; i++ {
		group.Add(1)
		go func(worker int) {
			defer group.Done()
			<-start
			claimed, claimErr := database.ClaimReviewRun(run.RunID, string(rune('a'+worker)), now, now.Add(time.Minute))
			results <- claimed
			errors <- claimErr
		}(i)
	}
	close(start)
	group.Wait()
	close(results)
	close(errors)

	winners := 0
	for claimed := range results {
		if claimed {
			winners++
		}
	}
	for claimErr := range errors {
		require.NoError(t, claimErr)
	}
	assert.Equal(t, 1, winners)
}

func TestGormDBReviewRunHistoryUsesRunIDTieBreaker(t *testing.T) {
	database := newTestDB(t)
	defer database.Close()

	now := time.Now().UTC().Truncate(time.Millisecond)
	first := reviewRunFixture("run-00000000000000000000000000000008", now)
	second := reviewRunFixture("run-00000000000000000000000000000009", now)
	require.NoError(t, database.CreateReviewRun(&first))
	completed := ReviewRunStatusCompleted
	require.NoError(t, database.PatchReviewRun(first.RunID, ReviewRunPatch{Status: &completed}))
	require.NoError(t, database.CreateReviewRun(&second))

	runs, err := database.ListReviewRuns(ReviewRunFilter{RepoOwner: "acme", RepoName: "widgets", PRNumber: 42})
	require.NoError(t, err)
	require.Len(t, runs, 2)
	assert.Equal(t, second.RunID, runs[0].RunID)
}

func TestGormDBMigratesReviewRunTables(t *testing.T) {
	database := newTestDB(t)
	defer database.Close()
	assert.True(t, database.db.Migrator().HasTable(&ReviewRunModel{}))
	assert.True(t, database.db.Migrator().HasTable(&ReviewStageAttemptModel{}))
	assert.True(t, database.db.Migrator().HasIndex(&ReviewRunModel{}, "idx_review_runs_status_lease"))
	assert.True(t, database.db.Migrator().HasIndex(&ReviewRunModel{}, "idx_review_runs_status_queue"))
	assert.True(t, database.db.Migrator().HasIndex(&ReviewRunModel{}, "idx_review_runs_one_live_per_pr"))
	assert.True(t, database.db.Migrator().HasColumn(&PRModel{}, "projection_run_id"))
}

func TestGormDBSkipMigrationsAddsMissingSQLiteProjectionColumn(t *testing.T) {
	path := filepath.Join(t.TempDir(), "skip-migrations.db")
	database, err := NewGormSQLite(path)
	require.NoError(t, err)
	require.NoError(t, database.db.Migrator().DropColumn(&PRModel{}, "ProjectionRunID"))
	assert.False(t, database.db.Migrator().HasColumn(&PRModel{}, "projection_run_id"))
	require.NoError(t, database.Close())

	t.Setenv("SKIP_DB_MIGRATIONS", "true")
	database, err = NewGormSQLite(path)
	require.NoError(t, err)
	defer database.Close()
	assert.True(t, database.db.Migrator().HasColumn(&PRModel{}, "projection_run_id"))
}

func TestGormDBOneLiveRunMigrationRepairsExistingRows(t *testing.T) {
	database := newTestDB(t)
	defer database.Close()
	require.NoError(t, database.db.Exec("DROP INDEX IF EXISTS idx_review_runs_one_live_per_pr").Error)

	now := time.Now().UTC().Truncate(time.Millisecond)
	older := reviewRunFixture("run-000000000000000000000000000000f1", now.Add(-time.Minute))
	newer := reviewRunFixture("run-000000000000000000000000000000f2", now)
	require.NoError(t, database.CreateReviewRun(&older))
	require.NoError(t, database.CreateReviewRun(&newer))
	require.NoError(t, database.SetPRGeneratingForReviewRun(
		newer.RepoOwner, newer.RepoName, newer.PRNumber, newer.CommitSHA,
		"Migration target", "alice", nil, false, newer.RunID,
	))
	require.NoError(t, database.db.Model(&PRModel{}).
		Where("repo_owner = ? AND repo_name = ? AND pr_number = ?", newer.RepoOwner, newer.RepoName, newer.PRNumber).
		UpdateColumn("projection_run_id", nil).Error)

	require.NoError(t, database.ensureIdempotentColumns())
	assert.True(t, database.db.Migrator().HasIndex(&ReviewRunModel{}, "idx_review_runs_one_live_per_pr"))
	loser, err := database.GetReviewRun(older.RunID)
	require.NoError(t, err)
	require.NotNil(t, loser)
	assert.Equal(t, ReviewRunStatusTimedOut, loser.Status)
	assert.Equal(t, "migration_deduped", loser.TerminalCode)
	assert.Equal(t, "migration", loser.FailureStage)
	require.NotNil(t, loser.CompletedAt)
	winner, err := database.GetReviewRun(newer.RunID)
	require.NoError(t, err)
	require.NotNil(t, winner)
	assert.Equal(t, ReviewRunStatusQueued, winner.Status)
	var pr PRModel
	require.NoError(t, database.db.Where(
		"repo_owner = ? AND repo_name = ? AND pr_number = ?", newer.RepoOwner, newer.RepoName, newer.PRNumber,
	).First(&pr).Error)
	assert.Empty(t, pr.ProjectionRunID)
}

func TestReviewStageAttemptUpsertCoversEveryMutableColumn(t *testing.T) {
	modelSchema, err := schema.Parse(&ReviewStageAttemptModel{}, &sync.Map{}, schema.NamingStrategy{})
	require.NoError(t, err)
	immutable := map[string]bool{
		"id": true, "run_id": true, "execution_attempt": true, "stage": true,
		"invocation_number": true, "attempt_number": true, "created_at": true,
	}
	mutable := make(map[string]bool, len(reviewStageAttemptMutableColumns))
	for _, column := range reviewStageAttemptMutableColumns {
		mutable[column] = true
	}
	for _, field := range modelSchema.Fields {
		if field.DBName == "" || immutable[field.DBName] {
			continue
		}
		assert.Truef(t, mutable[field.DBName], "column %s must be included in conflict updates or documented immutable", field.DBName)
		delete(mutable, field.DBName)
	}
	assert.Empty(t, mutable, "upsert update list contains unknown columns")
}
