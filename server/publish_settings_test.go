package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSettings_PublishKeysRoundTrip(t *testing.T) {
	server, _ := newTestServer(t, "tester")

	req := httptest.NewRequest(http.MethodPatch, "/api/settings", strings.NewReader(
		`{"publish_enabled_authors":"alice, bob","publish_inline_cap":3,"publish_inline_min_severity":"low"}`))
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
}

func TestSettings_PublishKeysDefaultToDisabled(t *testing.T) {
	server, _ := newTestServer(t, "tester")
	w := httptest.NewRecorder()
	server.handleSettings(w, httptest.NewRequest(http.MethodGet, "/api/settings", nil))

	var got map[string]any
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &got))
	assert.Equal(t, "", got["publish_enabled_authors"])
	assert.Equal(t, float64(5), got["publish_inline_cap"])
	assert.Equal(t, "medium", got["publish_inline_min_severity"])
}

func TestSettings_RejectsBadPublishSeverity(t *testing.T) {
	server, _ := newTestServer(t, "tester")
	w := httptest.NewRecorder()
	server.handleSettings(w, httptest.NewRequest(http.MethodPatch, "/api/settings", strings.NewReader(`{"publish_inline_min_severity":"urgent"}`)))
	assert.Equal(t, http.StatusBadRequest, w.Code)
}
