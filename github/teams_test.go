package github

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	gogithub "github.com/google/go-github/v57/github"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestParseTeamEntry(t *testing.T) {
	cases := []struct {
		entry   string
		slug    string
		isTeam  bool
		wantErr string
	}{
		{entry: "alice"},
		{entry: "*"},
		{entry: "dependabot[bot]"},
		{entry: "team:core", slug: "core", isTeam: true},
		{entry: " Team:Core-Devs ", slug: "core-devs", isTeam: true},
		{entry: "@acme/core", slug: "core", isTeam: true},
		{entry: "@ACME/Core", slug: "core", isTeam: true},
		{entry: "team:", isTeam: true, wantErr: `"team:" is not a valid team slug`},
		{entry: "team:-core", isTeam: true, wantErr: `"team:-core" is not a valid team slug`},
		{entry: "team:co re", isTeam: true, wantErr: `"team:co re" is not a valid team slug`},
		{entry: "@acme", isTeam: true, wantErr: `"@acme" must be @org/team-slug`},
		{entry: "@other/core", isTeam: true, wantErr: `"@other/core" names another organization; teams must belong to "acme"`},
	}
	for _, c := range cases {
		slug, isTeam, err := ParseTeamEntry(c.entry, "acme")
		assert.Equal(t, c.isTeam, isTeam, c.entry)
		if c.wantErr != "" {
			assert.EqualError(t, err, c.wantErr, c.entry)
			continue
		}
		require.NoError(t, err, c.entry)
		assert.Equal(t, c.slug, slug, c.entry)
	}

	_, isTeam, err := ParseTeamEntry("@acme/core", "")
	assert.True(t, isTeam)
	assert.EqualError(t, err, `"@acme/core" cannot be checked without a configured organization; use team:<slug>`)
}

func TestTeamSlugs(t *testing.T) {
	assert.Equal(t, []string{"core", "web"}, TeamSlugs("alice, team:core, *, @acme/web, TEAM:CORE, team:bad slug", "acme"))
	assert.Nil(t, TeamSlugs("alice,bob,*", "acme"))
	assert.Nil(t, TeamSlugs("", "acme"))
}

type fakeLister struct {
	calls   int
	members []string
	err     error
	slugs   []string
}

func (f *fakeLister) GetOrgTeamMembers(_ context.Context, org, slug string) ([]string, error) {
	f.calls++
	f.slugs = append(f.slugs, org+"/"+slug)
	if f.err != nil {
		return nil, f.err
	}
	return append([]string{}, f.members...), nil
}

func ghStatusError(status int) error {
	return fmt.Errorf("wrapped: %w", &gogithub.ErrorResponse{Response: &http.Response{StatusCode: status}})
}

func newClockedResolver(lister TeamMemberLister) (*TeamResolver, *time.Time) {
	now := time.Date(2026, 9, 28, 12, 0, 0, 0, time.UTC)
	r := NewTeamResolver("acme", lister)
	r.now = func() time.Time { return now }
	return r, &now
}

func TestTeamResolver_CachesWithinTTLAndRefreshesAfter(t *testing.T) {
	lister := &fakeLister{members: []string{"Alice", "bob"}}
	r, now := newClockedResolver(lister)
	ctx := context.Background()

	assert.True(t, r.IsMember(ctx, "core", "alice"))
	assert.True(t, r.IsMember(ctx, "Core", "ALICE"), "match is case-insensitive on both sides")
	assert.False(t, r.IsMember(ctx, "core", "carol"))
	assert.False(t, r.IsMember(ctx, "core", ""))
	assert.Equal(t, 1, lister.calls, "one fetch serves every match within the TTL")
	assert.Equal(t, []string{"acme/core"}, lister.slugs)

	*now = now.Add(teamCacheTTL - time.Second)
	assert.True(t, r.IsMember(ctx, "core", "bob"))
	assert.Equal(t, 1, lister.calls)

	lister.members = []string{"carol"}
	*now = now.Add(2 * time.Second)
	assert.True(t, r.IsMember(ctx, "core", "carol"))
	assert.False(t, r.IsMember(ctx, "core", "alice"), "a refreshed list drops former members")
	assert.Equal(t, 2, lister.calls)

	membership, err := r.Members(ctx, "core")
	require.NoError(t, err)
	assert.Equal(t, []string{"carol"}, membership.Logins)
	assert.Equal(t, *now, membership.ResolvedAt)
}

func TestTeamResolver_ServesStaleOnTransientError(t *testing.T) {
	lister := &fakeLister{members: []string{"alice"}}
	r, now := newClockedResolver(lister)
	ctx := context.Background()
	require.True(t, r.IsMember(ctx, "core", "alice"))
	resolvedAt := *now

	*now = now.Add(teamCacheTTL + time.Second)
	lister.err = errors.New("502 bad gateway")
	assert.True(t, r.IsMember(ctx, "core", "alice"), "the stale list still answers")
	assert.Equal(t, 2, lister.calls)
	membership, err := r.Members(ctx, "core")
	require.NoError(t, err)
	assert.Equal(t, resolvedAt, membership.ResolvedAt, "the served list is the old one")
	assert.Equal(t, 2, lister.calls, "a failed refresh is not retried until the retry interval passes")

	*now = now.Add(teamRetryAfter)
	lister.err = nil
	lister.members = []string{"bob"}
	assert.True(t, r.IsMember(ctx, "core", "bob"))
	assert.Equal(t, 3, lister.calls)
}

func TestTeamResolver_FailsClosedWhenNeverResolved(t *testing.T) {
	lister := &fakeLister{err: errors.New("dial tcp: timeout")}
	r, now := newClockedResolver(lister)
	ctx := context.Background()

	assert.False(t, r.IsMember(ctx, "core", "alice"))
	assert.False(t, r.IsMember(ctx, "core", "alice"))
	assert.Equal(t, 1, lister.calls, "a failed first lookup is not retried on every match")
	_, err := r.Members(ctx, "core")
	assert.ErrorContains(t, err, "timeout")

	*now = now.Add(teamRetryAfter)
	lister.err = nil
	lister.members = []string{"alice"}
	assert.True(t, r.IsMember(ctx, "core", "alice"))
	assert.Equal(t, 2, lister.calls)
}

func TestTeamResolver_NotFoundAndForbiddenMatchNobody(t *testing.T) {
	lister := &fakeLister{members: []string{"alice"}}
	r, now := newClockedResolver(lister)
	ctx := context.Background()
	require.True(t, r.IsMember(ctx, "core", "alice"))

	*now = now.Add(teamCacheTTL + time.Second)
	lister.err = ghStatusError(http.StatusNotFound)
	assert.False(t, r.IsMember(ctx, "core", "alice"), "a deleted team stops matching even with a cached list")
	_, err := r.Members(ctx, "core")
	assert.ErrorIs(t, err, ErrTeamNotFound)
	assert.Equal(t, 2, lister.calls, "a 404 is cached for the TTL")

	lister.err = ghStatusError(http.StatusForbidden)
	assert.False(t, r.IsMember(ctx, "hidden", "alice"))
	_, err = r.Members(ctx, "hidden")
	assert.ErrorIs(t, err, ErrTeamForbidden)
	assert.Equal(t, 3, lister.calls)
	assert.False(t, r.IsMember(ctx, "hidden", "alice"))
	assert.Equal(t, 3, lister.calls)

	assert.False(t, r.AnyMember(ctx, []string{"core", "hidden"}, "alice"))
}

func TestTeamResolver_ClassifiesRealClientErrorsAndPaginates(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/orgs/acme/teams/core/members":
			if r.URL.Query().Get("page") == "2" {
				fmt.Fprint(w, `[{"login":"carol"}]`)
				return
			}
			w.Header().Set("Link", fmt.Sprintf(`<%s/orgs/acme/teams/core/members?page=2&per_page=100>; rel="next"`, "http://"+r.Host))
			fmt.Fprint(w, `[{"login":"Alice"},{"login":"bob"}]`)
		case "/orgs/acme/teams/ghosts/members":
			w.WriteHeader(http.StatusNotFound)
			fmt.Fprint(w, `{"message":"Not Found"}`)
		case "/orgs/acme/teams/secret/members":
			w.WriteHeader(http.StatusForbidden)
			fmt.Fprint(w, `{"message":"Resource not accessible by integration"}`)
		default:
			t.Errorf("unexpected path %s", r.URL.Path)
		}
	}))
	defer ts.Close()
	r := NewTeamResolver("acme", NewTestClient(ts.URL, "bot"))
	ctx := context.Background()

	membership, err := r.Members(ctx, "core")
	require.NoError(t, err)
	assert.Equal(t, []string{"Alice", "bob", "carol"}, membership.Logins)
	assert.True(t, r.IsMember(ctx, "core", "carol"))

	_, err = r.Members(ctx, "ghosts")
	assert.ErrorIs(t, err, ErrTeamNotFound)
	_, err = r.Members(ctx, "secret")
	assert.ErrorIs(t, err, ErrTeamForbidden)
}
