package github

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
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

func TestBuildCIStatusQuery(t *testing.T) {
	prs := []PRInfo{
		{Owner: "owner1", Repo: "repo1", Number: 101},
		{Owner: "owner2", Repo: "repo2", Number: 102},
	}

	query := buildCIStatusQuery(prs)

	// The query must target each PR's current head via commits(last: 1),
	// never a stored commit oid (which can be stale after a push).
	if strings.Contains(query, "object(oid:") {
		t.Errorf("CI status query must not query by stored commit oid:\n%s", query)
	}
	for _, want := range []string{
		`pr0: repository(owner: "owner1", name: "repo1")`,
		`pullRequest(number: 101)`,
		`pr1: repository(owner: "owner2", name: "repo2")`,
		`pullRequest(number: 102)`,
		`commits(last: 1)`,
		`statusCheckRollup`,
	} {
		if !strings.Contains(query, want) {
			t.Errorf("Expected CI status query to contain %q:\n%s", want, query)
		}
	}
}

func TestBuildPRStateQueryIncludesDraft(t *testing.T) {
	query := buildPRStateQuery([]PRInfo{{Owner: "owner1", Repo: "repo1", Number: 7}})
	for _, want := range []string{"state", "headRefOid", "isDraft"} {
		if !strings.Contains(query, want) {
			t.Errorf("Expected PR state query to contain %q:\n%s", want, query)
		}
	}
}

func TestBuildReviewDataQueryIncludesCommitAndHead(t *testing.T) {
	client := NewClient("token", "current-user")
	query := client.buildReviewDataQuery("owner1", "repo1", []PullRequest{{Owner: "owner1", Repo: "repo1", Number: 7}})
	for _, want := range []string{"reviews(last: 100)", "commit { oid }", "headRefOid", "isDraft", "pageInfo { hasPreviousPage }"} {
		if !strings.Contains(query, want) {
			t.Errorf("Expected review data query to contain %q:\n%s", want, query)
		}
	}
	prLevel := query[:strings.Index(query, "reviews(last: 100)")]
	if !strings.Contains(prLevel, "state") {
		t.Errorf("Expected the pull request itself (not just its reviews) to select state:\n%s", query)
	}
}

func TestAttentionByUser(t *testing.T) {
	review := func(login, state, oid string) ReviewNode {
		node := ReviewNode{Author: &ReviewAuthor{Login: login}, State: state}
		if oid != "" {
			node.Commit = &ReviewCommit{OID: oid}
		}
		return node
	}

	tests := []struct {
		name      string
		reviews   []ReviewNode
		head      string
		truncated bool
		want      map[string]bool
	}{
		{
			name:    "changes requested at A, head moved to B",
			reviews: []ReviewNode{review("alice", "CHANGES_REQUESTED", "A")},
			head:    "B",
			want:    map[string]bool{"alice": true},
		},
		{
			name:    "commented on the new head after requesting changes",
			reviews: []ReviewNode{review("alice", "CHANGES_REQUESTED", "A"), review("alice", "COMMENTED", "B")},
			head:    "B",
			want:    map[string]bool{"alice": false},
		},
		{
			name:    "requested changes again on the new head",
			reviews: []ReviewNode{review("alice", "CHANGES_REQUESTED", "A"), review("alice", "CHANGES_REQUESTED", "B")},
			head:    "B",
			want:    map[string]bool{"alice": false},
		},
		{
			name:    "approved at A, head moved to B",
			reviews: []ReviewNode{review("alice", "APPROVED", "A")},
			head:    "B",
			want:    map[string]bool{"alice": false},
		},
		{
			name:    "changes requested then dismissed",
			reviews: []ReviewNode{review("alice", "CHANGES_REQUESTED", "A"), review("alice", "DISMISSED", "A")},
			head:    "B",
			want:    map[string]bool{"alice": false},
		},
		{
			name:    "another user's approval on the head does not clear",
			reviews: []ReviewNode{review("alice", "CHANGES_REQUESTED", "A"), review("bob", "APPROVED", "B")},
			head:    "B",
			want:    map[string]bool{"alice": true, "bob": false},
		},
		{
			name:    "pending review on the head is ignored",
			reviews: []ReviewNode{review("alice", "CHANGES_REQUESTED", "A"), review("alice", "PENDING", "B")},
			head:    "B",
			want:    map[string]bool{"alice": true},
		},
		{
			name:    "head equals the reviewed commit",
			reviews: []ReviewNode{review("alice", "CHANGES_REQUESTED", "A")},
			head:    "A",
			want:    map[string]bool{"alice": false},
		},
		{
			name:    "empty head leaves the user absent",
			reviews: []ReviewNode{review("alice", "CHANGES_REQUESTED", "A")},
			head:    "",
			want:    map[string]bool{},
		},
		{
			name:    "nil commit on the deciding review leaves the user absent",
			reviews: []ReviewNode{review("alice", "CHANGES_REQUESTED", "")},
			head:    "B",
			want:    map[string]bool{},
		},
		{
			name:    "nil commit on a later review leaves the user absent",
			reviews: []ReviewNode{review("alice", "CHANGES_REQUESTED", "A"), review("alice", "COMMENTED", "")},
			head:    "B",
			want:    map[string]bool{},
		},
		{
			name:    "nil commit on the deciding review but a comment on the head",
			reviews: []ReviewNode{review("alice", "CHANGES_REQUESTED", ""), review("alice", "COMMENTED", "B")},
			head:    "B",
			want:    map[string]bool{"alice": false},
		},
		{
			name:    "nil author is skipped",
			reviews: []ReviewNode{{Author: nil, State: "CHANGES_REQUESTED", Commit: &ReviewCommit{OID: "A"}}},
			head:    "B",
			want:    map[string]bool{},
		},
		{
			name:      "truncated history with only a comment in the window leaves the user absent",
			reviews:   []ReviewNode{review("alice", "COMMENTED", "A")},
			head:      "B",
			truncated: true,
			want:      map[string]bool{},
		},
		{
			name:      "truncated history with a decision in the window still decides",
			reviews:   []ReviewNode{review("alice", "CHANGES_REQUESTED", "A"), review("bob", "APPROVED", "A"), review("carol", "COMMENTED", "B")},
			head:      "B",
			truncated: true,
			want:      map[string]bool{"alice": true, "bob": false, "carol": false},
		},
		{
			name:      "truncated history with only a comment on the head clears",
			reviews:   []ReviewNode{review("alice", "COMMENTED", "B")},
			head:      "B",
			truncated: true,
			want:      map[string]bool{"alice": false},
		},
		{
			name:      "truncated history with only a comment on an old head leaves the user absent",
			reviews:   []ReviewNode{review("alice", "COMMENTED", "A")},
			head:      "B",
			truncated: true,
			want:      map[string]bool{},
		},
		{
			name:      "truncated history with only a nil-commit comment leaves the user absent",
			reviews:   []ReviewNode{review("alice", "COMMENTED", "")},
			head:      "B",
			truncated: true,
			want:      map[string]bool{},
		},
		{
			name:    "untruncated history with only a comment on an old head is false",
			reviews: []ReviewNode{review("alice", "COMMENTED", "A")},
			head:    "B",
			want:    map[string]bool{"alice": false},
		},
		{
			name:      "truncated history with a dismissal in the window clears",
			reviews:   []ReviewNode{review("alice", "DISMISSED", "A")},
			head:      "B",
			truncated: true,
			want:      map[string]bool{"alice": false},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			reviews := ReviewsData{Nodes: tt.reviews, PageInfo: ReviewsPageInfo{HasPreviousPage: tt.truncated}}
			got := attentionByUser(reviews, tt.head)
			if len(got) != len(tt.want) {
				t.Fatalf("attentionByUser() = %v, want %v", got, tt.want)
			}
			for login, want := range tt.want {
				if actual, ok := got[login]; !ok || actual != want {
					t.Errorf("attentionByUser()[%q] = %v (present=%v), want %v", login, actual, ok, want)
				}
			}
		})
	}
}

func TestFetchReviewDataForRepo_PopulatesAttentionAndHead(t *testing.T) {
	body := `{"data":{"pr0":{"pullRequest":{"number":7,"state":"MERGED","headRefOid":"B","isDraft":true,"reviews":{"nodes":[
		{"author":{"login":"alice"},"state":"CHANGES_REQUESTED","commit":{"oid":"A"}},
		{"author":{"login":"bob"},"state":"APPROVED","commit":{"oid":"B"}},
		{"author":{"login":"carol"},"state":"CHANGES_REQUESTED","commit":{"oid":"B"}}
	]}}}}}`
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(body))
	}))
	defer ts.Close()

	client := NewClient("test-token", "")
	client.httpClient = &http.Client{Transport: &redirectTransport{targetURL: ts.URL}}

	results, err := client.fetchReviewDataForRepo(context.Background(), []PullRequest{{Owner: "acme", Repo: "example", Number: 7}})
	if err != nil {
		t.Fatalf("fetchReviewDataForRepo failed: %v", err)
	}
	data := results["acme/example/7"]
	if data == nil {
		t.Fatalf("expected result for acme/example/7, got %v", results)
	}
	if data.HeadOID != "B" {
		t.Errorf("HeadOID = %q, want B", data.HeadOID)
	}
	if !data.IsDraft {
		t.Errorf("IsDraft = false, want true from the same response as the head")
	}
	if data.State != "MERGED" {
		t.Errorf("State = %q, want MERGED from the same response as the head", data.State)
	}
	if data.ApprovalCount != 1 || data.UserReviews["alice"] != "CHANGES_REQUESTED" {
		t.Errorf("existing review reduction changed: approvals=%d userReviews=%v", data.ApprovalCount, data.UserReviews)
	}
	if !data.AttentionByUser["alice"] {
		t.Errorf("expected alice to need attention, got %v", data.AttentionByUser)
	}
	if v, ok := data.AttentionByUser["bob"]; !ok || v {
		t.Errorf("expected bob present and false, got %v (present=%v)", v, ok)
	}
	if v, ok := data.AttentionByUser["carol"]; !ok || v {
		t.Errorf("expected carol (requested changes on the head) present and false, got %v (present=%v)", v, ok)
	}
}

func TestFetchReviewDataForRepo_TruncatedHistoryLeavesUndecidedUsersUnknown(t *testing.T) {
	body := `{"data":{"pr0":{"pullRequest":{"number":7,"headRefOid":"B","isDraft":false,"reviews":{
		"pageInfo":{"hasPreviousPage":true},
		"nodes":[
			{"author":{"login":"alice"},"state":"COMMENTED","commit":{"oid":"A"}},
			{"author":{"login":"bob"},"state":"APPROVED","commit":{"oid":"A"}}
		]}}}}}`
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(body))
	}))
	defer ts.Close()

	client := NewClient("test-token", "")
	client.httpClient = &http.Client{Transport: &redirectTransport{targetURL: ts.URL}}

	results, err := client.fetchReviewDataForRepo(context.Background(), []PullRequest{{Owner: "acme", Repo: "example", Number: 7}})
	if err != nil {
		t.Fatalf("fetchReviewDataForRepo failed: %v", err)
	}
	data := results["acme/example/7"]
	if data == nil {
		t.Fatalf("expected result for acme/example/7, got %v", results)
	}
	if _, ok := data.AttentionByUser["alice"]; ok {
		t.Errorf("expected alice absent when her decision may be outside the window, got %v", data.AttentionByUser)
	}
	if v, ok := data.AttentionByUser["bob"]; !ok || v {
		t.Errorf("expected bob present and false, got %v (present=%v)", v, ok)
	}
	if data.UserReviews["alice"] != "COMMENTED" {
		t.Errorf("existing review reduction changed: userReviews=%v", data.UserReviews)
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

func TestDecodeGraphQLResponse_PartialErrorStillDecodesData(t *testing.T) {
	// GitHub returns HTTP 200 with both `data` (partial, null where it failed)
	// and an `errors` array. The decoder must still populate the data it got.
	body := `{
		"data": {"pr0": {"pullRequest": {"number": 29205}}},
		"errors": [
			{"type": "FORBIDDEN", "message": "Resource not accessible by integration",
			 "path": ["pr0", "pullRequest", "timelineItems"]}
		]
	}`
	var result struct {
		Data map[string]struct {
			PullRequest struct {
				Number int `json:"number"`
			} `json:"pullRequest"`
		} `json:"data"`
	}
	if err := decodeGraphQLResponse("test", strings.NewReader(body), &result); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Data["pr0"].PullRequest.Number != 29205 {
		t.Errorf("expected data to decode despite partial errors, got %+v", result.Data)
	}
}

func TestDecodeGraphQLResponse_NoErrorsField(t *testing.T) {
	body := `{"data": {"x": 1}}`
	var result struct {
		Data map[string]int `json:"data"`
	}
	if err := decodeGraphQLResponse("test", strings.NewReader(body), &result); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Data["x"] != 1 {
		t.Errorf("expected clean decode, got %+v", result.Data)
	}
}

func TestDecodeGraphQLResponse_MalformedJSON(t *testing.T) {
	var result struct{}
	if err := decodeGraphQLResponse("test", strings.NewReader("{not json"), &result); err == nil {
		t.Error("expected error decoding malformed JSON")
	}
}

func TestDecodeGraphQLResponse_RateLimitReturnsError(t *testing.T) {
	body := `{"data":{"pr0":null},"errors":[{"type":"RATE_LIMITED","message":"API rate limit already exceeded for installation ID 123."}]}`
	var result struct {
		Data map[string]interface{} `json:"data"`
	}
	err := decodeGraphQLResponse("test", strings.NewReader(body), &result)
	if !errors.Is(err, ErrGraphQLRateLimited) {
		t.Fatalf("expected ErrGraphQLRateLimited, got %v", err)
	}
	// data must still be decoded (caller may inspect partial results)
	if _, ok := result.Data["pr0"]; !ok {
		t.Error("expected partial data to still decode")
	}
}

func TestDecodeGraphQLResponse_BenignErrorNotRateLimit(t *testing.T) {
	// A FORBIDDEN on a CI field must NOT be treated as rate limiting.
	body := `{"data":{"pr0":{"x":1}},"errors":[{"type":"FORBIDDEN","message":"Resource not accessible by integration","path":["pr0","object","statusCheckRollup"]}]}`
	var result struct {
		Data map[string]interface{} `json:"data"`
	}
	if err := decodeGraphQLResponse("test", strings.NewReader(body), &result); err != nil {
		t.Fatalf("benign partial error should not return an error, got %v", err)
	}
}

// Reorder guard: a rate-limited response whose body also fails to decode into
// the result must still surface as ErrGraphQLRateLimited (the actionable signal),
// not a generic decode error.
func TestDecodeGraphQLResponse_RateLimitWinsOverDecodeError(t *testing.T) {
	body := `{"data":{"pr0":{"pullRequest":{"number":"not-an-int"}}},"errors":[{"type":"RATE_LIMITED","message":"API rate limit already exceeded"}]}`
	var result struct {
		Data map[string]struct {
			PullRequest struct {
				Number int `json:"number"`
			} `json:"pullRequest"`
		} `json:"data"`
	}
	err := decodeGraphQLResponse("test", strings.NewReader(body), &result)
	if !errors.Is(err, ErrGraphQLRateLimited) {
		t.Fatalf("expected ErrGraphQLRateLimited to win over decode error, got %v", err)
	}
}
