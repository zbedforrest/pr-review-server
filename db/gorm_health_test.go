package db

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"pr-review-server/pkg/health"
)

func TestGormDB_HealthMetrics_CountsTheWindow(t *testing.T) {
	db := newTestDB(t)
	now := time.Date(2026, 9, 10, 14, 0, 0, 0, time.UTC)
	start := now.Add(-24 * time.Hour)
	mk := func(id, status string, completedAgo time.Duration, durationMS int64, fallback bool, code string) {
		completed := now.Add(-completedAgo)
		started := completed.Add(-time.Duration(durationMS) * time.Millisecond)
		require.NoError(t, db.db.Create(&ReviewRunModel{
			RunID: id, RepoOwner: "acme", RepoName: "example", PRNumber: 1, CommitSHA: "abc", TriggerSource: "poller", Status: status,
			RequestedConfigJSON: "{}", EffectiveConfigJSON: "{}", ConfigSourcesJSON: "{}", ConfigHash: "h", ConfigSchemaVersion: 3,
			AcceptedAt: started, QueuedAt: started, StartedAt: &started, CompletedAt: &completed, DurationMS: durationMS,
			ModelFallback: fallback, Verdict: "approve", CriticalCount: 1, TerminalCode: code,
		}).Error)
	}
	mk("r1", ReviewRunStatusCompleted, time.Hour, 300000, false, "")
	mk("r2", ReviewRunStatusCompleted, 2*time.Hour, 500000, true, "")
	mk("r3", ReviewRunStatusFailed, 3*time.Hour, 100000, false, "agent_error")
	mk("r4", ReviewRunStatusCompleted, 30*time.Hour, 200000, false, "") // outside the window
	queuedAt := now.Add(-12 * time.Minute)
	staleStart := now.Add(-3 * time.Hour)
	require.NoError(t, db.db.Create(&ReviewRunModel{
		RunID: "q1", RepoOwner: "acme", RepoName: "example", PRNumber: 2, CommitSHA: "def", TriggerSource: "api_v1", Status: ReviewRunStatusQueued,
		RequestedConfigJSON: "{}", EffectiveConfigJSON: "{}", ConfigSourcesJSON: "{}", ConfigHash: "h", ConfigSchemaVersion: 3,
		AcceptedAt: queuedAt, QueuedAt: queuedAt, StartedAt: &staleStart,
	}).Error)
	attemptStart, attemptEnd := start.Add(-10*time.Minute), start.Add(5*time.Minute)
	require.NoError(t, db.db.Create(&ReviewStageAttemptModel{
		ReviewRunID: "r3", ExecutionAttempt: 1, Stage: "agent", InvocationNumber: 1, AttemptNumber: 1, Status: "failed", ErrorCode: "wall_clock_timeout", StartedAt: &attemptStart, CompletedAt: &attemptEnd,
	}).Error)
	require.NoError(t, db.db.Create(&PollerLeaseModel{ID: "poller", Holder: "prism-00047", ExpiresAt: now.Add(time.Minute), Generation: 1}).Error)
	for _, pr := range []*PR{
		{RepoOwner: "acme", RepoName: "example", PRNumber: 1, PRState: "open"},
		{RepoOwner: "acme", RepoName: "example", PRNumber: 3, PRState: "closed"},
	} {
		pr.LastCommitSHA, pr.Title, pr.Author, pr.Status = "abc", "t", "a", "completed"
		require.NoError(t, db.UpsertPR(pr))
	}

	require.NoError(t, db.UpsertPublishedFinding(testPublished(func(p *PublishedFinding) { p.PublishedAt = now.Add(-time.Hour) })))
	require.NoError(t, db.UpsertPublishedFinding(testPublished(func(p *PublishedFinding) {
		p.Fingerprint, p.Kind, p.CommentID, p.PublishedAt = "summary", PublishedKindSummary, 77, now.Add(-time.Hour)
	})))
	_, err := db.RecordPublishedReply(&PublishedReply{RepoOwner: "acme", RepoName: "example", PRNumber: 1, RootCommentID: 1, AuthorCommentID: 10,
		Fingerprint: "f", AuthorID: 42, Class: "pushback", Action: "reacted", Body: "b", CreatedAt: now.Add(-3 * time.Hour)})
	require.NoError(t, err)
	require.NoError(t, db.db.Model(&PublishedReplyModel{}).Where("author_comment_id = 10").UpdateColumns(map[string]interface{}{"processed_at": now.Add(-3 * time.Hour), "attempts": 1, "updated_at": now.Add(-2 * time.Hour)}).Error)
	_, err = db.RecordPublishedReply(&PublishedReply{RepoOwner: "acme", RepoName: "example", PRNumber: 1, RootCommentID: 1, AuthorCommentID: 11,
		Fingerprint: "f", AuthorID: 42, Class: "pushback", Action: "reacted", Body: "b", CreatedAt: now.Add(-3 * time.Hour)})
	require.NoError(t, err)
	require.NoError(t, db.db.Model(&PublishedReplyModel{}).Where("author_comment_id = 11").UpdateColumn("processed_at", now.Add(-3*time.Hour)).Error)
	_, err = db.RecordPublishedReply(&PublishedReply{RepoOwner: "acme", RepoName: "example", PRNumber: 1, RootCommentID: 1, AuthorCommentID: 12,
		Fingerprint: "f", AuthorID: 42, Class: "pushback", Action: "reacted", Body: "b", CreatedAt: now.Add(-3 * time.Hour)})
	require.NoError(t, err)
	require.NoError(t, db.db.Model(&PublishedReplyModel{}).Where("author_comment_id = 12").UpdateColumns(map[string]interface{}{"processed_at": now.Add(-3 * time.Hour), "attempts": 1, "updated_at": now.Add(-10 * time.Minute)}).Error)
	_, err = db.RecordPublishedReply(&PublishedReply{RepoOwner: "acme", RepoName: "example", PRNumber: 3, RootCommentID: 1, AuthorCommentID: 13,
		Fingerprint: "f", AuthorID: 42, Class: "pushback", Action: "reacted", Body: "b", CreatedAt: now.Add(-3 * time.Hour)})
	require.NoError(t, err)
	require.NoError(t, db.db.Model(&PublishedReplyModel{}).Where("author_comment_id = 13").UpdateColumns(map[string]interface{}{"processed_at": now.Add(-3 * time.Hour), "attempts": 1, "updated_at": now.Add(-3 * time.Hour)}).Error)

	user := &User{GitHubID: -1, GitHubUsername: "prism_system"}
	require.NoError(t, db.CreateUser(user))
	require.NoError(t, db.CreateTelemetryEvents([]TelemetryEvent{
		{UserID: user.ID, Action: health.ActionRepeatedPost, Label: "fp=a.go:1:x"},
		{UserID: user.ID, Action: health.ActionRepeatedPost, Label: "fp=b.go:2:y"},
		{UserID: user.ID, Action: health.ActionSameCommitResolve, Label: "fp=a.go:1:x"},
		{UserID: user.ID, Action: "reply_decision", Label: "x"},
		{UserID: user.ID, Action: "search", Label: "not a health action"},
	}))
	require.NoError(t, db.db.Model(&TelemetryEventModel{}).Where("user_id = ?", user.ID).UpdateColumn("created_at", now.Add(-time.Hour)).Error)

	m, err := db.HealthMetrics(start, now, now, nil)
	require.NoError(t, err)
	assert.Equal(t, map[string]int{health.ActionRepeatedPost: 2, health.ActionSameCommitResolve: 1, "reply_decision": 1}, m.Telemetry)
	assert.Equal(t, 3, m.Runs.Total)
	assert.Equal(t, map[string]int{"completed": 2, "failed": 1}, m.Runs.ByStatus)
	assert.Equal(t, 1, m.Runs.ModelFallbacks)
	assert.ElementsMatch(t, []int64{300000, 500000}, m.Runs.DurationsMS)
	assert.Equal(t, map[string]int{"agent_error": 1}, m.Runs.ByTerminalCode)
	assert.Equal(t, 3, m.Runs.Criticals)
	assert.Equal(t, 1, m.Attempts["agent/wall_clock_timeout"])
	assert.Equal(t, 1, m.Queue.Queued)
	assert.InDelta(t, (12 * time.Minute).Seconds(), m.Queue.OldestQueuedAge.Seconds(), 1, "a requeued run is aged from queued_at, not its old start")
	assert.True(t, m.Lease.Present)
	assert.Equal(t, "prism-00047", m.Lease.Holder)
	assert.Equal(t, 1, m.Publish.Summaries)
	assert.Equal(t, 1, m.Publish.Inline)
	assert.Equal(t, 4, m.Replies.Handled)
	assert.Equal(t, 1, m.Replies.StuckPending, "untouched rows, rows active in the last hour, and rows on PRs the scan no longer visits are not stuck")
	assert.Empty(t, m.AutoReview.RoundsPerPRDay, "no automatic runs in this window")
}

func TestGormDB_HealthMetrics_RoundsPerPRDay(t *testing.T) {
	db := newTestDB(t)
	now := time.Date(2026, 9, 10, 14, 0, 0, 0, time.UTC)
	start := now.Add(-24 * time.Hour)
	auto := func(id string, number int, acceptedAgo time.Duration, status string) {
		accepted := now.Add(-acceptedAgo)
		require.NoError(t, db.db.Create(&ReviewRunModel{
			RunID: id, RepoOwner: "acme", RepoName: "example", PRNumber: number, CommitSHA: "abc", TriggerSource: ReviewTriggerAutoReady, Status: status,
			RequestedConfigJSON: "{}", EffectiveConfigJSON: "{}", ConfigSourcesJSON: "{}", ConfigHash: "h", ConfigSchemaVersion: 3,
			AcceptedAt: accepted, QueuedAt: accepted, CompletedAt: &accepted,
		}).Error)
	}
	auto("d1", 5, time.Hour, ReviewRunStatusCompleted)
	auto("d2", 5, 2*time.Hour, ReviewRunStatusCompleted)
	auto("d3", 5, 3*time.Hour, ReviewRunStatusFailed)
	auto("d4", 5, 15*time.Hour, ReviewRunStatusCompleted)
	auto("d5", 5, 25*time.Hour, ReviewRunStatusCompleted)
	auto("d6", 6, 4*time.Hour, ReviewRunStatusCompleted)
	auto("d7", 6, 5*time.Hour, ReviewRunStatusCancelled)
	auto("d8", 6, 6*time.Hour, ReviewRunStatusTimedOut)

	m, err := db.HealthMetrics(start, now, now, nil)
	require.NoError(t, err)
	assert.Equal(t, []int{2, 2, 1}, m.AutoReview.RoundsPerPRDay, "automatic rounds per PR per whole UTC day the window touches, oldest day first; cancelled, failed and timed-out runs do not count")
}

func TestGormDB_HealthReports_RoundTrip(t *testing.T) {
	db := newTestDB(t)
	now := time.Date(2026, 9, 10, 14, 0, 0, 0, time.UTC)
	require.NoError(t, db.SaveHealthReport(&HealthReport{WindowStart: now.Add(-24 * time.Hour), WindowEnd: now, Overall: "ok", Headline: "fine", ReportJSON: `{"a":1}`, Markdown: "# ok", CreatedAt: now}))
	require.NoError(t, db.SaveHealthReport(&HealthReport{WindowStart: now, WindowEnd: now.Add(24 * time.Hour), Overall: "warn", Headline: "meh", ReportJSON: `{}`, Markdown: "# warn", CreatedAt: now.Add(24 * time.Hour)}))
	rows, err := db.ListHealthReports(1)
	require.NoError(t, err)
	require.Len(t, rows, 1)
	assert.Equal(t, "warn", rows[0].Overall)
	assert.Equal(t, "# warn", rows[0].Markdown)

	require.NoError(t, db.SaveHealthReport(&HealthReport{WindowStart: now.Add(time.Hour), WindowEnd: now.Add(25 * time.Hour), Overall: "ok", Headline: "retry", ReportJSON: `{}`, Markdown: "# ok again", CreatedAt: now.Add(25 * time.Hour)}))
	rows, err = db.ListHealthReports(10)
	require.NoError(t, err)
	require.Len(t, rows, 2, "a rerun for the same UTC day replaces that day's report")
	assert.Equal(t, "# ok again", rows[0].Markdown)
}
