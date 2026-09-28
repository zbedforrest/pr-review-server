package poller

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

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
	assert.True(t, p.authorMatcher("team:core")("carol"))
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
	teamAware(p, mockGH, map[string][]string{"pilots": {"alice"}})
	p.cfg.AgenticReviews, p.cfg.AgentModel, p.cfg.AgentWallClockSec, p.cfg.AgentMaxTurns = true, "claude-fable-5", 900, 120
	p.cfg.ReviewDefaultProfile = "lite"

	require.NoError(t, database.SetSetting(SettingAutoReviewLiteAuthors, "team:pilots"))
	assert.Equal(t, "lite", p.autoReviewProfileFor("synchronize", "acme", "example", "alice"))
	assert.Equal(t, "full", p.autoReviewProfileFor("synchronize", "acme", "example", "bob"))
}
