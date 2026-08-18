package db

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
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
}

func TestGormDBCreateReviewRunRejectsIncompleteLedgerEntry(t *testing.T) {
	database := newTestDB(t)
	defer database.Close()

	err := database.CreateReviewRun(&ReviewRun{RunID: "run-00000000000000000000000000000005"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "complete PR target")
}

func TestGormDBUpsertReviewStageAttempt(t *testing.T) {
	database := newTestDB(t)
	defer database.Close()

	now := time.Now().UTC().Truncate(time.Millisecond)
	run := reviewRunFixture("run-00000000000000000000000000000004", now)
	require.NoError(t, database.CreateReviewRun(&run))
	attempt := ReviewStageAttempt{
		RunID:                run.RunID,
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

	attempt.AssistantTurns = 13
	attempt.StopReason = "complete"
	require.NoError(t, database.UpsertReviewStageAttempt(&attempt))

	attempts, err := database.ListReviewStageAttempts(run.RunID)
	require.NoError(t, err)
	require.Len(t, attempts, 1)
	assert.Equal(t, 13, attempts[0].AssistantTurns)
	assert.Equal(t, []string{"claude-fable-5", "claude-opus-4-8"}, attempts[0].ObservedServedModels)
	assert.True(t, attempts[0].ServingModelVerified)
}

func TestGormDBMigratesReviewRunTables(t *testing.T) {
	database := newTestDB(t)
	defer database.Close()
	assert.True(t, database.db.Migrator().HasTable(&ReviewRunModel{}))
	assert.True(t, database.db.Migrator().HasTable(&ReviewStageAttemptModel{}))
}
