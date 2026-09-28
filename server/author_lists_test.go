package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	gogithub "github.com/google/go-github/v57/github"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	gh "pr-review-server/github"
)

func TestNormalizeAuthorCSV(t *testing.T) {
	cases := []struct {
		name, in, want string
		teams          []string
		wantErr        string
	}{
		{name: "logins and wildcard", in: " Alice, *, dependabot[bot] ", want: "alice,*,dependabot[bot]"},
		{name: "team prefix", in: "alice,Team:Core", want: "alice,team:core", teams: []string{"core"}},
		{name: "org form normalises", in: "@Acme/Web,alice", want: "team:web,alice", teams: []string{"web"}},
		{name: "dedupes across forms", in: "team:core,@acme/core,alice,ALICE", want: "team:core,alice", teams: []string{"core"}},
		{name: "other org", in: "@other/core", wantErr: `"@other/core" names another organization; teams must belong to "acme"`},
		{name: "bad slug", in: "team:co re", wantErr: `"team:co re" is not a valid team slug`},
		{name: "bad login still rejected", in: "team:core,al ice", wantErr: `"al ice" is not a valid login`},
		{name: "empty", in: " , ", want: ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, teams, err := normalizeAuthorCSV(tc.in, "acme")
			if tc.wantErr != "" {
				require.EqualError(t, err, tc.wantErr)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tc.want, got)
			assert.Equal(t, tc.teams, teams)
		})
	}
}

type fakeTeamLister struct {
	teams map[string][]string
	err   error
}

func (f fakeTeamLister) GetOrgTeamMembers(_ context.Context, _, slug string) ([]string, error) {
	if f.err != nil {
		return nil, f.err
	}
	members, ok := f.teams[slug]
	if !ok {
		return nil, fmt.Errorf("wrapped: %w", &gogithub.ErrorResponse{Response: &http.Response{StatusCode: http.StatusNotFound}})
	}
	return members, nil
}

func TestSettings_AuthorListsAcceptTeamsAndReportMembers(t *testing.T) {
	server, database := newTestServer(t, "tester")
	server.SetTeamResolver(gh.NewTeamResolver("acme", fakeTeamLister{teams: map[string][]string{"core": {"Bob", "carol"}, "pilots": {}}}))

	w := patchSettings(t, server, `{"publish_enabled_authors":"Alice, @Acme/Core","auto_review_lite_authors":"team:pilots"}`)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	stored, _ := database.GetSetting(settingPublishEnabledAuthors)
	assert.Equal(t, "alice,team:core", stored)
	stored, _ = database.GetSetting("auto_review_lite_authors")
	assert.Equal(t, "team:pilots", stored)

	w = httptest.NewRecorder()
	server.handleSettings(w, httptest.NewRequest(http.MethodGet, "/api/settings", nil))
	require.Equal(t, http.StatusOK, w.Code)
	var got struct {
		Teams map[string]struct {
			Members    []string `json:"members"`
			ResolvedAt string   `json:"resolved_at"`
			Error      string   `json:"error"`
		} `json:"author_list_teams"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &got))
	require.Len(t, got.Teams, 2)
	assert.Equal(t, []string{"Bob", "carol"}, got.Teams["core"].Members)
	assert.NotEmpty(t, got.Teams["core"].ResolvedAt)
	assert.Empty(t, got.Teams["core"].Error)
	assert.Equal(t, []string{}, got.Teams["pilots"].Members)
}

func TestSettings_RejectsUnknownTeam(t *testing.T) {
	server, database := newTestServer(t, "tester")
	server.SetTeamResolver(gh.NewTeamResolver("acme", fakeTeamLister{teams: map[string][]string{"core": {"bob"}}}))

	w := patchSettings(t, server, `{"publish_enabled_authors":"alice,team:ghosts"}`)
	assert.Equal(t, http.StatusBadRequest, w.Code)
	assert.Equal(t, `publish_enabled_authors: team "ghosts" does not exist in acme`, strings.TrimSpace(w.Body.String()))
	stored, _ := database.GetSetting(settingPublishEnabledAuthors)
	assert.Equal(t, "", stored, "nothing is written when a team fails verification")

	w = patchSettings(t, server, `{"auto_review_lite_authors":"@acme/ghosts"}`)
	assert.Equal(t, http.StatusBadRequest, w.Code)
	assert.Contains(t, w.Body.String(), `auto_review_lite_authors: team "ghosts" does not exist`)

	w = patchSettings(t, server, `{"publish_enabled_authors":"@elsewhere/core"}`)
	assert.Equal(t, http.StatusBadRequest, w.Code)
	assert.Contains(t, w.Body.String(), "names another organization")
}

func TestSettings_TeamLookupOutageDoesNotSaveOrReject(t *testing.T) {
	server, database := newTestServer(t, "tester")
	server.SetTeamResolver(gh.NewTeamResolver("acme", fakeTeamLister{err: errors.New("dial tcp: timeout")}))

	w := patchSettings(t, server, `{"publish_enabled_authors":"team:core"}`)
	assert.Equal(t, http.StatusBadGateway, w.Code)
	assert.Contains(t, w.Body.String(), `could not look up team "core"`)
	stored, _ := database.GetSetting(settingPublishEnabledAuthors)
	assert.Equal(t, "", stored)
}

func TestSettings_TeamEntriesNeedAResolver(t *testing.T) {
	server, _ := newTestServer(t, "tester")
	w := patchSettings(t, server, `{"publish_enabled_authors":"alice,team:core"}`)
	assert.Equal(t, http.StatusBadRequest, w.Code)
	assert.Contains(t, w.Body.String(), "team entries need GITHUB_ORG_NAME")

	w = patchSettings(t, server, `{"publish_enabled_authors":"alice"}`)
	assert.Equal(t, http.StatusOK, w.Code, "plain lists are unaffected")

	w = httptest.NewRecorder()
	server.handleSettings(w, httptest.NewRequest(http.MethodGet, "/api/settings", nil))
	var got map[string]any
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &got))
	assert.Equal(t, map[string]any{}, got["author_list_teams"])
}
