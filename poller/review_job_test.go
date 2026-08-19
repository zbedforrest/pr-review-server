package poller

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"pr-review-server/db"
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

func TestAgentSlotWaitDoesNotStartBudgetOrLease(t *testing.T) {
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
	assert.Nil(t, run.LeaseExpiresAt)
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
		job.PR.Owner, job.PR.Repo, job.PR.Number, successorRunID, job.PR.CommitSHA, "successor.html", 0, 0, 0, "approve", false, `{}`,
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
