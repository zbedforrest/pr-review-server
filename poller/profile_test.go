package poller

import (
	"context"
	"encoding/json"
	"strings"
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

// snapshotFor freezes an already-resolved effective config without
// re-resolving it, so profile fields survive exactly as given.
func snapshotFor(t *testing.T, effective runconfig.Effective) runconfig.Snapshot {
	t.Helper()
	hash, err := runconfig.Hash(effective)
	require.NoError(t, err)
	return runconfig.Snapshot{Effective: effective, Hash: hash, Sources: map[string]string{}}
}

func liteReviewJob(t *testing.T, runID, profile string) ReviewJob {
	t.Helper()
	base := customReviewJob(t, runID).Config.Effective
	base.Agent.Backend, base.Agent.Model, base.Agent.Effort = service.AgentBackendClaude, "claude-fable-5", "medium"
	base.Agent.TurnBudgetUnit, base.Agent.TurnBudgetVersion = runconfig.TurnBudgetSemantics(service.AgentBackendClaude)
	expanded, err := runconfig.Expand(profile, base)
	require.NoError(t, err)
	job := customReviewJob(t, runID)
	job.Config = snapshotFor(t, expanded)
	return job
}

// liteAutoReviewFixture enables the agent with budgets that admit both lite
// profiles, the way prod is configured.
func liteAutoReviewFixture(t *testing.T) autoReviewFixture {
	t.Helper()
	f := newAutoReviewFixture(t, true, "*")
	f.p.cfg.AgenticReviews = true
	f.p.cfg.AgentWallClockSec = 900
	f.p.cfg.AgentMaxTurns = 120
	return f
}

func TestLiteJobSnapshotDisablesFirstPass(t *testing.T) {
	job := liteReviewJob(t, "run-51000000000000000000000000000001", runconfig.ProfileLite)
	require.NoError(t, job.Validate())
	assert.Equal(t, runconfig.ProfileLite, job.Config.Effective.Profile)
	assert.False(t, job.Config.Effective.FirstPass.Enabled)
	assert.Equal(t, 0, job.Config.Effective.FirstPass.Samples)
}

func TestReviewJobValidateRejectsConfigWithNeitherStage(t *testing.T) {
	job := liteReviewJob(t, "run-51000000000000000000000000000002", runconfig.ProfileLite)
	effective := job.Config.Effective
	effective.Agent.Enabled = false
	hash, err := runconfig.Hash(effective)
	require.NoError(t, err)
	job.Config.Effective, job.Config.Hash = effective, hash
	assert.ErrorContains(t, job.Validate(), "incomplete")
}

func TestGenerateReviewJobs_LiteSkipsFirstPassAndFirstPassSlot(t *testing.T) {
	database := NewMockDatabase()
	generator := NewMockReviewGenerator()
	p := newTestPollerFull(NewMockGitHubClient(), database, NewMockReviewStorage(), generator)
	p.firstPassSlots = make(chan struct{}, 1)
	p.firstPassSlots <- struct{}{}
	job := liteReviewJob(t, "run-51000000000000000000000000000003", runconfig.ProfileLite)
	require.NoError(t, database.UpsertPR(&db.PR{
		RepoOwner: job.PR.Owner, RepoName: job.PR.Repo, PRNumber: job.PR.Number, LastCommitSHA: job.PR.CommitSHA, Status: "generating",
	}))

	require.NoError(t, p.ProcessReviewJob(context.Background(), job))
	waitForReviewJob(t, p, job)

	run := waitForReviewRunStatus(t, database, job.RunID, db.ReviewRunStatusCompleted)
	assert.Equal(t, runconfig.ProfileLite, run.Profile, "the profile column follows the effective config")
	assert.Len(t, p.firstPassSlots, 1, "a lite run must neither wait for nor take a first-pass slot")
	generator.mu.Lock()
	require.Len(t, generator.GenerateReviewCalls, 1)
	assert.Equal(t, 0, generator.GenerateReviewCalls[0].NRequests)
	generator.mu.Unlock()

	pr, err := database.GetPR(job.PR.Owner, job.PR.Repo, job.PR.Number)
	require.NoError(t, err)
	var metadata payload.ReviewRunInfo
	require.NoError(t, json.Unmarshal([]byte(pr.ReviewRunJSON), &metadata))
	assert.Equal(t, runconfig.ProfileLite, metadata.Profile)
	assert.Equal(t, "Lite", metadata.ProfileLabel)
	assert.Equal(t, runconfig.ProfileLite, metadata.Config.Effective.Profile)
}

func TestEnsureReviewRunRecordsProfileColumn(t *testing.T) {
	database := NewMockDatabase()
	p := newTestPollerFull(NewMockGitHubClient(), database, NewMockReviewStorage(), NewMockReviewGenerator())
	p.cfg.AgentWallClockSec = 900
	job := liteReviewJob(t, "run-51000000000000000000000000000004", runconfig.ProfileLitePlus)
	require.NoError(t, p.ensureReviewRun(job))
	run, err := database.GetReviewRun(job.RunID)
	require.NoError(t, err)
	require.NotNil(t, run)
	assert.Equal(t, runconfig.ProfileLitePlus, run.Profile)
	assert.Equal(t, 600, run.AgentWallClockSec)
	assert.Equal(t, 120, run.AgentMaxTurns)
}

func TestAgentConfigForExecutionFollowsTheProfile(t *testing.T) {
	p := newTestPoller(NewMockGitHubClient(), NewMockDatabase())
	p.bugMemory = &service.BugMemoryLibrary{Version: "v1"}
	lite := liteReviewJob(t, "run-51000000000000000000000000000005", runconfig.ProfileLitePlus)
	cfg := p.agentConfigForExecution(&reviewExecution{Job: lite}, "tok")
	assert.Equal(t, runconfig.ToolsWithAgent, cfg.Tools)
	assert.Equal(t, runconfig.PromptLiteArmASub, cfg.Prompt)
	assert.True(t, cfg.SkipGates)
	assert.True(t, cfg.CollectCitedFiles)
	assert.False(t, cfg.RequiredChecks)
	assert.Same(t, p.bugMemory, cfg.BugMemory)
	assert.Equal(t, 600*time.Second, cfg.WallClock)

	full := customReviewJob(t, "run-51000000000000000000000000000006")
	fullCfg := p.agentConfigForExecution(&reviewExecution{Job: full}, "tok")
	assert.Equal(t, runconfig.ToolsDefault, fullCfg.Tools)
	assert.Equal(t, runconfig.PromptPipeline, fullCfg.Prompt)
	assert.False(t, fullCfg.SkipGates)
	assert.False(t, fullCfg.CollectCitedFiles)
	assert.True(t, fullCfg.RequiredChecks)

	noMemory := full
	effective := noMemory.Config.Effective
	effective.BugMemory = false
	noMemory.Config = snapshotFor(t, effective)
	assert.Nil(t, p.agentConfigForExecution(&reviewExecution{Job: noMemory}, "tok").BugMemory)
}

func TestProfileLabelAndHeader(t *testing.T) {
	base := customReviewJob(t, "run-51000000000000000000000000000007").Config.Effective
	lite, err := runconfig.Expand(runconfig.ProfileLite, base)
	require.NoError(t, err)
	exact := runconfig.DescribeProfile(lite, base)
	assert.Equal(t, "Lite", profileLabel(exact))

	lite.Agent.Effort = "high"
	custom := runconfig.DescribeProfile(lite, base)
	assert.Equal(t, "Custom (based on Lite): effort high (default medium)", profileLabel(custom))

	result := &service.ReviewResult{}
	applyProfileHeader(result, &reviewExecution{Profile: custom})
	assert.Equal(t, "Custom (based on Lite)", result.ProfileTitle)
	assert.Equal(t, []string{"effort high (default medium)"}, result.ProfileDeviations)
	assert.Empty(t, profileLabel(runconfig.ProfileDescription{}))
}

func TestParseAutoReviewProfilePolicy(t *testing.T) {
	policy, err := ParseAutoReviewProfilePolicy(`{"synchronize":"lite","poll_fallback":"Lite_Plus","repos":{"Acme/Example":{"synchronize":"full"}}}`)
	require.NoError(t, err)
	assert.Equal(t, "lite", policy.ProfileFor("synchronize", "other", "repo", "full"))
	assert.Equal(t, "lite_plus", policy.ProfileFor("poll_fallback", "other", "repo", "full"))
	assert.Equal(t, "full", policy.ProfileFor("synchronize", "acme", "example", "lite"), "repo override wins")
	assert.Equal(t, "lite", policy.ProfileFor("opened", "acme", "example", "lite"), "unmapped trigger uses the default")
	assert.JSONEq(t, `{"synchronize":"lite","poll_fallback":"lite_plus","repos":{"acme/example":{"synchronize":"full"}}}`, policy.JSON())
	assert.Equal(t, "", policy.Map()["opened"])

	empty, err := ParseAutoReviewProfilePolicy("  ")
	require.NoError(t, err)
	assert.Equal(t, "", empty.JSON())

	for _, bad := range []string{`{"pushed":"lite"}`, `{"synchronize":"turbo"}`, `{"repos":{"acme":{"synchronize":"lite"}}}`, `{"repos":{"a/b":{"nope":"lite"}}}`, `[1]`, `{"synchronize":3}`} {
		_, err := ParseAutoReviewProfilePolicy(bad)
		assert.Error(t, err, bad)
	}
}

func TestAutoReviewProfileFor_DefaultsRepoOverrideAndAuthorGate(t *testing.T) {
	database := NewMockDatabase()
	p := newTestPoller(NewMockGitHubClient(), database)
	assert.Equal(t, "full", p.autoReviewProfileFor("synchronize", "acme", "example", "alice"), "compiled default is full")

	p.cfg.ReviewDefaultProfile = "lite"
	assert.Equal(t, "full", p.autoReviewProfileFor("synchronize", "acme", "example", "alice"), "lite needs the author allowlist")
	require.NoError(t, database.SetSetting(SettingAutoReviewLiteAuthors, "Alice"))
	assert.Equal(t, "lite", p.autoReviewProfileFor("synchronize", "acme", "example", "alice"))
	assert.Equal(t, "full", p.autoReviewProfileFor("synchronize", "acme", "example", "bob"))

	require.NoError(t, database.SetSetting(SettingAutoReviewProfileByTrigger, `{"ready_for_review":"full","synchronize":"lite_plus","repos":{"acme/example":{"synchronize":"full"}}}`))
	assert.Equal(t, "full", p.autoReviewProfileFor("ready_for_review", "acme", "other", "alice"))
	assert.Equal(t, "lite_plus", p.autoReviewProfileFor("synchronize", "acme", "other", "alice"))
	assert.Equal(t, "full", p.autoReviewProfileFor("synchronize", "acme", "example", "alice"), "repo override")
	assert.Equal(t, "lite", p.autoReviewProfileFor("opened", "acme", "example", "alice"), "unmapped trigger uses the deployment default")

	require.NoError(t, database.SetSetting(SettingAutoReviewLiteAuthors, "*"))
	assert.Equal(t, "lite_plus", p.autoReviewProfileFor("synchronize", "acme", "other", "carol"))
	require.NoError(t, database.SetSetting(SettingAutoReviewLiteAuthors, ""))
	assert.Equal(t, "full", p.autoReviewProfileFor("synchronize", "acme", "other", "carol"), "empty allowlist is the kill switch")

	require.NoError(t, database.SetSetting(SettingAutoReviewProfileByTrigger, `not json`))
	require.NoError(t, database.SetSetting(SettingAutoReviewLiteAuthors, "*"))
	assert.Equal(t, "lite", p.autoReviewProfileFor("synchronize", "acme", "other", "carol"), "unparseable mapping falls back to the default")
	p.cfg.ReviewDefaultProfile = "bogus"
	assert.Equal(t, "full", p.autoReviewProfileFor("synchronize", "acme", "other", "carol"))
}

func autoReviewRunProfile(t *testing.T, f autoReviewFixture) string {
	t.Helper()
	runs := f.runs()
	require.Len(t, runs, 1)
	var effective runconfig.Effective
	require.NoError(t, json.Unmarshal([]byte(runs[0].EffectiveConfigJSON), &effective))
	assert.Equal(t, effective.Profile, runs[0].Profile)
	return runs[0].Profile
}

func TestAdmitAutoReviewIntentUsesTriggerProfile(t *testing.T) {
	f := liteAutoReviewFixture(t)
	require.NoError(t, f.db.SetSetting(SettingAutoReviewProfileByTrigger, `{"synchronize":"lite"}`))
	require.NoError(t, f.db.SetSetting(SettingAutoReviewLiteAuthors, "alice"))

	require.NoError(t, f.p.HandleWebhookDelivery(context.Background(), readyDelivery("synchronize", autoReviewNewHead, false)))
	waitForDetachedReviews(t, f.p)

	assert.Equal(t, runconfig.ProfileLite, autoReviewRunProfile(t, f))
	require.Len(t, f.generator.GenerateReviewCalls, 1)
	assert.Equal(t, 0, f.generator.GenerateReviewCalls[0].NRequests, "lite runs no first pass")
	assert.Equal(t, autoReviewTriggerSource, f.runs()[0].TriggerSource)
}

func TestAdmitAutoReviewIntentFallsBackToFullOutsideLiteAuthors(t *testing.T) {
	f := liteAutoReviewFixture(t)
	require.NoError(t, f.db.SetSetting(SettingAutoReviewProfileByTrigger, `{"synchronize":"lite"}`))
	require.NoError(t, f.db.SetSetting(SettingAutoReviewLiteAuthors, "bob"))

	require.NoError(t, f.p.HandleWebhookDelivery(context.Background(), readyDelivery("synchronize", autoReviewNewHead, false)))
	waitForDetachedReviews(t, f.p)

	assert.Equal(t, runconfig.ProfileFull, autoReviewRunProfile(t, f))
}

func TestWebhookSynchronizeAdmitsLiteAndSupersedesQueuedOlderHead(t *testing.T) {
	f := liteAutoReviewFixture(t)
	f.p.cfg.ReviewDefaultProfile = runconfig.ProfileLite
	require.NoError(t, f.db.SetSetting(SettingAutoReviewLiteAuthors, "*"))
	older := db.AutoReviewIntent{RepoOwner: "acme", RepoName: "example", PRNumber: 7, HeadSHA: autoReviewOldHead, Trigger: "ready_for_review"}
	_, err := f.db.EnsureAutoReviewIntent(&older, nil)
	require.NoError(t, err)

	require.NoError(t, f.p.HandleWebhookDelivery(context.Background(), readyDelivery("synchronize", autoReviewNewHead, false)))
	waitForDetachedReviews(t, f.p)

	byHead := map[string]db.AutoReviewIntent{}
	for _, intent := range f.intents(t) {
		byHead[intent.HeadSHA] = intent
	}
	assert.Equal(t, db.AutoReviewIntentSuperseded, byHead[autoReviewOldHead].Status)
	assert.Equal(t, db.AutoReviewIntentRunning, byHead[autoReviewNewHead].Status)
	assert.Equal(t, runconfig.ProfileLite, autoReviewRunProfile(t, f))
	assert.Equal(t, autoReviewNewHead, f.runs()[0].CommitSHA)
}

func TestBuildPublishRound_CarriesProfile(t *testing.T) {
	pr := github.PullRequest{Owner: "a", Repo: "b", Number: 1}
	assert.Empty(t, buildPublishRound(pr, payload.Payload{}, nil, nil, nil, "").ProfileFooter, "legacy sidecar")
	assert.Empty(t, buildPublishRound(pr, payload.Payload{ReviewRun: &payload.ReviewRunInfo{Profile: "full", ProfileLabel: "Full"}}, nil, nil, nil, "").ProfileFooter)
	assert.Equal(t, "PRism Lite", buildPublishRound(pr, payload.Payload{ReviewRun: &payload.ReviewRunInfo{Profile: "lite", ProfileLabel: "Lite"}}, nil, nil, nil, "").ProfileFooter)
	assert.Equal(t, "PRism Lite+ (custom)", buildPublishRound(pr, payload.Payload{ReviewRun: &payload.ReviewRunInfo{Profile: "lite_plus", ProfileLabel: "Custom (based on Lite+): effort high (default medium)"}}, nil, nil, nil, "").ProfileFooter)
	assert.Equal(t, "PRism Full (custom)", buildPublishRound(pr, payload.Payload{ReviewRun: &payload.ReviewRunInfo{Profile: "full", ProfileLabel: "Custom (based on Full): required checks off (default on)"}}, nil, nil, nil, "").ProfileFooter)
}

func TestDefaultReviewProfileFollowsDeploymentFlag(t *testing.T) {
	p := newTestPoller(NewMockGitHubClient(), NewMockDatabase())
	p.cfg.AgenticReviews = true
	p.cfg.AgentModel = "claude-fable-5"
	p.cfg.AgentWallClockSec = 900
	p.cfg.AgentMaxTurns = 120
	job, err := p.defaultReviewJob(github.PullRequest{Owner: "acme", Repo: "widgets", Number: 7, CommitSHA: strings.Repeat("0", 40)}, false, "poller")
	require.NoError(t, err)
	assert.Equal(t, runconfig.ProfileFull, job.Config.Effective.Profile)
	assert.True(t, job.Config.Effective.FirstPass.Enabled)

	p.cfg.ReviewDefaultProfile = "lite"
	job, err = p.defaultReviewJob(github.PullRequest{Owner: "acme", Repo: "widgets", Number: 7, CommitSHA: strings.Repeat("0", 40)}, false, "poller")
	require.NoError(t, err)
	assert.Equal(t, runconfig.ProfileLite, job.Config.Effective.Profile)
	assert.Equal(t, runconfig.LiteModel, job.Config.Effective.Agent.Model, "the lite model is admitted even when it is not the deployment model")
	assert.False(t, job.Config.Effective.FirstPass.Enabled)
	assert.Equal(t, runconfig.SourceDeploymentDefault, job.Config.Sources["profile"])
	_, _, err = p.ReviewConfigDefaultsAndPolicy()
	require.NoError(t, err)
}
