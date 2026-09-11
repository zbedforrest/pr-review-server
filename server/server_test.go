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

func TestHandleGetPRs_NeedsAttention_PerViewer(t *testing.T) {
	server, database := newTestServer(t, "")
	defer database.Close()

	alice := &db.User{GitHubID: 1, GitHubUsername: "alice"}
	bob := &db.User{GitHubID: 2, GitHubUsername: "bob"}
	require.NoError(t, database.CreateUser(alice))
	require.NoError(t, database.CreateUser(bob))

	require.NoError(t, database.UpsertPR(&db.PR{
		RepoOwner: "acme", RepoName: "example", PRNumber: 7, LastCommitSHA: "sha7", Status: "completed", Author: "carol",
	}))
	pr, err := database.GetPR("acme", "example", 7)
	require.NoError(t, err)

	changesRequested := "CHANGES_REQUESTED"
	flagged := true
	require.NoError(t, database.BatchUpsertUserPRViews([]db.UserPRViewBatchItem{
		{UserID: alice.ID, PRID: pr.ID, ReviewStatus: &changesRequested, NeedsAttention: &flagged},
		{UserID: bob.ID, PRID: pr.ID},
	}))

	getPRs := func(user *db.User) map[string]interface{} {
		req := addUserToRequest(httptest.NewRequest(http.MethodGet, "/api/prs", nil), user)
		w := httptest.NewRecorder()
		server.handleGetPRs(w, req)
		require.Equal(t, http.StatusOK, w.Code)
		var response []map[string]interface{}
		require.NoError(t, json.Unmarshal(w.Body.Bytes(), &response))
		require.Len(t, response, 1)
		return response[0]
	}

	assert.Equal(t, true, getPRs(alice)["needs_attention"])
	assert.Equal(t, false, getPRs(bob)["needs_attention"])

	aliceWS := server.getPRResponseForUser(alice.ID, "acme", "example", 7)
	require.NotNil(t, aliceWS)
	assert.True(t, aliceWS.NeedsAttention)
	bobWS := server.getPRResponseForUser(bob.ID, "acme", "example", 7)
	require.NotNil(t, bobWS)
	assert.False(t, bobWS.NeedsAttention)
	noRow := server.getPRResponseForUser(0, "acme", "example", 7)
	require.NotNil(t, noRow)
	assert.False(t, noRow.NeedsAttention)
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

// TestHandleGenerateReview_IngestsMergedPRNotInDB is the core scenario this
// endpoint exists for: a merged PR the poller never ingested. handleTriggerReview
// would 404, but generate-review fetches it from GitHub, upserts it, and starts
// a review.
func TestHandleGenerateReview_IngestsMergedPRNotInDB(t *testing.T) {
	headSHA := "merged1234567890abcdef1234567890abcdef1234"
	mockGH := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		// A merged (closed) PR — the kind the open-PR poller never records.
		fmt.Fprintf(w, `{"number":28951,"title":"a culprit PR","state":"closed","merged":true,"draft":false,"user":{"login":"someauthor"},"head":{"sha":"%s"}}`, headSHA)
	}))
	defer mockGH.Close()

	ghClient := gh.NewTestClient(mockGH.URL, "testuser")
	server, database := newTestServerWithGH(t, "testuser", ghClient)
	defer database.Close()

	// No PR pre-inserted: it must be created from the GitHub fetch alone.
	body := strings.NewReader(`{"owner":"owner","repo":"repo","number":28951}`)
	req := httptest.NewRequest(http.MethodPost, "/api/prs/generate-review", body)
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	server.handleGenerateReview(w, req)

	assert.Equal(t, http.StatusOK, w.Code)

	// The response must hand back the deterministic, DB-independent URLs the
	// review will be saved to, so the caller can poll for the result even after
	// the closed-PR cleanup prunes the row.
	var resp struct {
		Commit      string `json:"commit"`
		ReviewURL   string `json:"review_url"`
		FindingsURL string `json:"findings_url"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	assert.Equal(t, headSHA, resp.Commit)
	assert.Equal(t, "/reviews/owner_repo_28951_"+headSHA[:7]+".html", resp.ReviewURL)
	assert.Equal(t, "/reviews/owner_repo_28951_"+headSHA[:7]+".json", resp.FindingsURL)

	createdPR, err := database.GetPR("owner", "repo", 28951)
	require.NoError(t, err)
	require.NotNil(t, createdPR, "PR should have been ingested into the DB from GitHub")
	assert.Equal(t, headSHA, createdPR.LastCommitSHA, "review should target the PR's HEAD commit")
	assert.Equal(t, "generating", createdPR.Status)
	assert.Equal(t, "a culprit PR", createdPR.Title)
	assert.Equal(t, "someauthor", createdPR.Author)
	assert.NotNil(t, createdPR.GeneratingSince)
	// The mock PR payload has no created_at. The zero-time guard must keep us
	// from persisting 0001-01-01 (which sorts as a real ancient date under the
	// dashboard's "created_at DESC NULLS LAST" ordering); the nil falls through
	// to GORM's autoCreateTime, yielding a sane recent timestamp instead.
	require.NotNil(t, createdPR.CreatedAt)
	assert.Greater(t, createdPR.CreatedAt.Year(), 2000, "missing created_at must not persist as the ancient zero time")
}

// TestHandleGenerateReview_ClaimsPRForRequester: the authed requester gets a
// user_pr_views row with via_manual so the PR lands in their "Requested by
// Me" section — even for a merged PR they never tracked.
func TestHandleGenerateReview_ClaimsPRForRequester(t *testing.T) {
	headSHA := "merged1234567890abcdef1234567890abcdef1234"
	mockGH := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"number":42,"title":"t","state":"closed","merged":true,"draft":false,"user":{"login":"someauthor"},"head":{"sha":"%s"}}`, headSHA)
	}))
	defer mockGH.Close()

	ghClient := gh.NewTestClient(mockGH.URL, "testuser")
	server, database := newTestServerWithGH(t, "testuser", ghClient)
	defer database.Close()

	requester := &db.User{GitHubID: 777, GitHubUsername: "requester"}
	require.NoError(t, database.CreateUser(requester))

	body := strings.NewReader(`{"owner":"owner","repo":"repo","number":42,"source":"form"}`)
	req := httptest.NewRequest(http.MethodPost, "/api/prs/generate-review", body)
	req.Header.Set("Content-Type", "application/json")
	req = req.WithContext(context.WithValue(req.Context(), auth.UserContextKey, requester))
	w := httptest.NewRecorder()

	server.handleGenerateReview(w, req)
	assert.Equal(t, http.StatusOK, w.Code)

	prs, err := database.GetPRsForUserWithNotes(requester.ID)
	require.NoError(t, err)
	require.Len(t, prs, 1, "requester should now track the PR")
	assert.Equal(t, 42, prs[0].PRNumber)
	assert.True(t, prs[0].ViaManual, "requester's view must be marked via_manual")
	assert.False(t, prs[0].IsAuthor, "requester is not the PR author")
}

// TestHandleGenerateReview_APICallDoesNotClaimPR: only the dashboard's paste
// form (source=form) claims the PR into Requested by Me. Skill/API callers
// authenticate as the same user but must stay off their dashboard.
func TestHandleGenerateReview_APICallDoesNotClaimPR(t *testing.T) {
	headSHA := "merged1234567890abcdef1234567890abcdef1234"
	mockGH := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"number":43,"title":"t","state":"closed","merged":true,"draft":false,"user":{"login":"someauthor"},"head":{"sha":"%s"}}`, headSHA)
	}))
	defer mockGH.Close()

	ghClient := gh.NewTestClient(mockGH.URL, "testuser")
	server, database := newTestServerWithGH(t, "testuser", ghClient)
	defer database.Close()

	requester := &db.User{GitHubID: 778, GitHubUsername: "requester"}
	require.NoError(t, database.CreateUser(requester))

	body := strings.NewReader(`{"owner":"owner","repo":"repo","number":43}`)
	req := httptest.NewRequest(http.MethodPost, "/api/prs/generate-review", body)
	req.Header.Set("Content-Type", "application/json")
	req = req.WithContext(context.WithValue(req.Context(), auth.UserContextKey, requester))
	w := httptest.NewRecorder()

	server.handleGenerateReview(w, req)
	assert.Equal(t, http.StatusOK, w.Code, "the review itself must still run")

	prs, err := database.GetPRsForUserWithNotes(requester.ID)
	require.NoError(t, err)
	assert.Len(t, prs, 0, "a source-less request must not claim the PR")
}

func TestHandleGenerateReview_MethodNotAllowed(t *testing.T) {
	ghClient := gh.NewTestClient("http://unused.example", "testuser")
	server, database := newTestServerWithGH(t, "testuser", ghClient)
	defer database.Close()

	req := httptest.NewRequest(http.MethodGet, "/api/prs/generate-review", nil)
	w := httptest.NewRecorder()

	server.handleGenerateReview(w, req)

	assert.Equal(t, http.StatusMethodNotAllowed, w.Code)
}

func TestHandleGenerateReview_MissingFields_Returns400(t *testing.T) {
	ghClient := gh.NewTestClient("http://unused.example", "testuser")
	server, database := newTestServerWithGH(t, "testuser", ghClient)
	defer database.Close()

	body := strings.NewReader(`{"owner":"owner","repo":"repo","number":0}`)
	req := httptest.NewRequest(http.MethodPost, "/api/prs/generate-review", body)
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	server.handleGenerateReview(w, req)

	assert.Equal(t, http.StatusBadRequest, w.Code)
}

func TestHandleGenerateReview_GitHubError_Returns502(t *testing.T) {
	mockGH := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		fmt.Fprint(w, `{"message":"boom"}`)
	}))
	defer mockGH.Close()

	ghClient := gh.NewTestClient(mockGH.URL, "testuser")
	server, database := newTestServerWithGH(t, "testuser", ghClient)
	defer database.Close()

	body := strings.NewReader(`{"owner":"owner","repo":"repo","number":42}`)
	req := httptest.NewRequest(http.MethodPost, "/api/prs/generate-review", body)
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	server.handleGenerateReview(w, req)

	assert.Equal(t, http.StatusBadGateway, w.Code)

	// Nothing should have been written to the DB on a GitHub failure.
	pr, err := database.GetPR("owner", "repo", 42)
	require.NoError(t, err)
	assert.Nil(t, pr)
}

func TestHandleGenerateReview_PRNotFoundOnGitHub_Returns404(t *testing.T) {
	mockGH := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		fmt.Fprint(w, `{"message":"Not Found"}`)
	}))
	defer mockGH.Close()

	ghClient := gh.NewTestClient(mockGH.URL, "testuser")
	server, database := newTestServerWithGH(t, "testuser", ghClient)
	defer database.Close()

	body := strings.NewReader(`{"owner":"owner","repo":"repo","number":999999}`)
	req := httptest.NewRequest(http.MethodPost, "/api/prs/generate-review", body)
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	server.handleGenerateReview(w, req)

	assert.Equal(t, http.StatusNotFound, w.Code)
}

// =============================================================================
// handleSetPRHidden Tests — user-initiated Hidden section toggle
// =============================================================================

func TestHandleSetPRHidden_TogglesAndSurfacesInGetPRs(t *testing.T) {
	server, database := newTestServer(t, "testuser")
	defer database.Close()
	// Start broadcaster so BroadcastEventToUser doesn't block
	go server.broadcaster()

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
	require.NoError(t, database.UpsertPR(pr))
	prFromDB, err := database.GetPR("owner", "repo", 1)
	require.NoError(t, err)
	ensureUserPRView(t, database, user.ID, prFromDB.ID, false)

	setHidden := func(hidden bool) *httptest.ResponseRecorder {
		body := fmt.Sprintf(`{"owner":"owner","repo":"repo","number":1,"hidden":%t}`, hidden)
		req := httptest.NewRequest(http.MethodPost, "/api/prs/hidden", strings.NewReader(body))
		req = addUserToRequest(req, user)
		w := httptest.NewRecorder()
		server.handleSetPRHidden(w, req)
		return w
	}

	getPRs := func() []PRResponse {
		req := httptest.NewRequest(http.MethodGet, "/api/prs", nil)
		req = addUserToRequest(req, user)
		w := httptest.NewRecorder()
		server.handleGetPRs(w, req)
		require.Equal(t, http.StatusOK, w.Code)
		var response []PRResponse
		require.NoError(t, json.Unmarshal(w.Body.Bytes(), &response))
		return response
	}

	// Default: not hidden.
	response := getPRs()
	require.Len(t, response, 1)
	assert.False(t, response[0].Hidden)

	// Hide: the PR stays in the response, tagged hidden.
	w := setHidden(true)
	assert.Equal(t, http.StatusOK, w.Code)
	response = getPRs()
	require.Len(t, response, 1, "hidden PR must still be returned so the Hidden section can render it")
	assert.True(t, response[0].Hidden)

	// Unhide.
	w = setHidden(false)
	assert.Equal(t, http.StatusOK, w.Code)
	response = getPRs()
	require.Len(t, response, 1)
	assert.False(t, response[0].Hidden)
}

func TestHandleSetPRHidden_PRNotFound(t *testing.T) {
	server, database := newTestServer(t, "testuser")
	defer database.Close()

	user := createTestUser(t, database, "testuser")

	body := `{"owner":"owner","repo":"repo","number":999,"hidden":true}`
	req := httptest.NewRequest(http.MethodPost, "/api/prs/hidden", strings.NewReader(body))
	req = addUserToRequest(req, user)
	w := httptest.NewRecorder()

	server.handleSetPRHidden(w, req)

	assert.Equal(t, http.StatusNotFound, w.Code)
}

func TestHandleSetPRHidden_MethodNotAllowed(t *testing.T) {
	server, database := newTestServer(t, "testuser")
	defer database.Close()

	user := createTestUser(t, database, "testuser")

	req := httptest.NewRequest(http.MethodGet, "/api/prs/hidden", nil)
	req = addUserToRequest(req, user)
	w := httptest.NewRecorder()

	server.handleSetPRHidden(w, req)

	assert.Equal(t, http.StatusMethodNotAllowed, w.Code)
}

func TestGetPRResponseForUser_IncludesUserHidden(t *testing.T) {
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
	require.NoError(t, database.UpsertPR(pr))
	prFromDB, err := database.GetPR("owner", "repo", 1)
	require.NoError(t, err)
	ensureUserPRView(t, database, user.ID, prFromDB.ID, false)

	require.NoError(t, database.SetUserHiddenForPR(user.ID, prFromDB.ID, true))

	// Websocket broadcasts resolve per-recipient payloads through this path;
	// it must carry the hidden flag or updates would un-hide rows client-side.
	response := server.getPRResponseForUser(user.ID, "owner", "repo", 1)
	require.NotNil(t, response)
	assert.True(t, response.Hidden)
}

func TestHandleSetPRHidden_NoViewRow_Returns404(t *testing.T) {
	server, database := newTestServer(t, "testuser")
	defer database.Close()

	user := createTestUser(t, database, "testuser")

	// PR exists, but no user_pr_views row for this user.
	pr := &db.PR{
		RepoOwner:     "owner",
		RepoName:      "repo",
		PRNumber:      1,
		LastCommitSHA: "abc123",
		Status:        "completed",
		Title:         "Test PR",
		Author:        "otheruser",
	}
	require.NoError(t, database.UpsertPR(pr))

	body := `{"owner":"owner","repo":"repo","number":1,"hidden":true}`
	req := httptest.NewRequest(http.MethodPost, "/api/prs/hidden", strings.NewReader(body))
	req = addUserToRequest(req, user)
	w := httptest.NewRecorder()

	server.handleSetPRHidden(w, req)

	assert.Equal(t, http.StatusNotFound, w.Code,
		"0-row update must surface as 404 so the frontend rolls back instead of silently snapping back")
}
