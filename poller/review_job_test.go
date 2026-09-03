package poller

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"pr-review-server/config"
	"pr-review-server/db"
	"pr-review-server/gcs"
	"pr-review-server/github"
	"pr-review-server/pkg/reviewer/llm"
	"pr-review-server/pkg/reviewer/payload"
	"pr-review-server/pkg/reviewer/runconfig"
	"pr-review-server/pkg/reviewer/service"
	"pr-review-server/pkg/reviewer/types"
)

type blockingReviewGenerator struct {
	started chan int
	release chan struct{}
}

func (g *blockingReviewGenerator) GenerateReview(ctx context.Context, cfg ReviewGeneratorConfig) (*ReviewResult, error) {
	g.started <- cfg.PRNumber
	select {
	case <-g.release:
		return &ReviewResult{HTMLContent: []byte("<html>review</html>")}, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func newTestPollerWithGenerator(mockGH *MockGitHubClient, database db.Database, storage *MockReviewStorage, generator ReviewGenerator) *Poller {
	return &Poller{
		cfg: testConfig(), db: database, ghClient: mockGH, storage: storage,
		reviewGenerator: generator, reviewDir: "/tmp/test-reviews", activeReviews: make(map[string]ProcessInfo),
	}
}

func reviewJobSnapshot(t *testing.T, effective runconfig.Effective) runconfig.Snapshot {
	t.Helper()
	turnBudgetUnit, turnBudgetVersion := runconfig.TurnBudgetSemantics(effective.Agent.Backend)
	effective.Agent.TurnBudgetUnit = turnBudgetUnit
	effective.Agent.TurnBudgetVersion = turnBudgetVersion
	snapshot, err := runconfig.Resolve(runconfig.Overrides{}, effective, runconfig.Policy{
		Backends: map[string]runconfig.BackendPolicy{
			effective.Agent.Backend: {
				Available: true, Ready: true, PolicyEnabled: true, CredentialConfigured: true, ExecutableAvailable: true,
				TurnBudgetUnit: turnBudgetUnit, TurnBudgetVersion: turnBudgetVersion,
				Models:  []string{effective.Agent.Model},
				Efforts: []string{effective.Agent.Effort},
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
			TurnBudgetUnit: runconfig.TurnBudgetUnitCompletedNonReasoningItem, TurnBudgetVersion: runconfig.TurnBudgetVersion,
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

func TestPrepareReviewJobResolvesCallerOverridesWithinPolicy(t *testing.T) {
	database := NewMockDatabase()
	database.ReviewNRequests = 3
	p := newTestPoller(NewMockGitHubClient(), database)
	p.cfg.AgenticReviews = true
	p.cfg.AgentBackend = service.AgentBackendClaude
	p.cfg.AgentModel = "claude-fable-5"
	p.cfg.AgentEffort = "medium"
	p.cfg.AgentWallClockSec = 360
	p.cfg.AgentMaxTurns = 40
	p.cfg.OpenRouterAPIKey = "configured"
	p.cfg.ReviewAgentModelsOpenRouter = []string{"openai/gpt-5.6-sol"}
	p.cfg.ReviewAgentEffortsOpenRouter = []string{"medium", "high"}
	p.cfg.ReviewMaxWallClockSec = 900
	p.cfg.ReviewMaxTurns = 120
	p.cfg.ReviewMaxTurnsConfigured = true
	p.cfg.ReviewMaxFirstPassSamples = 5

	backend, model, effort := service.AgentBackendOpenRouter, "openai/gpt-5.6-sol", "high"
	wallClock, maxTurns, samples := 720, 100, 2
	userID := 17
	job, err := p.PrepareReviewJob(github.PullRequest{
		Owner: "acme", Repo: "widgets", Number: 7, CommitSHA: "0123456789abcdef0123456789abcdef01234567",
	}, runconfig.Overrides{
		Agent: &runconfig.AgentOverrides{
			Backend: &backend, Model: &model, Effort: &effort,
			WallClockSeconds: &wallClock, MaxTurns: &maxTurns,
		},
		FirstPass: &runconfig.FirstPassOverrides{Samples: &samples},
	}, true, "api_v1", &userID)
	require.NoError(t, err)
	assert.Equal(t, service.AgentBackendOpenRouter, job.Config.Effective.Agent.Backend)
	assert.Equal(t, model, job.Config.Effective.Agent.Model)
	assert.Equal(t, effort, job.Config.Effective.Agent.Effort)
	assert.Equal(t, wallClock, job.Config.Effective.Agent.WallClockSeconds)
	assert.Equal(t, maxTurns, job.Config.Effective.Agent.MaxTurns)
	assert.Equal(t, samples, job.Config.Effective.FirstPass.Samples)
	assert.Equal(t, runconfig.SourceRequest, job.Config.Sources["agent.model"])
	assert.Equal(t, "api_v1", job.TriggerSource)
	assert.Equal(t, &userID, job.RequestedByUserID)
	assert.True(t, job.Force)
	assert.NoError(t, p.validateReviewJob(job), "operator ceiling may exceed the active default budget")
}

func TestPrepareReviewJobRequiresModelWhenChangingBackend(t *testing.T) {
	p := newTestPoller(NewMockGitHubClient(), NewMockDatabase())
	p.cfg.AgenticReviews = true
	p.cfg.AgentBackend = service.AgentBackendClaude
	p.cfg.AgentModel = "claude-fable-5"
	p.cfg.OpenRouterAPIKey = "configured"
	p.cfg.ReviewAgentModelsOpenRouter = []string{"openai/gpt-5.6-sol"}
	backend := service.AgentBackendOpenRouter

	_, err := p.PrepareReviewJob(github.PullRequest{
		Owner: "acme", Repo: "widgets", Number: 7, CommitSHA: "0123456789abcdef0123456789abcdef01234567",
	}, runconfig.Overrides{Agent: &runconfig.AgentOverrides{Backend: &backend}}, true, "api_v1", nil)
	var validationErr *runconfig.ValidationError
	require.ErrorAs(t, err, &validationErr)
	assert.Equal(t, "agent.model", validationErr.Field)
}

func TestPrepareReviewJobDerivesTurnDefaultForSelectedBackend(t *testing.T) {
	p := newTestPoller(NewMockGitHubClient(), NewMockDatabase())
	p.cfg.AgenticReviews = true
	p.cfg.AgentBackend = service.AgentBackendClaude
	p.cfg.AgentModel = "claude-fable-5"
	p.cfg.AgentWallClockSec = fallbackReviewMaxWallClockSec
	p.cfg.AgentMaxTurns = fallbackClaudeMaxTurns
	p.cfg.ReviewMaxWallClockSec = fallbackReviewMaxWallClockSec
	p.cfg.ReviewMaxTurns = fallbackOpenRouterMaxTurns
	p.cfg.ReviewMaxTurnsConfigured = true
	p.cfg.OpenRouterAPIKey = "configured"
	p.cfg.ReviewAgentModelsOpenRouter = []string{service.DefaultOpenRouterAgentModel}
	backend := service.AgentBackendOpenRouter
	model := service.DefaultOpenRouterAgentModel

	job, err := p.PrepareReviewJob(github.PullRequest{
		Owner: "acme", Repo: "widgets", Number: 7, CommitSHA: "0123456789abcdef0123456789abcdef01234567",
	}, runconfig.Overrides{Agent: &runconfig.AgentOverrides{Backend: &backend, Model: &model}}, true, "api_v1", nil)
	require.NoError(t, err)
	assert.Equal(t, fallbackOpenRouterMaxTurns, job.Config.Effective.Agent.MaxTurns)
	assert.Equal(t, runconfig.SourceDerived, job.Config.Sources["agent.max_turns"])
	assert.Equal(t, runconfig.TurnBudgetUnitCompletedNonReasoningItem, job.Config.Effective.Agent.TurnBudgetUnit)
}

func TestReviewPolicyIsBoundedAndConsistentForZeroOrLowerCeilings(t *testing.T) {
	p := newTestPoller(NewMockGitHubClient(), NewMockDatabase())
	p.cfg.AgenticReviews = true
	p.cfg.AgentWallClockSec = 360
	p.cfg.AgentMaxTurns = 40
	p.cfg.ReviewMaxWallClockSec = 300
	p.cfg.ReviewMaxTurns = 20
	p.cfg.ReviewMaxTurnsConfigured = true

	defaults, policy, err := p.ReviewConfigDefaultsAndPolicy()
	require.NoError(t, err)
	assert.Equal(t, 360, policy.MaxWallClockSeconds)
	assert.Equal(t, 40, policy.MaxTurns)
	assert.Equal(t, 40, policy.Backends[service.AgentBackendClaude].MaxTurns)
	assert.Equal(t, 20, policy.Backends[service.AgentBackendOpenRouter].DefaultMaxTurns)
	assert.Equal(t, 20, policy.Backends[service.AgentBackendOpenRouter].MaxTurns)
	snapshot, err := runconfig.Resolve(runconfig.Overrides{}, defaults, policy)
	require.NoError(t, err)
	job := ReviewJob{
		PR: github.PullRequest{
			Owner: "acme", Repo: "widgets", Number: 7,
			CommitSHA: "0123456789abcdef0123456789abcdef01234567",
		},
		RunID: "run-10000000000000000000000000000009", Config: snapshot, TriggerSource: "api_v1",
	}
	assert.NoError(t, p.validateReviewJob(job), "admission must use the same raised ceiling advertised by capabilities")

	zero := newTestPoller(NewMockGitHubClient(), NewMockDatabase())
	zero.cfg = &config.Config{}
	zeroDefaults, zeroPolicy, err := zero.ReviewConfigDefaultsAndPolicy()
	require.NoError(t, err)
	assert.Equal(t, fallbackReviewMaxWallClockSec, zeroPolicy.MaxWallClockSeconds)
	assert.Equal(t, fallbackClaudeMaxTurns, zeroPolicy.MaxTurns)
	assert.Equal(t, fallbackReviewMaxFirstPassSamples, zeroPolicy.MaxFirstPassSamples)
	assert.False(t, zeroPolicy.Backends[service.AgentBackendClaude].Available)
	enabled := true
	wallClock, turns := fallbackReviewMaxWallClockSec, fallbackClaudeMaxTurns
	_, err = runconfig.Resolve(runconfig.Overrides{Agent: &runconfig.AgentOverrides{
		Enabled: &enabled, WallClockSeconds: &wallClock, MaxTurns: &turns,
	}}, zeroDefaults, zeroPolicy)
	require.Error(t, err, "AGENTIC_REVIEWS=false remains an operator feature gate")
}

func TestReviewBackendReadinessReportsStableReasonsAndTurnSemantics(t *testing.T) {
	p := newTestPoller(NewMockGitHubClient(), NewMockDatabase())
	p.cfg.AgenticReviews = true
	p.cfg.AnthropicAPIKey = ""
	p.cfg.OpenRouterAPIKey = ""
	p.lookPath = func(name string) (string, error) {
		if name == "git" || name == "claude" {
			return "/secret/bin/" + name, nil
		}
		return "", fmt.Errorf("%s missing", name)
	}

	defaults, policy, err := p.ReviewConfigDefaultsAndPolicy()
	require.NoError(t, err)
	claude := policy.Backends[service.AgentBackendClaude]
	assert.True(t, claude.Available)
	assert.True(t, claude.Ready)
	assert.True(t, claude.PolicyEnabled)
	assert.False(t, claude.CredentialConfigured, "Claude CLI may use its own OAuth session")
	assert.False(t, claude.CredentialRequired)
	assert.True(t, claude.ExecutableAvailable)
	assert.Empty(t, claude.UnavailableReasons)
	assert.Equal(t, runconfig.TurnBudgetUnitAssistantEvent, claude.TurnBudgetUnit)
	assert.Equal(t, fallbackClaudeMaxTurns, claude.DefaultMaxTurns)
	assert.Equal(t, fallbackClaudeMaxTurns, claude.MaxTurns)
	assert.Equal(t, runconfig.TurnBudgetUnitAssistantEvent, defaults.Agent.TurnBudgetUnit)

	openRouter := policy.Backends[service.AgentBackendOpenRouter]
	assert.False(t, openRouter.Available)
	assert.False(t, openRouter.Ready)
	assert.False(t, openRouter.CredentialConfigured)
	assert.True(t, openRouter.CredentialRequired)
	assert.False(t, openRouter.ExecutableAvailable)
	assert.ElementsMatch(t, []string{
		runconfig.BackendUnavailableCredentialMissing,
		runconfig.BackendUnavailableCLIMissing,
	}, openRouter.UnavailableReasons)
	assert.Equal(t, runconfig.TurnBudgetUnitCompletedNonReasoningItem, openRouter.TurnBudgetUnit)
	assert.Equal(t, fallbackOpenRouterMaxTurns, openRouter.DefaultMaxTurns)
	assert.Equal(t, fallbackOpenRouterMaxTurns, openRouter.MaxTurns)
}

func TestReviewConfigDefaultsAndPolicyIncludeFirstPassProviders(t *testing.T) {
	t.Setenv("GEMINI_PRO_MODEL", "")
	p := newTestPoller(NewMockGitHubClient(), NewMockDatabase())
	p.cfg.AnthropicAPIKey = "sk-ant-test"

	defaults, policy, err := p.ReviewConfigDefaultsAndPolicy()
	require.NoError(t, err)
	assert.Equal(t, "gemini", defaults.FirstPass.Provider)
	assert.Equal(t, llm.ProModelName(), defaults.FirstPass.Model)

	gemini := policy.FirstPassProviders["gemini"]
	assert.True(t, gemini.CredentialConfigured, "the deployment default provider is always admitted")
	assert.Contains(t, gemini.Models, llm.ProModelName())
	assert.Equal(t, llm.ProModelName(), gemini.DefaultModel)

	claude := policy.FirstPassProviders["claude"]
	assert.True(t, claude.CredentialConfigured)
	assert.Equal(t, llm.DefaultClaudeModel, claude.DefaultModel)
	assert.Contains(t, claude.Models, llm.DefaultClaudeModel)

	openRouter := policy.FirstPassProviders["openrouter"]
	assert.False(t, openRouter.CredentialConfigured, "no OpenRouter key is configured")
	assert.Equal(t, llm.DefaultOpenRouterModel, openRouter.DefaultModel)
}

func TestReviewConfigPolicyAdmitsActiveFirstPassProviderWithoutKey(t *testing.T) {
	p := newTestPoller(NewMockGitHubClient(), NewMockDatabase())
	p.cfg.FirstPassProvider = "claude"
	p.cfg.FirstPassModel = "claude-fable-5"

	defaults, policy, err := p.ReviewConfigDefaultsAndPolicy()
	require.NoError(t, err)
	assert.Equal(t, "claude", defaults.FirstPass.Provider)
	assert.Equal(t, "claude-fable-5", defaults.FirstPass.Model)
	assert.True(t, policy.FirstPassProviders["claude"].CredentialConfigured)
	assert.Contains(t, policy.FirstPassProviders["claude"].Models, "claude-fable-5")

	snapshot, err := runconfig.Resolve(runconfig.Overrides{}, defaults, policy)
	require.NoError(t, err)
	assert.Equal(t, "claude-fable-5", snapshot.Effective.FirstPass.Model)
}

func TestPrepareReviewJobResolvesAndGatesFirstPassOverrides(t *testing.T) {
	p := newTestPoller(NewMockGitHubClient(), NewMockDatabase())
	p.cfg.AnthropicAPIKey = "sk-ant-test"
	pr := github.PullRequest{
		Owner: "acme", Repo: "widgets", Number: 7, CommitSHA: "0123456789abcdef0123456789abcdef01234567",
	}

	provider := "claude"
	job, err := p.PrepareReviewJob(pr, runconfig.Overrides{
		FirstPass: &runconfig.FirstPassOverrides{Provider: &provider},
	}, true, "api_v1", nil)
	require.NoError(t, err)
	assert.Equal(t, "claude", job.Config.Effective.FirstPass.Provider)
	assert.Equal(t, llm.DefaultClaudeModel, job.Config.Effective.FirstPass.Model)
	assert.Equal(t, runconfig.SourceDerived, job.Config.Sources["first_pass.model"])

	openRouter := "openrouter"
	_, err = p.PrepareReviewJob(pr, runconfig.Overrides{
		FirstPass: &runconfig.FirstPassOverrides{Provider: &openRouter},
	}, true, "api_v1", nil)
	var validationErr *runconfig.ValidationError
	require.ErrorAs(t, err, &validationErr)
	assert.Equal(t, "first_pass.provider", validationErr.Field)

	offList := "claude-opus-9"
	_, err = p.PrepareReviewJob(pr, runconfig.Overrides{
		FirstPass: &runconfig.FirstPassOverrides{Provider: &provider, Model: &offList},
	}, true, "api_v1", nil)
	require.ErrorAs(t, err, &validationErr)
	assert.Equal(t, "first_pass.model", validationErr.Field)
}

func TestFirstPassClientForRunCachesPerProviderAndModel(t *testing.T) {
	t.Setenv("GEMINI_PRO_MODEL", "")
	p := newTestPoller(NewMockGitHubClient(), NewMockDatabase())
	p.cfg.GeminiAPIKey = "gem-key"
	p.cfg.AnthropicAPIKey = "ant-key"

	defaultClient, defaultInfo, err := p.firstPassClientForRun(runconfig.FirstPass{})
	require.NoError(t, err)
	assert.Equal(t, service.FirstPassInfo{Provider: "google", Backend: "gemini_api", Model: llm.ProModelName()}, defaultInfo)

	sameClient, _, err := p.firstPassClientForRun(runconfig.FirstPass{Provider: "gemini", Model: llm.ProModelName()})
	require.NoError(t, err)
	assert.True(t, defaultClient == sameClient, "identical provider+model must reuse the cached client")

	claudeClient, claudeInfo, err := p.firstPassClientForRun(runconfig.FirstPass{Provider: "claude", Model: "claude-fable-5"})
	require.NoError(t, err)
	assert.False(t, defaultClient == claudeClient)
	assert.Equal(t, service.FirstPassInfo{Provider: "anthropic", Backend: "anthropic_api", Model: "claude-fable-5"}, claudeInfo)

	otherClaude, _, err := p.firstPassClientForRun(runconfig.FirstPass{Provider: "claude", Model: llm.DefaultClaudeModel})
	require.NoError(t, err)
	assert.False(t, claudeClient == otherClaude, "different models must not share a client")

	_, _, err = p.firstPassClientForRun(runconfig.FirstPass{Provider: "openrouter", Model: "openai/gpt-5.6-sol"})
	require.ErrorContains(t, err, "OPENROUTER_API_KEY")

	uses := p.pipelineModelUses(runconfig.FirstPass{Provider: "claude", Model: "claude-fable-5"})
	require.Len(t, uses, 2)
	assert.Equal(t, payload.ModelUse{
		Stage: "first_pass", Provider: "anthropic", Backend: "anthropic_api", RequestedModel: "claude-fable-5",
	}, uses[0])
	assert.Equal(t, "classification_summary", uses[1].Stage)
}

func TestPipelineModelUsesRecordsGeminiThinkingAsEffort(t *testing.T) {
	t.Setenv("GEMINI_PRO_MODEL", "")
	p := newTestPoller(NewMockGitHubClient(), NewMockDatabase())
	p.cfg.GeminiAPIKey = "gem-key"
	p.cfg.AnthropicAPIKey = "ant-key"
	p.cfg.FirstPassThinking = "high"

	uses := p.pipelineModelUses(runconfig.FirstPass{Provider: "gemini", Model: llm.ProModelName()})
	require.Len(t, uses, 2)
	assert.Equal(t, "first_pass", uses[0].Stage)
	assert.Equal(t, "high", uses[0].Effort)

	claudeUses := p.pipelineModelUses(runconfig.FirstPass{Provider: "claude", Model: "claude-fable-5"})
	assert.Empty(t, claudeUses[0].Effort, "thinking level applies only to gemini first passes")

	p.cfg.FirstPassThinking = ""
	uses = p.pipelineModelUses(runconfig.FirstPass{Provider: "gemini", Model: llm.ProModelName()})
	assert.Empty(t, uses[0].Effort)
}

func TestFirstPassClientForRunFallsBackToDeploymentConfigForLegacySnapshots(t *testing.T) {
	p := newTestPoller(NewMockGitHubClient(), NewMockDatabase())
	p.cfg.FirstPassProvider = "claude"
	p.cfg.FirstPassModel = "claude-opus-5"
	p.cfg.AnthropicAPIKey = "ant-key"

	_, info, err := p.firstPassClientForRun(runconfig.FirstPass{})
	require.NoError(t, err)
	assert.Equal(t, service.FirstPassInfo{Provider: "anthropic", Backend: "anthropic_api", Model: "claude-opus-5"}, info)
}

func TestNewPollerCreatesProcessGlobalFirstPassCapacity(t *testing.T) {
	cfg := testConfig()
	cfg.ReviewMaxFirstPassConcurrent = 3
	cfg.AgentMaxConcurrent = 3
	p := New(cfg, NewMockDatabase(), nil, nil)
	require.NotNil(t, p.firstPassSlots)
	assert.Equal(t, 3, cap(p.firstPassSlots))
	require.NotNil(t, p.dispatchSlots)
	assert.Equal(t, 3, cap(p.dispatchSlots))
	assert.Equal(t, 3, cap(p.agentSlots))

	cfg.ReviewMaxFirstPassConcurrent = 0
	cfg.AgentMaxConcurrent = 0
	p = New(cfg, NewMockDatabase(), nil, nil)
	assert.Equal(t, fallbackReviewFirstPassConcurrent, cap(p.firstPassSlots))
	assert.Equal(t, fallbackReviewFirstPassConcurrent, cap(p.dispatchSlots))
	assert.Equal(t, fallbackAgentConcurrent, cap(p.agentSlots))
}

func TestReviewJobValidateRequiresCompleteIdempotencyMetadata(t *testing.T) {
	job := customReviewJob(t, "run-10000000000000000000000000000000")
	job.IdempotencyScope = "user:17"
	job.IdempotencyKeyHash = "key-hash"
	require.ErrorContains(t, job.Validate(), "request hash")

	job.RequestHash = "request-hash"
	require.NoError(t, job.Validate())
}

func TestGenerateReviewsBatchSurfacesInvalidDefaultsForEveryPR(t *testing.T) {
	database := NewMockDatabase()
	generator := NewMockReviewGenerator()
	p := newTestPollerFull(NewMockGitHubClient(), database, NewMockReviewStorage(), generator)
	p.cfg.AgenticReviews = true
	p.cfg.AgentBackend = service.AgentBackendOpenRouter
	p.cfg.AgentModel = service.DefaultOpenRouterAgentModel
	p.cfg.AgentEffort = "high"
	p.cfg.AgentWallClockSec = 30
	p.cfg.AgentMaxTurns = 5
	p.cfg.OpenRouterAPIKey = ""
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
	assert.Equal(t, 1, database.GetReviewNRequestsCalls, "one batch must freeze one deployment-default snapshot")
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
	job.IdempotencyScope = "user:9"
	job.IdempotencyKeyHash = "key-hash"
	job.RequestHash = "request-hash"
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
	assert.Equal(t, "user:9", run.IdempotencyScope)
	assert.Equal(t, "key-hash", run.IdempotencyKeyHash)
	assert.Equal(t, "request-hash", run.RequestHash)
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
	assert.GreaterOrEqual(t, metadata.QueueWaitMS, int64(0))
	require.NotEmpty(t, metadata.StageTimings)
	artifactSave := metadata.StageTimings[len(metadata.StageTimings)-1]
	assert.Equal(t, "artifact_save", artifactSave.Stage)
	assert.False(t, artifactSave.StartedAt.IsZero())
	assert.GreaterOrEqual(t, artifactSave.DurationMS, int64(0))
	assert.False(t, artifactSave.StartedAt.Before(metadata.StartedAt))
}

func TestProcessReviewJobRejectsLiveRunAcceptedByAnotherInstance(t *testing.T) {
	database, err := db.NewGormSQLite("file:" + filepath.Join(t.TempDir(), "cross-instance.db") + "?_busy_timeout=5000&_journal_mode=WAL")
	require.NoError(t, err)
	defer database.Close()
	generator := NewMockReviewGenerator()
	generator.SimulateDelay = 100 * time.Millisecond
	firstPoller := newTestPollerFull(NewMockGitHubClient(), database, NewMockReviewStorage(), generator)
	secondPoller := newTestPollerFull(NewMockGitHubClient(), database, NewMockReviewStorage(), generator)
	first := reviewJobWithoutAgent(t, "run-11000000000000000000000000000001")
	second := reviewJobWithoutAgent(t, "run-11000000000000000000000000000002")
	require.NoError(t, database.UpsertPR(&db.PR{
		RepoOwner: first.PR.Owner, RepoName: first.PR.Repo, PRNumber: first.PR.Number,
		LastCommitSHA: first.PR.CommitSHA, Status: "pending",
	}))

	require.NoError(t, firstPoller.ProcessReviewJob(context.Background(), first))
	err = secondPoller.ProcessReviewJob(context.Background(), second)
	require.ErrorIs(t, err, ErrReviewAlreadyTracked)
	assert.False(t, secondPoller.IsReviewTracked(second.PR.Owner, second.PR.Repo, second.PR.Number))
	run, getErr := database.GetReviewRun(second.RunID)
	require.NoError(t, getErr)
	assert.Nil(t, run)

	require.Eventually(t, func() bool {
		return !firstPoller.IsReviewTracked(first.PR.Owner, first.PR.Repo, first.PR.Number)
	}, 5*time.Second, 10*time.Millisecond)
}

func TestProcessReviewJobPublishesIdempotencyRowBeforeLocalConflict(t *testing.T) {
	database := NewMockDatabase()
	p := newTestPollerFull(NewMockGitHubClient(), database, NewMockReviewStorage(), NewMockReviewGenerator())
	first := reviewJobWithoutAgent(t, "run-12000000000000000000000000000001")
	second := reviewJobWithoutAgent(t, "run-12000000000000000000000000000002")
	for _, job := range []*ReviewJob{&first, &second} {
		job.IdempotencyScope = "user:17"
		job.IdempotencyKeyHash = "shared-key-hash"
		job.RequestHash = "shared-request-hash"
	}
	require.NoError(t, database.UpsertPR(&db.PR{
		RepoOwner: first.PR.Owner, RepoName: first.PR.Repo, PRNumber: first.PR.Number,
		LastCommitSHA: first.PR.CommitSHA, Status: "pending",
	}))

	enteredInsertWindow := make(chan struct{})
	releaseInsert := make(chan struct{})
	var blockOnce sync.Once
	database.GetReviewRunFunc = func(runID string) (*db.ReviewRun, error) {
		blockOnce.Do(func() {
			close(enteredInsertWindow)
			<-releaseInsert
		})
		database.mu.RLock()
		defer database.mu.RUnlock()
		run := database.ReviewRuns[runID]
		if run == nil {
			return nil, nil
		}
		copy := *run
		return &copy, nil
	}

	firstResult := make(chan error, 1)
	go func() { firstResult <- p.ProcessReviewJob(context.Background(), first) }()
	<-enteredInsertWindow
	secondResult := make(chan error, 1)
	go func() { secondResult <- p.ProcessReviewJob(context.Background(), second) }()
	select {
	case err := <-secondResult:
		t.Fatalf("second admission returned before durable insert: %v", err)
	case <-time.After(25 * time.Millisecond):
	}
	close(releaseInsert)
	require.NoError(t, <-firstResult)
	require.ErrorIs(t, <-secondResult, ErrReviewAlreadyTracked)

	persisted, err := database.GetReviewRunByIdempotency(first.IdempotencyScope, first.IdempotencyKeyHash)
	require.NoError(t, err)
	require.NotNil(t, persisted, "the conflict path must only return after the winning row is queryable")
	assert.Equal(t, first.RunID, persisted.RunID)
	waitForReviewJob(t, p, first)
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

func TestProcessReviewJobRejectsDisabledAgentBudgetBeyondStaleResetHorizon(t *testing.T) {
	database := NewMockDatabase()
	p := newTestPoller(NewMockGitHubClient(), database)
	p.cfg.AgentWallClockSec = 30
	job := customReviewJob(t, "run-12500000000000000000000000000002")
	job.Config.Effective.Agent.Enabled = false
	var err error
	job.Config.Hash, err = runconfig.Hash(job.Config.Effective)
	require.NoError(t, err)

	err = p.ProcessReviewJob(context.Background(), job)
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

func TestProcessReviewJobTerminalizesQueueWhenTrackingIsLostBeforeLeaseAttachment(t *testing.T) {
	database := NewMockDatabase()
	p := newTestPollerFull(NewMockGitHubClient(), database, NewMockReviewStorage(), NewMockReviewGenerator())
	job := reviewJobWithoutAgent(t, "run-21000000000000000000000000000001")
	entered := make(chan struct{})
	release := make(chan struct{})
	var blockFirstClaim sync.Once
	database.BeforeClaimOrRenewQueuedReviewRunLease = func() {
		blockFirstClaim.Do(func() {
			close(entered)
			<-release
		})
	}

	result := make(chan error, 1)
	go func() { result <- p.ProcessReviewJob(context.Background(), job) }()
	<-entered
	p.untrackReviewRun(job.PR.Owner, job.PR.Repo, job.PR.Number, job.RunID)
	close(release)

	err := <-result
	require.Error(t, err)
	assert.Contains(t, err.Error(), "track accepted review run")
	run, getErr := database.GetReviewRun(job.RunID)
	require.NoError(t, getErr)
	require.NotNil(t, run)
	assert.Equal(t, db.ReviewRunStatusCancelled, run.Status)
	assert.Equal(t, "dispatch_lost", run.TerminalCode)
	assert.Empty(t, run.LeaseHolder)
	assert.Nil(t, run.LeaseExpiresAt)
}

func TestReviewLedgerWritesRetryTransientDatabaseErrors(t *testing.T) {
	database := NewMockDatabase()
	p := newTestPoller(NewMockGitHubClient(), database)
	job := customReviewJob(t, "run-21500000000000000000000000000001")
	execution, err := p.beginReviewExecution(job)
	require.NoError(t, err)

	database.RenewReviewRunLeaseErrors = []error{errors.New("transient renew 1"), errors.New("transient renew 2")}
	assert.True(t, p.renewReviewExecutionForPublication(execution))
	database.PatchReviewRunAsHolderErrors = []error{errors.New("transient finish 1"), errors.New("transient finish 2")}
	completed := db.ReviewRunStatusCompleted
	assert.True(t, p.finishReviewExecution(execution, db.ReviewRunPatch{Status: &completed}))

	run, getErr := database.GetReviewRun(job.RunID)
	require.NoError(t, getErr)
	require.NotNil(t, run)
	assert.Equal(t, db.ReviewRunStatusCompleted, run.Status)
	assert.Empty(t, run.LeaseHolder)
	assert.Nil(t, run.LeaseExpiresAt)
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

func TestKilledAcceptedJobCannotBeReadopted(t *testing.T) {
	p := newTestPoller(NewMockGitHubClient(), NewMockDatabase())
	job := customReviewJob(t, "run-23500000000000000000000000000001")
	queuedCtx, tracked := p.tryTrackReviewJob(context.Background(), job)
	require.True(t, tracked)
	job.QueueLeaseHolder = "dispatcher-killed"
	require.True(t, p.setTrackedQueueLease(job, job.QueueLeaseHolder))
	require.True(t, p.killReview(job.PR.Owner, job.PR.Repo, job.PR.Number))
	assert.ErrorIs(t, queuedCtx.Err(), context.Canceled)

	adoptedCtx, adopted := p.trackOrAdoptReviewJob(queuedCtx, job)
	assert.False(t, adopted)
	assert.Nil(t, adoptedCtx)
	assert.False(t, p.IsReviewTracked(job.PR.Owner, job.PR.Repo, job.PR.Number))
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

	runCtx, queueWait, started := p.startTrackedReviewJob(job)
	assert.GreaterOrEqual(t, queueWait, time.Duration(0))
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

func TestAutomaticExecutionAdmissionHasCrashRecoveryLease(t *testing.T) {
	database := NewMockDatabase()
	p := newTestPoller(NewMockGitHubClient(), database)
	job := customReviewJob(t, "run-24350000000000000000000000000001")
	job.TriggerSource = "poller"
	before := time.Now().UTC()

	require.NoError(t, p.admitReviewRunForExecution(job))
	run, err := database.GetReviewRun(job.RunID)
	require.NoError(t, err)
	require.NotNil(t, run)
	assert.Equal(t, db.ReviewRunStatusQueued, run.Status)
	assert.NotEmpty(t, run.LeaseHolder)
	require.NotNil(t, run.LeaseExpiresAt)
	assert.WithinDuration(t, before.Add(ReviewQueueLeaseTTL), *run.LeaseExpiresAt, time.Second)

	abandoned, err := database.AbandonExpiredReviewRuns(
		before.Add(ReviewQueueLeaseTTL+ReviewLeaseCompletionGrace+time.Second),
		ReviewLeaseCompletionGrace,
		ReviewQueueAbandonAfter,
	)
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
	staleProjection := database.PRs[prDBKey(job.PR.Owner, job.PR.Repo, job.PR.Number)]
	staleProjection.GeneratingSince = &staleGeneratingSince
	staleProjection.ReviewHTMLPath = gcs.ReviewFileName(job.PR.Owner, job.PR.Repo, job.PR.Number, job.PR.CommitSHA)
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
	assert.Equal(t, db.ReviewRunStatusCompleted, run.Status)
	assert.Equal(t, "cache_restored", run.TerminalCode)
	assert.Equal(t, "restored_from_cache", run.PublicationStatus)
	assert.Equal(t, sidecarName, run.JSONPath)
	assert.Equal(t, 2, run.CriticalCount)
	assert.Equal(t, 3, run.MediumCount)
	assert.Equal(t, 4, run.LowCount)
	assert.Equal(t, "approve_suggestions", run.Verdict)
	assert.True(t, run.ModelFallback)
	assert.Equal(t, "unverified", run.ServingModelVerification)
	assert.Contains(t, run.ActualModelsJSON, `"served_model":"fallback"`)
}

func TestUnreadableCacheMetadataRegeneratesInsteadOfPublishingZeroCounts(t *testing.T) {
	database := NewMockDatabase()
	storage := NewMockReviewStorage()
	generator := NewMockReviewGenerator()
	p := newTestPollerFull(NewMockGitHubClient(), database, storage, generator)
	p.reviewDir = t.TempDir()
	job := reviewJobWithoutAgent(t, "run-24450000000000000000000000000001")
	job.Force = false
	storage.ExistingReviews[fmt.Sprintf("%s/%s/%d/%s", job.PR.Owner, job.PR.Repo, job.PR.Number, job.PR.CommitSHA)] = true
	require.NoError(t, database.UpsertPR(&db.PR{
		RepoOwner: job.PR.Owner, RepoName: job.PR.Repo, PRNumber: job.PR.Number,
		// ResetPRToOutdated updates the SHA while leaving old counts/run metadata,
		// but clears ReviewHTMLPath. A same-SHA match alone is therefore untrusted.
		LastCommitSHA: job.PR.CommitSHA, Status: "pending", CriticalCount: 99,
		ReviewVerdict: "request_changes", ReviewRunID: "wrong-run",
	}))

	require.NoError(t, p.ProcessReviewJob(context.Background(), job))
	waitForReviewJob(t, p, job)

	require.Len(t, generator.GenerateReviewCalls, 1)
	pr, err := database.GetPR(job.PR.Owner, job.PR.Repo, job.PR.Number)
	require.NoError(t, err)
	require.NotNil(t, pr)
	assert.Equal(t, "completed", pr.Status)
	assert.Equal(t, job.PR.CommitSHA, pr.LastCommitSHA)
	assert.Equal(t, generator.DefaultResult.CriticalCount, pr.CriticalCount)
	assert.Equal(t, generator.DefaultResult.MediumCount, pr.MediumCount)
	assert.Equal(t, generator.DefaultResult.LowCount, pr.LowCount)
	run, err := database.GetReviewRun(job.RunID)
	require.NoError(t, err)
	require.NotNil(t, run)
	assert.Equal(t, db.ReviewRunStatusCompleted, run.Status)
	assert.Equal(t, "success", run.TerminalCode)
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
	p.firstPassSlots = make(chan struct{}, 1)
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
	assert.Empty(t, p.firstPassSlots, "agent capacity must be acquired before first-pass capacity")

	<-p.agentSlots // allow execution to acquire the reserved slot
	waitForReviewJob(t, p, job)
	run, err = database.GetReviewRun(job.RunID)
	require.NoError(t, err)
	require.NotNil(t, run)
	assert.Equal(t, db.ReviewRunStatusCompleted, run.Status)
}

func TestSeparateAPIRunsShareProcessGlobalFirstPassCapacity(t *testing.T) {
	database := NewMockDatabase()
	generator := &blockingReviewGenerator{started: make(chan int, 2), release: make(chan struct{})}
	p := newTestPollerWithGenerator(NewMockGitHubClient(), database, NewMockReviewStorage(), generator)
	p.firstPassSlots = make(chan struct{}, 1)
	first := reviewJobWithoutAgent(t, "run-24510000000000000000000000000001")
	second := reviewJobWithoutAgent(t, "run-24510000000000000000000000000002")
	second.PR.Number = 8
	second.PR.CommitSHA = "1123456789abcdef0123456789abcdef01234567"
	for _, job := range []ReviewJob{first, second} {
		require.NoError(t, database.UpsertPR(&db.PR{
			RepoOwner: job.PR.Owner, RepoName: job.PR.Repo, PRNumber: job.PR.Number,
			LastCommitSHA: job.PR.CommitSHA, Status: "generating",
		}))
		require.NoError(t, p.ProcessReviewJob(context.Background(), job))
	}

	var activePR int
	select {
	case activePR = <-generator.started:
	case <-time.After(time.Second):
		t.Fatal("first review did not enter the generator")
	}
	select {
	case unexpected := <-generator.started:
		t.Fatalf("PR %d entered while PR %d held the global first-pass slot", unexpected, activePR)
	case <-time.After(50 * time.Millisecond):
	}
	assert.Len(t, p.firstPassSlots, 1)
	queuedRuns := 0
	for _, job := range []ReviewJob{first, second} {
		run, err := database.GetReviewRun(job.RunID)
		require.NoError(t, err)
		require.NotNil(t, run)
		if run.Status == db.ReviewRunStatusQueued && run.StartedAt == nil {
			queuedRuns++
		}
	}
	assert.Equal(t, 1, queuedRuns, "waiting capacity must not start the execution clock")

	generator.release <- struct{}{}
	select {
	case <-generator.started:
	case <-time.After(time.Second):
		t.Fatal("second review did not acquire the released first-pass slot")
	}
	generator.release <- struct{}{}
	waitForReviewJob(t, p, first)
	waitForReviewJob(t, p, second)
}

func TestSeparateAPIRunsShareProcessGlobalDispatchCapacity(t *testing.T) {
	database := NewMockDatabase()
	storage := NewMockReviewStorage()
	started := make(chan int, 2)
	release := make(chan struct{})
	storage.ReviewExistsFunc = func(ctx context.Context, _ string, _ string, prNumber int, _ string) (bool, error) {
		started <- prNumber
		select {
		case <-release:
			return false, nil
		case <-ctx.Done():
			return false, ctx.Err()
		}
	}
	p := newTestPollerFull(NewMockGitHubClient(), database, storage, NewMockReviewGenerator())
	p.dispatchSlots = make(chan struct{}, 1)
	p.firstPassSlots = make(chan struct{}, 2)
	first := reviewJobWithoutAgent(t, "run-24515000000000000000000000000001")
	second := reviewJobWithoutAgent(t, "run-24515000000000000000000000000002")
	first.Force = false
	second.Force = false
	second.PR.Number = 8
	second.PR.CommitSHA = "1123456789abcdef0123456789abcdef01234567"
	for _, job := range []ReviewJob{first, second} {
		require.NoError(t, database.UpsertPR(&db.PR{
			RepoOwner: job.PR.Owner, RepoName: job.PR.Repo, PRNumber: job.PR.Number,
			LastCommitSHA: job.PR.CommitSHA, Status: "generating",
		}))
		require.NoError(t, p.ProcessReviewJob(context.Background(), job))
	}

	var activePR int
	select {
	case activePR = <-started:
	case <-time.After(time.Second):
		t.Fatal("first review did not enter dispatch preflight")
	}
	select {
	case unexpected := <-started:
		t.Fatalf("PR %d entered preflight while PR %d held the global dispatch slot", unexpected, activePR)
	case <-time.After(50 * time.Millisecond):
	}
	for _, job := range []ReviewJob{first, second} {
		run, err := database.GetReviewRun(job.RunID)
		require.NoError(t, err)
		require.NotNil(t, run)
		assert.Equal(t, db.ReviewRunStatusQueued, run.Status)
		assert.Nil(t, run.StartedAt)
	}

	release <- struct{}{}
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("second review did not acquire the released dispatch slot")
	}
	release <- struct{}{}
	waitForReviewJob(t, p, first)
	waitForReviewJob(t, p, second)
}

func TestCapacityLifetimeReleasesFirstPassButRetainsAgentDuringPublication(t *testing.T) {
	database := NewMockDatabase()
	storage := NewMockReviewStorage()
	generator := NewMockReviewGenerator()
	p := newTestPollerFull(NewMockGitHubClient(), database, storage, generator)
	p.agentSlots = make(chan struct{}, 1)
	p.firstPassSlots = make(chan struct{}, 1)
	observed := make(chan [2]int, 1)
	storage.SaveSidecarFunc = func(context.Context, string, string, []byte) error {
		select {
		case observed <- [2]int{len(p.agentSlots), len(p.firstPassSlots)}:
		default:
		}
		return nil
	}
	job := customReviewJob(t, "run-24520000000000000000000000000001")
	require.NoError(t, database.UpsertPR(&db.PR{
		RepoOwner: job.PR.Owner, RepoName: job.PR.Repo, PRNumber: job.PR.Number,
		LastCommitSHA: job.PR.CommitSHA, Status: "generating",
	}))

	require.NoError(t, p.ProcessReviewJob(context.Background(), job))
	select {
	case capacities := <-observed:
		assert.Equal(t, [2]int{1, 0}, capacities, "publication retains agent capacity but releases first-pass capacity")
	case <-time.After(time.Second):
		t.Fatal("publication did not reach storage")
	}
	waitForReviewJob(t, p, job)
	assert.Empty(t, p.agentSlots)
	assert.Empty(t, p.firstPassSlots)
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

func TestOrganicTimeoutPreservesDetailedStageFailure(t *testing.T) {
	database := NewMockDatabase()
	p := newTestPoller(NewMockGitHubClient(), database)
	job := reviewJobWithoutAgent(t, "run-25600000000000000000000000000001")
	require.NoError(t, database.UpsertPR(&db.PR{
		RepoOwner: job.PR.Owner, RepoName: job.PR.Repo, PRNumber: job.PR.Number,
		LastCommitSHA: job.PR.CommitSHA, Status: "generating",
	}))
	execution, err := p.beginReviewExecution(job)
	require.NoError(t, err)
	require.NoError(t, database.SetPRGeneratingForReviewRun(
		job.PR.Owner, job.PR.Repo, job.PR.Number, job.PR.CommitSHA, job.PR.Title, job.PR.Author, nil, false, job.RunID,
	))
	timedOutCtx, cancel := context.WithCancelCause(context.Background())
	cancel(errReviewRunBudgetExceeded)
	stageErr := fmt.Errorf("agent failed after 57 turns: %w", context.DeadlineExceeded)

	assert.True(t, p.finishInterruptedReviewExecution(execution, timedOutCtx, "agent", stageErr))
	run, err := database.GetReviewRun(job.RunID)
	require.NoError(t, err)
	require.NotNil(t, run)
	assert.Equal(t, db.ReviewRunStatusTimedOut, run.Status)
	assert.Contains(t, run.ErrorSummary, errReviewRunBudgetExceeded.Error())
	assert.Contains(t, run.ErrorSummary, "agent failed after 57 turns")
}

func TestOrganicArtifactTimeoutProjectsBoundedPRError(t *testing.T) {
	database := NewMockDatabase()
	storage := NewMockReviewStorage()
	storage.SaveSidecarFunc = func(ctx context.Context, _ string, contentType string, _ []byte) error {
		if contentType == "text/html; charset=utf-8" {
			<-ctx.Done()
			return ctx.Err()
		}
		return nil
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

func TestRunAgentStageProjectionLossIsRecordedAsSuperseded(t *testing.T) {
	database := NewMockDatabase()
	p := newTestPoller(NewMockGitHubClient(), database)
	job := customReviewJob(t, "run-25950000000000000000000000000001")
	require.NoError(t, database.UpsertPR(&db.PR{
		RepoOwner: job.PR.Owner, RepoName: job.PR.Repo, PRNumber: job.PR.Number,
		LastCommitSHA: job.PR.CommitSHA, Status: "pending",
	}))
	execution, err := p.beginReviewExecution(job)
	require.NoError(t, err)
	require.NoError(t, database.SetPRGeneratingForReviewRun(
		job.PR.Owner, job.PR.Repo, job.PR.Number, job.PR.CommitSHA, job.PR.Title, job.PR.Author, nil, false, job.RunID,
	))
	successorRunID := "run-25950000000000000000000000000002"
	require.NoError(t, database.SetPRGeneratingForReviewRun(
		job.PR.Owner, job.PR.Repo, job.PR.Number, job.PR.CommitSHA, job.PR.Title, job.PR.Author, nil, false, successorRunID,
	))

	_, err = p.runAgentStage(context.Background(), execution, &service.ReviewResult{})
	require.ErrorIs(t, err, errReviewRunSuperseded)
	assert.True(t, p.finishSupersededReviewExecution(execution, err))

	run, getErr := database.GetReviewRun(job.RunID)
	require.NoError(t, getErr)
	require.NotNil(t, run)
	assert.Equal(t, db.ReviewRunStatusCancelled, run.Status)
	assert.Equal(t, "superseded", run.TerminalCode)
	assert.Equal(t, "projection", run.FailureStage)
	assert.Empty(t, run.LeaseHolder)
	assert.Nil(t, run.LeaseExpiresAt)
	pr, getErr := database.GetPR(job.PR.Owner, job.PR.Repo, job.PR.Number)
	require.NoError(t, getErr)
	require.NotNil(t, pr)
	assert.Equal(t, successorRunID, database.ProjectionRunIDs[prDBKey(job.PR.Owner, job.PR.Repo, job.PR.Number)])
	assert.Equal(t, "generating", pr.Status)
	assert.Empty(t, pr.ErrorMessage)
}

func TestCancelledArtifactSavePreservesResetPRState(t *testing.T) {
	database := NewMockDatabase()
	storage := NewMockReviewStorage()
	saveStarted := make(chan struct{})
	storage.SaveSidecarFunc = func(ctx context.Context, _ string, contentType string, _ []byte) error {
		if contentType == "text/html; charset=utf-8" {
			close(saveStarted)
			<-ctx.Done()
			return ctx.Err()
		}
		return nil
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
	storage.SaveSidecarFunc = func(_ context.Context, _ string, contentType string, _ []byte) error {
		if contentType == "text/html; charset=utf-8" {
			close(saveStarted)
			<-releaseSave
			return errors.New("storage unavailable")
		}
		return nil
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

	_, _, started := p.startTrackedReviewJob(job)
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
	assert.Equal(t, p.cfg.OpenRouterAPIKey, cfg.OpenRouterAPIKey)
	assert.Equal(t, p.cfg.AnthropicAPIKey, cfg.AnthropicAPIKey)
	assert.True(t, cfg.RequiredChecks)
}

func TestProviderAttemptObserverUpsertsLifecycleWithRunExecutionAttempt(t *testing.T) {
	database := NewMockDatabase()
	p := newTestPoller(NewMockGitHubClient(), database)
	job := customReviewJob(t, "run-40000000000000000000000000000001")
	exec, err := p.beginReviewExecution(job)
	require.NoError(t, err)
	started := time.Now().UTC().Add(-time.Second)
	completed := time.Now().UTC()
	observer := p.providerAttemptObserver(exec)
	require.NoError(t, observer(service.ProviderAttemptEvent{
		Stage: "first_pass", InvocationNumber: 2, AttemptNumber: 1,
		Provider: "google", Backend: "gemini_api", RequestedModel: "requested", ResolvedModel: "resolved",
		Status: "started", StartedAt: &started,
	}))
	require.NoError(t, observer(service.ProviderAttemptEvent{
		Stage: "first_pass", InvocationNumber: 2, AttemptNumber: 1,
		Provider: "google", Backend: "gemini_api", RequestedModel: "requested", ResolvedModel: "resolved",
		ObservedServedModels: []string{"served"}, PrimaryServedModel: "served", ServedModelSource: "response",
		ServingModelVerified: true, Status: "completed", AssistantTurns: 1, BudgetUnitsUsed: 9,
		TurnBudgetUnit: runconfig.TurnBudgetUnitCompletedNonReasoningItem, TurnBudgetVersion: runconfig.TurnBudgetVersion,
		InputTokens: 11, OutputTokens: 7, TotalTokens: 18,
		StartedAt: &started, CompletedAt: &completed, DurationMS: completed.Sub(started).Milliseconds(), StopReason: "completed",
	}))

	attempts, err := database.ListReviewStageAttempts(job.RunID)
	require.NoError(t, err)
	require.Len(t, attempts, 1, "started and terminal events share one durable natural key")
	attempt := attempts[0]
	assert.Equal(t, exec.ExecutionAttempt, attempt.ExecutionAttempt)
	assert.Equal(t, "first_pass", attempt.Stage)
	assert.Equal(t, 2, attempt.InvocationNumber)
	assert.Equal(t, "completed", attempt.Status)
	assert.Equal(t, []string{"served"}, attempt.ObservedServedModels)
	assert.Equal(t, 1, attempt.AssistantTurns)
	assert.Equal(t, 9, attempt.BudgetUnitsUsed)
	assert.Equal(t, runconfig.TurnBudgetUnitCompletedNonReasoningItem, attempt.TurnBudgetUnit)
	assert.Equal(t, runconfig.TurnBudgetVersion, attempt.TurnBudgetVersion)
	assert.Equal(t, int64(18), attempt.TotalTokens)
	assert.Equal(t, completed.Sub(started).Milliseconds(), attempt.DurationMS)
	assert.Equal(t, 2, database.UpsertStageAttemptAsHolderCalls)
	exec.recordProviderAttempt(service.ProviderAttemptEvent{
		Stage: "first_pass", InvocationNumber: 2, AttemptNumber: 2,
		Provider: "google", RequestedModel: "retry-success", Status: "completed",
	})
	exec.recordProviderAttempt(service.ProviderAttemptEvent{
		Stage: "first_pass", InvocationNumber: 3, AttemptNumber: 1,
		Provider: "google", RequestedModel: "failed-model", Status: "failed",
	})
	models := exec.providerModelUses()
	require.Len(t, models, 1)
	assert.Equal(t, "first_pass", models[0].Stage)
	assert.Equal(t, "retry-success", models[0].RequestedModel)
}

func TestProviderAttemptObserverRetriesAndRejectsStaleWorker(t *testing.T) {
	database := NewMockDatabase()
	p := newTestPoller(NewMockGitHubClient(), database)
	job := customReviewJob(t, "run-40000000000000000000000000000002")
	exec, err := p.beginReviewExecution(job)
	require.NoError(t, err)
	database.UpsertStageAttemptAsHolderErrors = []error{errors.New("transient 1"), errors.New("transient 2")}

	observer := p.providerAttemptObserver(exec)
	require.NoError(t, observer(service.ProviderAttemptEvent{
		Stage: "classification", InvocationNumber: 1, AttemptNumber: 1,
		Provider: "google", Backend: "gemini_api", RequestedModel: "flash", ResolvedModel: "flash", Status: "started",
	}))
	assert.Equal(t, reviewLedgerRetryAttempts, database.UpsertStageAttemptAsHolderCalls)

	successor := "successor-holder"
	require.NoError(t, database.PatchReviewRun(job.RunID, db.ReviewRunPatch{LeaseHolder: &successor}))
	err = observer(service.ProviderAttemptEvent{
		Stage: "classification", InvocationNumber: 2, AttemptNumber: 1,
		Provider: "google", Backend: "gemini_api", RequestedModel: "flash", ResolvedModel: "flash", Status: "started",
	})
	require.Error(t, err)
	require.ErrorIs(t, err, service.ErrProviderAttemptAborted)
	assert.Contains(t, err.Error(), "lease is no longer owned")
	assert.Equal(t, reviewLedgerRetryAttempts+1, database.UpsertStageAttemptAsHolderCalls, "a definitive fence rejection is not retried")
	attempts, listErr := database.ListReviewStageAttempts(job.RunID)
	require.NoError(t, listErr)
	require.Len(t, attempts, 1)
	assert.Equal(t, 1, attempts[0].InvocationNumber)
}

func TestSuccessfulFinalizationReusesFrozenArtifactTiming(t *testing.T) {
	database := NewMockDatabase()
	database.FinalizeReviewRunSuccessErrors = []error{errors.New("transient 1"), errors.New("transient 2")}
	storage := NewMockReviewStorage()
	generator := NewMockReviewGenerator()
	generator.DefaultResult.Diff = "diff --git a/a.go b/a.go"
	generator.DefaultResult.Comments = []types.LineComment{{FilePath: "SUMMARY", CommentBody: "**Verdict: approve.**"}}
	var htmlDurableAt time.Time
	storage.SaveSidecarFunc = func(_ context.Context, _ string, contentType string, _ []byte) error {
		if contentType == "text/html; charset=utf-8" {
			htmlDurableAt = time.Now().UTC()
		}
		return nil
	}
	p := newTestPollerFull(NewMockGitHubClient(), database, storage, generator)
	job := reviewJobWithoutAgent(t, "run-40000000000000000000000000000003")
	require.NoError(t, database.UpsertPR(&db.PR{
		RepoOwner: job.PR.Owner, RepoName: job.PR.Repo, PRNumber: job.PR.Number,
		LastCommitSHA: job.PR.CommitSHA, Status: "generating",
	}))
	var eventMu sync.Mutex
	events := 0
	p.EventFunc = func(string, interface{}) {
		eventMu.Lock()
		events++
		eventMu.Unlock()
	}

	require.NoError(t, p.ProcessReviewJob(context.Background(), job))
	waitForReviewJob(t, p, job)
	require.Len(t, database.FinalizeReviewRunSuccessCalls, reviewLedgerRetryAttempts)
	first := database.FinalizeReviewRunSuccessCalls[0]
	assert.False(t, first.CompletedAt.Before(htmlDurableAt), "completion is frozen only after durable HTML")
	for i, call := range database.FinalizeReviewRunSuccessCalls {
		assert.Equal(t, first.CompletedAt, call.CompletedAt)
		assert.Equal(t, first.DurationMS, call.DurationMS)
		if i > 0 {
			assert.True(t, call.LeaseCheckedAt.After(database.FinalizeReviewRunSuccessCalls[i-1].LeaseCheckedAt))
		}
	}

	run, err := database.GetReviewRun(job.RunID)
	require.NoError(t, err)
	require.NotNil(t, run)
	require.NotNil(t, run.CompletedAt)
	assert.Equal(t, first.CompletedAt, *run.CompletedAt)
	assert.Equal(t, first.DurationMS, run.DurationMS)
	assert.Equal(t, "published", run.PublicationStatus)
	pr, err := database.GetPR(job.PR.Owner, job.PR.Repo, job.PR.Number)
	require.NoError(t, err)
	require.NotNil(t, pr)
	require.NotNil(t, pr.LastReviewedAt)
	assert.Equal(t, first.CompletedAt, *pr.LastReviewedAt)
	assert.Equal(t, first.CanonicalPath, pr.ReviewHTMLPath, "the PR projection remains compatible with canonical readers")
	assert.Equal(t, first.ReviewRunJSON, pr.ReviewRunJSON)
	var projected payload.ReviewRunInfo
	require.NoError(t, json.Unmarshal([]byte(pr.ReviewRunJSON), &projected))
	assert.Equal(t, first.CompletedAt, projected.CompletedAt)
	assert.Equal(t, first.DurationMS, projected.DurationMS)

	storage.mu.Lock()
	sidecars := append([]struct {
		Filename    string
		ContentType string
		Content     []byte
	}(nil), storage.SaveSidecarCalls...)
	storage.mu.Unlock()
	var immutable payload.Payload
	found := false
	for _, sidecar := range sidecars {
		if sidecar.Filename == first.JSONPath && sidecar.ContentType == "application/json" {
			require.NoError(t, json.Unmarshal(sidecar.Content, &immutable))
			found = true
			break
		}
	}
	require.True(t, found, "immutable structured sidecar must be written")
	require.NotNil(t, immutable.ReviewRun)
	assert.Equal(t, first.CompletedAt, immutable.ReviewRun.CompletedAt)
	assert.Equal(t, first.DurationMS, immutable.ReviewRun.DurationMS)
	eventMu.Lock()
	assert.Equal(t, 2, events, "generating and published are the only updates")
	eventMu.Unlock()
}

func TestPublishedReviewRetriesCanonicalAliases(t *testing.T) {
	database := NewMockDatabase()
	storage := NewMockReviewStorage()
	aliasAttempts := 0
	storage.SaveReviewFunc = func(_ context.Context, owner, repo string, prNumber int, commitSHA string, _ []byte) (string, error) {
		aliasAttempts++
		if aliasAttempts < reviewLedgerRetryAttempts {
			return "", fmt.Errorf("transient canonical write %d", aliasAttempts)
		}
		return gcs.ReviewFileName(owner, repo, prNumber, commitSHA), nil
	}
	p := newTestPollerFull(NewMockGitHubClient(), database, storage, NewMockReviewGenerator())
	job := reviewJobWithoutAgent(t, "run-4000000000000000000000000000000b")
	require.NoError(t, database.UpsertPR(&db.PR{
		RepoOwner: job.PR.Owner, RepoName: job.PR.Repo, PRNumber: job.PR.Number,
		LastCommitSHA: job.PR.CommitSHA, Status: "generating",
	}))

	require.NoError(t, p.ProcessReviewJob(context.Background(), job))
	waitForReviewJob(t, p, job)

	require.Len(t, storage.SaveReviewCalls, reviewLedgerRetryAttempts)
	assert.Equal(t, reviewLedgerRetryAttempts, aliasAttempts)
	require.Len(t, database.FinalizeReviewRunSuccessCalls, 1)
	finalization := database.FinalizeReviewRunSuccessCalls[0]
	assert.Equal(t, gcs.ReviewFileName(job.PR.Owner, job.PR.Repo, job.PR.Number, job.PR.CommitSHA), finalization.CanonicalPath)
	pr, err := database.GetPR(job.PR.Owner, job.PR.Repo, job.PR.Number)
	require.NoError(t, err)
	require.NotNil(t, pr)
	assert.Equal(t, finalization.CanonicalPath, pr.ReviewHTMLPath)

	canonicalJSON := gcs.ReviewJSONFileName(finalization.CanonicalPath)
	storage.mu.Lock()
	defer storage.mu.Unlock()
	foundCanonicalSidecar := false
	for _, call := range storage.SaveSidecarCalls {
		if call.Filename == canonicalJSON && call.ContentType == "application/json" {
			foundCanonicalSidecar = true
			break
		}
	}
	assert.True(t, foundCanonicalSidecar, "the promised findings URL must be durable after retry")
}

func TestImmutableJSONFailurePreventsSuccessfulFinalization(t *testing.T) {
	database := NewMockDatabase()
	storage := NewMockReviewStorage()
	storage.SaveSidecarFunc = func(_ context.Context, _ string, contentType string, _ []byte) error {
		if contentType == "application/json" {
			return errors.New("immutable JSON unavailable")
		}
		return nil
	}
	p := newTestPollerFull(NewMockGitHubClient(), database, storage, NewMockReviewGenerator())
	job := reviewJobWithoutAgent(t, "run-40000000000000000000000000000007")
	require.NoError(t, database.UpsertPR(&db.PR{
		RepoOwner: job.PR.Owner, RepoName: job.PR.Repo, PRNumber: job.PR.Number,
		LastCommitSHA: job.PR.CommitSHA, Status: "generating",
	}))

	require.NoError(t, p.ProcessReviewJob(context.Background(), job))
	waitForReviewJob(t, p, job)
	assert.Empty(t, database.FinalizeReviewRunSuccessCalls)
	assert.Empty(t, storage.SaveReviewCalls, "failed immutable artifacts must not update canonical aliases")
	run, err := database.GetReviewRun(job.RunID)
	require.NoError(t, err)
	require.NotNil(t, run)
	assert.Equal(t, db.ReviewRunStatusFailed, run.Status)
	assert.Equal(t, "artifact_save_failed", run.TerminalCode)
	assert.Empty(t, run.JSONPath, "a missing immutable sidecar must never be advertised")
	pr, err := database.GetPR(job.PR.Owner, job.PR.Repo, job.PR.Number)
	require.NoError(t, err)
	require.NotNil(t, pr)
	assert.Equal(t, "error", pr.Status)
}

func TestFinalizationRetryExhaustionTerminalizesRunAndProjectsError(t *testing.T) {
	database := NewMockDatabase()
	database.FinalizeReviewRunSuccessErrors = []error{
		errors.New("database unavailable 1"), errors.New("database unavailable 2"), errors.New("database unavailable 3"),
	}
	storage := NewMockReviewStorage()
	p := newTestPollerFull(NewMockGitHubClient(), database, storage, NewMockReviewGenerator())
	job := reviewJobWithoutAgent(t, "run-40000000000000000000000000000008")
	require.NoError(t, database.UpsertPR(&db.PR{
		RepoOwner: job.PR.Owner, RepoName: job.PR.Repo, PRNumber: job.PR.Number,
		LastCommitSHA: job.PR.CommitSHA, Status: "generating",
	}))

	require.NoError(t, p.ProcessReviewJob(context.Background(), job))
	waitForReviewJob(t, p, job)
	require.Len(t, database.FinalizeReviewRunSuccessCalls, reviewLedgerRetryAttempts)
	assert.Empty(t, storage.SaveReviewCalls, "failed finalization must not update canonical aliases")
	run, err := database.GetReviewRun(job.RunID)
	require.NoError(t, err)
	require.NotNil(t, run)
	assert.Equal(t, db.ReviewRunStatusFailed, run.Status)
	assert.Equal(t, "finalization_failed", run.TerminalCode)
	assert.Equal(t, "publication", run.FailureStage)
	assert.Empty(t, run.LeaseHolder)
	pr, err := database.GetPR(job.PR.Owner, job.PR.Repo, job.PR.Number)
	require.NoError(t, err)
	require.NotNil(t, pr)
	assert.Equal(t, "error", pr.Status)
	assert.Contains(t, pr.ErrorMessage, "database unavailable 3")
}

func TestStaleWorkerCannotFinalizeOrBroadcastAfterArtifactSave(t *testing.T) {
	database := NewMockDatabase()
	storage := NewMockReviewStorage()
	job := reviewJobWithoutAgent(t, "run-40000000000000000000000000000004")
	storage.SaveSidecarFunc = func(_ context.Context, _ string, contentType string, _ []byte) error {
		if contentType == "text/html; charset=utf-8" {
			successor := "successor-holder"
			if err := database.PatchReviewRun(job.RunID, db.ReviewRunPatch{LeaseHolder: &successor}); err != nil {
				return err
			}
		}
		return nil
	}
	p := newTestPollerFull(NewMockGitHubClient(), database, storage, NewMockReviewGenerator())
	require.NoError(t, database.UpsertPR(&db.PR{
		RepoOwner: job.PR.Owner, RepoName: job.PR.Repo, PRNumber: job.PR.Number,
		LastCommitSHA: job.PR.CommitSHA, Status: "generating",
	}))
	var eventMu sync.Mutex
	events := 0
	p.EventFunc = func(string, interface{}) {
		eventMu.Lock()
		events++
		eventMu.Unlock()
	}

	require.NoError(t, p.ProcessReviewJob(context.Background(), job))
	waitForReviewJob(t, p, job)
	require.Len(t, database.FinalizeReviewRunSuccessCalls, 1)
	run, err := database.GetReviewRun(job.RunID)
	require.NoError(t, err)
	require.NotNil(t, run)
	assert.Equal(t, db.ReviewRunStatusRunning, run.Status)
	assert.Equal(t, "successor-holder", run.LeaseHolder)
	pr, err := database.GetPR(job.PR.Owner, job.PR.Repo, job.PR.Number)
	require.NoError(t, err)
	require.NotNil(t, pr)
	assert.Equal(t, "generating", pr.Status)
	assert.Empty(t, pr.ReviewRunID)
	assert.Empty(t, storage.SaveReviewCalls, "a fenced-out run must not overwrite the canonical alias")
	eventMu.Lock()
	assert.Equal(t, 1, events, "a fenced-out finalization must not emit a completion update")
	eventMu.Unlock()
}

func TestSupersededFinalizationCompletesWithoutBroadcast(t *testing.T) {
	database := NewMockDatabase()
	storage := NewMockReviewStorage()
	job := reviewJobWithoutAgent(t, "run-40000000000000000000000000000005")
	successorRunID := "run-40000000000000000000000000000006"
	storage.SaveSidecarFunc = func(_ context.Context, _ string, contentType string, _ []byte) error {
		if contentType == "text/html; charset=utf-8" {
			if err := database.SetPRGeneratingForReviewRun(
				job.PR.Owner, job.PR.Repo, job.PR.Number, job.PR.CommitSHA,
				job.PR.Title, job.PR.Author, nil, false, successorRunID,
			); err != nil {
				return err
			}
		}
		return nil
	}
	p := newTestPollerFull(NewMockGitHubClient(), database, storage, NewMockReviewGenerator())
	require.NoError(t, database.UpsertPR(&db.PR{
		RepoOwner: job.PR.Owner, RepoName: job.PR.Repo, PRNumber: job.PR.Number,
		LastCommitSHA: job.PR.CommitSHA, Status: "generating",
	}))
	var eventMu sync.Mutex
	events := 0
	p.EventFunc = func(string, interface{}) {
		eventMu.Lock()
		events++
		eventMu.Unlock()
	}

	require.NoError(t, p.ProcessReviewJob(context.Background(), job))
	waitForReviewJob(t, p, job)
	run, err := database.GetReviewRun(job.RunID)
	require.NoError(t, err)
	require.NotNil(t, run)
	assert.Equal(t, db.ReviewRunStatusCompleted, run.Status)
	assert.Equal(t, "success", run.TerminalCode)
	assert.Equal(t, "superseded", run.PublicationStatus)
	assert.Empty(t, run.LeaseHolder)
	pr, err := database.GetPR(job.PR.Owner, job.PR.Repo, job.PR.Number)
	require.NoError(t, err)
	require.NotNil(t, pr)
	assert.Equal(t, "generating", pr.Status)
	assert.Equal(t, successorRunID, database.ProjectionRunIDs[prDBKey(job.PR.Owner, job.PR.Repo, job.PR.Number)])
	assert.Empty(t, pr.ReviewRunID)
	assert.Empty(t, storage.SaveReviewCalls, "a superseded run must not overwrite the canonical alias")
	eventMu.Lock()
	assert.Equal(t, 1, events, "a superseded success must not emit a completion update")
	eventMu.Unlock()
}

func TestSupersededOlderCommitPreservesNonCollidingCanonicalAlias(t *testing.T) {
	database := NewMockDatabase()
	storage := NewMockReviewStorage()
	job := reviewJobWithoutAgent(t, "run-40000000000000000000000000000009")
	const successorRunID = "run-4000000000000000000000000000000a"
	const successorSHA = "fedcba9876543210fedcba9876543210fedcba98"
	storage.SaveSidecarFunc = func(_ context.Context, _ string, contentType string, _ []byte) error {
		if contentType == "text/html; charset=utf-8" {
			return database.SetPRGeneratingForReviewRun(
				job.PR.Owner, job.PR.Repo, job.PR.Number, successorSHA,
				job.PR.Title, job.PR.Author, nil, false, successorRunID,
			)
		}
		return nil
	}
	p := newTestPollerFull(NewMockGitHubClient(), database, storage, NewMockReviewGenerator())
	require.NoError(t, database.UpsertPR(&db.PR{
		RepoOwner: job.PR.Owner, RepoName: job.PR.Repo, PRNumber: job.PR.Number,
		LastCommitSHA: job.PR.CommitSHA, Status: "generating",
	}))

	require.NoError(t, p.ProcessReviewJob(context.Background(), job))
	waitForReviewJob(t, p, job)
	run, err := database.GetReviewRun(job.RunID)
	require.NoError(t, err)
	require.NotNil(t, run)
	assert.Equal(t, "superseded", run.PublicationStatus)
	require.Len(t, storage.SaveReviewCalls, 1, "an older commit may safely retain its sha-only alias")
	assert.Equal(t, job.PR.CommitSHA, storage.SaveReviewCalls[0].CommitSHA)
	pr, err := database.GetPR(job.PR.Owner, job.PR.Repo, job.PR.Number)
	require.NoError(t, err)
	require.NotNil(t, pr)
	assert.Equal(t, successorSHA, pr.LastCommitSHA)
	assert.Equal(t, successorRunID, database.ProjectionRunIDs[prDBKey(job.PR.Owner, job.PR.Repo, job.PR.Number)])
}

func TestLocalReviewAliasPreservesImmutableRunHistory(t *testing.T) {
	pr := &db.PR{
		RepoOwner: "acme", RepoName: "widgets", PRNumber: 7,
		LastCommitSHA: "abcdef0123456789",
		ReviewHTMLPath: gcs.ReviewRunFileName(
			"acme", "widgets", 7, "abcdef0123456789", "run-0123456789abcdef0123456789abcdef",
		),
	}
	if got, want := localReviewAlias(*pr), gcs.ReviewFileName("acme", "widgets", 7, pr.LastCommitSHA); got != want {
		t.Fatalf("localReviewAlias(run path) = %q, want canonical alias %q", got, want)
	}

	pr.ReviewHTMLPath = "legacy-custom-review.html"
	if got := localReviewAlias(*pr); got != pr.ReviewHTMLPath {
		t.Fatalf("localReviewAlias(legacy path) = %q, want %q", got, pr.ReviewHTMLPath)
	}
}
