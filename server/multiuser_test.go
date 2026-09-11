package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"pr-review-server/db"
)

func getUserAs(t *testing.T, server *Server, user *db.User) map[string]any {
	w := httptest.NewRecorder()
	server.handleGetUser(w, addUserToRequest(httptest.NewRequest(http.MethodGet, "/api/user", nil), user))
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	var got map[string]any
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &got))
	return got
}

func TestGetUser_IsAdminTrueForAdmin(t *testing.T) {
	server, _ := newNonDevTestServer(t)
	server.cfg.AdminLogins = []string{"alice"}

	got := getUserAs(t, server, &db.User{ID: 1, GitHubUsername: "alice"})
	assert.Equal(t, "alice", got["github_username"])
	assert.Equal(t, true, got["is_admin"])
}

func TestGetUser_IsAdminFalseForMember(t *testing.T) {
	server, _ := newNonDevTestServer(t)
	server.cfg.AdminLogins = []string{"alice"}

	got := getUserAs(t, server, &db.User{ID: 2, GitHubUsername: "bob"})
	assert.Equal(t, false, got["is_admin"])
}

func TestGetUser_WithoutUserIsUnauthorized(t *testing.T) {
	server, _ := newNonDevTestServer(t)
	w := httptest.NewRecorder()
	server.handleGetUser(w, httptest.NewRequest(http.MethodGet, "/api/user", nil))
	assert.Equal(t, http.StatusUnauthorized, w.Code)
}
