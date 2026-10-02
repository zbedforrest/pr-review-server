package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"pr-review-server/db"
	"pr-review-server/pkg/feedback"
)

type fakeFeedbackGitHub struct {
	review    []feedback.Comment
	issue     []feedback.Comment
	reactions map[int64][]feedback.Reaction
}

func (f fakeFeedbackGitHub) ListReviewComments(context.Context, string, string, int) ([]feedback.Comment, error) {
	return f.review, nil
}
func (f fakeFeedbackGitHub) ListIssueComments(context.Context, string, string, int) ([]feedback.Comment, error) {
	return f.issue, nil
}
func (f fakeFeedbackGitHub) ListReviewCommentReactions(_ context.Context, _, _ string, id int64) ([]feedback.Reaction, error) {
	return f.reactions[id], nil
}
func (f fakeFeedbackGitHub) ListIssueCommentReactions(_ context.Context, _, _ string, id int64) ([]feedback.Reaction, error) {
	return f.reactions[id], nil
}

type stubClassifier struct{ calls int }

func (c *stubClassifier) Classify(_ context.Context, it feedback.Item) (feedback.Label, string) {
	c.calls++
	return feedback.Lexicon(it), "stub"
}

func seedFeedbackPR(t *testing.T, database *db.GormDB) {
	t.Helper()
	require.NoError(t, database.UpsertPR(&db.PR{RepoOwner: "acme", RepoName: "example", PRNumber: 42, PRState: "open", LastCommitSHA: "abc", Title: "Add retry", Author: "sam-q", Status: "completed"}))
	require.NoError(t, database.UpsertPublishedFinding(&db.PublishedFinding{RepoOwner: "acme", RepoName: "example", PRNumber: 42, Kind: db.PublishedKindSummary, Fingerprint: db.PublishedKindSummary, ReviewedSHA: "abc", LastSeenSHA: "abc", CommentID: 200, State: db.PublishedStateOpen, PublishedAt: time.Now().UTC().Add(-48 * time.Hour)}))
	require.NoError(t, database.UpsertPublishedFinding(&db.PublishedFinding{RepoOwner: "acme", RepoName: "example", PRNumber: 42, Kind: db.PublishedKindFinding, Fingerprint: "pkg/retry.go:4:deadbeef0123", ReviewedSHA: "abc", LastSeenSHA: "abc", CommentID: 100, State: db.PublishedStateOpen, PublishedAt: time.Now().UTC().Add(-48 * time.Hour)}))
}

func feedbackFixtureGitHub(now time.Time) fakeFeedbackGitHub {
	recent := now.Add(-2 * time.Hour)
	return fakeFeedbackGitHub{
		review: []feedback.Comment{
			{ID: 100, Author: "prism-bot[bot]", IsBot: true, Body: "finding", CreatedAt: now.Add(-48 * time.Hour), Reactions: 1},
			{ID: 101, InReplyToID: 100, Author: "dana-dev", Body: "This is the third time it flagged the same line after I explained it. Please stop.", CreatedAt: recent},
			{ID: 103, InReplyToID: 100, Author: "sam-q", Body: "Good catch, fixed in 3f2a1c9.", CreatedAt: recent},
			{ID: 105, InReplyToID: 100, Author: "lee-ops", Body: "This is wrong, the value is validated upstream.", CreatedAt: now.Add(-3 * 24 * time.Hour)},
		},
		issue: []feedback.Comment{
			{ID: 200, Author: "prism-bot[bot]", IsBot: true, Body: "summary", CreatedAt: now.Add(-48 * time.Hour)},
			{ID: 201, Author: "lee-ops", Body: "@prism-bot this comment is wrong, the lock is already held.", CreatedAt: recent},
		},
		reactions: map[int64][]feedback.Reaction{100: {{ID: 300, User: "lee-ops", Content: "-1", CreatedAt: recent}}},
	}
}

func TestDailyHealth_FeedbackScanIsIdempotentAndCountsFrustration(t *testing.T) {
	server, database := newTestServer(t, "tester")
	server.cfg.HealthJobToken = "secret-job-token"
	server.cfg.FeedbackDigest = true
	server.cfg.MentionHandle = "prism-bot"
	seedLease(t, database)
	seedFeedbackPR(t, database)
	now := time.Now().UTC()
	server.feedbackGitHub = feedbackFixtureGitHub(now)
	classifier := &stubClassifier{}
	server.feedbackClassify = classifier

	run := func() map[string]any {
		req := httptest.NewRequest(http.MethodPost, "/api/health/daily", nil)
		req.Header.Set("X-Prism-Job-Token", "secret-job-token")
		w := httptest.NewRecorder()
		server.handleDailyHealthJob(w, req)
		require.Equal(t, http.StatusOK, w.Code, w.Body.String())
		var got map[string]any
		require.NoError(t, json.Unmarshal(w.Body.Bytes(), &got))
		return got
	}
	report := run()
	assert.Equal(t, "warn", report["overall"])
	rows, err := database.ListHealthReports(1)
	require.NoError(t, err)
	md := rows[0].Markdown
	assert.Contains(t, md, "🟡 **author feedback**: 1 happy, 0 neutral, 2 frustrated, 1 very frustrated")
	assert.Contains(t, md, `@dana-dev (very): "This is the third time it flagged the same line after I explained it. Please stop." https://github.com/acme/example/pull/42#discussion_r101`)
	assert.Contains(t, md, `@lee-ops: "reacted -1" https://github.com/acme/example/pull/42#discussion_r100`)
	assert.Contains(t, md, `happy: @sam-q: "Good catch, fixed in 3f2a1c9." https://github.com/acme/example/pull/42#discussion_r103`)
	assert.Equal(t, 4, classifier.calls)

	items, err := database.ListFeedbackItems(now.Add(-24*time.Hour), now.Add(time.Minute))
	require.NoError(t, err)
	require.Len(t, items, 4)
	week, err := database.ListFeedbackItems(now.Add(-7*24*time.Hour), now.Add(time.Minute))
	require.NoError(t, err)
	assert.Len(t, week, 5, "the backfilled older reply is stored for the week view")
	stats, err := database.GetTelemetryStats(1)
	require.NoError(t, err)
	assert.Equal(t, 3, telemetryCount(stats, "feedback_frustrated"), "the older reply is not counted as today's frustration")
	var report1 map[string]any
	require.NoError(t, json.Unmarshal([]byte(rows[0].ReportJSON), &report1))
	assert.EqualValues(t, 3, report1["metrics"].(map[string]any)["telemetry"].(map[string]any)["feedback_frustrated"], "the counter lands inside the report's own window")

	run()
	items, err = database.ListFeedbackItems(now.Add(-24*time.Hour), now.Add(time.Minute))
	require.NoError(t, err)
	assert.Len(t, items, 4, "a second run stores nothing new")
	assert.Equal(t, 4, classifier.calls, "a second run classifies nothing")
	stats, err = database.GetTelemetryStats(1)
	require.NoError(t, err)
	assert.Equal(t, 3, telemetryCount(stats, "feedback_frustrated"), "a second run counts nothing twice")
}

func telemetryCount(stats *db.TelemetryStats, action string) int {
	for _, a := range stats.ByAction {
		if a.Action == action {
			return a.Count
		}
	}
	return 0
}

func TestDailyHealth_FeedbackLineWhenScanIsOffOrUnavailable(t *testing.T) {
	server, database := newTestServer(t, "tester")
	server.cfg.HealthJobToken = "secret-job-token"
	seedLease(t, database)
	for _, tc := range []struct {
		enabled bool
		want    string
	}{{false, "author feedback**: not scanned (FEEDBACK_DIGEST=false)"}, {true, "author feedback**: not scanned (no GitHub client)"}} {
		server.cfg.FeedbackDigest = tc.enabled
		req := httptest.NewRequest(http.MethodPost, "/api/health/daily", nil)
		req.Header.Set("X-Prism-Job-Token", "secret-job-token")
		w := httptest.NewRecorder()
		server.handleDailyHealthJob(w, req)
		require.Equal(t, http.StatusOK, w.Code)
		rows, err := database.ListHealthReports(1)
		require.NoError(t, err)
		assert.Contains(t, rows[0].Markdown, tc.want)
	}
}

func TestFeedbackEndpoint_ServesDayAndWeekAsJSONOrMarkdown(t *testing.T) {
	server, database := newTestServer(t, "tester")
	user := createTestUser(t, database, "tester")
	now := time.Now().UTC()
	_, err := database.SaveFeedbackItems([]db.FeedbackItem{
		{Source: "reply", ItemID: 1, CommentID: 10, RepoOwner: "acme", RepoName: "example", PRNumber: 42, Author: "dana-dev", Body: "Useless noise, turn this off. " + strings.Repeat("The rest of this comment quotes the whole handler. ", 6) + "SECRET-TAIL", Label: "very_frustrated", Classifier: "lexicon", URL: "https://github.com/acme/example/pull/42#discussion_r1", CreatedAt: now.Add(-2 * time.Hour), ObservedAt: now},
		{Source: "comment", ItemID: 2, CommentID: 2, RepoOwner: "acme", RepoName: "example", PRNumber: 43, Author: "sam-q", Body: "@prism-bot thanks, helpful", Label: "happy", Classifier: "lexicon", URL: "https://github.com/acme/example/pull/43#issuecomment-2", CreatedAt: now.Add(-3 * 24 * time.Hour), ObservedAt: now},
	})
	require.NoError(t, err)

	get := func(query string) *httptest.ResponseRecorder {
		req := addUserToRequest(httptest.NewRequest(http.MethodGet, "/api/health/feedback"+query, nil), user)
		w := httptest.NewRecorder()
		server.handleFeedback(w, req)
		return w
	}
	w := get("")
	require.Equal(t, http.StatusOK, w.Code)
	var day feedback.Digest
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &day))
	assert.Equal(t, 1, day.Days)
	assert.Equal(t, 1, day.Counts["very_frustrated"])
	require.Len(t, day.Items, 1)
	assert.Equal(t, "dana-dev", day.Items[0].Author)
	assert.True(t, strings.HasSuffix(day.Items[0].Quote, "..."))
	assert.NotContains(t, w.Body.String(), "SECRET-TAIL", "the JSON view carries the quote, not the stored body")

	w = get("?days=7&format=json")
	var week feedback.Digest
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &week))
	assert.Len(t, week.Items, 2)

	w = get("?days=7&format=md")
	require.Equal(t, http.StatusOK, w.Code)
	assert.True(t, strings.HasPrefix(w.Header().Get("Content-Type"), "text/markdown"))
	assert.Contains(t, w.Body.String(), "# PRism author feedback: last 7 days")
	assert.Contains(t, w.Body.String(), "@sam-q on acme/example#43 (comment)")

	assert.Equal(t, http.StatusBadRequest, get("?days=3").Code)
	assert.Equal(t, http.StatusBadRequest, get("?format=xml").Code)
}
