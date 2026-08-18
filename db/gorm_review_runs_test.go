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

func TestGormDBAllowsMultipleRunsWithoutIdempotencyKey(t *testing.T) {
	database := newTestDB(t)
	defer database.Close()

	for _, id := range []string{
		"run-00000000000000000000000000000012",
		"run-00000000000000000000000000000013",
	} {
		run := reviewRunFixture(id, time.Now().UTC())
		run.IdempotencyScope = ""
		run.IdempotencyKeyHash = ""
		require.NoError(t, database.CreateReviewRun(&run))
	}
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

	count, err := database.AbandonExpiredReviewRuns(now, 2*time.Minute, time.Hour)
	require.NoError(t, err)
	assert.Equal(t, 1, count)

	abandoned, err := database.GetReviewRun(expired.RunID)
	require.NoError(t, err)
	require.NotNil(t, abandoned)
	assert.Equal(t, ReviewRunStatusTimedOut, abandoned.Status)
	assert.Equal(t, "lease_abandoned", abandoned.TerminalCode)
	assert.Equal(t, "dispatch", abandoned.FailureStage)
	assert.Empty(t, abandoned.LeaseHolder)
	assert.Nil(t, abandoned.LeaseExpiresAt)
	require.NotNil(t, abandoned.CompletedAt)
	assert.WithinDuration(t, now, *abandoned.CompletedAt, time.Millisecond)

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
