package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"pr-review-server/db"
	"pr-review-server/pkg/reviewer/runconfig"
	"pr-review-server/poller"
)

func patchSettings(t *testing.T, server *Server, body string) *httptest.ResponseRecorder {
	t.Helper()
	w := httptest.NewRecorder()
	server.handleSettings(w, addUserToRequest(httptest.NewRequest(http.MethodPatch, "/api/settings", strings.NewReader(body)), &db.User{GitHubUsername: "tester"}))
	return w
}

func TestSettings_AutoReviewProfileByTriggerRoundTrip(t *testing.T) {
	server, database := newTestServer(t, "tester")
	w := patchSettings(t, server, `{"auto_review_profile_by_trigger":{"ready_for_review":"full","synchronize":"lite","poll_fallback":"lite_plus","repos":{"Acme/Example":{"synchronize":"full"}}}}`)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())

	var got map[string]any
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &got))
	mapping := got["auto_review_profile_by_trigger"].(map[string]any)
	assert.Equal(t, "full", mapping["ready_for_review"])
	assert.Equal(t, "", mapping["opened"], "unset triggers stay empty so the default keeps applying")
	assert.Equal(t, "lite", mapping["synchronize"])
	assert.Equal(t, "lite_plus", mapping["poll_fallback"])
	assert.Equal(t, map[string]any{"acme/example": map[string]any{"synchronize": "full"}}, mapping["repos"])
	assert.Equal(t, "full", got["review_default_profile"])
	assert.Equal(t, []any{"full", "lite", "lite_plus"}, got["review_profiles"])

	stored, err := database.GetSetting(poller.SettingAutoReviewProfileByTrigger)
	require.NoError(t, err)
	assert.JSONEq(t, `{"ready_for_review":"full","synchronize":"lite","poll_fallback":"lite_plus","repos":{"acme/example":{"synchronize":"full"}}}`, stored)

	// The string form of the same mapping is accepted too.
	w = patchSettings(t, server, `{"auto_review_profile_by_trigger":"{\"synchronize\":\"full\"}"}`)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	stored, _ = database.GetSetting(poller.SettingAutoReviewProfileByTrigger)
	assert.JSONEq(t, `{"synchronize":"full"}`, stored)
}

func TestSettings_AutoReviewProfileByTriggerRejectsUnknownTriggerOrProfile(t *testing.T) {
	server, database := newTestServer(t, "tester")
	for _, body := range []string{
		`{"auto_review_profile_by_trigger":{"pushed":"lite"}}`,
		`{"auto_review_profile_by_trigger":{"synchronize":"turbo"}}`,
		`{"auto_review_profile_by_trigger":{"repos":{"acme":{"synchronize":"lite"}}}}`,
		`{"auto_review_profile_by_trigger":"nonsense"}`,
	} {
		w := patchSettings(t, server, body)
		assert.Equal(t, http.StatusBadRequest, w.Code, body)
		assert.Contains(t, w.Body.String(), "auto_review_profile_by_trigger", body)
	}
	stored, err := database.GetSetting(poller.SettingAutoReviewProfileByTrigger)
	require.NoError(t, err)
	assert.Empty(t, stored, "rejected writes must not touch the setting")
}

func TestSettings_AutoReviewLiteAuthorsNormalizesLogins(t *testing.T) {
	server, database := newTestServer(t, "tester")
	w := patchSettings(t, server, `{"auto_review_lite_authors":" Alice, bob ,*"}`)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	var got map[string]any
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &got))
	assert.Equal(t, "alice,bob,*", got["auto_review_lite_authors"])
	stored, _ := database.GetSetting(poller.SettingAutoReviewLiteAuthors)
	assert.Equal(t, "alice,bob,*", stored)

	w = patchSettings(t, server, `{"auto_review_lite_authors":"not a login!"}`)
	assert.Equal(t, http.StatusBadRequest, w.Code)
	assert.Contains(t, w.Body.String(), "auto_review_lite_authors")
}

func TestSettings_ReviewDefaultProfileReflectsDeploymentFlag(t *testing.T) {
	server, _ := newTestServer(t, "tester")
	server.cfg.ReviewDefaultProfile = "lite"
	w := httptest.NewRecorder()
	server.handleSettings(w, httptest.NewRequest(http.MethodGet, "/api/settings", nil))
	require.Equal(t, http.StatusOK, w.Code)
	var got map[string]any
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &got))
	assert.Equal(t, "lite", got["review_default_profile"])
	assert.Equal(t, "", got["auto_review_lite_authors"])
}

func TestCreateReviewRunAcceptsProfileOverride(t *testing.T) {
	headSHA := "0123456789abcdef0123456789abcdef01234567"
	s, database, apiPoller, userID := newReviewAPIServer(t, githubPRResponse(headSHA))
	body := `{"target":{"owner":"acme","repo":"widgets","pull_request":42,"expected_head_sha":"` + headSHA + `"},"publish":false,"config":{"profile":"lite"}}`
	recorder := httptest.NewRecorder()
	s.handleReviewRuns(recorder, addReviewAPIUser(httptest.NewRequest(http.MethodPost, reviewRunsPath, strings.NewReader(body)), *userID))
	require.Equal(t, http.StatusAccepted, recorder.Code, recorder.Body.String())
	require.Len(t, apiPoller.jobs, 1)
	effective := apiPoller.jobs[0].Config.Effective
	assert.Equal(t, runconfig.ProfileLite, effective.Profile)
	assert.False(t, effective.FirstPass.Enabled)
	assert.Equal(t, runconfig.ToolsDefault, effective.Agent.Tools)
	assert.Equal(t, 300, effective.Agent.WallClockSeconds)
	assert.Equal(t, runconfig.SourceRequest, apiPoller.jobs[0].Config.Sources["profile"])
	assert.Contains(t, recorder.Body.String(), `"profile":"lite"`)

	persisted, err := database.GetReviewRun(apiPoller.jobs[0].RunID)
	require.NoError(t, err)
	assert.Equal(t, runconfig.ProfileLite, persisted.Profile)

	bad := strings.Replace(body, `"lite"`, `"turbo"`, 1)
	recorder = httptest.NewRecorder()
	s.handleReviewRuns(recorder, addReviewAPIUser(httptest.NewRequest(http.MethodPost, reviewRunsPath, strings.NewReader(bad)), *userID))
	assert.Equal(t, http.StatusUnprocessableEntity, recorder.Code)
	assert.Contains(t, recorder.Body.String(), "profile")

	mixed := strings.Replace(body, `{"profile":"lite"}`, `{"profile":"lite","first_pass":{"samples":2}}`, 1)
	recorder = httptest.NewRecorder()
	s.handleReviewRuns(recorder, addReviewAPIUser(httptest.NewRequest(http.MethodPost, reviewRunsPath, strings.NewReader(mixed)), *userID))
	assert.Equal(t, http.StatusUnprocessableEntity, recorder.Code)
	assert.Contains(t, recorder.Body.String(), "first_pass overrides are not allowed")
}

func TestReviewCapabilitiesListProfiles(t *testing.T) {
	s, _, apiPoller, userID := newReviewAPIServer(t, githubPRResponse("0123456789abcdef0123456789abcdef01234567"))
	apiPoller.policy.DefaultProfile = "lite"
	recorder := httptest.NewRecorder()
	s.handleReviewCapabilities(recorder, addReviewAPIUser(httptest.NewRequest(http.MethodGet, reviewCapabilitiesPath, nil), *userID))
	require.Equal(t, http.StatusOK, recorder.Code)

	var got struct {
		SchemaVersion  int                            `json:"schema_version"`
		DefaultProfile string                         `json:"default_profile"`
		Profiles       map[string]runconfig.Effective `json:"profiles"`
	}
	require.NoError(t, json.Unmarshal(recorder.Body.Bytes(), &got))
	assert.Equal(t, 4, got.SchemaVersion)
	assert.Equal(t, "lite", got.DefaultProfile)
	require.Len(t, got.Profiles, 3)
	lite := got.Profiles["lite"]
	assert.Equal(t, runconfig.Agent{
		Enabled: true, Backend: "claude", Model: "claude-fable-5-1", Effort: "medium", WallClockSeconds: 300, MaxTurns: 60,
		Tools: "Read,Grep,Glob,Bash", Prompt: "lite_arm_a", TurnBudgetUnit: "assistant_event", TurnBudgetVersion: 1,
	}, lite.Agent)
	assert.Equal(t, runconfig.FirstPass{}, lite.FirstPass)
	assert.False(t, lite.RequiredChecks)
	assert.False(t, lite.Gates)
	assert.True(t, lite.BugMemory)
	assert.Equal(t, "Read,Grep,Glob,Bash,Agent", got.Profiles["lite_plus"].Agent.Tools)
	assert.Equal(t, 600, got.Profiles["lite_plus"].Agent.WallClockSeconds)
	assert.Equal(t, "claude-fable-5", got.Profiles["full"].Agent.Model)
	assert.True(t, got.Profiles["full"].FirstPass.Enabled)
}

func TestReviewRunResponseIncludesProfileAndAttemptCost(t *testing.T) {
	s, database, apiPoller, userID := newReviewAPIServer(t, githubPRResponse("0123456789abcdef0123456789abcdef01234567"))
	runID := "run-70000000000000000000000000000000"
	insertReviewAPIRun(t, database, apiPoller, runID, time.Date(2026, 9, 18, 12, 0, 0, 0, time.UTC))
	require.NoError(t, database.UpsertReviewStageAttempt(&db.ReviewStageAttempt{
		RunID: runID, ExecutionAttempt: 1, Stage: "agent", InvocationNumber: 1, AttemptNumber: 1,
		Provider: "anthropic", Backend: "claude", RequestedModel: "claude-fable-5-1", Status: "completed", CostUSD: 0.4321,
	}))
	recorder := httptest.NewRecorder()
	s.handleReviewRunByID(recorder, addReviewAPIUser(httptest.NewRequest(http.MethodGet, reviewRunsPathPrefix+runID, nil), *userID))
	require.Equal(t, http.StatusOK, recorder.Code)
	assert.Contains(t, recorder.Body.String(), `"profile":"full"`)
	assert.Contains(t, recorder.Body.String(), `"cost_usd":0.4321`)
}

func TestStatusSnapshotIncludesProfileCounts(t *testing.T) {
	server, database := newTestServer(t, "status-user")
	defer database.Close()
	now := time.Now().UTC().Truncate(time.Millisecond)
	completed := db.ReviewRunStatusCompleted
	duration := int64(120000)
	for i, profile := range []string{"lite", "lite", ""} {
		runID := "run-8000000000000000000000000000000" + string(rune('0'+i))
		require.NoError(t, database.CreateReviewRun(&db.ReviewRun{
			RunID: runID, RepoOwner: "acme", RepoName: "widgets", PRNumber: 10 + i, CommitSHA: strings.Repeat("a", 40),
			TriggerSource: "api_v1", Status: db.ReviewRunStatusQueued, Profile: profile,
			RequestedConfigJSON: "{}", EffectiveConfigJSON: "{}", ConfigSourcesJSON: "{}", ConfigHash: strings.Repeat("b", 64),
			ConfigSchemaVersion: 4, AcceptedAt: now, QueuedAt: now,
		}))
		require.NoError(t, database.PatchReviewRun(runID, db.ReviewRunPatch{Status: &completed, DurationMS: &duration}))
		require.NoError(t, database.UpsertReviewStageAttempt(&db.ReviewStageAttempt{
			RunID: runID, ExecutionAttempt: 1, Stage: "agent", InvocationNumber: 1, AttemptNumber: 1, Status: "completed", CostUSD: 0.5,
		}))
	}
	snapshot, err := server.buildStatusSnapshot(context.Background())
	require.NoError(t, err)
	assert.Equal(t, db.ReviewProfileStats{Runs: 2, P50DurationMS: 120000, MeanCostUSD: 0.5}, snapshot.Profiles24h["lite"])
	assert.Equal(t, db.ReviewProfileStats{Runs: 1, P50DurationMS: 120000, MeanCostUSD: 0.5}, snapshot.Profiles24h["full"])
	body, err := json.Marshal(snapshot)
	require.NoError(t, err)
	assert.Contains(t, string(body), `"profiles_24h":{`)
}
