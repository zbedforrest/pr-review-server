package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"pr-review-server/auth"
	"pr-review-server/config"
	"pr-review-server/db"
	gh "pr-review-server/github"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// newTestServer creates a server with an in-memory database for testing
func newTestServer(t *testing.T, username string) (*Server, *db.GormDB) {
	database, err := db.NewGormSQLite(":memory:")
	require.NoError(t, err)

	cfg := &config.Config{
		GitHubUsername: username,
		ServerPort:     "8080",
		ReviewsDir:     "/tmp/reviews",
	}

	server := New(cfg, database, nil, nil)
	return server, database
}

// createTestUser creates a test user and returns it
func createTestUser(t *testing.T, database *db.GormDB, username string) *db.User {
	user := &db.User{
		GitHubID:       12345,
		GitHubUsername: username,
	}
	err := database.CreateUser(user)
	require.NoError(t, err)
	return user
}

// addUserToRequest adds a user to the request context
func addUserToRequest(r *http.Request, user *db.User) *http.Request {
	ctx := context.WithValue(r.Context(), auth.UserContextKey, user)
	return r.WithContext(ctx)
}

// ensureUserPRView creates a user_pr_view record for the test
func ensureUserPRView(t *testing.T, database *db.GormDB, userID, prID int, isAuthor bool) {
	err := database.EnsureUserPRView(userID, prID, isAuthor)
	require.NoError(t, err)
}

// =============================================================================
// getPRResponse Tests - Ensures IsMine is correctly set for WebSocket broadcasts
// =============================================================================

func TestGetPRResponse_IsMine_AuthorMatchesUsername(t *testing.T) {
	server, database := newTestServer(t, "testuser")
	defer database.Close()

	// Create a PR authored by the configured user
	pr := &db.PR{
		RepoOwner:     "owner",
		RepoName:      "repo",
		PRNumber:      1,
		LastCommitSHA: "abc123",
		Status:        "completed",
		Title:         "Test PR",
		Author:        "testuser",
	}
	err := database.UpsertPR(pr)
	require.NoError(t, err)

	// Get PR response (used for WebSocket broadcasts)
	response := server.getPRResponse("owner", "repo", 1)
	require.NotNil(t, response)

	// IsMine should be true when author matches configured username
	assert.True(t, response.IsMine, "IsMine should be true when author matches GitHubUsername")
	assert.Equal(t, "testuser", response.Author)
}

func TestGetPRResponse_IsMine_AuthorDoesNotMatchUsername(t *testing.T) {
	server, database := newTestServer(t, "testuser")
	defer database.Close()

	// Create a PR authored by someone else
	pr := &db.PR{
		RepoOwner:     "owner",
		RepoName:      "repo",
		PRNumber:      2,
		LastCommitSHA: "def456",
		Status:        "pending",
		Title:         "Someone else's PR",
		Author:        "otheruser",
	}
	err := database.UpsertPR(pr)
	require.NoError(t, err)

	response := server.getPRResponse("owner", "repo", 2)
	require.NotNil(t, response)

	// IsMine should be false when author doesn't match
	assert.False(t, response.IsMine, "IsMine should be false when author doesn't match GitHubUsername")
	assert.Equal(t, "otheruser", response.Author)
}

func TestGetPRResponse_IsMine_CaseInsensitive(t *testing.T) {
	server, database := newTestServer(t, "TestUser")
	defer database.Close()

	// Create a PR with lowercase author that should match uppercase username
	pr := &db.PR{
		RepoOwner:     "owner",
		RepoName:      "repo",
		PRNumber:      3,
		LastCommitSHA: "ghi789",
		Status:        "completed",
		Title:         "Case test PR",
		Author:        "testuser", // lowercase
	}
	err := database.UpsertPR(pr)
	require.NoError(t, err)

	response := server.getPRResponse("owner", "repo", 3)
	require.NotNil(t, response)

	// IsMine should be true (case-insensitive comparison)
	assert.True(t, response.IsMine, "IsMine comparison should be case-insensitive")
}

func TestGetPRResponse_IsMine_EmptyAuthor(t *testing.T) {
	server, database := newTestServer(t, "testuser")
	defer database.Close()

	// Create a PR with empty author
	pr := &db.PR{
		RepoOwner:     "owner",
		RepoName:      "repo",
		PRNumber:      4,
		LastCommitSHA: "jkl012",
		Status:        "pending",
		Title:         "No author PR",
		Author:        "",
	}
	err := database.UpsertPR(pr)
	require.NoError(t, err)

	response := server.getPRResponse("owner", "repo", 4)
	require.NotNil(t, response)

	// IsMine should be false when author is empty
	assert.False(t, response.IsMine, "IsMine should be false when author is empty")
}

func TestGetPRResponse_IsMine_EmptyUsername(t *testing.T) {
	server, database := newTestServer(t, "")
	defer database.Close()

	// Create a PR
	pr := &db.PR{
		RepoOwner:     "owner",
		RepoName:      "repo",
		PRNumber:      5,
		LastCommitSHA: "mno345",
		Status:        "pending",
		Title:         "Test PR",
		Author:        "someuser",
	}
	err := database.UpsertPR(pr)
	require.NoError(t, err)

	response := server.getPRResponse("owner", "repo", 5)
	require.NotNil(t, response)

	// IsMine should be false when configured username is empty
	assert.False(t, response.IsMine, "IsMine should be false when GitHubUsername is not configured")
}

func TestGetPRResponse_NotFound(t *testing.T) {
	server, database := newTestServer(t, "testuser")
	defer database.Close()

	// Try to get response for non-existent PR
	response := server.getPRResponse("owner", "repo", 999)
	assert.Nil(t, response, "Response should be nil for non-existent PR")
}

// =============================================================================
// handleGetPRs Tests - Ensures API response includes IsMine correctly
// =============================================================================

func TestHandleGetPRs_IsMine_CorrectlySet(t *testing.T) {
	server, database := newTestServer(t, "myusername")
	defer database.Close()

	// Create test user
	user := createTestUser(t, database, "myusername")

	// Create PRs with different authors
	myPR := &db.PR{
		RepoOwner:     "owner",
		RepoName:      "repo",
		PRNumber:      1,
		LastCommitSHA: "sha1",
		Status:        "completed",
		Title:         "My PR",
		Author:        "myusername",
	}
	err := database.UpsertPR(myPR)
	require.NoError(t, err)

	// Get the PR IDs after insert
	myPRFromDB, err := database.GetPR("owner", "repo", 1)
	require.NoError(t, err)

	otherPR := &db.PR{
		RepoOwner:     "owner",
		RepoName:      "repo",
		PRNumber:      2,
		LastCommitSHA: "sha2",
		Status:        "pending",
		Title:         "Other PR",
		Author:        "otheruser",
	}
	err = database.UpsertPR(otherPR)
	require.NoError(t, err)

	otherPRFromDB, err := database.GetPR("owner", "repo", 2)
	require.NoError(t, err)

	// Create user_pr_view records
	ensureUserPRView(t, database, user.ID, myPRFromDB.ID, true)     // My PR - isAuthor=true
	ensureUserPRView(t, database, user.ID, otherPRFromDB.ID, false) // Other PR - isAuthor=false

	// Make request with user in context
	req := httptest.NewRequest(http.MethodGet, "/api/prs", nil)
	req = addUserToRequest(req, user)
	w := httptest.NewRecorder()

	server.handleGetPRs(w, req)

	assert.Equal(t, http.StatusOK, w.Code)

	var response []PRResponse
	err = json.Unmarshal(w.Body.Bytes(), &response)
	require.NoError(t, err)
	require.Len(t, response, 2)

	// Find each PR and verify IsMine
	prMap := make(map[int]PRResponse)
	for _, pr := range response {
		prMap[pr.Number] = pr
	}

	assert.True(t, prMap[1].IsMine, "PR authored by user should have IsMine=true")
	assert.False(t, prMap[2].IsMine, "PR authored by other should have IsMine=false")
}

func TestHandleGetPRs_AllFieldsPresent(t *testing.T) {
	server, database := newTestServer(t, "testuser")
	defer database.Close()

	// Create test user
	user := createTestUser(t, database, "testuser")

	now := time.Now().UTC().Truncate(time.Second)
	reviewedAt := now.Add(-1 * time.Hour)
	generatingSince := now.Add(-5 * time.Minute)

	pr := &db.PR{
		RepoOwner:       "owner",
		RepoName:        "repo",
		PRNumber:        1,
		LastCommitSHA:   "abc123def456",
		Status:          "generating",
		Title:           "Full Test PR",
		Author:          "testuser",
		ReviewHTMLPath:  "review-owner-repo-1-abc123def456.html",
		LastReviewedAt:  &reviewedAt,
		GeneratingSince: &generatingSince,
		CreatedAt:       &now,
		Draft:           true,
		ApprovalCount:   2,
		CIState:         "failure",
		CIFailedChecks:  `["lint", "test"]`,
	}
	err := database.UpsertPR(pr)
	require.NoError(t, err)

	// Get the PR ID after insert
	prFromDB, err := database.GetPR("owner", "repo", 1)
	require.NoError(t, err)

	// Create user_pr_view record
	ensureUserPRView(t, database, user.ID, prFromDB.ID, true) // isAuthor=true

	req := httptest.NewRequest(http.MethodGet, "/api/prs", nil)
	req = addUserToRequest(req, user)
	w := httptest.NewRecorder()

	server.handleGetPRs(w, req)

	assert.Equal(t, http.StatusOK, w.Code)

	var response []PRResponse
	err = json.Unmarshal(w.Body.Bytes(), &response)
	require.NoError(t, err)
	require.Len(t, response, 1)

	pr1 := response[0]
	assert.Equal(t, "owner", pr1.Owner)
	assert.Equal(t, "repo", pr1.Repo)
	assert.Equal(t, 1, pr1.Number)
	assert.Equal(t, "abc123def456", pr1.CommitSHA)
	assert.Equal(t, "generating", pr1.Status)
	assert.Equal(t, "Full Test PR", pr1.Title)
	assert.Equal(t, "testuser", pr1.Author)
	assert.True(t, pr1.IsMine, "IsMine must be set in API response")
	assert.True(t, pr1.Draft)
	assert.Equal(t, 2, pr1.ApprovalCount)
	assert.Equal(t, "failure", pr1.CIState)
	assert.Equal(t, []string{"lint", "test"}, pr1.CIFailedChecks)
	assert.NotNil(t, pr1.LastReviewedAt)
	assert.NotNil(t, pr1.GeneratingSince)
	assert.NotNil(t, pr1.CreatedAt)
}

// =============================================================================
// PRResponse JSON Serialization Tests
// =============================================================================

func TestPRResponse_JSONSerialization_IncludesIsMine(t *testing.T) {
	response := PRResponse{
		Owner:          "owner",
		Repo:           "repo",
		Number:         1,
		CommitSHA:      "abc123",
		Status:         "completed",
		Title:          "Test PR",
		Author:         "testuser",
		IsMine:         true,
		Draft:          false,
		ApprovalCount:  1,
		CIState:        "success",
		CIFailedChecks: []string{},
	}

	jsonBytes, err := json.Marshal(response)
	require.NoError(t, err)

	// Verify IsMine is in the JSON
	var decoded map[string]interface{}
	err = json.Unmarshal(jsonBytes, &decoded)
	require.NoError(t, err)

	isMine, exists := decoded["is_mine"]
	assert.True(t, exists, "is_mine field must exist in JSON output")
	assert.True(t, isMine.(bool), "is_mine should be true")
}

func TestPRResponse_JSONSerialization_IsMine_False(t *testing.T) {
	response := PRResponse{
		Owner:          "owner",
		Repo:           "repo",
		Number:         1,
		CommitSHA:      "abc123",
		Status:         "pending",
		Title:          "Other PR",
		Author:         "otheruser",
		IsMine:         false,
		CIFailedChecks: []string{},
	}

	jsonBytes, err := json.Marshal(response)
	require.NoError(t, err)

	var decoded map[string]interface{}
	err = json.Unmarshal(jsonBytes, &decoded)
	require.NoError(t, err)

	isMine, exists := decoded["is_mine"]
	assert.True(t, exists, "is_mine field must exist in JSON output even when false")
	assert.False(t, isMine.(bool), "is_mine should be false")
}

// =============================================================================
// ViaTeams and ReviewStatus Tests - Ensures user_pr_views data is in API response
// =============================================================================

func TestHandleGetPRs_ViaTeams_IncludedInResponse(t *testing.T) {
	server, database := newTestServer(t, "testuser")
	defer database.Close()

	user := createTestUser(t, database, "testuser")

	pr := &db.PR{
		RepoOwner:     "owner",
		RepoName:      "repo",
		PRNumber:      1,
		LastCommitSHA: "abc123",
		Status:        "completed",
		Title:         "Test PR",
		Author:        "otheruser",
	}
	err := database.UpsertPR(pr)
	require.NoError(t, err)

	prFromDB, err := database.GetPR("owner", "repo", 1)
	require.NoError(t, err)

	// Create user_pr_view with via_teams
	ensureUserPRView(t, database, user.ID, prFromDB.ID, false)
	err = database.UpdateUserViaTeams(user.ID, prFromDB.ID, []string{"frontend-team", "reviewers"})
	require.NoError(t, err)

	req := httptest.NewRequest(http.MethodGet, "/api/prs", nil)
	req = addUserToRequest(req, user)
	w := httptest.NewRecorder()

	server.handleGetPRs(w, req)

	assert.Equal(t, http.StatusOK, w.Code)

	var response []PRResponse
	err = json.Unmarshal(w.Body.Bytes(), &response)
	require.NoError(t, err)
	require.Len(t, response, 1)

	// Verify via_teams is populated from user_pr_views
	assert.Equal(t, []string{"frontend-team", "reviewers"}, response[0].ViaTeams,
		"via_teams must be included in API response from user_pr_views")
}

func TestHandleGetPRs_ReviewStatus_IncludedInResponse(t *testing.T) {
	server, database := newTestServer(t, "testuser")
	defer database.Close()

	user := createTestUser(t, database, "testuser")

	pr := &db.PR{
		RepoOwner:      "owner",
		RepoName:       "repo",
		PRNumber:       1,
		LastCommitSHA:  "abc123",
		Status:         "completed",
		Title:          "Test PR",
		Author:         "otheruser",
		MyReviewStatus: "COMMENTED", // This is in prs table, should NOT be used
	}
	err := database.UpsertPR(pr)
	require.NoError(t, err)

	prFromDB, err := database.GetPR("owner", "repo", 1)
	require.NoError(t, err)

	// Create user_pr_view with different review status
	ensureUserPRView(t, database, user.ID, prFromDB.ID, false)
	err = database.UpdateUserReviewStatus(user.ID, prFromDB.ID, "APPROVED")
	require.NoError(t, err)

	req := httptest.NewRequest(http.MethodGet, "/api/prs", nil)
	req = addUserToRequest(req, user)
	w := httptest.NewRecorder()

	server.handleGetPRs(w, req)

	assert.Equal(t, http.StatusOK, w.Code)

	var response []PRResponse
	err = json.Unmarshal(w.Body.Bytes(), &response)
	require.NoError(t, err)
	require.Len(t, response, 1)

	// Verify review status comes from user_pr_views, NOT prs table
	assert.Equal(t, "APPROVED", response[0].MyReviewStatus,
		"my_review_status must come from user_pr_views, not prs table")
}

func TestPRResponse_JSONSerialization_ViaTeams_Exists(t *testing.T) {
	response := PRResponse{
		Owner:          "owner",
		Repo:           "repo",
		Number:         1,
		CommitSHA:      "abc123",
		Status:         "completed",
		Title:          "Test PR",
		Author:         "otheruser",
		IsMine:         false,
		ViaTeams:       []string{"team-a", "team-b"},
		CIFailedChecks: []string{},
	}

	jsonBytes, err := json.Marshal(response)
	require.NoError(t, err)

	var decoded map[string]interface{}
	err = json.Unmarshal(jsonBytes, &decoded)
	require.NoError(t, err)

	viaTeams, exists := decoded["via_teams"]
	assert.True(t, exists, "via_teams field must exist in JSON output")
	teams := viaTeams.([]interface{})
	assert.Len(t, teams, 2)
	assert.Equal(t, "team-a", teams[0].(string))
	assert.Equal(t, "team-b", teams[1].(string))
}

func TestPRResponse_JSONSerialization_MyReviewStatus_Exists(t *testing.T) {
	response := PRResponse{
		Owner:          "owner",
		Repo:           "repo",
		Number:         1,
		CommitSHA:      "abc123",
		Status:         "completed",
		Title:          "Test PR",
		Author:         "otheruser",
		IsMine:         false,
		MyReviewStatus: "APPROVED",
		CIFailedChecks: []string{},
	}

	jsonBytes, err := json.Marshal(response)
	require.NoError(t, err)

	var decoded map[string]interface{}
	err = json.Unmarshal(jsonBytes, &decoded)
	require.NoError(t, err)

	status, exists := decoded["my_review_status"]
	assert.True(t, exists, "my_review_status field must exist in JSON output")
	assert.Equal(t, "APPROVED", status.(string))
}

// =============================================================================
// handleTriggerReview Tests
// =============================================================================

// newTestServerWithGH creates a server with an in-memory database and a GitHub client for testing
func newTestServerWithGH(t *testing.T, username string, ghClient *gh.Client) (*Server, *db.GormDB) {
	database, err := db.NewGormSQLite(":memory:")
	require.NoError(t, err)

	cfg := &config.Config{
		GitHubUsername: username,
		ServerPort:     "8080",
		ReviewsDir:     "/tmp/reviews",
	}

	s := New(cfg, database, ghClient, nil)
	// Start broadcaster so BroadcastEvent doesn't block
	go s.broadcaster()
	return s, database
}

func TestHandleTriggerReview_FetchesLatestCommitSHA(t *testing.T) {
	// Mock GitHub API that returns a PR with a specific HEAD SHA
	latestSHA := "newcommit999888777666555444333222111000aaa"
	mockGH := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"number":1,"head":{"sha":"%s"}}`, latestSHA)
	}))
	defer mockGH.Close()

	ghClient := gh.NewTestClient(mockGH.URL, "testuser")
	server, database := newTestServerWithGH(t, "testuser", ghClient)
	defer database.Close()

	// Insert a PR with a stale commit SHA
	staleSHA := "oldcommitaaa111222333444555666777888999000"
	createdAt := time.Now().UTC().Truncate(time.Second)
	pr := &db.PR{
		RepoOwner:     "owner",
		RepoName:      "repo",
		PRNumber:      1,
		LastCommitSHA: staleSHA,
		Status:        "completed",
		Title:         "Test PR",
		Author:        "otheruser",
		CreatedAt:     &createdAt,
	}
	err := database.UpsertPR(pr)
	require.NoError(t, err)

	// Trigger review
	body := strings.NewReader(`{"owner":"owner","repo":"repo","number":1}`)
	req := httptest.NewRequest(http.MethodPost, "/api/prs/trigger-review", body)
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	server.handleTriggerReview(w, req)

	assert.Equal(t, http.StatusOK, w.Code)

	// Verify the DB was updated with the latest SHA, not the stale one
	updatedPR, err := database.GetPR("owner", "repo", 1)
	require.NoError(t, err)
	assert.Equal(t, latestSHA, updatedPR.LastCommitSHA, "Trigger review should update to latest commit SHA from GitHub")
	assert.Equal(t, "generating", updatedPR.Status, "Status should be set to generating")
	assert.NotNil(t, updatedPR.GeneratingSince, "GeneratingSince should be set")
}

func TestHandleTriggerReview_GitHubAPIError_Returns500(t *testing.T) {
	// Mock GitHub API that returns an error
	mockGH := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		fmt.Fprint(w, `{"message":"internal error"}`)
	}))
	defer mockGH.Close()

	ghClient := gh.NewTestClient(mockGH.URL, "testuser")
	server, database := newTestServerWithGH(t, "testuser", ghClient)
	defer database.Close()

	// Insert a PR
	pr := &db.PR{
		RepoOwner:     "owner",
		RepoName:      "repo",
		PRNumber:      1,
		LastCommitSHA: "oldsha123456789012345678901234567890abcd",
		Status:        "completed",
		Title:         "Test PR",
		Author:        "otheruser",
	}
	err := database.UpsertPR(pr)
	require.NoError(t, err)

	// Trigger review - should fail because GitHub API returns error
	body := strings.NewReader(`{"owner":"owner","repo":"repo","number":1}`)
	req := httptest.NewRequest(http.MethodPost, "/api/prs/trigger-review", body)
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	server.handleTriggerReview(w, req)

	assert.Equal(t, http.StatusInternalServerError, w.Code, "Should return 500 when GitHub API fails")

	// Verify the PR status was NOT changed (still completed, not generating)
	unchangedPR, err := database.GetPR("owner", "repo", 1)
	require.NoError(t, err)
	assert.Equal(t, "completed", unchangedPR.Status, "PR status should remain unchanged when GitHub API fails")
	assert.Equal(t, "oldsha123456789012345678901234567890abcd", unchangedPR.LastCommitSHA, "Commit SHA should remain unchanged")
}

func TestHandleTriggerReview_PRNotFound_Returns404(t *testing.T) {
	mockGH := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("GitHub API should not be called when PR is not in database")
	}))
	defer mockGH.Close()

	ghClient := gh.NewTestClient(mockGH.URL, "testuser")
	server, database := newTestServerWithGH(t, "testuser", ghClient)
	defer database.Close()

	// Don't insert any PR - trigger review for non-existent PR
	body := strings.NewReader(`{"owner":"owner","repo":"repo","number":999}`)
	req := httptest.NewRequest(http.MethodPost, "/api/prs/trigger-review", body)
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	server.handleTriggerReview(w, req)

	assert.Equal(t, http.StatusNotFound, w.Code, "Should return 404 for non-existent PR")
}
