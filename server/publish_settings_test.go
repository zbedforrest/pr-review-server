package server

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
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
		`{"publish_enabled_authors":"alice, bob","publish_inline_cap":3,"publish_inline_min_severity":"low","publish_reply_mode":"react","publish_show_unverified":false}`))
	w := httptest.NewRecorder()
	server.handleSettings(w, req)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())

	w = httptest.NewRecorder()
	server.handleSettings(w, httptest.NewRequest(http.MethodGet, "/api/settings", nil))
	require.Equal(t, http.StatusOK, w.Code)

	var got map[string]any
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &got))
	assert.Equal(t, "alice, bob", got["publish_enabled_authors"])
	assert.Equal(t, float64(3), got["publish_inline_cap"])
	assert.Equal(t, "low", got["publish_inline_min_severity"])
	assert.Equal(t, "react", got["publish_reply_mode"])
	assert.Equal(t, false, got["publish_show_unverified"])
}

func TestSettings_PublishShowUnverifiedRoundTripsBackOn(t *testing.T) {
	server, database := newTestServer(t, "tester")
	require.NoError(t, database.SetSetting("publish_show_unverified", "false"))

	w := httptest.NewRecorder()
	server.handleSettings(w, httptest.NewRequest(http.MethodPatch, "/api/settings", strings.NewReader(`{"publish_show_unverified":true}`)))
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
	server.handleSettings(w, httptest.NewRequest(http.MethodPatch, "/api/settings", strings.NewReader(`{"publish_reply_mode":"chatty"}`)))
	assert.Equal(t, http.StatusBadRequest, w.Code)
}

func TestSettings_RejectsBadPublishSeverity(t *testing.T) {
	server, _ := newTestServer(t, "tester")
	w := httptest.NewRecorder()
	server.handleSettings(w, httptest.NewRequest(http.MethodPatch, "/api/settings", strings.NewReader(`{"publish_inline_min_severity":"urgent"}`)))
	assert.Equal(t, http.StatusBadRequest, w.Code)
}

func TestSettings_ReplyModeTransitionStampsAndClearsActivation(t *testing.T) {
	server, database := newTestServer(t, "tester")
	patch := func(body string) {
		w := httptest.NewRecorder()
		server.handleSettings(w, httptest.NewRequest(http.MethodPatch, "/api/settings", strings.NewReader(body)))
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
	server.handleSettings(w, httptest.NewRequest(http.MethodPatch, "/api/settings", strings.NewReader(`{"publish_reply_mode":"react"}`)))
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
	server.handleSettings(w, httptest.NewRequest(http.MethodPatch, "/api/settings", strings.NewReader(`{"publish_reply_mode":"react"}`)))
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
