package github

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestBuildPRAliasMap(t *testing.T) {
	prs := []PullRequest{
		{Number: 101},
		{Number: 102},
		{Number: 103},
	}

	aliases := buildPRAliasMap(prs)

	if len(aliases) != 3 {
		t.Errorf("Expected 3 aliases, got %d", len(aliases))
	}

	// Check mapping
	if aliases["pr0"] != 101 {
		t.Errorf("Expected pr0 -> 101, got %d", aliases["pr0"])
	}
	if aliases["pr1"] != 102 {
		t.Errorf("Expected pr1 -> 102, got %d", aliases["pr1"])
	}
	if aliases["pr2"] != 103 {
		t.Errorf("Expected pr2 -> 103, got %d", aliases["pr2"])
	}
}

func TestBuildPRInfoAliasMap(t *testing.T) {
	prs := []PRInfoWithCommit{
		{Owner: "owner1", Repo: "repo1", Number: 101, CommitSHA: "abc123"},
		{Owner: "owner2", Repo: "repo2", Number: 102, CommitSHA: "def456"},
	}

	aliases := buildPRInfoAliasMap(prs)

	if len(aliases) != 2 {
		t.Errorf("Expected 2 aliases, got %d", len(aliases))
	}

	// Check first mapping
	prInfo0 := aliases["pr0"]
	if prInfo0.Owner != "owner1" || prInfo0.Repo != "repo1" || prInfo0.Number != 101 {
		t.Errorf("Expected pr0 -> (owner1, repo1, 101), got (%s, %s, %d)", prInfo0.Owner, prInfo0.Repo, prInfo0.Number)
	}
}

func TestIsValidReviewState(t *testing.T) {
	tests := []struct {
		state    string
		expected bool
	}{
		{"APPROVED", true},
		{"CHANGES_REQUESTED", true},
		{"COMMENTED", true},
		{"PENDING", false},
		{"DISMISSED", false},
	}

	for _, tt := range tests {
		result := isValidReviewState(tt.state)
		if result != tt.expected {
			t.Errorf("isValidReviewState(%q) = %v, expected %v", tt.state, result, tt.expected)
		}
	}
}

func TestPrKey(t *testing.T) {
	key := prKey("owner", "repo", 123)
	expected := "owner/repo/123"
	if key != expected {
		t.Errorf("prKey(owner, repo, 123) = %q, expected %q", key, expected)
	}
}

func TestParseRepoFromURL(t *testing.T) {
	tests := []struct {
		url           string
		expectedOwner string
		expectedRepo  string
		expectError   bool
	}{
		{"https://api.github.com/repos/owner/repo", "owner", "repo", false},
		{"https://api.github.com/repos/org/project", "org", "project", false},
		{"x", "", "", true}, // Only 1 part when split by "/"
		{"", "", "", true},
	}

	for _, tt := range tests {
		owner, repo, err := parseRepoFromURL(tt.url)
		if tt.expectError {
			if err == nil {
				t.Errorf("parseRepoFromURL(%q) expected error, got nil", tt.url)
			}
		} else {
			if err != nil {
				t.Errorf("parseRepoFromURL(%q) unexpected error: %v", tt.url, err)
			}
			if owner != tt.expectedOwner || repo != tt.expectedRepo {
				t.Errorf("parseRepoFromURL(%q) = (%q, %q), expected (%q, %q)", tt.url, owner, repo, tt.expectedOwner, tt.expectedRepo)
			}
		}
	}
}

func TestCountUserApprovals(t *testing.T) {
	client := NewClient("token", "current-user")

	reviews := ReviewsData{
		Nodes: []ReviewNode{
			{Author: &ReviewAuthor{Login: "user1"}, State: "APPROVED"},
			{Author: &ReviewAuthor{Login: "user2"}, State: "CHANGES_REQUESTED"},
			{Author: &ReviewAuthor{Login: "current-user"}, State: "APPROVED"},
			{Author: &ReviewAuthor{Login: "user1"}, State: "COMMENTED"}, // user1's latest
			{Author: nil, State: "APPROVED"},                            // Bot/deleted user - should be skipped
			{Author: &ReviewAuthor{Login: "user3"}, State: "PENDING"},   // Should be skipped
		},
	}

	approvalCount, myReviewStatus, userReviews := client.countUserApprovals(reviews)

	// user1: COMMENTED (latest), user2: CHANGES_REQUESTED, current-user: APPROVED
	// So only current-user has APPROVED
	if approvalCount != 1 {
		t.Errorf("Expected 1 approval, got %d", approvalCount)
	}

	if myReviewStatus != "APPROVED" {
		t.Errorf("Expected myReviewStatus to be APPROVED, got %q", myReviewStatus)
	}

	// Verify userReviews map contains all users with valid review states
	if len(userReviews) != 3 {
		t.Errorf("Expected 3 user reviews, got %d", len(userReviews))
	}
	if userReviews["user1"] != "COMMENTED" {
		t.Errorf("Expected user1 state COMMENTED, got %q", userReviews["user1"])
	}
	if userReviews["user2"] != "CHANGES_REQUESTED" {
		t.Errorf("Expected user2 state CHANGES_REQUESTED, got %q", userReviews["user2"])
	}
	if userReviews["current-user"] != "APPROVED" {
		t.Errorf("Expected current-user state APPROVED, got %q", userReviews["current-user"])
	}
	// user3 had PENDING which should be excluded
	if _, exists := userReviews["user3"]; exists {
		t.Errorf("Expected user3 to be excluded (PENDING), but found in map")
	}
}

func TestCountUserApprovals_DismissedClearsPreviousState(t *testing.T) {
	client := NewClient("token", "current-user")

	// Scenario: user requested changes, then approved, then the approval was dismissed
	// (e.g., stale review dismissed due to new commits).
	// The user should have NO active review state after dismissal.
	reviews := ReviewsData{
		Nodes: []ReviewNode{
			{Author: &ReviewAuthor{Login: "reviewer1"}, State: "CHANGES_REQUESTED"},
			{Author: &ReviewAuthor{Login: "reviewer1"}, State: "DISMISSED"}, // approval was dismissed
			{Author: &ReviewAuthor{Login: "reviewer2"}, State: "APPROVED"},  // still valid
		},
	}

	approvalCount, _, userReviews := client.countUserApprovals(reviews)

	// reviewer1 should be absent (dismissed clears their state)
	if _, exists := userReviews["reviewer1"]; exists {
		t.Errorf("Expected reviewer1 to be absent after dismissal, but found state %q", userReviews["reviewer1"])
	}

	// reviewer2 should still be approved
	if userReviews["reviewer2"] != "APPROVED" {
		t.Errorf("Expected reviewer2 state APPROVED, got %q", userReviews["reviewer2"])
	}

	// Only reviewer2's approval should count
	if approvalCount != 1 {
		t.Errorf("Expected 1 approval, got %d", approvalCount)
	}
}

func TestCountUserApprovals_DismissedThenReApproved(t *testing.T) {
	client := NewClient("token", "current-user")

	// Scenario: user's review was dismissed, then they re-approved.
	// The re-approval should be the final state.
	reviews := ReviewsData{
		Nodes: []ReviewNode{
			{Author: &ReviewAuthor{Login: "reviewer1"}, State: "APPROVED"},
			{Author: &ReviewAuthor{Login: "reviewer1"}, State: "DISMISSED"},
			{Author: &ReviewAuthor{Login: "reviewer1"}, State: "APPROVED"}, // re-approved
		},
	}

	approvalCount, _, userReviews := client.countUserApprovals(reviews)

	if userReviews["reviewer1"] != "APPROVED" {
		t.Errorf("Expected reviewer1 state APPROVED after re-approval, got %q", userReviews["reviewer1"])
	}
	if approvalCount != 1 {
		t.Errorf("Expected 1 approval, got %d", approvalCount)
	}
}

func TestExtractReviewerGroups(t *testing.T) {
	client := NewClient("token", "current-user")

	type orgInfo = struct {
		Login string `json:"login"`
	}
	type reqReviewer = struct {
		TypeName     string  `json:"__typename"`
		Login        string  `json:"login"`
		Name         string  `json:"name"`
		Slug         string  `json:"slug"`
		Organization orgInfo `json:"organization"`
	}

	tests := []struct {
		name     string
		items    TimelineItemsData
		expected []string
	}{
		{
			name: "Team request only",
			items: TimelineItemsData{
				Nodes: []TimelineReviewRequested{
					{RequestedReviewer: reqReviewer{TypeName: "Team", Name: "backend-team", Slug: "backend-team"}},
				},
			},
			expected: []string{"backend-team"},
		},
		{
			name: "Personal request only - no teams, user in requestedUsers",
			items: TimelineItemsData{
				Nodes: []TimelineReviewRequested{
					{RequestedReviewer: reqReviewer{TypeName: "User", Login: "current-user"}},
				},
			},
			expected: []string{},
		},
		{
			name: "Team and personal request - both captured separately",
			items: TimelineItemsData{
				Nodes: []TimelineReviewRequested{
					{RequestedReviewer: reqReviewer{TypeName: "User", Login: "current-user"}},
					{RequestedReviewer: reqReviewer{TypeName: "Team", Name: "frontend-team", Slug: "frontend-team"}},
				},
			},
			expected: []string{"frontend-team"},
		},
		{
			name:     "No events",
			items:    TimelineItemsData{Nodes: []TimelineReviewRequested{}},
			expected: []string{},
		},
		{
			name: "Duplicate team requests are deduplicated",
			items: TimelineItemsData{
				Nodes: []TimelineReviewRequested{
					{RequestedReviewer: reqReviewer{TypeName: "Team", Name: "backend-team", Slug: "backend-team"}},
					{RequestedReviewer: reqReviewer{TypeName: "Team", Name: "backend-team", Slug: "backend-team"}},
				},
			},
			expected: []string{"backend-team"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result, _, _, _ := client.extractReviewerGroups(tt.items)
			if len(result) != len(tt.expected) {
				t.Errorf("Expected %v, got %v", tt.expected, result)
				return
			}
			for i, v := range result {
				if v != tt.expected[i] {
					t.Errorf("Expected %v, got %v", tt.expected, result)
					return
				}
			}
		})
	}
}

func TestExtractReviewerGroups_CollectsSlugs(t *testing.T) {
	client := NewClient("token", "current-user")

	type orgInfo = struct {
		Login string `json:"login"`
	}
	type reqReviewer = struct {
		TypeName     string  `json:"__typename"`
		Login        string  `json:"login"`
		Name         string  `json:"name"`
		Slug         string  `json:"slug"`
		Organization orgInfo `json:"organization"`
	}

	items := TimelineItemsData{
		Nodes: []TimelineReviewRequested{
			{RequestedReviewer: reqReviewer{TypeName: "Team", Name: "Platform", Slug: "platform", Organization: orgInfo{Login: "myorg"}}},
			{RequestedReviewer: reqReviewer{TypeName: "Team", Name: "Security Team", Slug: "security-team", Organization: orgInfo{Login: "myorg"}}},
			{RequestedReviewer: reqReviewer{TypeName: "User", Login: "current-user"}},
		},
	}

	groups, slugs, orgName, requestedUsers := client.extractReviewerGroups(items)

	if len(groups) != 2 {
		t.Errorf("Expected 2 groups, got %d", len(groups))
	}
	if slugs["Platform"] != "platform" {
		t.Errorf("Expected slug 'platform' for Platform, got %q", slugs["Platform"])
	}
	if slugs["Security Team"] != "security-team" {
		t.Errorf("Expected slug 'security-team' for Security Team, got %q", slugs["Security Team"])
	}
	if orgName != "myorg" {
		t.Errorf("Expected orgName 'myorg', got %q", orgName)
	}
	if len(requestedUsers) != 1 || requestedUsers[0] != "current-user" {
		t.Errorf("Expected requestedUsers ['current-user'], got %v", requestedUsers)
	}
}

func TestExtractReviewerGroups_OrgNameEmpty_WhenNoTeams(t *testing.T) {
	client := NewClient("token", "current-user")

	type orgInfo = struct {
		Login string `json:"login"`
	}
	type reqReviewer = struct {
		TypeName     string  `json:"__typename"`
		Login        string  `json:"login"`
		Name         string  `json:"name"`
		Slug         string  `json:"slug"`
		Organization orgInfo `json:"organization"`
	}

	// Only user requests, no teams — orgName should be empty
	items := TimelineItemsData{
		Nodes: []TimelineReviewRequested{
			{RequestedReviewer: reqReviewer{TypeName: "User", Login: "current-user"}},
		},
	}

	_, _, orgName, _ := client.extractReviewerGroups(items)
	if orgName != "" {
		t.Errorf("Expected empty orgName for user-only requests, got %q", orgName)
	}
}

func TestParseCIStatusFromRollup(t *testing.T) {
	tests := []struct {
		name           string
		rollup         *StatusCheckRollup
		expectedState  string
		expectedFailed []string
	}{
		{
			name: "All success",
			rollup: &StatusCheckRollup{
				State: "SUCCESS",
				Contexts: ContextsData{
					Nodes: []CheckNode{
						{TypeName: "CheckRun", Name: "test", Status: "COMPLETED", Conclusion: "SUCCESS"},
						{TypeName: "CheckRun", Name: "lint", Status: "COMPLETED", Conclusion: "SUCCESS"},
					},
				},
			},
			expectedState:  "success",
			expectedFailed: nil,
		},
		{
			name: "Pending check",
			rollup: &StatusCheckRollup{
				State: "PENDING",
				Contexts: ContextsData{
					Nodes: []CheckNode{
						{TypeName: "CheckRun", Name: "test", Status: "IN_PROGRESS"},
					},
				},
			},
			expectedState:  "pending",
			expectedFailed: nil,
		},
		{
			name: "Failed check",
			rollup: &StatusCheckRollup{
				State: "FAILURE",
				Contexts: ContextsData{
					Nodes: []CheckNode{
						{TypeName: "CheckRun", Name: "test", Status: "COMPLETED", Conclusion: "FAILURE"},
						{TypeName: "CheckRun", Name: "lint", Status: "COMPLETED", Conclusion: "SUCCESS"},
					},
				},
			},
			expectedState:  "failure",
			expectedFailed: []string{"test"},
		},
		{
			name: "StatusContext failure",
			rollup: &StatusCheckRollup{
				State: "FAILURE",
				Contexts: ContextsData{
					Nodes: []CheckNode{
						{TypeName: "StatusContext", Context: "ci/build", State: "FAILURE"},
					},
				},
			},
			expectedState:  "failure",
			expectedFailed: []string{"ci/build"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			state, failed := parseCIStatusFromRollup(tt.rollup)
			if state != tt.expectedState {
				t.Errorf("Expected state %q, got %q", tt.expectedState, state)
			}
			if len(failed) != len(tt.expectedFailed) {
				t.Errorf("Expected %d failed checks, got %d", len(tt.expectedFailed), len(failed))
				return
			}
			for i, f := range failed {
				if f != tt.expectedFailed[i] {
					t.Errorf("Expected failed[%d] = %q, got %q", i, tt.expectedFailed[i], f)
				}
			}
		})
	}
}

func TestExecuteGraphQL(t *testing.T) {
	mockResponse := `{"data": {"test": "value"}}`

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" {
			t.Errorf("Expected POST request, got %s", r.Method)
		}
		if r.Header.Get("Authorization") != "Bearer test-token" {
			t.Errorf("Expected Bearer test-token, got %s", r.Header.Get("Authorization"))
		}
		if r.Header.Get("Content-Type") != "application/json" {
			t.Errorf("Expected application/json, got %s", r.Header.Get("Content-Type"))
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(mockResponse))
	}))
	defer ts.Close()

	client := NewClient("test-token", "test-user")
	client.httpClient = &http.Client{
		Transport: &redirectTransport{targetURL: ts.URL},
	}

	var result struct {
		Data struct {
			Test string `json:"test"`
		} `json:"data"`
	}

	err := client.executeGraphQL(context.Background(), "query { test }", &result)
	if err != nil {
		t.Fatalf("executeGraphQL failed: %v", err)
	}

	if result.Data.Test != "value" {
		t.Errorf("Expected test = 'value', got %q", result.Data.Test)
	}
}

func TestExecuteGraphQL_Error(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer ts.Close()

	client := NewClient("test-token", "test-user")
	client.httpClient = &http.Client{
		Transport: &redirectTransport{targetURL: ts.URL},
	}

	var result struct{}
	err := client.executeGraphQL(context.Background(), "query { test }", &result)
	if err == nil {
		t.Error("Expected error for 500 response, got nil")
	}
}
