package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"pr-review-server/auth"
	"pr-review-server/config"
	"pr-review-server/db"
	gh "pr-review-server/github"

	"github.com/gorilla/websocket"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func newWebSocketTestServerApp(t *testing.T, username string) (*Server, *db.GormDB) {
	t.Helper()

	dbPath := filepath.Join(t.TempDir(), "websocket-test.db")
	database, err := db.NewGormSQLite(dbPath)
	require.NoError(t, err)

	cfg := &config.Config{
		GitHubUsername: username,
		ServerPort:     "8080",
		ReviewsDir:     "/tmp/reviews",
	}

	server := New(cfg, database, nil, nil)
	return server, database
}

func startWebSocketTestServer(t *testing.T, server *Server) *httptest.Server {
	t.Helper()

	mux := http.NewServeMux()
	mux.HandleFunc("/ws", server.handleWebSocket)
	return httptest.NewServer(mux)
}

func wsURLFromHTTP(httpURL string) string {
	return "ws" + strings.TrimPrefix(httpURL, "http")
}

func createTestSession(t *testing.T, database *db.GormDB, userID int, expiresAt time.Time) *db.Session {
	t.Helper()

	session := &db.Session{
		ID:        "session-" + expiresAt.UTC().Format("20060102150405.000000000"),
		UserID:    userID,
		ExpiresAt: expiresAt,
	}
	require.NoError(t, database.CreateSession(session))
	return session
}

func dialWebSocket(t *testing.T, rawURL string, sessionID string) (*websocket.Conn, *http.Response, error) {
	t.Helper()

	headers := http.Header{}
	if sessionID != "" {
		headers.Add("Cookie", (&http.Cookie{
			Name:  auth.SessionCookieName,
			Value: sessionID,
		}).String())
	}

	return websocket.DefaultDialer.Dial(rawURL, headers)
}

func waitForCondition(t *testing.T, description string, condition func() bool) {
	t.Helper()

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if condition() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}

	t.Fatalf("timed out waiting for %s", description)
}

func readWebSocketMessage(t *testing.T, conn *websocket.Conn) WebSocketMessage {
	t.Helper()

	var message WebSocketMessage
	require.NoError(t, conn.SetReadDeadline(time.Now().Add(time.Second)))
	require.NoError(t, conn.ReadJSON(&message))
	return message
}

func TestHandleWebSocket_ValidSessionUpgradeBindsUser(t *testing.T) {
	server, database := newWebSocketTestServerApp(t, "")
	defer database.Close()
	server.cfg.GitHubAppClientID = "client-id"

	user := createTestUser(t, database, "ws-user")
	session := createTestSession(t, database, user.ID, time.Now().Add(time.Hour))

	ts := startWebSocketTestServer(t, server)
	defer ts.Close()

	conn, _, err := dialWebSocket(t, wsURLFromHTTP(ts.URL)+"/ws", session.ID)
	require.NoError(t, err)
	defer conn.Close()

	waitForCondition(t, "websocket client registration", func() bool {
		server.clientsMux.RLock()
		defer server.clientsMux.RUnlock()

		for _, client := range server.clients {
			return client.UserID == user.ID && client.GitHubUsername == user.GitHubUsername
		}
		return false
	})

	message := readWebSocketMessage(t, conn)
	assert.Equal(t, "status_snapshot", message.Type)
}

func TestHandleWebSocket_MissingSessionRejected(t *testing.T) {
	server, database := newWebSocketTestServerApp(t, "")
	defer database.Close()
	server.cfg.GitHubAppClientID = "client-id"

	ts := startWebSocketTestServer(t, server)
	defer ts.Close()

	conn, resp, err := dialWebSocket(t, wsURLFromHTTP(ts.URL)+"/ws", "")
	require.Error(t, err)
	assert.Nil(t, conn)
	require.NotNil(t, resp)
	assert.Equal(t, http.StatusUnauthorized, resp.StatusCode)
}

func TestHandleWebSocket_ExpiredSessionRejected(t *testing.T) {
	server, database := newWebSocketTestServerApp(t, "")
	defer database.Close()
	server.cfg.GitHubAppClientID = "client-id"

	user := createTestUser(t, database, "expired-user")
	session := createTestSession(t, database, user.ID, time.Now().Add(-time.Hour))

	ts := startWebSocketTestServer(t, server)
	defer ts.Close()

	conn, resp, err := dialWebSocket(t, wsURLFromHTTP(ts.URL)+"/ws", session.ID)
	require.Error(t, err)
	assert.Nil(t, conn)
	require.NotNil(t, resp)
	assert.Equal(t, http.StatusUnauthorized, resp.StatusCode)
}

func TestHandleWebSocket_TelemetryBatchPersistsEvents(t *testing.T) {
	server, database := newWebSocketTestServerApp(t, "")
	defer database.Close()
	server.cfg.GitHubAppClientID = "client-id"

	user := createTestUser(t, database, "telemetry-user")
	session := createTestSession(t, database, user.ID, time.Now().Add(time.Hour))

	ts := startWebSocketTestServer(t, server)
	defer ts.Close()

	conn, _, err := dialWebSocket(t, wsURLFromHTTP(ts.URL)+"/ws", session.ID)
	require.NoError(t, err)
	defer conn.Close()

	longLabel := strings.Repeat("a", 300)
	require.NoError(t, conn.WriteJSON(map[string]interface{}{
		"type": "telemetry_batch",
		"payload": map[string]interface{}{
			"events": []map[string]interface{}{
				{"action": "search", "label": longLabel},
				{"action": "not_allowed", "label": "ignored"},
			},
		},
	}))

	waitForCondition(t, "telemetry persistence", func() bool {
		stats, err := database.GetTelemetryStats(1)
		require.NoError(t, err)
		return stats.TotalEvents == 1
	})

	stats, err := database.GetTelemetryStats(1)
	require.NoError(t, err)
	assert.Equal(t, 1, stats.TotalEvents)
	assert.Equal(t, 1, stats.ActiveUsers)
	require.NotEmpty(t, stats.ByAction)
	assert.Equal(t, "search", stats.ByAction[0].Action)
	require.NotEmpty(t, stats.TopSearches)
	assert.Len(t, stats.TopSearches[0].Label, 255)
}

func TestHandleWebSocket_TelemetryBatchCapEnforced(t *testing.T) {
	server, database := newWebSocketTestServerApp(t, "")
	defer database.Close()
	server.cfg.GitHubAppClientID = "client-id"

	user := createTestUser(t, database, "cap-user")
	session := createTestSession(t, database, user.ID, time.Now().Add(time.Hour))

	ts := startWebSocketTestServer(t, server)
	defer ts.Close()

	conn, _, err := dialWebSocket(t, wsURLFromHTTP(ts.URL)+"/ws", session.ID)
	require.NoError(t, err)
	defer conn.Close()

	events := make([]map[string]interface{}, 0, 101)
	for i := 0; i < 101; i++ {
		events = append(events, map[string]interface{}{
			"action": "search",
			"label":  "query-" + strings.Repeat("x", i%5),
		})
	}

	require.NoError(t, conn.WriteJSON(map[string]interface{}{
		"type": "telemetry_batch",
		"payload": map[string]interface{}{
			"events": events,
		},
	}))

	waitForCondition(t, "capped telemetry persistence", func() bool {
		stats, err := database.GetTelemetryStats(1)
		require.NoError(t, err)
		return stats.TotalEvents == 100
	})
}

func TestHandleWebSocket_MalformedAndUnknownMessagesDoNotBreakConnection(t *testing.T) {
	server, database := newWebSocketTestServerApp(t, "")
	defer database.Close()
	server.cfg.GitHubAppClientID = "client-id"

	user := createTestUser(t, database, "resilient-user")
	session := createTestSession(t, database, user.ID, time.Now().Add(time.Hour))

	ts := startWebSocketTestServer(t, server)
	defer ts.Close()

	conn, _, err := dialWebSocket(t, wsURLFromHTTP(ts.URL)+"/ws", session.ID)
	require.NoError(t, err)
	defer conn.Close()

	require.NoError(t, conn.WriteMessage(websocket.TextMessage, []byte("{not valid json")))
	require.NoError(t, conn.WriteJSON(map[string]interface{}{
		"type":    "unknown_type",
		"payload": map[string]interface{}{"ignored": true},
	}))
	require.NoError(t, conn.WriteJSON(map[string]interface{}{
		"type": "telemetry_batch",
		"payload": map[string]interface{}{
			"events": []map[string]interface{}{
				{"action": "search", "label": "still-works"},
			},
		},
	}))

	waitForCondition(t, "telemetry after malformed message", func() bool {
		stats, err := database.GetTelemetryStats(1)
		require.NoError(t, err)
		return stats.TotalEvents == 1
	})
}

func TestBroadcastEvent_PRUpdateStillDeliveredOverWebSocket(t *testing.T) {
	server, database := newWebSocketTestServerApp(t, "")
	defer database.Close()
	server.cfg.GitHubAppClientID = "client-id"

	user := createTestUser(t, database, "broadcast-user")
	session := createTestSession(t, database, user.ID, time.Now().Add(time.Hour))

	pr := &db.PR{
		RepoOwner:     "owner",
		RepoName:      "repo",
		PRNumber:      7,
		LastCommitSHA: "abc123",
		Status:        "pending",
		Title:         "Broadcast PR",
		Author:        "someone",
	}
	require.NoError(t, database.UpsertPR(pr))

	storedPR, err := database.GetPR("owner", "repo", 7)
	require.NoError(t, err)
	require.NotNil(t, storedPR)
	ensureUserPRView(t, database, user.ID, storedPR.ID, false)

	go server.broadcaster()

	ts := startWebSocketTestServer(t, server)
	defer ts.Close()

	conn, _, err := dialWebSocket(t, wsURLFromHTTP(ts.URL)+"/ws", session.ID)
	require.NoError(t, err)
	defer conn.Close()

	initialMessage := readWebSocketMessage(t, conn)
	assert.Equal(t, "status_snapshot", initialMessage.Type)

	server.BroadcastEvent(EventPRUpdated, map[string]interface{}{
		"owner":  "owner",
		"repo":   "repo",
		"number": 7,
	})

	message := readWebSocketMessage(t, conn)
	assert.Equal(t, EventPRUpdated, message.Type)

	payloadBytes, err := json.Marshal(message.Payload)
	require.NoError(t, err)

	var response PRResponse
	require.NoError(t, json.Unmarshal(payloadBytes, &response))
	assert.Equal(t, "owner", response.Owner)
	assert.Equal(t, "repo", response.Repo)
	assert.Equal(t, 7, response.Number)
	assert.Equal(t, "Broadcast PR", response.Title)
}

func TestHandleTriggerReview_EmitsStatusSnapshotAfterPRUpdate(t *testing.T) {
	latestSHA := "newcommit999888777666555444333222111000aaa"
	mockGH := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"number":1,"head":{"sha":"` + latestSHA + `"}}`))
	}))
	defer mockGH.Close()

	ghClient := gh.NewTestClient(mockGH.URL, "testuser")
	server, database := newWebSocketTestServerApp(t, "")
	defer database.Close()
	server.cfg.GitHubAppClientID = "client-id"
	server.ghClient = ghClient
	go server.broadcaster()

	user := createTestUser(t, database, "status-user")
	session := createTestSession(t, database, user.ID, time.Now().Add(time.Hour))

	pr := &db.PR{
		ID:             1,
		RepoOwner:      "owner",
		RepoName:       "repo",
		PRNumber:       1,
		LastCommitSHA:  "oldcommit123",
		Status:         "completed",
		Title:          "Trigger review PR",
		Author:         "otheruser",
		ApprovalCount:  0,
		MyReviewStatus: "",
	}
	require.NoError(t, database.UpsertPR(pr))
	storedPR, err := database.GetPR("owner", "repo", 1)
	require.NoError(t, err)
	require.NotNil(t, storedPR)
	ensureUserPRView(t, database, user.ID, storedPR.ID, false)

	ts := startWebSocketTestServer(t, server)
	defer ts.Close()

	conn, _, err := dialWebSocket(t, wsURLFromHTTP(ts.URL)+"/ws", session.ID)
	require.NoError(t, err)
	defer conn.Close()

	initial := readWebSocketMessage(t, conn)
	assert.Equal(t, "status_snapshot", initial.Type)

	body := strings.NewReader(`{"owner":"owner","repo":"repo","number":1}`)
	req := httptest.NewRequest(http.MethodPost, "/api/prs/trigger-review", body)
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	server.handleTriggerReview(w, req)
	assert.Equal(t, http.StatusOK, w.Code)

	prUpdate := readWebSocketMessage(t, conn)
	assert.Equal(t, EventPRUpdated, prUpdate.Type)

	statusUpdate := readWebSocketMessage(t, conn)
	assert.Equal(t, "status_snapshot", statusUpdate.Type)
}
