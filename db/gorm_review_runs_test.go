package db

import (
	"errors"
	"fmt"
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

func claimedProjectedReviewRunFixture(t *testing.T, database *GormDB, runID, holder string, now time.Time) ReviewRunSuccessFinalization {
	t.Helper()
	run := reviewRunFixture(runID, now)
	require.NoError(t, database.CreateReviewRun(&run))
	require.NoError(t, database.SetPRGeneratingForReviewRun(
		run.RepoOwner, run.RepoName, run.PRNumber, run.CommitSHA,
		"Review me", "alice", nil, false, run.RunID,
	))
	claimed, err := database.ClaimReviewRun(run.RunID, holder, now, now.Add(time.Minute))
	require.NoError(t, err)
	require.True(t, claimed)

	return ReviewRunSuccessFinalization{
		RunID:                    run.RunID,
		Holder:                   holder,
		ExecutionAttempt:         1,
		LeaseCheckedAt:           now.Add(time.Second),
		CompletedAt:              now.Add(2 * time.Second),
		DurationMS:               2000,
		HTMLPath:                 "reviews/runs/" + run.RunID + "/review.html",
		JSONPath:                 "reviews/runs/" + run.RunID + "/review.json",
		CanonicalPath:            "acme_widgets_42_0123456.html",
		Critical:                 1,
		Medium:                   2,
		Low:                      3,
		Verdict:                  "request_changes",
		ModelFallback:            true,
		ServingModelVerification: "verified",
		ActualModelsJSON:         `[{"stage":"agent","model":"claude-opus-4-8"}]`,
		ReviewRunJSON:            `{"run_id":"` + run.RunID + `"}`,
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

func TestGormDBListReviewRunsCursorHasNoDuplicatesOrSkips(t *testing.T) {
	database := newTestDB(t)
	defer database.Close()

	base := time.Now().UTC().Truncate(time.Millisecond)
	fixtures := []struct {
		runID      string
		acceptedAt time.Time
	}{
		{"run-00000000000000000000000000000006", base},
		{"run-00000000000000000000000000000005", base},
		{"run-00000000000000000000000000000004", base},
		{"run-00000000000000000000000000000003", base.Add(-time.Minute)},
		{"run-00000000000000000000000000000002", base.Add(-time.Minute)},
		{"run-00000000000000000000000000000001", base.Add(-2 * time.Minute)},
	}
	completed := ReviewRunStatusCompleted
	for _, fixture := range fixtures {
		run := reviewRunFixture(fixture.runID, fixture.acceptedAt)
		run.IdempotencyScope, run.IdempotencyKeyHash = "", ""
		require.NoError(t, database.CreateReviewRun(&run))
		require.NoError(t, database.PatchReviewRun(run.RunID, ReviewRunPatch{Status: &completed}))
	}

	const pageSize = 2
	filter := ReviewRunFilter{
		RepoOwner: "acme",
		RepoName:  "widgets",
		PRNumber:  42,
		Limit:     pageSize + 1,
	}
	var got []string
	for {
		page, err := database.ListReviewRuns(filter)
		require.NoError(t, err)
		if len(page) == 0 {
			break
		}

		visible := page
		if len(visible) > pageSize {
			visible = visible[:pageSize]
		}
		for _, run := range visible {
			got = append(got, run.RunID)
		}
		if len(page) <= pageSize {
			break
		}
		last := visible[len(visible)-1]
		filter.BeforeAcceptedAt = last.AcceptedAt
		filter.BeforeRunID = last.RunID
	}

	want := make([]string, len(fixtures))
	for i, fixture := range fixtures {
		want[i] = fixture.runID
	}
	assert.Equal(t, want, got)
}

func TestGormDBListReviewRunsCursorComposesWithFilters(t *testing.T) {
	database := newTestDB(t)
	defer database.Close()

	base := time.Now().UTC().Truncate(time.Millisecond)
	const (
		cursorRunID = "run-00000000000000000000000000000050"
		targetSHA   = "0123456789abcdef0123456789abcdef01234567"
		otherSHA    = "89abcdef0123456789abcdef0123456789abcdef"
	)
	type fixture struct {
		runID      string
		acceptedAt time.Time
		owner      string
		repo       string
		prNumber   int
		commitSHA  string
		status     string
	}
	fixtures := []fixture{
		// Newer than the cursor tuple: excluded by the cursor.
		{"run-00000000000000000000000000000070", base.Add(time.Minute), "acme", "widgets", 42, targetSHA, ReviewRunStatusCompleted},
		{"run-00000000000000000000000000000060", base, "acme", "widgets", 42, targetSHA, ReviewRunStatusCompleted},
		// Older than the cursor tuple: these are the only expected rows.
		{"run-00000000000000000000000000000040", base, "acme", "widgets", 42, targetSHA, ReviewRunStatusCompleted},
		{"run-00000000000000000000000000000030", base.Add(-time.Minute), "acme", "widgets", 42, targetSHA, ReviewRunStatusCompleted},
		// Cursor-eligible rows excluded by each ordinary filter.
		{"run-00000000000000000000000000000025", base.Add(-time.Minute), "other", "widgets", 42, targetSHA, ReviewRunStatusCompleted},
		{"run-00000000000000000000000000000024", base.Add(-time.Minute), "acme", "other", 42, targetSHA, ReviewRunStatusCompleted},
		{"run-00000000000000000000000000000023", base.Add(-time.Minute), "acme", "widgets", 7, targetSHA, ReviewRunStatusCompleted},
		{"run-00000000000000000000000000000022", base.Add(-time.Minute), "acme", "widgets", 42, otherSHA, ReviewRunStatusCompleted},
		{"run-00000000000000000000000000000021", base.Add(-time.Minute), "acme", "widgets", 42, targetSHA, ReviewRunStatusFailed},
	}
	for _, fixture := range fixtures {
		run := reviewRunFixture(fixture.runID, fixture.acceptedAt)
		run.RepoOwner = fixture.owner
		run.RepoName = fixture.repo
		run.PRNumber = fixture.prNumber
		run.CommitSHA = fixture.commitSHA
		run.IdempotencyScope, run.IdempotencyKeyHash = "", ""
		require.NoError(t, database.CreateReviewRun(&run))
		require.NoError(t, database.PatchReviewRun(run.RunID, ReviewRunPatch{Status: &fixture.status}))
	}

	runs, err := database.ListReviewRuns(ReviewRunFilter{
		RepoOwner:        "acme",
		RepoName:         "widgets",
		PRNumber:         42,
		CommitSHA:        targetSHA,
		Status:           ReviewRunStatusCompleted,
		BeforeAcceptedAt: base,
		BeforeRunID:      cursorRunID,
		Limit:            10,
	})
	require.NoError(t, err)
	require.Len(t, runs, 2)
	assert.Equal(t, "run-00000000000000000000000000000040", runs[0].RunID)
	assert.Equal(t, "run-00000000000000000000000000000030", runs[1].RunID)
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
	updated, err = database.SetPRErrorIfNoLiveReview(owner, repo, prNum, "delayed admission failure")
	require.NoError(t, err)
	assert.False(t, updated, "an unfenced admission error must not overwrite a completed projection")

	pr, err := database.GetPR(owner, repo, prNum)
	require.NoError(t, err)
	require.NotNil(t, pr)
	assert.Equal(t, "completed", pr.Status)
	assert.Equal(t, "winner.html", pr.ReviewHTMLPath)
	assert.Equal(t, runB, pr.ReviewRunID)
}

func TestGormDBFinalizeReviewRunSuccessPublishesAtomicallyAndIsIdempotent(t *testing.T) {
	database := newTestDB(t)
	defer database.Close()

	now := time.Now().UTC().Truncate(time.Millisecond)
	input := claimedProjectedReviewRunFixture(
		t, database, "run-000000000000000000000000000000f1", "worker-a", now,
	)
	result, err := database.FinalizeReviewRunSuccess(input)
	require.NoError(t, err)
	assert.Equal(t, ReviewRunFinalizationResult{
		Finalized: true, Published: true, PublicationStatus: "published",
	}, result)

	run, err := database.GetReviewRun(input.RunID)
	require.NoError(t, err)
	require.NotNil(t, run)
	assert.Equal(t, ReviewRunStatusCompleted, run.Status)
	require.NotNil(t, run.CompletedAt)
	assert.Equal(t, input.CompletedAt, *run.CompletedAt)
	assert.Equal(t, input.DurationMS, run.DurationMS)
	assert.Equal(t, input.HTMLPath, run.HTMLPath)
	assert.Equal(t, input.JSONPath, run.JSONPath)
	assert.Equal(t, input.Critical, run.CriticalCount)
	assert.Equal(t, input.Medium, run.MediumCount)
	assert.Equal(t, input.Low, run.LowCount)
	assert.Equal(t, input.Verdict, run.Verdict)
	assert.Equal(t, input.ModelFallback, run.ModelFallback)
	assert.Equal(t, input.ServingModelVerification, run.ServingModelVerification)
	assert.Equal(t, input.ActualModelsJSON, run.ActualModelsJSON)
	assert.Equal(t, "published", run.PublicationStatus)
	assert.Equal(t, "success", run.TerminalCode)
	assert.Empty(t, run.LeaseHolder)
	assert.Nil(t, run.LeaseExpiresAt)

	pr, err := database.GetPR(run.RepoOwner, run.RepoName, run.PRNumber)
	require.NoError(t, err)
	require.NotNil(t, pr)
	assert.Equal(t, "completed", pr.Status)
	assert.Equal(t, input.CanonicalPath, pr.ReviewHTMLPath)
	assert.Equal(t, input.RunID, pr.ReviewRunID)
	assert.Equal(t, input.ReviewRunJSON, pr.ReviewRunJSON)
	assert.Equal(t, input.Critical, pr.CriticalCount)
	assert.Equal(t, input.Medium, pr.MediumCount)
	assert.Equal(t, input.Low, pr.LowCount)
	assert.Equal(t, input.Verdict, pr.ReviewVerdict)
	assert.Equal(t, input.ModelFallback, pr.ModelFallback)
	require.NotNil(t, pr.LastReviewedAt)
	assert.Equal(t, input.CompletedAt, *pr.LastReviewedAt)

	// A retry after an ambiguous commit response cannot satisfy the live-lease
	// predicate because finalization cleared the lease. It still returns the
	// already-stored outcome for this same execution attempt.
	input.LeaseCheckedAt = now.Add(3 * time.Second)
	retry, err := database.FinalizeReviewRunSuccess(input)
	require.NoError(t, err)
	assert.Equal(t, result, retry)
}

func TestGormDBFinalizeReviewRunSuccessRecordsSupersededProjection(t *testing.T) {
	database := newTestDB(t)
	defer database.Close()

	now := time.Now().UTC().Truncate(time.Millisecond)
	input := claimedProjectedReviewRunFixture(
		t, database, "run-000000000000000000000000000000f2", "worker-a", now,
	)
	const replacementRunID = "run-000000000000000000000000000000f3"
	require.NoError(t, database.SetPRGeneratingForReviewRun(
		"acme", "widgets", 42, "0123456789abcdef0123456789abcdef01234567",
		"Newer review", "alice", nil, false, replacementRunID,
	))

	result, err := database.FinalizeReviewRunSuccess(input)
	require.NoError(t, err)
	assert.Equal(t, ReviewRunFinalizationResult{
		Finalized: true, Published: false, PublicationStatus: "superseded",
	}, result)

	run, err := database.GetReviewRun(input.RunID)
	require.NoError(t, err)
	require.NotNil(t, run)
	assert.Equal(t, ReviewRunStatusCompleted, run.Status)
	assert.Equal(t, "success", run.TerminalCode)
	assert.Equal(t, "superseded", run.PublicationStatus)

	var projected PRModel
	require.NoError(t, database.db.Where(
		"repo_owner = ? AND repo_name = ? AND pr_number = ?", "acme", "widgets", 42,
	).First(&projected).Error)
	assert.Equal(t, replacementRunID, projected.ProjectionRunID)
	assert.Equal(t, "generating", projected.Status)
	assert.Empty(t, projected.ReviewRunID)

	input.LeaseCheckedAt = now.Add(3 * time.Second)
	retry, err := database.FinalizeReviewRunSuccess(input)
	require.NoError(t, err)
	assert.Equal(t, result, retry)
}

func TestGormDBFinalizeReviewRunSuccessRejectsStaleExecution(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*ReviewRunSuccessFinalization, time.Time)
	}{
		{
			name: "wrong holder",
			mutate: func(input *ReviewRunSuccessFinalization, _ time.Time) {
				input.Holder = "worker-b"
			},
		},
		{
			name: "wrong execution attempt",
			mutate: func(input *ReviewRunSuccessFinalization, _ time.Time) {
				input.ExecutionAttempt++
			},
		},
		{
			name: "expired lease",
			mutate: func(input *ReviewRunSuccessFinalization, now time.Time) {
				input.LeaseCheckedAt = now.Add(time.Minute)
			},
		},
	}
	for index, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			database := newTestDB(t)
			defer database.Close()

			now := time.Now().UTC().Truncate(time.Millisecond)
			input := claimedProjectedReviewRunFixture(
				t, database, fmt.Sprintf("run-000000000000000000000000000000e%d", index), "worker-a", now,
			)
			test.mutate(&input, now)
			result, err := database.FinalizeReviewRunSuccess(input)
			require.NoError(t, err)
			assert.Equal(t, ReviewRunFinalizationResult{}, result)

			run, err := database.GetReviewRun(input.RunID)
			require.NoError(t, err)
			require.NotNil(t, run)
			assert.Equal(t, ReviewRunStatusRunning, run.Status)
			assert.Equal(t, "worker-a", run.LeaseHolder)
			pr, err := database.GetPR(run.RepoOwner, run.RepoName, run.PRNumber)
			require.NoError(t, err)
			require.NotNil(t, pr)
			assert.Equal(t, "generating", pr.Status)
			assert.Empty(t, pr.ReviewRunID)
		})
	}
}

func TestGormDBFinalizeReviewRunSuccessRollsBackPRPublication(t *testing.T) {
	database := newTestDB(t)
	defer database.Close()

	now := time.Now().UTC().Truncate(time.Millisecond)
	input := claimedProjectedReviewRunFixture(
		t, database, "run-000000000000000000000000000000f4", "worker-a", now,
	)
	require.NoError(t, database.db.Exec(`CREATE TRIGGER fail_review_run_completion
		BEFORE UPDATE OF status ON review_runs
		WHEN NEW.status = 'completed'
		BEGIN
			SELECT RAISE(ABORT, 'forced terminalization failure');
		END`).Error)

	_, err := database.FinalizeReviewRunSuccess(input)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "forced terminalization failure")

	run, err := database.GetReviewRun(input.RunID)
	require.NoError(t, err)
	require.NotNil(t, run)
	assert.Equal(t, ReviewRunStatusRunning, run.Status)
	assert.Equal(t, "worker-a", run.LeaseHolder)
	assert.Empty(t, run.PublicationStatus)

	pr, err := database.GetPR(run.RepoOwner, run.RepoName, run.PRNumber)
	require.NoError(t, err)
	require.NotNil(t, pr)
	assert.Equal(t, "generating", pr.Status)
	assert.Empty(t, pr.ReviewRunID)
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

func TestGormDBTreatsGitHubTargetCasingAsOneLivePR(t *testing.T) {
	database := newTestDB(t)
	defer database.Close()

	first := reviewRunFixture("run-00000000000000000000000000000016", time.Now().UTC())
	first.RepoOwner, first.RepoName = "Acme", "Widgets"
	first.IdempotencyScope, first.IdempotencyKeyHash = "", ""
	require.NoError(t, database.CreateReviewRun(&first))

	second := reviewRunFixture("run-00000000000000000000000000000017", time.Now().UTC())
	second.RepoOwner, second.RepoName = "acme", "widgets"
	second.IdempotencyScope, second.IdempotencyKeyHash = "", ""
	err := database.CreateReviewRun(&second)
	require.ErrorIs(t, err, ErrReviewRunActiveConflict)

	runs, err := database.ListReviewRuns(ReviewRunFilter{RepoOwner: "ACME", RepoName: "WIDGETS", PRNumber: first.PRNumber})
	require.NoError(t, err)
	require.Len(t, runs, 1)
	assert.Equal(t, first.RunID, runs[0].RunID)
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
		BudgetUnitsUsed:      11,
		StartedAt:            &now,
	}
	require.NoError(t, database.UpsertReviewStageAttempt(&attempt))
	require.NotZero(t, attempt.ID)
	require.False(t, attempt.CreatedAt.IsZero())
	createdID := attempt.ID
	createdAt := attempt.CreatedAt

	attempt.AssistantTurns = 13
	attempt.BudgetUnitsUsed = 14
	attempt.StopReason = "complete"
	require.NoError(t, database.UpsertReviewStageAttempt(&attempt))
	assert.Equal(t, createdID, attempt.ID)
	assert.Equal(t, createdAt, attempt.CreatedAt)

	attempts, err := database.ListReviewStageAttempts(run.RunID)
	require.NoError(t, err)
	require.Len(t, attempts, 1)
	assert.Equal(t, 13, attempts[0].AssistantTurns)
	assert.Equal(t, 14, attempts[0].BudgetUnitsUsed)
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

func TestGormDBHolderWritesRejectLeaseExpiredByPostLockClock(t *testing.T) {
	database := newTestDB(t)
	defer database.Close()

	now := time.Now().UTC().Truncate(time.Millisecond)
	input := claimedProjectedReviewRunFixture(
		t, database, "run-000000000000000000000000000000f5", "worker-a", now,
	)
	expiredAt := time.Now().UTC().Add(-time.Second)
	require.NoError(t, database.db.Model(&ReviewRunModel{}).
		Where("run_id = ?", input.RunID).Update("lease_expires_at", expiredAt).Error)
	input.LeaseCheckedAt = now.Add(-time.Minute)

	result, err := database.FinalizeReviewRunSuccess(input)
	require.NoError(t, err)
	assert.Equal(t, ReviewRunFinalizationResult{}, result)
	attempt := ReviewStageAttempt{
		RunID: input.RunID, ExecutionAttempt: input.ExecutionAttempt,
		Stage: "agent", InvocationNumber: 1, AttemptNumber: 1,
		Provider: "anthropic", Status: "started",
	}
	accepted, err := database.UpsertReviewStageAttemptAsHolder(&attempt, input.Holder, now.Add(-time.Minute))
	require.NoError(t, err)
	assert.False(t, accepted)
}

func TestGormDBUpsertReviewStageAttemptAsHolderIsLeaseFenced(t *testing.T) {
	database := newTestDB(t)
	defer database.Close()

	now := time.Now().UTC().Truncate(time.Millisecond)
	finalization := claimedProjectedReviewRunFixture(
		t, database, "run-000000000000000000000000000000a1", "worker-a", now,
	)
	startedAt := now.Add(time.Second)
	attempt := ReviewStageAttempt{
		RunID:            finalization.RunID,
		ExecutionAttempt: finalization.ExecutionAttempt,
		Stage:            "agent",
		InvocationNumber: 1,
		AttemptNumber:    1,
		Provider:         "anthropic",
		RequestedModel:   "claude-fable-5",
		Status:           "started",
		StartedAt:        &startedAt,
	}

	accepted, err := database.UpsertReviewStageAttemptAsHolder(&attempt, "worker-b", now.Add(time.Second))
	require.NoError(t, err)
	assert.False(t, accepted)
	assert.Zero(t, attempt.ID)

	accepted, err = database.UpsertReviewStageAttemptAsHolder(&attempt, "worker-a", now.Add(time.Second))
	require.NoError(t, err)
	assert.True(t, accepted)
	require.NotZero(t, attempt.ID)
	createdID := attempt.ID
	createdAt := attempt.CreatedAt

	completedAt := now.Add(2 * time.Second)
	attempt.Status = "completed"
	attempt.CompletedAt = &completedAt
	attempt.DurationMS = 1000
	attempt.AssistantTurns = 8
	accepted, err = database.UpsertReviewStageAttemptAsHolder(&attempt, "worker-a", now.Add(2*time.Second))
	require.NoError(t, err)
	assert.True(t, accepted)
	assert.Equal(t, createdID, attempt.ID)
	assert.Equal(t, createdAt, attempt.CreatedAt)

	staleExecution := attempt
	staleExecution.ID = 0
	staleExecution.ExecutionAttempt++
	staleExecution.AttemptNumber++
	accepted, err = database.UpsertReviewStageAttemptAsHolder(&staleExecution, "worker-a", now.Add(2*time.Second))
	require.NoError(t, err)
	assert.False(t, accepted)
	assert.Zero(t, staleExecution.ID)

	expired := attempt
	expired.ID = 0
	expired.AttemptNumber += 2
	accepted, err = database.UpsertReviewStageAttemptAsHolder(&expired, "worker-a", now.Add(time.Minute))
	require.NoError(t, err)
	assert.False(t, accepted)
	assert.Zero(t, expired.ID)

	result, err := database.FinalizeReviewRunSuccess(finalization)
	require.NoError(t, err)
	require.True(t, result.Finalized)
	afterTerminal := attempt
	afterTerminal.ID = 0
	afterTerminal.AttemptNumber += 3
	accepted, err = database.UpsertReviewStageAttemptAsHolder(&afterTerminal, "worker-a", now.Add(3*time.Second))
	require.NoError(t, err)
	assert.False(t, accepted)
	assert.Zero(t, afterTerminal.ID)

	attempts, err := database.ListReviewStageAttempts(finalization.RunID)
	require.NoError(t, err)
	require.Len(t, attempts, 1)
	assert.Equal(t, "completed", attempts[0].Status)
	assert.Equal(t, 8, attempts[0].AssistantTurns)
}

func TestGormDBListReviewStageAttemptsUsesPipelineOrder(t *testing.T) {
	database := newTestDB(t)
	defer database.Close()

	now := time.Now().UTC().Truncate(time.Millisecond)
	run := reviewRunFixture("run-000000000000000000000000000000a2", now)
	require.NoError(t, database.CreateReviewRun(&run))

	fixtures := []ReviewStageAttempt{
		{RunID: run.RunID, ExecutionAttempt: 1, Stage: "agent", InvocationNumber: 1, AttemptNumber: 1},
		{RunID: run.RunID, ExecutionAttempt: 1, Stage: "summary", InvocationNumber: 1, AttemptNumber: 1},
		{RunID: run.RunID, ExecutionAttempt: 1, Stage: "custom", InvocationNumber: 1, AttemptNumber: 1},
		{RunID: run.RunID, ExecutionAttempt: 1, Stage: "classification_summary", InvocationNumber: 2, AttemptNumber: 1},
		{RunID: run.RunID, ExecutionAttempt: 1, Stage: "classification", InvocationNumber: 1, AttemptNumber: 1},
		{RunID: run.RunID, ExecutionAttempt: 1, Stage: "first_pass", InvocationNumber: 2, AttemptNumber: 1},
		{RunID: run.RunID, ExecutionAttempt: 1, Stage: "first_pass", InvocationNumber: 1, AttemptNumber: 2},
		{RunID: run.RunID, ExecutionAttempt: 1, Stage: "first_pass", InvocationNumber: 1, AttemptNumber: 1},
		{RunID: run.RunID, ExecutionAttempt: 2, Stage: "first_pass", InvocationNumber: 1, AttemptNumber: 1},
	}
	for index := range fixtures {
		fixtures[index].Status = "completed"
		fixtures[index].StartedAt = &now
		require.NoError(t, database.UpsertReviewStageAttempt(&fixtures[index]))
	}

	attempts, err := database.ListReviewStageAttempts(run.RunID)
	require.NoError(t, err)
	require.Len(t, attempts, len(fixtures))
	got := make([]string, 0, len(attempts))
	for _, attempt := range attempts {
		got = append(got, fmt.Sprintf("%d/%s/%d/%d", attempt.ExecutionAttempt, attempt.Stage, attempt.InvocationNumber, attempt.AttemptNumber))
	}
	assert.Equal(t, []string{
		"1/first_pass/1/1",
		"1/first_pass/1/2",
		"1/first_pass/2/1",
		"1/classification/1/1",
		"1/classification_summary/2/1",
		"1/summary/1/1",
		"1/agent/1/1",
		"1/custom/1/1",
		"2/first_pass/1/1",
	}, got)
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

func TestGormDBAbandonsExpiredRunsInBoundedBatches(t *testing.T) {
	database := newTestDB(t)
	defer database.Close()

	now := time.Now().UTC().Truncate(time.Millisecond)
	models := make([]ReviewRunModel, reviewRunAbandonBatchSize+1)
	for i := range models {
		run := reviewRunFixture(fmt.Sprintf("run-%032x", i+100), now.Add(-2*time.Hour))
		run.PRNumber = i + 1000
		run.IdempotencyScope = ""
		run.IdempotencyKeyHash = ""
		models[i] = reviewRunToModel(run)
	}
	// Keep each INSERT below SQLite's older 999-bind-parameter ceiling too.
	require.NoError(t, database.db.CreateInBatches(&models, 20).Error)

	count, err := database.AbandonExpiredReviewRuns(now, 2*time.Minute, time.Hour)
	require.NoError(t, err)
	assert.Equal(t, reviewRunAbandonBatchSize, count)
	var timedOut int64
	require.NoError(t, database.db.Model(&ReviewRunModel{}).Where("status = ?", ReviewRunStatusTimedOut).Count(&timedOut).Error)
	assert.Equal(t, int64(reviewRunAbandonBatchSize), timedOut)

	count, err = database.AbandonExpiredReviewRuns(now, 2*time.Minute, time.Hour)
	require.NoError(t, err)
	assert.Equal(t, 1, count)
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
	require.NoError(t, database.db.Exec("DROP INDEX IF EXISTS idx_review_runs_one_live_per_pr_ci").Error)

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
	assert.True(t, database.db.Migrator().HasIndex(&ReviewRunModel{}, "idx_review_runs_one_live_per_pr_ci"))
	assert.True(t, database.db.Migrator().HasIndex(&ReviewRunModel{}, "idx_review_runs_pr_history_ci"))
	assert.True(t, database.db.Migrator().HasIndex(&ReviewRunModel{}, "idx_review_runs_global_history"))
	assert.True(t, database.db.Migrator().HasColumn(&PRModel{}, "projection_run_id"))
	assert.True(t, database.db.Migrator().HasColumn(&ReviewStageAttemptModel{}, "budget_units_used"))
}

func TestGormDBSkipMigrationsAppliesIdempotentSchemaUpdates(t *testing.T) {
	path := filepath.Join(t.TempDir(), "skip-migrations.db")
	database, err := NewGormSQLite(path)
	require.NoError(t, err)
	require.NoError(t, database.db.Migrator().DropColumn(&PRModel{}, "ProjectionRunID"))
	require.NoError(t, database.db.Migrator().DropColumn(&ReviewStageAttemptModel{}, "BudgetUnitsUsed"))
	require.NoError(t, database.db.Exec("DROP INDEX IF EXISTS idx_prs_review_path").Error)
	assert.False(t, database.db.Migrator().HasColumn(&PRModel{}, "projection_run_id"))
	assert.False(t, database.db.Migrator().HasColumn(&ReviewStageAttemptModel{}, "budget_units_used"))
	assert.False(t, database.db.Migrator().HasIndex(&PRModel{}, "idx_prs_review_path"))
	require.NoError(t, database.Close())

	t.Setenv("SKIP_DB_MIGRATIONS", "true")
	database, err = NewGormSQLite(path)
	require.NoError(t, err)
	defer database.Close()
	assert.True(t, database.db.Migrator().HasColumn(&PRModel{}, "projection_run_id"))
	assert.True(t, database.db.Migrator().HasColumn(&ReviewStageAttemptModel{}, "budget_units_used"))
	assert.True(t, database.db.Migrator().HasIndex(&PRModel{}, "idx_prs_review_path"))
}

func TestGormDBOneLiveRunMigrationRepairsExistingRows(t *testing.T) {
	database := newTestDB(t)
	defer database.Close()
	require.NoError(t, database.db.Exec("DROP INDEX IF EXISTS idx_review_runs_one_live_per_pr").Error)
	require.NoError(t, database.db.Exec("DROP INDEX IF EXISTS idx_review_runs_one_live_per_pr_ci").Error)

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
	assert.True(t, database.db.Migrator().HasIndex(&ReviewRunModel{}, "idx_review_runs_one_live_per_pr_ci"))
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

func TestGormDBOneLiveRunMigrationPreservesActiveWorkerOverNewerOrphan(t *testing.T) {
	database := newTestDB(t)
	defer database.Close()
	require.NoError(t, database.db.Exec("DROP INDEX IF EXISTS idx_review_runs_one_live_per_pr").Error)
	require.NoError(t, database.db.Exec("DROP INDEX IF EXISTS idx_review_runs_one_live_per_pr_ci").Error)

	now := time.Now().UTC().Truncate(time.Millisecond)
	active := reviewRunFixture("run-000000000000000000000000000000f3", now.Add(-time.Minute))
	orphan := reviewRunFixture("run-000000000000000000000000000000f4", now)
	require.NoError(t, database.CreateReviewRun(&active))
	claimed, err := database.ClaimReviewRun(active.RunID, "worker-active", now, now.Add(time.Hour))
	require.NoError(t, err)
	require.True(t, claimed)
	require.NoError(t, database.CreateReviewRun(&orphan))

	require.NoError(t, database.ensureIdempotentColumns())
	survivor, err := database.GetReviewRun(active.RunID)
	require.NoError(t, err)
	require.NotNil(t, survivor)
	assert.Equal(t, ReviewRunStatusRunning, survivor.Status)
	assert.Equal(t, "worker-active", survivor.LeaseHolder)
	loser, err := database.GetReviewRun(orphan.RunID)
	require.NoError(t, err)
	require.NotNil(t, loser)
	assert.Equal(t, ReviewRunStatusTimedOut, loser.Status)
	assert.Equal(t, "migration_deduped", loser.TerminalCode)
	assert.True(t, database.db.Migrator().HasIndex(&ReviewRunModel{}, "idx_review_runs_one_live_per_pr"))
	assert.True(t, database.db.Migrator().HasIndex(&ReviewRunModel{}, "idx_review_runs_one_live_per_pr_ci"))
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
