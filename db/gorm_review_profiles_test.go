package db

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestReviewRunProfileColumnRoundTrip(t *testing.T) {
	database := newTestDB(t)
	defer database.Close()

	now := time.Now().UTC().Truncate(time.Millisecond)
	run := reviewRunFixture("run-00000000000000000000000000000501", now)
	run.Profile = "lite_plus"
	require.NoError(t, database.CreateReviewRun(&run))
	loaded, err := database.GetReviewRun(run.RunID)
	require.NoError(t, err)
	require.NotNil(t, loaded)
	assert.Equal(t, "lite_plus", loaded.Profile)

	legacy := reviewRunFixture("run-00000000000000000000000000000502", now)
	legacy.IdempotencyKeyHash, legacy.IdempotencyScope = "", ""
	legacy.PRNumber = 43
	require.NoError(t, database.CreateReviewRun(&legacy))
	loaded, err = database.GetReviewRun(legacy.RunID)
	require.NoError(t, err)
	assert.Equal(t, "", loaded.Profile, "runs recorded without a profile stay empty rather than defaulting")
}

func TestStageAttemptCostUSDRoundTrip(t *testing.T) {
	database := newTestDB(t)
	defer database.Close()

	now := time.Now().UTC().Truncate(time.Millisecond)
	run := reviewRunFixture("run-00000000000000000000000000000503", now)
	require.NoError(t, database.CreateReviewRun(&run))
	attempt := ReviewStageAttempt{
		RunID: run.RunID, ExecutionAttempt: 1, Stage: "agent", InvocationNumber: 1, AttemptNumber: 1,
		Provider: "anthropic", Backend: "claude", RequestedModel: "claude-fable-5-1", Status: "completed",
		InputTokens: 1300, OutputTokens: 300, TotalTokens: 1600, CostUSD: 0.4321,
	}
	require.NoError(t, database.UpsertReviewStageAttempt(&attempt))
	attempt.CostUSD = 0.5
	require.NoError(t, database.UpsertReviewStageAttempt(&attempt))
	attempts, err := database.ListReviewStageAttempts(run.RunID)
	require.NoError(t, err)
	require.Len(t, attempts, 1)
	assert.InDelta(t, 0.5, attempts[0].CostUSD, 1e-9)
}

func TestEnsureIdempotentColumnsAddsProfileAndCost(t *testing.T) {
	database := newTestDB(t)
	defer database.Close()
	require.NoError(t, database.db.Migrator().DropColumn(&ReviewRunModel{}, "Profile"))
	require.NoError(t, database.db.Migrator().DropColumn(&ReviewStageAttemptModel{}, "CostUSD"))
	require.False(t, database.db.Migrator().HasColumn(&ReviewRunModel{}, "profile"))

	require.NoError(t, database.ensureIdempotentColumns())
	assert.True(t, database.db.Migrator().HasColumn(&ReviewRunModel{}, "profile"))
	assert.True(t, database.db.Migrator().HasColumn(&ReviewStageAttemptModel{}, "cost_usd"))
	require.NoError(t, database.ensureIdempotentColumns(), "re-running must be a no-op")
}

func TestReviewProfileStatsAggregatesRunsByProfile(t *testing.T) {
	database := newTestDB(t)
	defer database.Close()
	now := time.Now().UTC().Truncate(time.Millisecond)
	completed := ReviewRunStatusCompleted
	failed := ReviewRunStatusFailed
	create := func(runID, profile string, pr int, status *string, durationMS int64, cost float64, stop string) {
		run := reviewRunFixture(runID, now.Add(-time.Hour))
		run.Profile, run.PRNumber = profile, pr
		run.IdempotencyScope, run.IdempotencyKeyHash = "", ""
		require.NoError(t, database.CreateReviewRun(&run))
		require.NoError(t, database.PatchReviewRun(runID, ReviewRunPatch{Status: status, DurationMS: &durationMS}))
		require.NoError(t, database.UpsertReviewStageAttempt(&ReviewStageAttempt{
			RunID: runID, ExecutionAttempt: 1, Stage: "agent", InvocationNumber: 1, AttemptNumber: 1, Status: "completed", CostUSD: cost, StopReason: stop,
		}))
	}
	create("run-00000000000000000000000000000601", "lite", 1, &completed, 100000, 0.80, "completed")
	create("run-00000000000000000000000000000602", "lite", 2, &completed, 140000, 1.00, "completed")
	create("run-00000000000000000000000000000603", "lite", 3, &failed, 300000, 0.30, "wall_clock_timeout")
	create("run-00000000000000000000000000000604", "", 4, &completed, 500000, 2.90, "completed")
	queued := reviewRunFixture("run-00000000000000000000000000000605", now)
	queued.Profile, queued.PRNumber, queued.IdempotencyScope, queued.IdempotencyKeyHash = "lite", 5, "", ""
	require.NoError(t, database.CreateReviewRun(&queued))
	old := reviewRunFixture("run-00000000000000000000000000000606", now.Add(-48*time.Hour))
	old.Profile, old.PRNumber, old.IdempotencyScope, old.IdempotencyKeyHash = "lite", 6, "", ""
	require.NoError(t, database.CreateReviewRun(&old))
	require.NoError(t, database.PatchReviewRun(old.RunID, ReviewRunPatch{Status: &completed}))

	stats, err := database.ReviewProfileStats(now.Add(-24 * time.Hour))
	require.NoError(t, err)
	require.Contains(t, stats, "lite")
	require.Contains(t, stats, "full")
	lite := stats["lite"]
	assert.Equal(t, 3, lite.Runs, "queued and stale runs are excluded")
	assert.Equal(t, int64(140000), lite.P50DurationMS)
	assert.InDelta(t, 0.70, lite.MeanCostUSD, 1e-9)
	assert.Equal(t, 1, lite.Timeouts)
	assert.Equal(t, ReviewProfileStats{Runs: 1, P50DurationMS: 500000, MeanCostUSD: 2.90}, stats["full"], "legacy rows count as full")
}

func TestSummarizeReviewProfilesEmpty(t *testing.T) {
	assert.Empty(t, SummarizeReviewProfiles(nil))
}
