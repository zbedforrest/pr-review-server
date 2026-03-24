package poller

import (
	"testing"
	"time"

	"pr-review-server/db"
	"pr-review-server/github"
)

func TestMergePRLists_NoDuplicates(t *testing.T) {
	reviewPRs := []github.PullRequest{
		{Owner: "owner", Repo: "repo", Number: 1, CommitSHA: "sha1"},
		{Owner: "owner", Repo: "repo", Number: 2, CommitSHA: "sha2"},
	}
	myPRs := []github.PullRequest{
		{Owner: "owner", Repo: "repo", Number: 2, CommitSHA: "sha2"}, // Duplicate
		{Owner: "owner", Repo: "repo", Number: 3, CommitSHA: "sha3"},
	}
	dbPRs := []db.PR{
		{RepoOwner: "owner", RepoName: "repo", PRNumber: 1, LastCommitSHA: "sha1"}, // Duplicate
		{RepoOwner: "owner", RepoName: "repo", PRNumber: 4, LastCommitSHA: "sha4"},
	}

	result := mergePRLists(reviewPRs, myPRs, dbPRs)

	// Should have 4 unique PRs (1, 2, 3, 4)
	if len(result) != 4 {
		t.Errorf("expected 4 unique PRs, got %d", len(result))
	}

	// Verify all expected PRs are present
	expected := map[int]bool{1: false, 2: false, 3: false, 4: false}
	for _, pr := range result {
		expected[pr.Number] = true
	}
	for num, found := range expected {
		if !found {
			t.Errorf("PR #%d was not found in result", num)
		}
	}
}

func TestMergePRLists_EmptyInputs(t *testing.T) {
	result := mergePRLists(nil, nil, nil)
	if len(result) != 0 {
		t.Errorf("expected empty result for empty inputs, got %d PRs", len(result))
	}

	// Test with just one non-empty input
	reviewPRs := []github.PullRequest{
		{Owner: "owner", Repo: "repo", Number: 1, CommitSHA: "sha1"},
	}
	result = mergePRLists(reviewPRs, nil, nil)
	if len(result) != 1 {
		t.Errorf("expected 1 PR, got %d", len(result))
	}
}

func TestGroupPRsByRepo_GroupsCorrectly(t *testing.T) {
	prs := []github.PullRequest{
		{Owner: "owner1", Repo: "repo1", Number: 1},
		{Owner: "owner1", Repo: "repo1", Number: 2},
		{Owner: "owner1", Repo: "repo2", Number: 3},
		{Owner: "owner2", Repo: "repo1", Number: 4},
	}

	grouped := groupPRsByRepo(prs)

	// Should have 3 groups
	if len(grouped) != 3 {
		t.Errorf("expected 3 groups, got %d", len(grouped))
	}

	// Check owner1/repo1 group
	if len(grouped["owner1/repo1"]) != 2 {
		t.Errorf("expected 2 PRs in owner1/repo1, got %d", len(grouped["owner1/repo1"]))
	}

	// Check owner1/repo2 group
	if len(grouped["owner1/repo2"]) != 1 {
		t.Errorf("expected 1 PR in owner1/repo2, got %d", len(grouped["owner1/repo2"]))
	}

	// Check owner2/repo1 group
	if len(grouped["owner2/repo1"]) != 1 {
		t.Errorf("expected 1 PR in owner2/repo1, got %d", len(grouped["owner2/repo1"]))
	}
}

func TestGroupPRsByRepo_EmptyInput(t *testing.T) {
	grouped := groupPRsByRepo(nil)
	if len(grouped) != 0 {
		t.Errorf("expected empty map for nil input, got %d groups", len(grouped))
	}

	grouped = groupPRsByRepo([]github.PullRequest{})
	if len(grouped) != 0 {
		t.Errorf("expected empty map for empty input, got %d groups", len(grouped))
	}
}

func TestBuildPRFromDB_ConvertsAllFields(t *testing.T) {
	createdAt := time.Date(2024, 1, 15, 10, 30, 0, 0, time.UTC)
	dbPR := db.PR{
		RepoOwner:     "owner",
		RepoName:      "repo",
		PRNumber:      123,
		LastCommitSHA: "abc123def456",
		Title:         "Test PR Title",
		Author:        "testauthor",
		CreatedAt:     &createdAt,
		Draft:         true,
	}

	ghPR := buildPRFromDB(dbPR)

	if ghPR.Owner != "owner" {
		t.Errorf("expected Owner 'owner', got '%s'", ghPR.Owner)
	}
	if ghPR.Repo != "repo" {
		t.Errorf("expected Repo 'repo', got '%s'", ghPR.Repo)
	}
	if ghPR.Number != 123 {
		t.Errorf("expected Number 123, got %d", ghPR.Number)
	}
	if ghPR.CommitSHA != "abc123def456" {
		t.Errorf("expected CommitSHA 'abc123def456', got '%s'", ghPR.CommitSHA)
	}
	if ghPR.Title != "Test PR Title" {
		t.Errorf("expected Title 'Test PR Title', got '%s'", ghPR.Title)
	}
	if ghPR.Author != "testauthor" {
		t.Errorf("expected Author 'testauthor', got '%s'", ghPR.Author)
	}
	if ghPR.URL != "https://github.com/owner/repo/pull/123" {
		t.Errorf("expected URL 'https://github.com/owner/repo/pull/123', got '%s'", ghPR.URL)
	}
	if ghPR.CreatedAt == nil || !ghPR.CreatedAt.Equal(createdAt) {
		t.Errorf("expected CreatedAt %v, got %v", createdAt, ghPR.CreatedAt)
	}
	if !ghPR.Draft {
		t.Errorf("expected Draft true, got false")
	}
}

func TestResolveTeamReviewStatuses_AllPending(t *testing.T) {
	result := resolveTeamReviewStatuses(
		[]string{"platform", "security"},
		nil,
		map[string][]string{
			"platform": {"alice", "bob"},
			"security": {"carol"},
		},
		map[string]string{"platform": "platform", "security": "security"},
		map[string]string{}, // no reviews
		"",                  // no current user
	)

	if result["platform"] != "pending" {
		t.Errorf("expected platform=pending, got %q", result["platform"])
	}
	if result["security"] != "pending" {
		t.Errorf("expected security=pending, got %q", result["security"])
	}
}

func TestResolveTeamReviewStatuses_OneTeamApproved(t *testing.T) {
	result := resolveTeamReviewStatuses(
		[]string{"platform", "security"},
		nil,
		map[string][]string{
			"platform": {"alice", "bob"},
			"security": {"carol"},
		},
		map[string]string{"platform": "platform", "security": "security"},
		map[string]string{"alice": "APPROVED"},
		"",
	)

	if result["platform"] != "approved" {
		t.Errorf("expected platform=approved, got %q", result["platform"])
	}
	if result["security"] != "pending" {
		t.Errorf("expected security=pending, got %q", result["security"])
	}
}

func TestResolveTeamReviewStatuses_ChangesRequestedTakesPriority(t *testing.T) {
	result := resolveTeamReviewStatuses(
		[]string{"platform"},
		nil,
		map[string][]string{
			"platform": {"alice", "bob"},
		},
		map[string]string{"platform": "platform"},
		map[string]string{
			"alice": "APPROVED",
			"bob":   "CHANGES_REQUESTED",
		},
		"",
	)

	if result["platform"] != "changes_requested" {
		t.Errorf("expected platform=changes_requested, got %q", result["platform"])
	}
}

func TestResolveTeamReviewStatuses_HighWaterMark(t *testing.T) {
	// Team was in previousTeams but not currentTeams (member already reviewed)
	result := resolveTeamReviewStatuses(
		[]string{},                       // current: empty (all teams reviewed)
		[]string{"platform", "security"}, // previous: had both teams
		map[string][]string{
			"platform": {"alice"},
			"security": {"carol"},
		},
		map[string]string{"platform": "platform", "security": "security"},
		map[string]string{"alice": "APPROVED"},
		"",
	)

	if len(result) != 2 {
		t.Errorf("expected 2 teams in result, got %d", len(result))
	}
	if result["platform"] != "approved" {
		t.Errorf("expected platform=approved, got %q", result["platform"])
	}
	if result["security"] != "pending" {
		t.Errorf("expected security=pending, got %q", result["security"])
	}
}

func TestResolveTeamReviewStatuses_EmptyInputs(t *testing.T) {
	result := resolveTeamReviewStatuses(nil, nil, nil, nil, nil, "")
	if len(result) != 0 {
		t.Errorf("expected empty result, got %v", result)
	}
}

func TestResolveTeamReviewStatuses_UnknownTeamSlug(t *testing.T) {
	result := resolveTeamReviewStatuses(
		[]string{"unknown-team"},
		nil,
		map[string][]string{},
		map[string]string{}, // no slug mapping
		map[string]string{"alice": "APPROVED"},
		"",
	)

	if result["unknown-team"] != "pending" {
		t.Errorf("expected unknown-team=pending, got %q", result["unknown-team"])
	}
}

func TestResolveTeamReviewStatuses_MyTeamPending(t *testing.T) {
	// Current user "bob" is on platform team. Platform is unsatisfied → my_pending.
	// Security team (user is NOT a member) stays "pending".
	result := resolveTeamReviewStatuses(
		[]string{"platform", "security"},
		nil,
		map[string][]string{
			"platform": {"alice", "bob"},
			"security": {"carol"},
		},
		map[string]string{"platform": "platform", "security": "security"},
		map[string]string{}, // no reviews
		"bob",
	)

	if result["platform"] != "my_pending" {
		t.Errorf("expected platform=my_pending, got %q", result["platform"])
	}
	if result["security"] != "pending" {
		t.Errorf("expected security=pending, got %q", result["security"])
	}
}

func TestResolveTeamReviewStatuses_MyTeamApproved_NoPrefix(t *testing.T) {
	// Current user "bob" is on platform team but it's already approved → stays "approved" (no my_ prefix)
	result := resolveTeamReviewStatuses(
		[]string{"platform"},
		nil,
		map[string][]string{
			"platform": {"alice", "bob"},
		},
		map[string]string{"platform": "platform"},
		map[string]string{"alice": "APPROVED"},
		"bob",
	)

	if result["platform"] != "approved" {
		t.Errorf("expected platform=approved, got %q", result["platform"])
	}
}

func TestResolveTeamReviewStatuses_MyTeamChangesRequested(t *testing.T) {
	// Current user "bob" is on platform team with changes_requested → my_changes_requested
	result := resolveTeamReviewStatuses(
		[]string{"platform"},
		nil,
		map[string][]string{
			"platform": {"alice", "bob"},
		},
		map[string]string{"platform": "platform"},
		map[string]string{"alice": "CHANGES_REQUESTED"},
		"bob",
	)

	if result["platform"] != "my_changes_requested" {
		t.Errorf("expected platform=my_changes_requested, got %q", result["platform"])
	}
}

func TestResolveTeamReviewStatuses_MyTeamCaseInsensitive(t *testing.T) {
	// Username matching should be case-insensitive
	result := resolveTeamReviewStatuses(
		[]string{"platform"},
		nil,
		map[string][]string{
			"platform": {"Alice", "Bob"},
		},
		map[string]string{"platform": "platform"},
		map[string]string{},
		"bob", // lowercase vs "Bob" in members
	)

	if result["platform"] != "my_pending" {
		t.Errorf("expected platform=my_pending, got %q", result["platform"])
	}
}

func TestParseTeamNames(t *testing.T) {
	result := parseTeamNames([]string{"team1:approved", "team2:pending", "__PERSONAL__"})
	if len(result) != 2 {
		t.Errorf("expected 2 team names, got %d: %v", len(result), result)
	}
	if result[0] != "team1" {
		t.Errorf("expected team1, got %q", result[0])
	}
	if result[1] != "team2" {
		t.Errorf("expected team2, got %q", result[1])
	}

	// Test with no status suffix (backward compat)
	result = parseTeamNames([]string{"platform"})
	if len(result) != 1 || result[0] != "platform" {
		t.Errorf("expected [platform], got %v", result)
	}

	// Test empty
	result = parseTeamNames(nil)
	if len(result) != 0 {
		t.Errorf("expected empty, got %v", result)
	}
}

func TestContainsPersonal(t *testing.T) {
	if !containsPersonal([]string{"team1", "__PERSONAL__"}) {
		t.Error("expected true when __PERSONAL__ present")
	}
	if containsPersonal([]string{"team1", "team2"}) {
		t.Error("expected false when __PERSONAL__ not present")
	}
	if containsPersonal(nil) {
		t.Error("expected false for nil input")
	}
}

func TestSlugifyTeamName(t *testing.T) {
	tests := []struct {
		input    string
		expected string
	}{
		{"Viewer Team", "viewer-team"},
		{"Platform Engineers", "platform-engineers"},
		{"security", "security"},
		{"My Cool Team!", "my-cool-team"},
		{"  spaces  ", "spaces"},
		{"UPPER-case", "upper-case"},
		{"already-slugified", "already-slugified"},
		{"multiple   spaces", "multiple-spaces"},
	}
	for _, tt := range tests {
		result := slugifyTeamName(tt.input)
		if result != tt.expected {
			t.Errorf("slugifyTeamName(%q) = %q, want %q", tt.input, result, tt.expected)
		}
	}
}

func TestResolveTeamReviewStatuses_DerivedSlugFallback(t *testing.T) {
	// Team "Viewer Team" is not in teamSlugs, but slugifyTeamName("Viewer Team") -> "viewer-team"
	// which has members in teamMembers
	result := resolveTeamReviewStatuses(
		[]string{"Viewer Team"},
		nil,
		map[string][]string{
			"viewer-team": {"alice"},
		},
		map[string]string{}, // no explicit slug mapping
		map[string]string{"alice": "APPROVED"},
		"",
	)

	if result["Viewer Team"] != "approved" {
		t.Errorf("expected Viewer Team=approved, got %q", result["Viewer Team"])
	}
}

func TestCollectUniqueSlugs(t *testing.T) {
	data := map[string]*github.ReviewerGroupData{
		"owner/repo/1": {TeamSlugs: map[string]string{"Platform": "platform", "Security": "security"}},
		"owner/repo/2": {TeamSlugs: map[string]string{"Platform": "platform"}}, // duplicate
	}
	slugs := collectUniqueSlugs(data)
	if len(slugs) != 2 {
		t.Errorf("expected 2 unique slugs, got %d", len(slugs))
	}
	if !slugs["platform"] || !slugs["security"] {
		t.Errorf("expected platform and security slugs, got %v", slugs)
	}
}

func TestShouldUpdateViaTeams(t *testing.T) {
	tests := []struct {
		name     string
		input    []string
		expected bool
	}{
		{"non-empty teams", []string{"platform:pending", "security:approved"}, true},
		{"single team", []string{"platform:pending"}, true},
		{"personal only", []string{"__PERSONAL__"}, true},
		{"teams plus personal", []string{"platform:approved", "__PERSONAL__"}, true},
		{"nil", nil, false},
		{"empty slice", []string{}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := shouldUpdateViaTeams(tt.input)
			if got != tt.expected {
				t.Errorf("shouldUpdateViaTeams(%v) = %v, want %v", tt.input, got, tt.expected)
			}
		})
	}
}
