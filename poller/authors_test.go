package poller

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"pr-review-server/db"
	"pr-review-server/github"
)

// teamAware gives the poller a resolver over the mock GitHub client, which
// answers team lookups from the given slug -> members map; a missing slug
// fails the lookup.
func teamAware(p *Poller, mockGH *MockGitHubClient, teams map[string][]string) {
	mockGH.GetOrgTeamMembersFunc = func(_ context.Context, org, slug string) ([]string, error) {
		if org != "acme" {
			return nil, errors.New("unexpected org " + org)
		}
		members, ok := teams[slug]
		if !ok {
			return nil, errors.New("lookup failed")
		}
		return members, nil
	}
	p.teams = github.NewTeamResolver("acme", mockGH)
}

func TestAuthorAllowed_MixesLoginsWildcardAndTeams(t *testing.T) {
	mockGH := NewMockGitHubClient()
	p := newTestPoller(mockGH, NewMockDatabase())
	teamAware(p, mockGH, map[string][]string{"core": {"Bob", "carol"}})

	assert.True(t, p.authorAllowed("alice,team:core", "alice"))
	assert.True(t, p.authorAllowed("alice,team:core", "BOB"), "team membership matches case-insensitively")
	assert.True(t, p.authorAllowed("alice,team:core", "carol"))
	assert.False(t, p.authorAllowed("alice,team:core", "dave"))
	assert.False(t, p.authorAllowed("alice,team:core", ""))
	assert.True(t, p.authorAllowed("*", "dave"))
	assert.False(t, p.authorAllowed("team:unknown", "bob"), "an unresolved team matches nobody")
	assert.False(t, p.authorAllowed("core", "bob"), "a bare slug is a login, not a team")
}

func TestAuthorAllowed_WithoutResolverTeamsMatchNobody(t *testing.T) {
	p := newTestPoller(NewMockGitHubClient(), NewMockDatabase())
	assert.True(t, p.authorAllowed("alice,team:core", "alice"))
	assert.False(t, p.authorAllowed("alice,team:core", "bob"))
}

func TestPublishAndAutoReviewGatesFollowTeamMembership(t *testing.T) {
	f := newAutoReviewFixture(t, true, "team:core")
	teamAware(f.p, f.gh, map[string][]string{"core": {"alice"}})

	allowed, err := f.p.publishAllowedFor("Alice")
	require.NoError(t, err)
	assert.True(t, allowed)
	eligible, err := f.p.autoReviewEligible("alice")
	require.NoError(t, err)
	assert.True(t, eligible)

	allowed, err = f.p.publishAllowedFor("bob")
	require.NoError(t, err)
	assert.False(t, allowed)
	eligible, err = f.p.autoReviewEligible("bob")
	require.NoError(t, err)
	assert.False(t, eligible)
}

func TestAutoReviewProfileFor_TeamEntryGatesLite(t *testing.T) {
	database := NewMockDatabase()
	mockGH := NewMockGitHubClient()
	p := newTestPoller(mockGH, database)
	p.cfg.OpenRouterAPIKey = "test-openrouter-key"
	teamAware(p, mockGH, map[string][]string{"pilots": {"alice"}})
	p.cfg.AgenticReviews, p.cfg.AgentModel, p.cfg.AgentWallClockSec, p.cfg.AgentMaxTurns = true, "claude-fable-5", 900, 120
	p.cfg.ReviewDefaultProfile = "lite"

	require.NoError(t, database.SetSetting(SettingAutoReviewLiteAuthors, "team:pilots"))
	assert.Equal(t, "lite", p.autoReviewProfileFor("synchronize", "acme", "example", "alice"))
	assert.Equal(t, "full", p.autoReviewProfileFor("synchronize", "acme", "example", "bob"))
}

func TestPublishGate_OptedOutLoginInATeamIsDeniedOnEveryPostingPath(t *testing.T) {
	database := NewMockDatabase()
	mockGH := NewMockGitHubClient()
	p := newTestPoller(mockGH, database)
	teamAware(p, mockGH, map[string][]string{"xo-team": {"alice", "bob"}})
	require.NoError(t, database.SetSetting(settingPublishEnabledAuthors, "team:xo-team,carol,*"))
	require.NoError(t, database.SetSetting(SettingPublishOptOutAuthors, " Alice ,carol"))
	require.NoError(t, database.SetSetting(settingAutoReviewReadyPRs, "true"))
	require.NoError(t, database.SetSetting(SettingAutoReviewLiteAuthors, "team:xo-team"))
	p.cfg.OpenRouterAPIKey = "test-openrouter-key"
	p.cfg.AgenticReviews, p.cfg.AgentModel, p.cfg.AgentWallClockSec, p.cfg.AgentMaxTurns = true, "claude-fable-5", 900, 120
	p.cfg.ReviewDefaultProfile = "lite"

	gate, err := p.publishGate()
	require.NoError(t, err)
	paths := map[string]func(string) bool{
		"review round": func(author string) bool {
			allowed, err := p.publishAllowedFor(author)
			require.NoError(t, err)
			return allowed
		},
		"replies and mentions": gate.Allowed,
	}
	cases := []struct {
		author string
		want   bool
		why    string
	}{
		{"alice", false, "opted out beats team membership"},
		{"ALICE", false, "opt-out matches case-insensitively"},
		{"carol", false, "opted out beats a login entry"},
		{"bob", true, "team member who did not opt out"},
		{"dave", true, "wildcard still admits everyone else"},
	}
	for name, allowed := range paths {
		for _, tc := range cases {
			assert.Equal(t, tc.want, allowed(tc.author), "%s: %s: %s", name, tc.author, tc.why)
		}
	}
	assert.True(t, gate.OptedOut("Alice"))
	assert.False(t, gate.OptedOut("bob"))

	eligible, err := p.autoReviewEligible("alice")
	require.NoError(t, err)
	assert.True(t, eligible, "opt-out stops posting only; dashboard reviews continue")
	assert.Equal(t, "lite", p.autoReviewProfileFor("synchronize", "acme", "example", "alice"), "the lite gate ignores the opt-out")
}

type unreadableSettings struct{ db.Database }

func (unreadableSettings) GetSetting(string) (string, error) { return "", errors.New("db down") }

func TestPublishGate_ReadErrorDeniesEveryone(t *testing.T) {
	p := newTestPoller(NewMockGitHubClient(), NewMockDatabase())
	p.db = unreadableSettings{p.db}

	gate, err := p.publishGate()
	require.Error(t, err)
	assert.False(t, gate.Allowed("alice"))
}

func TestAdmittingEntry_PrefersLoginThenTeamThenWildcard(t *testing.T) {
	mockGH := NewMockGitHubClient()
	p := newTestPoller(mockGH, NewMockDatabase())
	teamAware(p, mockGH, map[string][]string{"xo-team": {"alice", "bob"}})
	ctx := context.Background()

	assert.Equal(t, "login", AdmittingEntry(ctx, p.teams, "team:xo-team,Alice,*", "alice"))
	assert.Equal(t, "xo-team", AdmittingEntry(ctx, p.teams, "team:xo-team,*", "BOB"))
	assert.Equal(t, "*", AdmittingEntry(ctx, p.teams, "team:xo-team,*", "dave"))
	assert.Equal(t, "", AdmittingEntry(ctx, p.teams, "team:xo-team", "dave"))
	assert.Equal(t, "", AdmittingEntry(ctx, p.teams, "*", ""))
	assert.Equal(t, "", AdmittingEntry(ctx, nil, "team:xo-team", "alice"), "without a resolver teams match nobody")
}
