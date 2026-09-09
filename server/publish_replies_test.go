package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"pr-review-server/db"
)

func TestPublishReplies_ReportsRecentRepliesCountsAndUnlinkedRoots(t *testing.T) {
	server, database := newTestServer(t, "tester")
	require.NoError(t, database.SetSetting("publish_reply_mode", " React "))
	pr := &db.PR{RepoOwner: "acme", RepoName: "example", PRNumber: 7, PRState: "open", LastCommitSHA: "abc", Title: "t", Author: "a", Status: "completed"}
	require.NoError(t, database.UpsertPR(pr))
	require.NoError(t, database.UpsertPublishedFinding(&db.PublishedFinding{
		RepoOwner: "acme", RepoName: "example", PRNumber: 7, Kind: db.PublishedKindFinding,
		Fingerprint: "a.go:1:abc", ReviewedSHA: "abc", LastSeenSHA: "abc", ReviewID: 500, State: db.PublishedStateOpen, PublishedAt: time.Now(),
	}))
	base := time.Date(2026, 9, 9, 12, 0, 0, 0, time.UTC)
	for i, class := range []string{"resolution", "question", "pushback"} {
		_, err := database.RecordPublishedReply(&db.PublishedReply{
			RepoOwner: "acme", RepoName: "example", PRNumber: 7, RootCommentID: 100, AuthorCommentID: int64(101 + i),
			Fingerprint: "a.go:1:abc", AuthorID: 42, Class: class, Action: "reacted", Body: "body " + class, CreatedAt: base.Add(time.Duration(i) * time.Minute),
		})
		require.NoError(t, err)
	}

	w := httptest.NewRecorder()
	server.handlePublishReplies(w, httptest.NewRequest(http.MethodGet, "/api/publish/replies?limit=2", nil))
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())

	var got struct {
		Mode     string           `json:"mode"`
		Total    int              `json:"total"`
		ByAction map[string]int   `json:"by_action"`
		ByClass  map[string]int   `json:"by_class"`
		Unlinked int              `json:"unlinked_roots"`
		Recent   []map[string]any `json:"recent"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &got))
	assert.Equal(t, "react", got.Mode)
	assert.Equal(t, 3, got.Total)
	assert.Equal(t, map[string]int{"reacted": 3}, got.ByAction)
	assert.Equal(t, map[string]int{"resolution": 1, "question": 1, "pushback": 1}, got.ByClass)
	assert.Equal(t, 1, got.Unlinked)
	require.Len(t, got.Recent, 2)
	assert.Equal(t, "pushback", got.Recent[0]["class"])
	assert.Equal(t, float64(103), got.Recent[0]["author_comment_id"])
	assert.Equal(t, "https://github.com/acme/example/pull/7#discussion_r103", got.Recent[0]["url"])
}

func TestPublishReplies_RejectsWrites(t *testing.T) {
	server, _ := newTestServer(t, "tester")
	w := httptest.NewRecorder()
	server.handlePublishReplies(w, httptest.NewRequest(http.MethodPost, "/api/publish/replies", nil))
	assert.Equal(t, http.StatusMethodNotAllowed, w.Code)
}
