package poller

import (
	"context"
	"errors"
	"testing"
	"time"

	"pr-review-server/db"
	"pr-review-server/github"
)

// pruneFixture returns the common scaffolding for pruneStaleViaTeams tests:
// one PR (owner/repo/1, DB ID 10) and one user (alice, ID 1) with an existing
// via_teams row linking them.
func pruneFixture(viaTeams string) (*MockDatabase, *MockGitHubClient, []github.PullRequest, map[string]*db.PR, []db.User) {
	mockDB := NewMockDatabase()
	mockGH := NewMockGitHubClient()

	allPRs := []github.PullRequest{{Owner: "owner", Repo: "repo", Number: 1, Author: "author"}}
	dbPRMap := map[string]*db.PR{
		"owner/repo/1": {ID: 10, RepoOwner: "owner", RepoName: "repo", PRNumber: 1},
	}
	users := []db.User{{ID: 1, GitHubUsername: "alice"}}

	mockDB.UserPRViews[viewMockKey(1, 10)] = &db.UserPRView{
		UserID:   1,
		PRID:     10,
		ViaTeams: viaTeams,
	}
	return mockDB, mockGH, allPRs, dbPRMap, users
}

func freshGroupData(teams ...string) map[string]*github.ReviewerGroupData {
	slugs := make(map[string]string, len(teams))
	for _, t := range teams {
		slugs[t] = slugifyTeamName(t)
	}
	return map[string]*github.ReviewerGroupData{
		"owner/repo/1": {
			Owner: "owner", Repo: "repo", Number: 1,
			ReviewerGroups: teams,
			TeamSlugs:      slugs,
			OrgName:        "testorg",
		},
	}
}

func TestPruneStaleViaTeams_FreshData_EntitledRowKept(t *testing.T) {
	mockDB, mockGH, allPRs, dbPRMap, users := pruneFixture(`["Platform:my_pending"]`)
	poller := newTestPoller(mockGH, mockDB)

	entitled := map[userPRViewKey]bool{{UserID: 1, PRID: 10}: true}
	changedPRs := map[string]bool{}
	poller.pruneStaleViaTeams(context.Background(), "testorg", allPRs, dbPRMap,
		freshGroupData("Platform"), entitled, nil, users, changedPRs)

	if len(mockDB.BatchPruneViaTeamsCalls) != 0 {
		t.Fatalf("expected no prune calls, got %v", mockDB.BatchPruneViaTeamsCalls)
	}
	if changedPRs["owner/repo/1"] {
		t.Error("expected PR not to be marked changed")
	}
}

func TestPruneStaleViaTeams_FreshData_StaleRowClearedAndHidden(t *testing.T) {
	mockDB, mockGH, allPRs, dbPRMap, users := pruneFixture(`["Platform:my_pending"]`)
	poller := newTestPoller(mockGH, mockDB)

	changedPRs := map[string]bool{}
	poller.pruneStaleViaTeams(context.Background(), "testorg", allPRs, dbPRMap,
		freshGroupData("Platform"), map[userPRViewKey]bool{}, nil, users, changedPRs)

	if len(mockDB.BatchPruneViaTeamsCalls) != 1 {
		t.Fatalf("expected 1 prune call, got %d", len(mockDB.BatchPruneViaTeamsCalls))
	}
	prunes := mockDB.BatchPruneViaTeamsCalls[0]
	if len(prunes) != 1 || prunes[0].UserID != 1 || prunes[0].PRID != 10 {
		t.Fatalf("unexpected prunes: %+v", prunes)
	}
	if !prunes[0].Hide {
		t.Error("expected Hide=true for non-author row with no review status")
	}
	view := mockDB.UserPRViews[viewMockKey(1, 10)]
	if view.ViaTeams != "[]" || !view.Hidden {
		t.Errorf("expected cleared+hidden view, got via_teams=%q hidden=%v", view.ViaTeams, view.Hidden)
	}
	if !changedPRs["owner/repo/1"] {
		t.Error("expected PR marked changed for snapshot broadcast")
	}
}

func TestPruneStaleViaTeams_FreshData_AuthorRowClearedNotHidden(t *testing.T) {
	mockDB, mockGH, allPRs, dbPRMap, users := pruneFixture(`["Platform:pending"]`)
	mockDB.UserPRViews[viewMockKey(1, 10)].IsAuthor = true
	poller := newTestPoller(mockGH, mockDB)

	poller.pruneStaleViaTeams(context.Background(), "testorg", allPRs, dbPRMap,
		freshGroupData("Platform"), map[userPRViewKey]bool{}, nil, users, map[string]bool{})

	if len(mockDB.BatchPruneViaTeamsCalls) != 1 {
		t.Fatalf("expected 1 prune call, got %d", len(mockDB.BatchPruneViaTeamsCalls))
	}
	if mockDB.BatchPruneViaTeamsCalls[0][0].Hide {
		t.Error("expected Hide=false for author row")
	}
	view := mockDB.UserPRViews[viewMockKey(1, 10)]
	if view.ViaTeams != "[]" || view.Hidden {
		t.Errorf("expected cleared but visible view, got via_teams=%q hidden=%v", view.ViaTeams, view.Hidden)
	}
}

func TestPruneStaleViaTeams_FreshData_ReviewedRowClearedNotHidden(t *testing.T) {
	mockDB, mockGH, allPRs, dbPRMap, users := pruneFixture(`["Platform:pending"]`)
	mockDB.UserPRViews[viewMockKey(1, 10)].ReviewStatus = "APPROVED"
	poller := newTestPoller(mockGH, mockDB)

	poller.pruneStaleViaTeams(context.Background(), "testorg", allPRs, dbPRMap,
		freshGroupData("Platform"), map[userPRViewKey]bool{}, nil, users, map[string]bool{})

	if len(mockDB.BatchPruneViaTeamsCalls) != 1 {
		t.Fatalf("expected 1 prune call, got %d", len(mockDB.BatchPruneViaTeamsCalls))
	}
	if mockDB.BatchPruneViaTeamsCalls[0][0].Hide {
		t.Error("expected Hide=false for row with review activity")
	}
}

func TestPruneStaleViaTeams_FreshData_FailedSlugSkipsPR(t *testing.T) {
	mockDB, mockGH, allPRs, dbPRMap, users := pruneFixture(`["Platform:my_pending"]`)
	poller := newTestPoller(mockGH, mockDB)

	// Membership for "platform" couldn't be fetched this cycle — the entitled
	// set is unreliable for this PR, so the row must be kept.
	failedSlugs := map[string]bool{"platform": true}
	poller.pruneStaleViaTeams(context.Background(), "testorg", allPRs, dbPRMap,
		freshGroupData("Platform"), map[userPRViewKey]bool{}, failedSlugs, users, map[string]bool{})

	if len(mockDB.BatchPruneViaTeamsCalls) != 0 {
		t.Fatalf("expected no prune calls when member fetch failed, got %v", mockDB.BatchPruneViaTeamsCalls)
	}
}

func TestPruneStaleViaTeams_FreshData_RowWithNotesClearedNotHidden(t *testing.T) {
	mockDB, mockGH, allPRs, dbPRMap, users := pruneFixture(`["Platform:pending"]`)
	mockDB.UserPRViews[viewMockKey(1, 10)].Notes = "check perf"
	poller := newTestPoller(mockGH, mockDB)

	poller.pruneStaleViaTeams(context.Background(), "testorg", allPRs, dbPRMap,
		freshGroupData("Platform"), map[userPRViewKey]bool{}, nil, users, map[string]bool{})

	if len(mockDB.BatchPruneViaTeamsCalls) != 1 {
		t.Fatalf("expected 1 prune call, got %d", len(mockDB.BatchPruneViaTeamsCalls))
	}
	if mockDB.BatchPruneViaTeamsCalls[0][0].Hide {
		t.Error("expected Hide=false: hiding would bury the user's notes")
	}
	view := mockDB.UserPRViews[viewMockKey(1, 10)]
	if view.ViaTeams != "[]" || view.Hidden {
		t.Errorf("expected cleared but visible view, got via_teams=%q hidden=%v", view.ViaTeams, view.Hidden)
	}
}

func TestPruneStaleViaTeams_UnchangedPR_StillMemberKept(t *testing.T) {
	mockDB, mockGH, allPRs, dbPRMap, users := pruneFixture(`["Platform:my_pending"]`)
	mockGH.GetOrgTeamMembersFunc = func(ctx context.Context, orgName, teamSlug string) ([]string, error) {
		return []string{"ALICE"}, nil // case differs from row's user on purpose
	}
	poller := newTestPoller(mockGH, mockDB)

	// No fresh reviewer-group data for this PR → membership path.
	poller.pruneStaleViaTeams(context.Background(), "testorg", allPRs, dbPRMap,
		map[string]*github.ReviewerGroupData{}, map[userPRViewKey]bool{}, nil, users, map[string]bool{})

	if len(mockDB.BatchPruneViaTeamsCalls) != 0 {
		t.Fatalf("expected no prune calls for current member, got %v", mockDB.BatchPruneViaTeamsCalls)
	}
}

func TestPruneStaleViaTeams_UnchangedPR_LeftTeamPruned(t *testing.T) {
	mockDB, mockGH, allPRs, dbPRMap, users := pruneFixture(`["Platform:my_pending"]`)
	var requestedSlugs []string
	mockGH.GetOrgTeamMembersFunc = func(ctx context.Context, orgName, teamSlug string) ([]string, error) {
		requestedSlugs = append(requestedSlugs, teamSlug)
		return []string{"bob"}, nil
	}
	poller := newTestPoller(mockGH, mockDB)

	changedPRs := map[string]bool{}
	poller.pruneStaleViaTeams(context.Background(), "testorg", allPRs, dbPRMap,
		map[string]*github.ReviewerGroupData{}, map[userPRViewKey]bool{}, nil, users, changedPRs)

	if len(requestedSlugs) != 1 || requestedSlugs[0] != "platform" {
		t.Errorf("expected membership lookup for slug 'platform', got %v", requestedSlugs)
	}
	if len(mockDB.BatchPruneViaTeamsCalls) != 1 {
		t.Fatalf("expected 1 prune call, got %d", len(mockDB.BatchPruneViaTeamsCalls))
	}
	view := mockDB.UserPRViews[viewMockKey(1, 10)]
	if view.ViaTeams != "[]" || !view.Hidden {
		t.Errorf("expected cleared+hidden view, got via_teams=%q hidden=%v", view.ViaTeams, view.Hidden)
	}
	if !changedPRs["owner/repo/1"] {
		t.Error("expected PR marked changed")
	}
}

func TestPruneStaleViaTeams_UnchangedPR_PersonalRequestKept(t *testing.T) {
	mockDB, mockGH, allPRs, dbPRMap, users := pruneFixture(`["__PERSONAL__"]`)
	mockGH.GetOrgTeamMembersFunc = func(ctx context.Context, orgName, teamSlug string) ([]string, error) {
		t.Fatalf("unexpected membership lookup for personally-requested row")
		return nil, nil
	}
	poller := newTestPoller(mockGH, mockDB)

	poller.pruneStaleViaTeams(context.Background(), "testorg", allPRs, dbPRMap,
		map[string]*github.ReviewerGroupData{}, map[userPRViewKey]bool{}, nil, users, map[string]bool{})

	if len(mockDB.BatchPruneViaTeamsCalls) != 0 {
		t.Fatalf("expected no prune calls for personal request, got %v", mockDB.BatchPruneViaTeamsCalls)
	}
}

func TestPruneStaleViaTeams_UnchangedPR_MemberFetchErrorKept(t *testing.T) {
	mockDB, mockGH, allPRs, dbPRMap, users := pruneFixture(`["Platform:my_pending"]`)
	mockGH.GetOrgTeamMembersFunc = func(ctx context.Context, orgName, teamSlug string) ([]string, error) {
		return nil, errors.New("boom")
	}
	poller := newTestPoller(mockGH, mockDB)

	poller.pruneStaleViaTeams(context.Background(), "testorg", allPRs, dbPRMap,
		map[string]*github.ReviewerGroupData{}, map[userPRViewKey]bool{}, nil, users, map[string]bool{})

	if len(mockDB.BatchPruneViaTeamsCalls) != 0 {
		t.Fatalf("expected no prune calls when membership is unverifiable, got %v", mockDB.BatchPruneViaTeamsCalls)
	}
}

func TestPruneStaleViaTeams_EmptyOrgName_NoOp(t *testing.T) {
	mockDB, mockGH, allPRs, dbPRMap, users := pruneFixture(`["Platform:my_pending"]`)
	poller := newTestPoller(mockGH, mockDB)

	poller.pruneStaleViaTeams(context.Background(), "", allPRs, dbPRMap,
		freshGroupData("Platform"), map[userPRViewKey]bool{}, nil, users, map[string]bool{})

	if len(mockDB.BatchPruneViaTeamsCalls) != 0 {
		t.Fatalf("expected no prune calls without an org name, got %v", mockDB.BatchPruneViaTeamsCalls)
	}
}

// e2eTeamPollSetup wires a full mock stack where alice sees owner/repo/1 via
// the "Platform" team. Returns the poller plus the mutable members slice
// holder so tests can simulate membership changes between cycles.
func e2eTeamPollSetup(t *testing.T) (*Poller, *MockDatabase, *MockGitHubClient, *[]string) {
	t.Helper()
	mockGH := NewMockGitHubClient()
	mockDB := NewMockDatabase()
	mockStorage := NewMockReviewStorage()
	mockGenerator := NewMockReviewGenerator()

	mockGH.PRsRequestingReview = []github.PullRequest{
		{Owner: "owner", Repo: "repo", Number: 1, CommitSHA: "abc123def456789012345678901234567890abcd", Title: "Test PR", Author: "author"},
	}
	mockGH.GetPRHeadSHAResults["owner/repo/1"] = struct {
		SHA string
		Err error
	}{"abc123def456789012345678901234567890abcd", nil}
	mockGH.IsPROpenResults["owner/repo/1"] = struct {
		IsOpen bool
		Err    error
	}{true, nil}
	mockGH.BatchGetReviewerGroupsResults = map[string]*github.ReviewerGroupData{
		"owner/repo/1": {
			Owner: "owner", Repo: "repo", Number: 1,
			ReviewerGroups: []string{"Platform"},
			TeamSlugs:      map[string]string{"Platform": "platform"},
			OrgName:        "testorg",
		},
	}

	members := []string{"alice"}
	mockGH.GetOrgTeamMembersFunc = func(ctx context.Context, orgName, teamSlug string) ([]string, error) {
		return members, nil
	}
	mockDB.Users = []db.User{{ID: 1, GitHubUsername: "alice"}}

	return newTestPollerFull(mockGH, mockDB, mockStorage, mockGenerator), mockDB, mockGH, &members
}

// expireTeamCache forces the next getTeamMembers call to refetch instead of
// serving the 5-minute cache from the previous cycle.
func expireTeamCache(p *Poller) {
	p.teamCacheMutex.Lock()
	p.teamMemberCache = nil
	p.teamCacheExpiry = time.Time{}
	p.teamCacheMutex.Unlock()
}

// End-to-end regression for issue #7: a user who leaves a team must have the
// stale via_teams row cleared and the PR hidden on the next poll cycle.
func TestPoll_PrunesStaleViaTeams_UserLeftTeam(t *testing.T) {
	poller, mockDB, _, members := e2eTeamPollSetup(t)
	ctx := context.Background()

	// Cycle 1: alice is on Platform, so the view row gets via_teams.
	poller.poll(ctx, false)
	view := mockDB.UserPRViews[viewMockKey(1, 0)] // mock PRs keep ID 0
	if view == nil {
		t.Fatal("expected a user PR view for alice after cycle 1")
	}
	if view.ViaTeams != `["Platform:my_pending"]` {
		t.Fatalf("expected via_teams for Platform after cycle 1, got %q", view.ViaTeams)
	}
	if view.Hidden {
		t.Fatal("expected view visible after cycle 1")
	}

	// Alice leaves the team.
	*members = []string{}
	expireTeamCache(poller)

	// Cycle 2: the reconciliation pass must clear and hide the stale row.
	poller.poll(ctx, false)
	view = mockDB.UserPRViews[viewMockKey(1, 0)]
	if view.ViaTeams != "[]" {
		t.Errorf("expected via_teams cleared after alice left the team, got %q", view.ViaTeams)
	}
	if !view.Hidden {
		t.Error("expected view hidden: alice is not the author and has no review activity")
	}
}

// End-to-end regression for issue #7's second case: a team removed from the
// PR's reviewer list must stop granting visibility to its members.
func TestPoll_PrunesStaleViaTeams_TeamRemovedFromPR(t *testing.T) {
	poller, mockDB, mockGH, _ := e2eTeamPollSetup(t)
	ctx := context.Background()

	poller.poll(ctx, false)
	view := mockDB.UserPRViews[viewMockKey(1, 0)]
	if view == nil || view.ViaTeams != `["Platform:my_pending"]` {
		t.Fatalf("expected via_teams for Platform after cycle 1, got %+v", view)
	}

	// The Platform team is removed from the PR's reviewer list (alice is
	// still a member of the team — that no longer matters).
	mockGH.BatchGetReviewerGroupsResults["owner/repo/1"].ReviewerGroups = nil
	mockGH.BatchGetReviewerGroupsResults["owner/repo/1"].TeamSlugs = nil
	expireTeamCache(poller)

	poller.poll(ctx, false)
	view = mockDB.UserPRViews[viewMockKey(1, 0)]
	if view.ViaTeams != "[]" {
		t.Errorf("expected via_teams cleared after team removed from PR, got %q", view.ViaTeams)
	}
	if !view.Hidden {
		t.Error("expected view hidden after team removed from PR")
	}
}

// Membership flapping guard: a user who is still on the team must keep the
// row across cycles (no prune, still visible).
func TestPoll_KeepsViaTeams_WhileStillMember(t *testing.T) {
	poller, mockDB, _, _ := e2eTeamPollSetup(t)
	ctx := context.Background()

	poller.poll(ctx, false)
	expireTeamCache(poller)
	poller.poll(ctx, false)

	view := mockDB.UserPRViews[viewMockKey(1, 0)]
	if view == nil || view.ViaTeams != `["Platform:my_pending"]` {
		t.Fatalf("expected via_teams intact after two cycles, got %+v", view)
	}
	if view.Hidden {
		t.Error("expected view still visible")
	}
	if len(mockDB.BatchPruneViaTeamsCalls) != 0 {
		t.Errorf("expected no prune calls, got %v", mockDB.BatchPruneViaTeamsCalls)
	}
}

func TestPruneStaleViaTeams_UnknownUserRowSkipped(t *testing.T) {
	mockDB, mockGH, allPRs, dbPRMap, _ := pruneFixture(`["Platform:my_pending"]`)
	poller := newTestPoller(mockGH, mockDB)

	// Row's user (ID 1) is not in allUsers, so no entitlement was computed.
	otherUsers := []db.User{{ID: 2, GitHubUsername: "bob"}}
	poller.pruneStaleViaTeams(context.Background(), "testorg", allPRs, dbPRMap,
		freshGroupData("Platform"), map[userPRViewKey]bool{}, nil, otherUsers, map[string]bool{})

	if len(mockDB.BatchPruneViaTeamsCalls) != 0 {
		t.Fatalf("expected unknown-user row to be skipped, got %v", mockDB.BatchPruneViaTeamsCalls)
	}
}
