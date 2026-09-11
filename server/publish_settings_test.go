package server

import (
	"bytes"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"pr-review-server/db"
	"pr-review-server/pkg/publisher"
)

func TestSettings_PublishKeysRoundTrip(t *testing.T) {
	server, _ := newTestServer(t, "tester")

	req := httptest.NewRequest(http.MethodPatch, "/api/settings", strings.NewReader(
		`{"publish_enabled_authors":"Alice, *","publish_inline_cap":3,"publish_inline_min_severity":"low","publish_reply_mode":"react","publish_show_unverified":false}`))
	w := httptest.NewRecorder()
	server.handleSettings(w, addUserToRequest(req, &db.User{GitHubUsername: "tester"}))
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())

	w = httptest.NewRecorder()
	server.handleSettings(w, httptest.NewRequest(http.MethodGet, "/api/settings", nil))
	require.Equal(t, http.StatusOK, w.Code)

	var got map[string]any
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &got))
	assert.Equal(t, "alice,*", got["publish_enabled_authors"])
	assert.Equal(t, float64(3), got["publish_inline_cap"])
	assert.Equal(t, "low", got["publish_inline_min_severity"])
	assert.Equal(t, "react", got["publish_reply_mode"])
	assert.Equal(t, false, got["publish_show_unverified"])
}

func TestSettings_PublishShowUnverifiedRoundTripsBackOn(t *testing.T) {
	server, database := newTestServer(t, "tester")
	require.NoError(t, database.SetSetting("publish_show_unverified", "false"))

	w := httptest.NewRecorder()
	server.handleSettings(w, addUserToRequest(httptest.NewRequest(http.MethodPatch, "/api/settings", strings.NewReader(`{"publish_show_unverified":true}`)), &db.User{GitHubUsername: "tester"}))
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())

	var got map[string]any
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &got))
	assert.Equal(t, true, got["publish_show_unverified"])
	stored, _ := database.GetSetting("publish_show_unverified")
	assert.Equal(t, "true", stored)
}

func TestSettings_PublishKeysDefaultToDisabled(t *testing.T) {
	server, _ := newTestServer(t, "tester")
	w := httptest.NewRecorder()
	server.handleSettings(w, httptest.NewRequest(http.MethodGet, "/api/settings", nil))

	var got map[string]any
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &got))
	assert.Equal(t, "", got["publish_enabled_authors"])
	assert.Equal(t, float64(publisher.DefaultInlineCap), got["publish_inline_cap"])
	assert.Equal(t, "medium", got["publish_inline_min_severity"])
	assert.Equal(t, "off", got["publish_reply_mode"])
	assert.Equal(t, true, got["publish_show_unverified"])
}

func TestSettings_RejectsBadReplyMode(t *testing.T) {
	server, _ := newTestServer(t, "tester")
	w := httptest.NewRecorder()
	server.handleSettings(w, addUserToRequest(httptest.NewRequest(http.MethodPatch, "/api/settings", strings.NewReader(`{"publish_reply_mode":"chatty"}`)), &db.User{GitHubUsername: "tester"}))
	assert.Equal(t, http.StatusBadRequest, w.Code)
}

func TestSettings_RejectsBadPublishSeverity(t *testing.T) {
	server, _ := newTestServer(t, "tester")
	w := httptest.NewRecorder()
	server.handleSettings(w, addUserToRequest(httptest.NewRequest(http.MethodPatch, "/api/settings", strings.NewReader(`{"publish_inline_min_severity":"urgent"}`)), &db.User{GitHubUsername: "tester"}))
	assert.Equal(t, http.StatusBadRequest, w.Code)
}

func TestSettings_ReplyModeTransitionStampsAndClearsActivation(t *testing.T) {
	server, database := newTestServer(t, "tester")
	patch := func(body string) {
		w := httptest.NewRecorder()
		server.handleSettings(w, addUserToRequest(httptest.NewRequest(http.MethodPatch, "/api/settings", strings.NewReader(body)), &db.User{GitHubUsername: "tester"}))
		require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	}
	get := func() map[string]any {
		w := httptest.NewRecorder()
		server.handleSettings(w, httptest.NewRequest(http.MethodGet, "/api/settings", nil))
		var got map[string]any
		require.NoError(t, json.Unmarshal(w.Body.Bytes(), &got))
		return got
	}

	patch(`{"publish_reply_mode":"observe"}`)
	first, _ := get()["publish_reply_enabled_at"].(string)
	require.NotEmpty(t, first, "leaving off must stamp the activation time")

	patch(`{"publish_reply_mode":"react"}`)
	assert.Equal(t, first, get()["publish_reply_enabled_at"], "observe to react keeps the original stamp")

	patch(`{"publish_reply_mode":"off"}`)
	assert.Equal(t, "", get()["publish_reply_enabled_at"], "off clears the stamp so re-enabling starts fresh")
	v, _ := database.GetSetting("publish_reply_enabled_at")
	assert.Equal(t, "", v)
}

type settingReadFails struct {
	db.Database
	key string
}

func (f settingReadFails) GetSetting(key string) (string, error) {
	if key == f.key {
		return "", errors.New("db down")
	}
	return f.Database.GetSetting(key)
}

func TestSettings_ReplyModeChangeRefusesToStampWhenTheCurrentModeIsUnreadable(t *testing.T) {
	server, database := newTestServer(t, "tester")
	require.NoError(t, database.SetSetting("publish_reply_mode", "observe"))
	require.NoError(t, database.SetSetting("publish_reply_enabled_at", "2026-09-08T12:00:00Z"))
	server.db = settingReadFails{Database: database, key: "publish_reply_mode"}

	w := httptest.NewRecorder()
	server.handleSettings(w, addUserToRequest(httptest.NewRequest(http.MethodPatch, "/api/settings", strings.NewReader(`{"publish_reply_mode":"react"}`)), &db.User{GitHubUsername: "tester"}))
	assert.Equal(t, http.StatusInternalServerError, w.Code)

	stamp, _ := database.GetSetting("publish_reply_enabled_at")
	assert.Equal(t, "2026-09-08T12:00:00Z", stamp, "a read error must never move the activation stamp")
	mode, _ := database.GetSetting("publish_reply_mode")
	assert.Equal(t, "observe", mode)
}

type settingWriteFails struct {
	db.Database
	key string
}

func (f settingWriteFails) SetSetting(key, value string) error {
	if key == f.key {
		return errors.New("db down")
	}
	return f.Database.SetSetting(key, value)
}

func TestSettings_EnablingRepliesWritesTheStampBeforeTheMode(t *testing.T) {
	server, database := newTestServer(t, "tester")
	server.db = settingWriteFails{Database: database, key: "publish_reply_mode"}

	w := httptest.NewRecorder()
	server.handleSettings(w, addUserToRequest(httptest.NewRequest(http.MethodPatch, "/api/settings", strings.NewReader(`{"publish_reply_mode":"react"}`)), &db.User{GitHubUsername: "tester"}))
	assert.Equal(t, http.StatusInternalServerError, w.Code)

	mode, _ := database.GetSetting("publish_reply_mode")
	stamp, _ := database.GetSetting("publish_reply_enabled_at")
	assert.Equal(t, "", mode, "a failed enable must leave the mode off")
	assert.NotEmpty(t, stamp, "the stamp is written first so the mode is never on without it")
}

func TestSettings_DefaultInlineCapMatchesThePublisher(t *testing.T) {
	server, _ := newTestServer(t, "tester")
	w := httptest.NewRecorder()
	server.handleSettings(w, httptest.NewRequest(http.MethodGet, "/api/settings", nil))
	var got map[string]any
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &got))
	assert.Equal(t, float64(publisher.DefaultInlineCap), got["publish_inline_cap"], "the dashboard must show the cap the poller applies")
}

func settingsPatchAs(server *Server, user *db.User, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPatch, "/api/settings", strings.NewReader(body))
	if user != nil {
		req = addUserToRequest(req, user)
	}
	w := httptest.NewRecorder()
	server.handleSettings(w, req)
	return w
}

func settingsGet(t *testing.T, server *Server, user *db.User) map[string]any {
	req := httptest.NewRequest(http.MethodGet, "/api/settings", nil)
	if user != nil {
		req = addUserToRequest(req, user)
	}
	w := httptest.NewRecorder()
	server.handleSettings(w, req)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	var got map[string]any
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &got))
	return got
}

func TestSettings_NonAdminPatchIsDeniedAndWritesNothing(t *testing.T) {
	server, database := newNonDevTestServer(t)
	server.cfg.AdminLogins = []string{"alice"}
	require.NoError(t, database.SetSetting("publish_reply_mode", "observe"))

	var buf bytes.Buffer
	log.SetOutput(&buf)
	defer log.SetOutput(os.Stderr)

	w := settingsPatchAs(server, &db.User{GitHubUsername: "bob"}, `{"publish_reply_mode":"react"}`)
	assert.Equal(t, http.StatusForbidden, w.Code)
	assert.Equal(t, "admin required\n", w.Body.String())

	stored, _ := database.GetSetting("publish_reply_mode")
	assert.Equal(t, "observe", stored)
	assert.Equal(t, 1, strings.Count(buf.String(), "\n"), buf.String())
	assert.Contains(t, buf.String(), "[SETTINGS] denied actor=bob method=PATCH")
}

func TestSettings_PatchWithoutUserIsUnauthorized(t *testing.T) {
	server, database := newNonDevTestServer(t)
	w := settingsPatchAs(server, nil, `{"publish_reply_mode":"react"}`)
	assert.Equal(t, http.StatusUnauthorized, w.Code)
	stored, _ := database.GetSetting("publish_reply_mode")
	assert.Equal(t, "", stored)
}

func TestSettings_NonAdminGetIncludesAdminLists(t *testing.T) {
	server, database := newNonDevTestServer(t)
	server.cfg.AdminLogins = []string{"alice"}
	require.NoError(t, database.SetSetting(settingAdminLogins, "Carol, dave,,carol"))

	got := settingsGet(t, server, &db.User{GitHubUsername: "bob"})
	assert.Equal(t, "carol,dave", got["admin_logins"])
	assert.Equal(t, []any{"alice"}, got["admin_logins_fixed"])
}

func TestSettings_AdminLoginsFixedIsNeverNull(t *testing.T) {
	server, _ := newTestServer(t, "tester")
	w := httptest.NewRecorder()
	server.handleSettings(w, httptest.NewRequest(http.MethodGet, "/api/settings", nil))
	require.Equal(t, http.StatusOK, w.Code)
	assert.Contains(t, w.Body.String(), `"admin_logins_fixed":[]`)
}

func TestSettings_AdminLoginsRoundTripNormalized(t *testing.T) {
	server, database := newNonDevTestServer(t)
	server.cfg.AdminLogins = []string{"root"}
	admin := &db.User{GitHubUsername: "root"}

	w := settingsPatchAs(server, admin, `{"admin_logins":"Alice, bob,,alice"}`)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())

	stored, _ := database.GetSetting(settingAdminLogins)
	assert.Equal(t, "alice,bob", stored)
	assert.Equal(t, "alice,bob", settingsGet(t, server, admin)["admin_logins"])
	assert.True(t, server.isAdmin(&db.User{GitHubUsername: "Bob"}))
}

func TestSettings_AdminLoginsRejectsStar(t *testing.T) {
	server, database := newNonDevTestServer(t)
	server.cfg.AdminLogins = []string{"root"}

	w := settingsPatchAs(server, &db.User{GitHubUsername: "root"}, `{"admin_logins":"alice,*"}`)
	assert.Equal(t, http.StatusBadRequest, w.Code)
	assert.Contains(t, w.Body.String(), `"*" is not a valid login`)
	stored, _ := database.GetSetting(settingAdminLogins)
	assert.Equal(t, "", stored)
}

func TestSettings_PublishEnabledAuthorsRejectsInvalidLogin(t *testing.T) {
	server, database := newTestServer(t, "tester")
	w := settingsPatchAs(server, &db.User{GitHubUsername: "tester"}, `{"publish_enabled_authors":"alice,al ice"}`)
	assert.Equal(t, http.StatusBadRequest, w.Code)
	assert.Contains(t, w.Body.String(), `"al ice" is not a valid login`)
	stored, _ := database.GetSetting("publish_enabled_authors")
	assert.Equal(t, "", stored)
}

func TestSettings_ReviewNRequestsMustBePositive(t *testing.T) {
	server, database := newTestServer(t, "tester")
	w := settingsPatchAs(server, &db.User{GitHubUsername: "tester"}, `{"review_n_requests":0}`)
	assert.Equal(t, http.StatusBadRequest, w.Code)
	n, _ := database.GetReviewNRequests()
	assert.Equal(t, 3, n)
}

func TestSettings_ReplyModeErrorListsEveryMode(t *testing.T) {
	server, _ := newTestServer(t, "tester")
	w := settingsPatchAs(server, &db.User{GitHubUsername: "tester"}, `{"publish_reply_mode":"chatty"}`)
	require.Equal(t, http.StatusBadRequest, w.Code)
	for _, mode := range []string{"off", "observe", "react", "shadow", "respond"} {
		assert.Contains(t, w.Body.String(), mode)
	}
}

func TestSettings_PatchLogsActorKeyOldNewPerChangedKey(t *testing.T) {
	server, database := newTestServer(t, "tester")
	require.NoError(t, database.SetSetting("review_n_requests", "3"))
	require.NoError(t, database.SetSetting("auto_review_requested_prs", "true"))

	var buf bytes.Buffer
	log.SetOutput(&buf)
	defer log.SetOutput(os.Stderr)

	w := settingsPatchAs(server, &db.User{GitHubUsername: "alice"}, `{"review_n_requests":2,"auto_review_requested_prs":false,"publish_inline_cap":4}`)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())

	lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
	require.Len(t, lines, 3, buf.String())
	assert.Contains(t, lines[0], `[SETTINGS] actor=alice key=auto_review_requested_prs old="true" new="false"`)
	assert.Contains(t, lines[1], `[SETTINGS] actor=alice key=review_n_requests old="3" new="2"`)
	assert.Contains(t, lines[2], `[SETTINGS] actor=alice key=publish_inline_cap old="" new="4"`)
}
