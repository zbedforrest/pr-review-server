package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"pr-review-server/db"
	gh "pr-review-server/github"
)

func optOutAs(t *testing.T, server *Server, method string, user *db.User) *httptest.ResponseRecorder {
	t.Helper()
	w := httptest.NewRecorder()
	req := httptest.NewRequest(method, publishOptOutPath, nil)
	if user != nil {
		req = addUserToRequest(req, user)
	}
	server.handlePublishOptOut(w, req)
	return w
}

func TestSettings_PublishOptOutAuthorsRoundTripsLoginsOnly(t *testing.T) {
	server, database := newTestServer(t, "tester")

	w := patchSettings(t, server, `{"publish_opt_out_authors":" Alice, bob "}`)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	stored, _ := database.GetSetting(settingPublishOptOutAuthors)
	assert.Equal(t, "alice,bob", stored)
	var got map[string]any
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &got))
	assert.Equal(t, "alice,bob", got["publish_opt_out_authors"])

	for _, bad := range []string{`"*"`, `"team:core"`, `"dependabot[bot]"`} {
		w = patchSettings(t, server, `{"publish_opt_out_authors":`+bad+`}`)
		assert.Equal(t, http.StatusBadRequest, w.Code, bad)
		assert.Contains(t, w.Body.String(), "publish_opt_out_authors:")
	}
	stored, _ = database.GetSetting(settingPublishOptOutAuthors)
	assert.Equal(t, "alice,bob", stored, "a rejected value writes nothing")
}

func TestGetUser_ReportsPublishEnrollment(t *testing.T) {
	server, database := newNonDevTestServer(t)
	server.SetTeamResolver(gh.NewTeamResolver("acme", fakeTeamLister{teams: map[string][]string{"xo-team": {"Bob"}}}))
	require.NoError(t, database.SetSetting(settingPublishEnabledAuthors, "alice,team:xo-team"))
	require.NoError(t, database.SetSetting(settingPublishOptOutAuthors, "bob"))

	cases := []struct {
		login    string
		enrolled bool
		optedOut bool
		via      string
	}{
		{"Alice", true, false, "login"},
		{"bob", true, true, "xo-team"},
		{"dave", false, false, ""},
	}
	for _, tc := range cases {
		got := getUserAs(t, server, &db.User{ID: 1, GitHubUsername: tc.login})
		assert.Equal(t, tc.enrolled, got["publish_enrolled"], tc.login)
		assert.Equal(t, tc.optedOut, got["publish_opted_out"], tc.login)
		assert.Equal(t, tc.via, got["enrolled_via"], tc.login)
	}

	require.NoError(t, database.SetSetting(settingPublishEnabledAuthors, "team:xo-team,*"))
	assert.Equal(t, "*", getUserAs(t, server, &db.User{ID: 1, GitHubUsername: "dave"})["enrolled_via"])
	assert.Equal(t, "xo-team", getUserAs(t, server, &db.User{ID: 1, GitHubUsername: "bob"})["enrolled_via"], "a team is more specific than the wildcard")
}

func TestPublishOptOut_PostLeavesAndDeleteRejoins(t *testing.T) {
	server, database := newNonDevTestServer(t)
	require.NoError(t, database.SetSetting(settingPublishEnabledAuthors, "*"))
	require.NoError(t, database.SetSetting(settingPublishOptOutAuthors, "alice"))
	bob := &db.User{ID: 2, GitHubUsername: "Bob"}

	w := optOutAs(t, server, http.MethodPost, bob)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	var got map[string]any
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &got))
	assert.Equal(t, true, got["publish_opted_out"])
	assert.Equal(t, true, got["publish_enrolled"])
	stored, _ := database.GetSetting(settingPublishOptOutAuthors)
	assert.Equal(t, "alice,bob", stored)

	w = optOutAs(t, server, http.MethodPost, bob)
	require.Equal(t, http.StatusOK, w.Code)
	stored, _ = database.GetSetting(settingPublishOptOutAuthors)
	assert.Equal(t, "alice,bob", stored, "leaving twice writes the login once")

	w = optOutAs(t, server, http.MethodDelete, bob)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &got))
	assert.Equal(t, false, got["publish_opted_out"])
	stored, _ = database.GetSetting(settingPublishOptOutAuthors)
	assert.Equal(t, "alice", stored, "the other entries stay")
}

func TestPublishOptOut_NeedsAUserAndAWriteMethod(t *testing.T) {
	server, database := newNonDevTestServer(t)
	assert.Equal(t, http.StatusUnauthorized, optOutAs(t, server, http.MethodPost, nil).Code)
	assert.Equal(t, http.StatusMethodNotAllowed, optOutAs(t, server, http.MethodGet, &db.User{ID: 1, GitHubUsername: "bob"}).Code)
	stored, _ := database.GetSetting(settingPublishOptOutAuthors)
	assert.Empty(t, stored)
}
