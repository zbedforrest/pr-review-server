package poller

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"pr-review-server/db"
	"pr-review-server/gcs"
	"pr-review-server/github"
	"pr-review-server/pkg/reviewer/payload"
	"pr-review-server/pkg/reviewer/runconfig"
	"pr-review-server/pkg/reviewer/service"
)

func reviewJobSnapshot(t *testing.T, effective runconfig.Effective) runconfig.Snapshot {
	t.Helper()
	snapshot, err := runconfig.Resolve(runconfig.Overrides{}, effective, runconfig.Policy{
		Backends: map[string]runconfig.BackendPolicy{
			effective.Agent.Backend: {
				Available: true,
				Models:    []string{effective.Agent.Model},
				Efforts:   []string{effective.Agent.Effort},
			},
		},
		MaxWallClockSeconds: effective.Agent.WallClockSeconds,
		MaxTurns:            effective.Agent.MaxTurns,
		MaxFirstPassSamples: effective.FirstPass.Samples,
	})
	require.NoError(t, err)
	return snapshot
}

func customReviewJob(t *testing.T, runID string) ReviewJob {
	t.Helper()
	snapshot := reviewJobSnapshot(t, runconfig.Effective{
		SchemaVersion: runconfig.SchemaVersion,
		Agent: runconfig.Agent{
			Enabled: true, Backend: service.AgentBackendOpenRouter, Model: service.DefaultOpenRouterAgentModel,
			Effort: "xhigh", WallClockSeconds: 73, MaxTurns: 19,
		},
		FirstPass:      runconfig.FirstPass{Samples: 4},
		RequiredChecks: true,
	})
	return ReviewJob{
		PR: github.PullRequest{
			Owner: "acme", Repo: "widgets", Number: 7,
			CommitSHA: "0123456789abcdef0123456789abcdef01234567", Title: "Review me", Author: "alice",
		},
		RunID: runID, Config: snapshot, TriggerSource: "api", Force: true,
	}
}

func reviewJobWithoutAgent(t *testing.T, runID string) ReviewJob {
	t.Helper()
	job := customReviewJob(t, runID)
	effective := job.Config.Effective
	effective.Agent.Enabled = false
	effective.Agent.WallClockSeconds = 0
	job.Config = reviewJobSnapshot(t, effective)
	return job
}

func waitForReviewJob(t *testing.T, p *Poller, job ReviewJob) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for p.IsReviewTracked(job.PR.Owner, job.PR.Repo, job.PR.Number) {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for review job %s", job.RunID)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func waitForReviewRunStatus(t *testing.T, database *MockDatabase, runID string, statuses ...string) *db.ReviewRun {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		run, err := database.GetReviewRun(runID)
		require.NoError(t, err)
		if run != nil {
			for _, status := range statuses {
				if run.Status == status {
					return run
				}
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for review run %s status in %v", runID, statuses)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestDefaultReviewJobSnapshotsDeploymentConfig(t *testing.T) {
	database := NewMockDatabase()
	database.ReviewNRequests = 5
	p := newTestPoller(NewMockGitHubClient(), database)
	p.cfg.AgenticReviews = true
	p.cfg.AgentBackend = service.AgentBackendClaude
	p.cfg.AgentModel = "claude-fable-5"
	p.cfg.AgentEffort = "high"
	p.cfg.AgentWallClockSec = 91
	p.cfg.AgentMaxTurns = 27
	p.cfg.RequiredChecks = true

	job, err := p.defaultReviewJob(github.PullRequest{
		Owner: "acme", Repo: "widgets", Number: 7, CommitSHA: "0123456789abcdef0123456789abcdef01234567",
	}, false, "poller")
	require.NoError(t, err)
	assert.Equal(t, "claude-fable-5", job.Config.Effective.Agent.Model)
	assert.Equal(t, 91, job.Config.Effective.Agent.WallClockSeconds)
	assert.Equal(t, 27, job.Config.Effective.Agent.MaxTurns)
	assert.Equal(t, 5, job.Config.Effective.FirstPass.Samples)
	assert.True(t, job.Config.Effective.RequiredChecks)
	assert.Equal(t, ReviewPipelineMargin+91*time.Second, reviewTimeout(job.Config.Effective))
}

func TestGenerateReviewsBatchSurfacesInvalidDefaultsForEveryPR(t *testing.T) {
	t.Setenv("OPENROUTER_API_KEY", "")
	database := NewMockDatabase()
	generator := NewMockReviewGenerator()
	p := newTestPollerFull(NewMockGitHubClient(), database, NewMockReviewStorage(), generator)
	p.cfg.AgenticReviews = true
	p.cfg.AgentBackend = service.AgentBackendOpenRouter
	p.cfg.AgentModel = service.DefaultOpenRouterAgentModel
	p.cfg.AgentEffort = "high"
	p.cfg.AgentWallClockSec = 30
	p.cfg.AgentMaxTurns = 5
	prs := []github.PullRequest{
		{Owner: "acme", Repo: "widgets", Number: 7, CommitSHA: "0123456789abcdef0123456789abcdef01234567"},
		{Owner: "acme", Repo: "widgets", Number: 8, CommitSHA: "1123456789abcdef0123456789abcdef01234567"},
	}
	for _, pr := range prs {
		require.NoError(t, database.UpsertPR(&db.PR{
			RepoOwner: pr.Owner, RepoName: pr.Repo, PRNumber: pr.Number,
			LastCommitSHA: pr.CommitSHA, Status: "pending",
		}))
	}

	err := p.generateReviewsBatch(context.Background(), prs, false)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "invalid deployment review defaults")
	for _, target := range prs {
		pr, getErr := database.GetPR(target.Owner, target.Repo, target.Number)
		require.NoError(t, getErr)
		require.NotNil(t, pr)
		assert.Equal(t, "error", pr.Status)
		assert.Contains(t, pr.ErrorMessage, "invalid deployment review defaults")
	}
	assert.Empty(t, generator.GenerateReviewCalls)
}

func TestProcessReviewJobPersistsConfigAndCompletesLedger(t *testing.T) {
	database := NewMockDatabase()
	storage := NewMockReviewStorage()
	generator := NewMockReviewGenerator()
	generator.SimulateDelay = 50 * time.Millisecond
	p := newTestPollerFull(NewMockGitHubClient(), database, storage, generator)
	job := customReviewJob(t, "run-10000000000000000000000000000001")
	require.NoError(t, database.UpsertPR(&db.PR{
		ID: 9, RepoOwner: job.PR.Owner, RepoName: job.PR.Repo, PRNumber: job.PR.Number,
		LastCommitSHA: job.PR.CommitSHA, Status: "generating", Title: job.PR.Title, Author: job.PR.Author,
	}))

	require.NoError(t, p.ProcessReviewJob(context.Background(), job))
	assert.True(t, p.IsReviewTracked(job.PR.Owner, job.PR.Repo, job.PR.Number))
	accepted, err := database.GetReviewRun(job.RunID)
	require.NoError(t, err)
	require.NotNil(t, accepted)
	assert.Contains(t, []string{db.ReviewRunStatusQueued, db.ReviewRunStatusRunning}, accepted.Status)
	waitForReviewJob(t, p, job)

	run, err := database.GetReviewRun(job.RunID)
	require.NoError(t, err)
	require.NotNil(t, run)
	assert.Equal(t, db.ReviewRunStatusCompleted, run.Status)
	assert.Equal(t, "published", run.PublicationStatus)
	assert.Equal(t, service.DefaultOpenRouterAgentModel, run.AgentModel)
	assert.Equal(t, 73, run.AgentWallClockSec)
	assert.Equal(t, 19, run.AgentMaxTurns)
	assert.Equal(t, 1, run.ExecutionAttempt)
	assert.Empty(t, run.LeaseHolder)
	assert.Nil(t, run.LeaseExpiresAt)

	generator.mu.Lock()
	require.Len(t, generator.GenerateReviewCalls, 1)
	assert.Equal(t, 4, generator.GenerateReviewCalls[0].NRequests)
	assert.Equal(t, job.RunID, generator.GenerateReviewCalls[0].RunID)
	assert.Equal(t, job.Config.Hash, generator.GenerateReviewCalls[0].Config.Hash)
	generator.mu.Unlock()

	pr, err := database.GetPR(job.PR.Owner, job.PR.Repo, job.PR.Number)
	require.NoError(t, err)
	require.NotNil(t, pr)
	assert.Equal(t, job.RunID, pr.ReviewRunID)
	var metadata payload.ReviewRunInfo
	require.NoError(t, json.Unmarshal([]byte(pr.ReviewRunJSON), &metadata))
	assert.Equal(t, 1, metadata.ExecutionAttempt)
	require.NotNil(t, metadata.Config)
	assert.Equal(t, job.Config.Hash, metadata.Config.Hash)
}

func TestProcessReviewJobRejectsBudgetBeyondStaleResetHorizon(t *testing.T) {
	database := NewMockDatabase()
	p := newTestPoller(NewMockGitHubClient(), database)
	p.cfg.AgentWallClockSec = 30
	job := customReviewJob(t, "run-12500000000000000000000000000001")

	err := p.ProcessReviewJob(context.Background(), job)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "exceeds deployment maximum")
	run, getErr := database.GetReviewRun(job.RunID)
	require.NoError(t, getErr)
	assert.Nil(t, run)
}

func TestProcessReviewJobOutlivesRequestContext(t *testing.T) {
	database := NewMockDatabase()
	p := newTestPollerFull(NewMockGitHubClient(), database, NewMockReviewStorage(), NewMockReviewGenerator())
	job := customReviewJob(t, "run-15000000000000000000000000000001")
	require.NoError(t, database.UpsertPR(&db.PR{
		RepoOwner: job.PR.Owner, RepoName: job.PR.Repo, PRNumber: job.PR.Number,
		LastCommitSHA: job.PR.CommitSHA, Status: "generating",
	}))
	requestCtx, cancelRequest := context.WithCancel(context.Background())
	cancelRequest()

	require.NoError(t, p.ProcessReviewJob(requestCtx, job))
	waitForReviewJob(t, p, job)
	run, err := database.GetReviewRun(job.RunID)
	require.NoError(t, err)
	require.NotNil(t, run)
	assert.Equal(t, db.ReviewRunStatusCompleted, run.Status)
}

func TestProcessReviewJobRejectsSecondActiveRun(t *testing.T) {
	database := NewMockDatabase()
	generator := NewMockReviewGenerator()
	generator.SimulateDelay = 200 * time.Millisecond
	p := newTestPollerFull(NewMockGitHubClient(), database, NewMockReviewStorage(), generator)
	first := customReviewJob(t, "run-20000000000000000000000000000001")
	second := customReviewJob(t, "run-20000000000000000000000000000002")
	require.NoError(t, database.UpsertPR(&db.PR{
		RepoOwner: first.PR.Owner, RepoName: first.PR.Repo, PRNumber: first.PR.Number,
		LastCommitSHA: first.PR.CommitSHA, Status: "generating",
	}))
	require.NoError(t, p.ProcessReviewJob(context.Background(), first))
	err := p.ProcessReviewJob(context.Background(), second)
	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrReviewAlreadyTracked))
	missing, getErr := database.GetReviewRun(second.RunID)
	require.NoError(t, getErr)
	assert.Nil(t, missing)
	waitForReviewJob(t, p, first)
}

func TestGenerateReviewJobRejectsRivalBeforeMutatingPRState(t *testing.T) {
	database := NewMockDatabase()
	p := newTestPollerFull(NewMockGitHubClient(), database, NewMockReviewStorage(), NewMockReviewGenerator())
	owner := customReviewJob(t, "run-22500000000000000000000000000001")
	rival := customReviewJob(t, "run-22500000000000000000000000000002")
	reviewCtx, tracked := p.tryTrackReviewJob(context.Background(), owner)
	require.True(t, tracked)
	require.NoError(t, reviewCtx.Err())
	require.NoError(t, database.UpsertPR(&db.PR{
		RepoOwner: owner.PR.Owner, RepoName: owner.PR.Repo, PRNumber: owner.PR.Number,
		LastCommitSHA: owner.PR.CommitSHA, Status: "agent_reviewing",
	}))

	require.NoError(t, p.generateReviewJobs(context.Background(), []ReviewJob{rival}))
	pr, err := database.GetPR(owner.PR.Owner, owner.PR.Repo, owner.PR.Number)
	require.NoError(t, err)
	require.NotNil(t, pr)
	assert.Equal(t, "agent_reviewing", pr.Status)
	run, err := database.GetReviewRun(rival.RunID)
	require.NoError(t, err)
	require.NotNil(t, run)
	assert.Equal(t, db.ReviewRunStatusCancelled, run.Status)
	assert.Equal(t, "pr_already_claimed", run.TerminalCode)
	assert.NoError(t, reviewCtx.Err(), "rejecting a rival must not cancel the owning run")
	p.untrackReviewRun(owner.PR.Owner, owner.PR.Repo, owner.PR.Number, owner.RunID)
}

func TestGenerateReviewJobsSkipsInvalidJobAndRunsValidSibling(t *testing.T) {
	database := NewMockDatabase()
	p := newTestPollerFull(NewMockGitHubClient(), database, NewMockReviewStorage(), NewMockReviewGenerator())
	invalid := customReviewJob(t, "run-22700000000000000000000000000001")
	invalid.PR.CommitSHA = ""
	valid := customReviewJob(t, "run-22700000000000000000000000000002")
	valid.PR.Number = 8
	require.NoError(t, database.UpsertPR(&db.PR{
		RepoOwner: valid.PR.Owner, RepoName: valid.PR.Repo, PRNumber: valid.PR.Number,
		LastCommitSHA: valid.PR.CommitSHA, Status: "pending",
	}))

	err := p.generateReviewJobs(context.Background(), []ReviewJob{invalid, valid})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "complete PR target")
	waitForReviewJob(t, p, valid)
	run, getErr := database.GetReviewRun(valid.RunID)
	require.NoError(t, getErr)
	require.NotNil(t, run)
	assert.Equal(t, db.ReviewRunStatusCompleted, run.Status)
}

func TestRunScopedCleanupCannotCancelReplacement(t *testing.T) {
	p := newTestPoller(NewMockGitHubClient(), NewMockDatabase())
	first := customReviewJob(t, "run-23000000000000000000000000000001")
	second := customReviewJob(t, "run-23000000000000000000000000000002")
	firstCtx, tracked := p.tryTrackReviewJob(context.Background(), first)
	require.True(t, tracked)
	adoptedCtx, adopted := p.trackOrAdoptReviewJob(context.Background(), first)
	require.True(t, adopted)
	assert.Equal(t, firstCtx, adoptedCtx)
	p.untrackReviewRun(first.PR.Owner, first.PR.Repo, first.PR.Number, first.RunID)
	secondCtx, tracked := p.tryTrackReviewJob(context.Background(), second)
	require.True(t, tracked)

	p.untrackReviewRun(first.PR.Owner, first.PR.Repo, first.PR.Number, first.RunID)
	assert.NoError(t, secondCtx.Err())
	assert.True(t, p.IsReviewTracked(second.PR.Owner, second.PR.Repo, second.PR.Number))
	p.untrackReviewRun(second.PR.Owner, second.PR.Repo, second.PR.Number, second.RunID)
}

func TestQueuedReviewBudgetStartsAtExecution(t *testing.T) {
	p := newTestPoller(NewMockGitHubClient(), NewMockDatabase())
	job := customReviewJob(t, "run-24000000000000000000000000000001")
	queuedCtx, tracked := p.tryTrackReviewJob(context.Background(), job)
	require.True(t, tracked)
	_, hasQueuedDeadline := queuedCtx.Deadline()
	assert.False(t, hasQueuedDeadline)
	p.reviewsMutex.Lock()
	queuedInfo := p.activeReviews[prKey(job.PR.Owner, job.PR.Repo, job.PR.Number)]
	p.reviewsMutex.Unlock()
	assert.True(t, queuedInfo.StartTime.IsZero())

	runCtx, started := p.startTrackedReviewJob(job)
	require.True(t, started)
	deadline, hasRunDeadline := runCtx.Deadline()
	require.True(t, hasRunDeadline)
	assert.WithinDuration(t, time.Now().Add(reviewTimeout(job.Config.Effective)), deadline, time.Second)
	p.reviewsMutex.Lock()
	runningInfo := p.activeReviews[prKey(job.PR.Owner, job.PR.Repo, job.PR.Number)]
	p.reviewsMutex.Unlock()
	assert.False(t, runningInfo.StartTime.IsZero())
	assert.Equal(t, reviewTimeout(job.Config.Effective), runningInfo.Timeout)
	p.untrackReviewRun(job.PR.Owner, job.PR.Repo, job.PR.Number, job.RunID)
}

func TestStartedReviewKeysExcludeQueuedJobs(t *testing.T) {
	p := newTestPoller(NewMockGitHubClient(), NewMockDatabase())
	queued := customReviewJob(t, "run-24100000000000000000000000000001")
	_, tracked := p.tryTrackReviewJob(context.Background(), queued)
	require.True(t, tracked)

	runningKey := prKey("acme", "running", 8)
	p.trackReviewWithTimeout(context.Background(), "acme", "running", 8, 0,
		"run-24100000000000000000000000000002", time.Minute)

	started := p.startedReviewKeys()
	assert.NotContains(t, started, prKey(queued.PR.Owner, queued.PR.Repo, queued.PR.Number))
	assert.Equal(t, "run-24100000000000000000000000000002", started[runningKey])

	p.untrackReviewRun(queued.PR.Owner, queued.PR.Repo, queued.PR.Number, queued.RunID)
	p.untrackReviewRun("acme", "running", 8, "run-24100000000000000000000000000002")
}

func TestShouldReviewSkipsPendingQueuedJob(t *testing.T) {
	pr := github.PullRequest{Number: 7, CommitSHA: "0123456789abcdef0123456789abcdef01234567"}
	dbPR := &db.PR{Status: "pending", LastCommitSHA: pr.CommitSHA}
	assert.False(t, shouldReview(pr, dbPR, true, true))
	assert.True(t, shouldReview(pr, dbPR, false, true))
}

func TestProviderInitFailureRejectsAcceptedRunAndProjectsError(t *testing.T) {
	database := NewMockDatabase()
	p := newTestPoller(NewMockGitHubClient(), database)
	job := customReviewJob(t, "run-24300000000000000000000000000001")
	require.NoError(t, database.UpsertPR(&db.PR{
		RepoOwner: job.PR.Owner, RepoName: job.PR.Repo, PRNumber: job.PR.Number,
		LastCommitSHA: job.PR.CommitSHA, Status: "generating",
	}))
	_, tracked := p.tryTrackReviewJob(context.Background(), job)
	require.True(t, tracked)

	p.rejectProviderInitJobs([]ReviewJob{job}, errors.New("provider temporarily unavailable"))

	run, err := database.GetReviewRun(job.RunID)
	require.NoError(t, err)
	require.NotNil(t, run)
	assert.Equal(t, db.ReviewRunStatusFailed, run.Status)
	assert.Equal(t, "provider_init_failed", run.TerminalCode)
	assert.Equal(t, "dispatch", run.FailureStage)
	assert.Empty(t, run.LeaseHolder)
	assert.Nil(t, run.LeaseExpiresAt)
	pr, err := database.GetPR(job.PR.Owner, job.PR.Repo, job.PR.Number)
	require.NoError(t, err)
	require.NotNil(t, pr)
	assert.Equal(t, "error", pr.Status)
	assert.Contains(t, pr.ErrorMessage, "provider temporarily unavailable")
	assert.Empty(t, database.ProjectionRunIDs[prDBKey(job.PR.Owner, job.PR.Repo, job.PR.Number)])
	assert.False(t, p.IsReviewTracked(job.PR.Owner, job.PR.Repo, job.PR.Number))
}

func TestProviderInitFailureDoesNotCreateRunForAutomaticCandidate(t *testing.T) {
	database := NewMockDatabase()
	p := newTestPoller(NewMockGitHubClient(), database)
	job := customReviewJob(t, "run-24300000000000000000000000000002")
	job.TriggerSource = "poller"
	require.NoError(t, database.UpsertPR(&db.PR{
		RepoOwner: job.PR.Owner, RepoName: job.PR.Repo, PRNumber: job.PR.Number,
		LastCommitSHA: job.PR.CommitSHA, Status: "pending",
	}))
	_, tracked := p.tryTrackReviewJob(context.Background(), job)
	require.True(t, tracked)

	p.rejectProviderInitJobs([]ReviewJob{job}, errors.New("provider temporarily unavailable"))

	run, err := database.GetReviewRun(job.RunID)
	require.NoError(t, err)
	assert.Nil(t, run)
	pr, err := database.GetPR(job.PR.Owner, job.PR.Repo, job.PR.Number)
	require.NoError(t, err)
	require.NotNil(t, pr)
	assert.Equal(t, "pending", pr.Status)
	assert.False(t, p.IsReviewTracked(job.PR.Owner, job.PR.Repo, job.PR.Number))
}

func TestAcceptedQueuedRunLeaseIsRenewedAndCrashExpiresQuickly(t *testing.T) {
	database := NewMockDatabase()
	p := newTestPoller(NewMockGitHubClient(), database)
	job := customReviewJob(t, "run-24300000000000000000000000000004")
	_, tracked := p.tryTrackReviewJob(context.Background(), job)
	require.True(t, tracked)
	holder := newHolderID()
	now := time.Now().UTC()
	require.NoError(t, p.ensureReviewRunWithQueueLease(job, holder, now.Add(ReviewQueueLeaseTTL)))
	created, err := database.GetReviewRun(job.RunID)
	require.NoError(t, err)
	require.NotNil(t, created)
	assert.Equal(t, holder, created.LeaseHolder)
	require.NotNil(t, created.LeaseExpiresAt)
	leased, err := database.ClaimOrRenewQueuedReviewRunLease(job.RunID, holder, now, now.Add(ReviewQueueLeaseTTL))
	require.NoError(t, err)
	require.True(t, leased)
	require.True(t, p.setTrackedQueueLease(job, holder))

	p.renewTrackedQueueLeases(now.Add(time.Minute))
	run, err := database.GetReviewRun(job.RunID)
	require.NoError(t, err)
	require.NotNil(t, run)
	require.NotNil(t, run.LeaseExpiresAt)
	assert.WithinDuration(t, now.Add(time.Minute+ReviewQueueLeaseTTL), *run.LeaseExpiresAt, time.Second)

	p.untrackReviewRun(job.PR.Owner, job.PR.Repo, job.PR.Number, job.RunID)
	abandoned, err := database.AbandonExpiredReviewRuns(now.Add(time.Minute+ReviewQueueLeaseTTL+ReviewLeaseCompletionGrace+time.Second), ReviewLeaseCompletionGrace, ReviewQueueAbandonAfter)
	require.NoError(t, err)
	assert.Equal(t, 1, abandoned)
	run, err = database.GetReviewRun(job.RunID)
	require.NoError(t, err)
	require.NotNil(t, run)
	assert.Equal(t, db.ReviewRunStatusTimedOut, run.Status)
	assert.Equal(t, "queue_abandoned", run.TerminalCode)
}

func TestLostQueuedDispatcherLeaseCancelsOnlyMatchingLocalOwner(t *testing.T) {
	database := NewMockDatabase()
	p := newTestPoller(NewMockGitHubClient(), database)
	job := customReviewJob(t, "run-24300000000000000000000000000007")
	job.QueueLeaseHolder = "dispatcher-old"
	queuedCtx, tracked := p.tryTrackReviewJob(context.Background(), job)
	require.True(t, tracked)
	require.True(t, p.setTrackedQueueLease(job, job.QueueLeaseHolder))
	require.NoError(t, p.ensureReviewRunWithQueueLease(job, "dispatcher-new", time.Now().Add(ReviewQueueLeaseTTL)))

	p.renewTrackedQueueLeases(time.Now().UTC())

	assert.ErrorIs(t, queuedCtx.Err(), context.Canceled)
	assert.False(t, p.IsReviewTracked(job.PR.Owner, job.PR.Repo, job.PR.Number))
	run, err := database.GetReviewRun(job.RunID)
	require.NoError(t, err)
	require.NotNil(t, run)
	assert.Equal(t, db.ReviewRunStatusQueued, run.Status)
	assert.Equal(t, "dispatcher-new", run.LeaseHolder)
	assert.False(t, p.rejectQueuedReviewJob(job, db.ReviewRunStatusCancelled, "cancelled", "dispatch", context.Canceled))
	run, err = database.GetReviewRun(job.RunID)
	require.NoError(t, err)
	require.NotNil(t, run)
	assert.Equal(t, db.ReviewRunStatusQueued, run.Status, "stale dispatcher must not cancel its successor")
}

func TestAutomaticCacheRecoveryRepairsTerminalProjectionWithoutLedgerChurn(t *testing.T) {
	database := NewMockDatabase()
	storage := NewMockReviewStorage()
	p := newTestPollerFull(NewMockGitHubClient(), database, storage, NewMockReviewGenerator())
	job := customReviewJob(t, "run-24300000000000000000000000000006")
	job.TriggerSource = "poller"
	job.Force = false
	storage.ExistingReviews[fmt.Sprintf("%s/%s/%d/%s", job.PR.Owner, job.PR.Repo, job.PR.Number, job.PR.CommitSHA)] = true
	terminalOwner := customReviewJob(t, "run-24300000000000000000000000000005")
	require.NoError(t, p.ensureReviewRun(terminalOwner))
	require.NoError(t, database.SetPRGeneratingForReviewRun(
		job.PR.Owner, job.PR.Repo, job.PR.Number, job.PR.CommitSHA, job.PR.Title, job.PR.Author, job.PR.CreatedAt, job.PR.Draft, terminalOwner.RunID,
	))
	staleGeneratingSince := time.Now().Add(-ReviewQueueLeaseTTL - time.Second)
	database.PRs[prDBKey(job.PR.Owner, job.PR.Repo, job.PR.Number)].GeneratingSince = &staleGeneratingSince
	failed := db.ReviewRunStatusFailed
	require.NoError(t, database.PatchReviewRun(terminalOwner.RunID, db.ReviewRunPatch{Status: &failed}))

	require.NoError(t, p.generateReviewJobs(context.Background(), []ReviewJob{job}))

	pr, err := database.GetPR(job.PR.Owner, job.PR.Repo, job.PR.Number)
	require.NoError(t, err)
	require.NotNil(t, pr)
	assert.Equal(t, "completed", pr.Status)
	run, err := database.GetReviewRun(job.RunID)
	require.NoError(t, err)
	assert.Nil(t, run, "an unaccepted automatic cache hit must not create a ledger row")
}

func TestUnclaimedTerminalRunReleasesGeneratingProjection(t *testing.T) {
	database := NewMockDatabase()
	p := newTestPollerFull(NewMockGitHubClient(), database, NewMockReviewStorage(), NewMockReviewGenerator())
	job := reviewJobWithoutAgent(t, "run-24300000000000000000000000000003")
	require.NoError(t, database.UpsertPR(&db.PR{
		RepoOwner: job.PR.Owner, RepoName: job.PR.Repo, PRNumber: job.PR.Number,
		LastCommitSHA: job.PR.CommitSHA, Status: "pending",
	}))
	require.NoError(t, p.ensureReviewRun(job))
	terminal := db.ReviewRunStatusTimedOut
	require.NoError(t, database.PatchReviewRun(job.RunID, db.ReviewRunPatch{Status: &terminal}))

	require.NoError(t, p.generateReviewJobs(context.Background(), []ReviewJob{job}))

	pr, err := database.GetPR(job.PR.Owner, job.PR.Repo, job.PR.Number)
	require.NoError(t, err)
	require.NotNil(t, pr)
	assert.Equal(t, "error", pr.Status)
	assert.Contains(t, pr.ErrorMessage, ErrReviewRunNotClaimed.Error())
}

func TestQueuedCacheLookupHasIndependentTimeout(t *testing.T) {
	storage := NewMockReviewStorage()
	storage.ReviewExistsFunc = func(ctx context.Context, _ string, _ string, _ int, _ string) (bool, error) {
		<-ctx.Done()
		return false, ctx.Err()
	}
	p := newTestPollerWithStorage(NewMockGitHubClient(), NewMockDatabase(), storage)
	started := time.Now()
	exists, err := p.reviewExistsWithTimeout(context.Background(), 20*time.Millisecond, "acme", "widgets", 7, "abc1234")
	assert.False(t, exists)
	assert.ErrorIs(t, err, context.DeadlineExceeded)
	assert.Less(t, time.Since(started), 500*time.Millisecond)
}

func TestCacheRestoreUsesExactCommitSidecarMetadata(t *testing.T) {
	database := NewMockDatabase()
	storage := NewMockReviewStorage()
	p := newTestPollerFull(NewMockGitHubClient(), database, storage, NewMockReviewGenerator())
	p.reviewDir = t.TempDir()
	job := customReviewJob(t, "run-24400000000000000000000000000001")
	job.Force = false
	storage.ExistingReviews[fmt.Sprintf("%s/%s/%d/%s", job.PR.Owner, job.PR.Repo, job.PR.Number, job.PR.CommitSHA)] = true
	artifactRunID := "run-24400000000000000000000000000000"
	sidecar := payload.Payload{
		SchemaVersion: "1", Owner: job.PR.Owner, Repo: job.PR.Repo,
		PRNumber: job.PR.Number, CommitSHA: job.PR.CommitSHA,
		Counts:   payload.Counts{Critical: 2, Medium: 3, Low: 4},
		Findings: []payload.Finding{{Severity: "medium", File: "SUMMARY", Comment: "**Verdict: approve with suggestions.**"}},
		ReviewRun: &payload.ReviewRunInfo{
			RunID:  artifactRunID,
			Models: []payload.ModelUse{{Stage: "agent", RequestedModel: "requested", ServedModel: "fallback", Fallback: true}},
		},
	}
	body, err := json.Marshal(sidecar)
	require.NoError(t, err)
	sidecarName := gcs.ReviewJSONFileName(gcs.ReviewFileName(job.PR.Owner, job.PR.Repo, job.PR.Number, job.PR.CommitSHA))
	require.NoError(t, os.WriteFile(filepath.Join(p.reviewDir, sidecarName), body, 0600))
	require.NoError(t, database.UpsertPR(&db.PR{
		RepoOwner: job.PR.Owner, RepoName: job.PR.Repo, PRNumber: job.PR.Number,
		LastCommitSHA: "different-commit", Status: "pending", CriticalCount: 99,
		ReviewVerdict: "request_changes", ReviewRunID: "wrong-run",
	}))

	require.NoError(t, p.ProcessReviewJob(context.Background(), job))
	waitForReviewJob(t, p, job)
	pr, err := database.GetPR(job.PR.Owner, job.PR.Repo, job.PR.Number)
	require.NoError(t, err)
	require.NotNil(t, pr)
	assert.Equal(t, job.PR.CommitSHA, pr.LastCommitSHA)
	assert.Equal(t, 2, pr.CriticalCount)
	assert.Equal(t, 3, pr.MediumCount)
	assert.Equal(t, 4, pr.LowCount)
	assert.Equal(t, "approve_suggestions", pr.ReviewVerdict)
	assert.True(t, pr.ModelFallback)
	assert.Equal(t, artifactRunID, pr.ReviewRunID)
	assert.Contains(t, pr.ReviewRunJSON, artifactRunID)
	run, err := database.GetReviewRun(job.RunID)
	require.NoError(t, err)
	require.NotNil(t, run)
	assert.Equal(t, db.ReviewRunStatusCancelled, run.Status)
	assert.Equal(t, "review_cached", run.TerminalCode)
}

func TestMonitorReapsAbandonedQueuedTracking(t *testing.T) {
	p := newTestPoller(NewMockGitHubClient(), NewMockDatabase())
	job := customReviewJob(t, "run-24200000000000000000000000000001")
	queuedCtx, queuedCancel := context.WithCancel(context.Background())
	key := prKey(job.PR.Owner, job.PR.Repo, job.PR.Number)
	p.activeReviews[key] = ProcessInfo{
		TrackedAt: time.Now().Add(-ReviewQueueAbandonAfter - time.Minute),
		Timeout:   reviewTimeout(job.Config.Effective), RunID: job.RunID,
		Ctx: queuedCtx, Cancel: queuedCancel,
	}
	monitorCtx, stopMonitor := context.WithCancel(context.Background())
	ticker := time.NewTicker(5 * time.Millisecond)
	defer ticker.Stop()
	defer stopMonitor()
	go p.monitorReviewerProcesses(monitorCtx, ticker)

	require.Eventually(t, func() bool {
		return !p.IsReviewTracked(job.PR.Owner, job.PR.Repo, job.PR.Number)
	}, time.Second, 5*time.Millisecond)
	assert.ErrorIs(t, queuedCtx.Err(), context.Canceled)
}

func TestAgentSlotWaitDoesNotStartBudgetOrExecutionLease(t *testing.T) {
	database := NewMockDatabase()
	generator := NewMockReviewGenerator()
	p := newTestPollerFull(NewMockGitHubClient(), database, NewMockReviewStorage(), generator)
	p.agentSlots = make(chan struct{}, 1)
	p.agentSlots <- struct{}{} // occupy the only agent slot
	job := customReviewJob(t, "run-24500000000000000000000000000001")
	require.NoError(t, database.UpsertPR(&db.PR{
		RepoOwner: job.PR.Owner, RepoName: job.PR.Repo, PRNumber: job.PR.Number,
		LastCommitSHA: job.PR.CommitSHA, Status: "generating",
	}))

	require.NoError(t, p.ProcessReviewJob(context.Background(), job))
	run, err := database.GetReviewRun(job.RunID)
	require.NoError(t, err)
	require.NotNil(t, run)
	assert.Equal(t, db.ReviewRunStatusQueued, run.Status)
	assert.NotEmpty(t, run.LeaseHolder)
	require.NotNil(t, run.LeaseExpiresAt, "queued acceptance must have a renewable dispatcher lease")
	assert.True(t, run.LeaseExpiresAt.After(time.Now()))
	p.reviewsMutex.Lock()
	queuedInfo := p.activeReviews[prKey(job.PR.Owner, job.PR.Repo, job.PR.Number)]
	p.reviewsMutex.Unlock()
	assert.True(t, queuedInfo.StartTime.IsZero())
	_, hasDeadline := queuedInfo.Ctx.Deadline()
	assert.False(t, hasDeadline)

	<-p.agentSlots // allow execution to acquire the reserved slot
	waitForReviewJob(t, p, job)
	run, err = database.GetReviewRun(job.RunID)
	require.NoError(t, err)
	require.NotNil(t, run)
	assert.Equal(t, db.ReviewRunStatusCompleted, run.Status)
}

func TestProcessReviewJobPersistsTerminalFailure(t *testing.T) {
	database := NewMockDatabase()
	generator := NewMockReviewGenerator()
	job := customReviewJob(t, "run-25000000000000000000000000000001")
	generator.Results["acme/widgets/7"] = struct {
		Result *ReviewResult
		Err    error
	}{Err: errors.New("provider unavailable")}
	p := newTestPollerFull(NewMockGitHubClient(), database, NewMockReviewStorage(), generator)
	require.NoError(t, database.UpsertPR(&db.PR{
		RepoOwner: job.PR.Owner, RepoName: job.PR.Repo, PRNumber: job.PR.Number,
		LastCommitSHA: job.PR.CommitSHA, Status: "generating",
	}))

	require.NoError(t, p.ProcessReviewJob(context.Background(), job))
	waitForReviewJob(t, p, job)
	run, err := database.GetReviewRun(job.RunID)
	require.NoError(t, err)
	require.NotNil(t, run)
	assert.Equal(t, db.ReviewRunStatusFailed, run.Status)
	assert.Equal(t, "review_failed", run.TerminalCode)
	assert.Equal(t, "generation", run.FailureStage)
	assert.Contains(t, run.ErrorSummary, "provider unavailable")
	assert.Empty(t, run.LeaseHolder)
	assert.Nil(t, run.LeaseExpiresAt)
}

func TestOrganicGenerationTimeoutProjectsBoundedPRError(t *testing.T) {
	database := NewMockDatabase()
	generator := NewMockReviewGenerator()
	generator.SimulateDelay = 100 * time.Millisecond
	p := newTestPollerFull(NewMockGitHubClient(), database, NewMockReviewStorage(), generator)
	p.reviewPipelineMargin = 20 * time.Millisecond
	job := reviewJobWithoutAgent(t, "run-25500000000000000000000000000001")
	require.NoError(t, database.UpsertPR(&db.PR{
		RepoOwner: job.PR.Owner, RepoName: job.PR.Repo, PRNumber: job.PR.Number,
		LastCommitSHA: job.PR.CommitSHA, Status: "generating",
	}))

	require.NoError(t, p.ProcessReviewJob(context.Background(), job))
	waitForReviewJob(t, p, job)
	run, err := database.GetReviewRun(job.RunID)
	require.NoError(t, err)
	require.NotNil(t, run)
	assert.Equal(t, db.ReviewRunStatusTimedOut, run.Status)
	assert.Equal(t, "run_timeout", run.TerminalCode)
	assert.Equal(t, "execution", run.FailureStage)
	assert.Contains(t, run.ErrorSummary, "wall-clock budget exceeded")
	pr, err := database.GetPR(job.PR.Owner, job.PR.Repo, job.PR.Number)
	require.NoError(t, err)
	require.NotNil(t, pr)
	assert.Equal(t, "error", pr.Status)
	assert.Equal(t, reviewBudgetExceededMessage, pr.ErrorMessage)
}

func TestOrganicArtifactTimeoutProjectsBoundedPRError(t *testing.T) {
	database := NewMockDatabase()
	storage := NewMockReviewStorage()
	storage.SaveReviewFunc = func(ctx context.Context, _ string, _ string, _ int, _ string, _ []byte) (string, error) {
		<-ctx.Done()
		return "", ctx.Err()
	}
	p := newTestPollerFull(NewMockGitHubClient(), database, storage, NewMockReviewGenerator())
	p.reviewPipelineMargin = 50 * time.Millisecond
	job := reviewJobWithoutAgent(t, "run-25700000000000000000000000000001")
	require.NoError(t, database.UpsertPR(&db.PR{
		RepoOwner: job.PR.Owner, RepoName: job.PR.Repo, PRNumber: job.PR.Number,
		LastCommitSHA: job.PR.CommitSHA, Status: "generating",
	}))

	require.NoError(t, p.ProcessReviewJob(context.Background(), job))
	waitForReviewJob(t, p, job)
	run, err := database.GetReviewRun(job.RunID)
	require.NoError(t, err)
	require.NotNil(t, run)
	assert.Equal(t, db.ReviewRunStatusTimedOut, run.Status)
	assert.Equal(t, "run_timeout", run.TerminalCode)
	assert.Equal(t, "artifact_save", run.FailureStage)
	pr, err := database.GetPR(job.PR.Owner, job.PR.Repo, job.PR.Number)
	require.NoError(t, err)
	require.NotNil(t, pr)
	assert.Equal(t, "error", pr.Status)
	assert.Equal(t, reviewBudgetExceededMessage, pr.ErrorMessage)
}

func TestOrganicTimeoutCannotClobberSuccessorRun(t *testing.T) {
	database := NewMockDatabase()
	p := newTestPoller(NewMockGitHubClient(), database)
	job := reviewJobWithoutAgent(t, "run-25800000000000000000000000000001")
	require.NoError(t, database.UpsertPR(&db.PR{
		RepoOwner: job.PR.Owner, RepoName: job.PR.Repo, PRNumber: job.PR.Number,
		LastCommitSHA: job.PR.CommitSHA, Status: "agent_reviewing",
	}))
	execution, err := p.beginReviewExecution(job)
	require.NoError(t, err)
	require.NoError(t, database.SetPRGeneratingForReviewRun(
		job.PR.Owner, job.PR.Repo, job.PR.Number, job.PR.CommitSHA, job.PR.Title, job.PR.Author, nil, false, job.RunID,
	))
	successorRunID := "run-25800000000000000000000000000002"
	require.NoError(t, database.SetPRGeneratingForReviewRun(
		job.PR.Owner, job.PR.Repo, job.PR.Number, job.PR.CommitSHA, job.PR.Title, job.PR.Author, nil, false, successorRunID,
	))
	projected, err := database.MarkPRCompletedForReviewRun(
		job.PR.Owner, job.PR.Repo, job.PR.Number, successorRunID, successorRunID, job.PR.CommitSHA, "successor.html", 0, 0, 0, "approve", false, `{}`,
	)
	require.NoError(t, err)
	require.True(t, projected)
	timedOutCtx, cancel := context.WithCancelCause(context.Background())
	cancel(errReviewRunBudgetExceeded)

	assert.True(t, p.finishInterruptedReviewExecution(execution, timedOutCtx, "execution", context.DeadlineExceeded))
	run, err := database.GetReviewRun(job.RunID)
	require.NoError(t, err)
	require.NotNil(t, run)
	assert.Equal(t, db.ReviewRunStatusTimedOut, run.Status)
	assert.Equal(t, "run_timeout", run.TerminalCode)
	pr, err := database.GetPR(job.PR.Owner, job.PR.Repo, job.PR.Number)
	require.NoError(t, err)
	require.NotNil(t, pr)
	assert.Equal(t, "completed", pr.Status)
	assert.Empty(t, pr.ErrorMessage)
	assert.Equal(t, successorRunID, pr.ReviewRunID)
}

func TestRunAgentStageRequiresPreReservedSlot(t *testing.T) {
	p := newTestPoller(NewMockGitHubClient(), NewMockDatabase())
	p.agentSlots = make(chan struct{}, 1)
	job := customReviewJob(t, "run-25900000000000000000000000000001")

	_, err := p.runAgentStage(context.Background(), &reviewExecution{Job: job}, &service.ReviewResult{})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "concurrency slot was not reserved")
	assert.Empty(t, p.agentSlots)
}

func TestCancelledArtifactSavePreservesResetPRState(t *testing.T) {
	database := NewMockDatabase()
	storage := NewMockReviewStorage()
	saveStarted := make(chan struct{})
	storage.SaveReviewFunc = func(ctx context.Context, _ string, _ string, _ int, _ string, _ []byte) (string, error) {
		close(saveStarted)
		<-ctx.Done()
		return "", ctx.Err()
	}
	job := customReviewJob(t, "run-26000000000000000000000000000001")
	p := newTestPollerFull(NewMockGitHubClient(), database, storage, NewMockReviewGenerator())
	require.NoError(t, database.UpsertPR(&db.PR{
		RepoOwner: job.PR.Owner, RepoName: job.PR.Repo, PRNumber: job.PR.Number,
		LastCommitSHA: job.PR.CommitSHA, Status: "generating",
	}))
	require.NoError(t, p.ProcessReviewJob(context.Background(), job))
	select {
	case <-saveStarted:
	case <-time.After(5 * time.Second):
		t.Fatal("review did not reach artifact save")
	}
	require.NoError(t, database.UpdatePRStatus(job.PR.Owner, job.PR.Repo, job.PR.Number, "pending"))
	require.True(t, p.killReview(job.PR.Owner, job.PR.Repo, job.PR.Number))
	run := waitForReviewRunStatus(t, database, job.RunID, db.ReviewRunStatusCancelled)
	assert.Equal(t, "cancelled", run.TerminalCode)
	assert.Equal(t, "artifact_save", run.FailureStage)
	pr, err := database.GetPR(job.PR.Owner, job.PR.Repo, job.PR.Number)
	require.NoError(t, err)
	require.NotNil(t, pr)
	assert.Equal(t, "pending", pr.Status)
	assert.Empty(t, pr.ErrorMessage)
}

func TestProcessReviewJobStaleWorkerCannotPublishLatestProjection(t *testing.T) {
	database := NewMockDatabase()
	generator := NewMockReviewGenerator()
	generator.SimulateDelay = 200 * time.Millisecond
	job := customReviewJob(t, "run-27500000000000000000000000000001")
	p := newTestPollerFull(NewMockGitHubClient(), database, NewMockReviewStorage(), generator)
	require.NoError(t, database.UpsertPR(&db.PR{
		RepoOwner: job.PR.Owner, RepoName: job.PR.Repo, PRNumber: job.PR.Number,
		LastCommitSHA: job.PR.CommitSHA, Status: "generating",
	}))
	require.NoError(t, p.ProcessReviewJob(context.Background(), job))

	deadline := time.Now().Add(2 * time.Second)
	for {
		run, err := database.GetReviewRun(job.RunID)
		require.NoError(t, err)
		if run != nil && run.Status == db.ReviewRunStatusRunning {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("run was not claimed")
		}
		time.Sleep(5 * time.Millisecond)
	}
	expired := time.Now().UTC().Add(-time.Second)
	require.NoError(t, database.PatchReviewRun(job.RunID, db.ReviewRunPatch{LeaseExpiresAt: &expired}))
	waitForReviewJob(t, p, job)

	pr, err := database.GetPR(job.PR.Owner, job.PR.Repo, job.PR.Number)
	require.NoError(t, err)
	require.NotNil(t, pr)
	assert.NotEqual(t, "completed", pr.Status)
	assert.Empty(t, pr.ReviewRunID)
	run, err := database.GetReviewRun(job.RunID)
	require.NoError(t, err)
	require.NotNil(t, run)
	abandoned, err := database.AbandonExpiredReviewRuns(time.Now().UTC(), 0, ReviewQueueAbandonAfter)
	require.NoError(t, err)
	assert.Equal(t, 1, abandoned)
	run, err = database.GetReviewRun(job.RunID)
	require.NoError(t, err)
	require.NotNil(t, run)
	assert.Equal(t, db.ReviewRunStatusTimedOut, run.Status)
	assert.Equal(t, "lease_abandoned", run.TerminalCode)
	assert.Empty(t, run.LeaseHolder)
	assert.Nil(t, run.LeaseExpiresAt)
	assert.Empty(t, run.PublicationStatus)
}

func TestProcessReviewJobStaleGenerationFailureCannotProjectPRError(t *testing.T) {
	database := NewMockDatabase()
	generator := NewMockReviewGenerator()
	generator.SimulateDelay = 200 * time.Millisecond
	generator.Results["acme/widgets/7"] = struct {
		Result *ReviewResult
		Err    error
	}{Err: errors.New("provider unavailable")}
	job := customReviewJob(t, "run-27600000000000000000000000000001")
	p := newTestPollerFull(NewMockGitHubClient(), database, NewMockReviewStorage(), generator)
	require.NoError(t, database.UpsertPR(&db.PR{
		RepoOwner: job.PR.Owner, RepoName: job.PR.Repo, PRNumber: job.PR.Number,
		LastCommitSHA: job.PR.CommitSHA, Status: "generating",
	}))
	require.NoError(t, p.ProcessReviewJob(context.Background(), job))
	waitForReviewRunStatus(t, database, job.RunID, db.ReviewRunStatusRunning)

	expired := time.Now().UTC().Add(-time.Second)
	require.NoError(t, database.PatchReviewRun(job.RunID, db.ReviewRunPatch{LeaseExpiresAt: &expired}))
	require.NoError(t, database.UpdatePRStatus(job.PR.Owner, job.PR.Repo, job.PR.Number, "agent_reviewing"))
	waitForReviewJob(t, p, job)

	pr, err := database.GetPR(job.PR.Owner, job.PR.Repo, job.PR.Number)
	require.NoError(t, err)
	require.NotNil(t, pr)
	assert.Equal(t, "agent_reviewing", pr.Status)
	assert.Empty(t, pr.ErrorMessage)
}

func TestProcessReviewJobStaleArtifactFailureCannotProjectPRError(t *testing.T) {
	database := NewMockDatabase()
	storage := NewMockReviewStorage()
	saveStarted := make(chan struct{})
	releaseSave := make(chan struct{})
	storage.SaveReviewFunc = func(context.Context, string, string, int, string, []byte) (string, error) {
		close(saveStarted)
		<-releaseSave
		return "", errors.New("storage unavailable")
	}
	job := customReviewJob(t, "run-27700000000000000000000000000001")
	p := newTestPollerFull(NewMockGitHubClient(), database, storage, NewMockReviewGenerator())
	require.NoError(t, database.UpsertPR(&db.PR{
		RepoOwner: job.PR.Owner, RepoName: job.PR.Repo, PRNumber: job.PR.Number,
		LastCommitSHA: job.PR.CommitSHA, Status: "generating",
	}))
	require.NoError(t, p.ProcessReviewJob(context.Background(), job))
	select {
	case <-saveStarted:
	case <-time.After(5 * time.Second):
		t.Fatal("review did not reach artifact save")
	}
	expired := time.Now().UTC().Add(-time.Second)
	require.NoError(t, database.PatchReviewRun(job.RunID, db.ReviewRunPatch{LeaseExpiresAt: &expired}))
	require.NoError(t, database.UpdatePRStatus(job.PR.Owner, job.PR.Repo, job.PR.Number, "agent_reviewing"))
	close(releaseSave)
	waitForReviewJob(t, p, job)

	pr, err := database.GetPR(job.PR.Owner, job.PR.Repo, job.PR.Number)
	require.NoError(t, err)
	require.NotNil(t, pr)
	assert.Equal(t, "agent_reviewing", pr.Status)
	assert.Empty(t, pr.ErrorMessage)
}

func TestGetReviewerStatusExcludesQueuedJobs(t *testing.T) {
	p := newTestPoller(NewMockGitHubClient(), NewMockDatabase())
	job := customReviewJob(t, "run-27800000000000000000000000000001")
	_, tracked := p.tryTrackReviewJob(context.Background(), job)
	require.True(t, tracked)
	defer p.untrackReviewRun(job.PR.Owner, job.PR.Repo, job.PR.Number, job.RunID)

	running, duration := p.GetReviewerStatus()
	assert.False(t, running)
	assert.Zero(t, duration)

	_, started := p.startTrackedReviewJob(job)
	require.True(t, started)
	running, duration = p.GetReviewerStatus()
	assert.True(t, running)
	assert.GreaterOrEqual(t, duration, time.Duration(0))
}

func TestPublicationRenewalNeverShortensLease(t *testing.T) {
	database := NewMockDatabase()
	p := newTestPoller(NewMockGitHubClient(), database)
	job := customReviewJob(t, "run-28000000000000000000000000000001")
	exec, err := p.beginReviewExecution(job)
	require.NoError(t, err)
	before, err := database.GetReviewRun(job.RunID)
	require.NoError(t, err)
	require.NotNil(t, before)
	require.NotNil(t, before.LeaseExpiresAt)
	assert.True(t, p.renewReviewExecutionForPublication(exec))
	after, err := database.GetReviewRun(job.RunID)
	require.NoError(t, err)
	require.NotNil(t, after)
	require.NotNil(t, after.LeaseExpiresAt)
	assert.False(t, after.LeaseExpiresAt.Before(*before.LeaseExpiresAt))
}

func TestBeginReviewExecutionReleasesLeaseWhenClaimReloadFails(t *testing.T) {
	database := NewMockDatabase()
	p := newTestPoller(NewMockGitHubClient(), database)
	job := customReviewJob(t, "run-29000000000000000000000000000001")
	getCalls := 0
	database.GetReviewRunFunc = func(string) (*db.ReviewRun, error) {
		getCalls++
		if getCalls == 1 {
			return nil, nil
		}
		return nil, errors.New("transient reload failure")
	}

	exec, err := p.beginReviewExecution(job)
	assert.Nil(t, exec)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "transient reload failure")
	database.GetReviewRunFunc = nil
	run, err := database.GetReviewRun(job.RunID)
	require.NoError(t, err)
	require.NotNil(t, run)
	assert.Equal(t, db.ReviewRunStatusFailed, run.Status)
	assert.Equal(t, "claim_reload_failed", run.TerminalCode)
	assert.Equal(t, "dispatch", run.FailureStage)
	assert.Empty(t, run.LeaseHolder)
	assert.Nil(t, run.LeaseExpiresAt)
	assert.NotNil(t, run.CompletedAt)
}

func TestAgentConfigComesFromReviewJob(t *testing.T) {
	p := newTestPoller(NewMockGitHubClient(), NewMockDatabase())
	p.cfg.AgentWallClockSec = 999
	p.cfg.AgentMaxTurns = 999
	p.cfg.AgentBackend = service.AgentBackendClaude
	p.cfg.AgentModel = "deployment-model"
	p.cfg.AgentEffort = "low"
	job := customReviewJob(t, "run-30000000000000000000000000000001")
	exec := &reviewExecution{Job: job}

	cfg := p.agentConfigForExecution(exec, "github-token")
	assert.Equal(t, 73*time.Second, cfg.WallClock)
	assert.Equal(t, 19, cfg.MaxTurns)
	assert.Equal(t, service.AgentBackendOpenRouter, cfg.Backend)
	assert.Equal(t, service.DefaultOpenRouterAgentModel, cfg.Model)
	assert.Equal(t, "xhigh", cfg.Effort)
	assert.True(t, cfg.RequiredChecks)
}

func TestRecordGeminiAttemptsUsesRunExecutionAttempt(t *testing.T) {
	database := NewMockDatabase()
	p := newTestPoller(NewMockGitHubClient(), database)
	job := customReviewJob(t, "run-40000000000000000000000000000001")
	exec, err := p.beginReviewExecution(job)
	require.NoError(t, err)
	started := time.Now().UTC().Add(-time.Second)
	completed := time.Now().UTC()
	p.recordGeminiAttempts(exec, started, completed, "completed", "")

	attempts, err := database.ListReviewStageAttempts(job.RunID)
	require.NoError(t, err)
	require.Len(t, attempts, 2)
	for _, attempt := range attempts {
		assert.Equal(t, 1, attempt.ExecutionAttempt)
		assert.Equal(t, "completed", attempt.Status)
	}
	assert.Equal(t, "classification_summary", attempts[0].Stage)
	assert.Equal(t, "first_pass", attempts[1].Stage)
	assert.Zero(t, attempts[0].DurationMS)
	assert.Equal(t, "aggregate_window", attempts[1].StopReason)
}
