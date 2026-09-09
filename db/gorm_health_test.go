package db

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
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
	require.NoError(t, db.db.Create(&ReviewRunModel{
		RunID: "q1", RepoOwner: "acme", RepoName: "example", PRNumber: 2, CommitSHA: "def", TriggerSource: "api_v1", Status: ReviewRunStatusQueued,
		RequestedConfigJSON: "{}", EffectiveConfigJSON: "{}", ConfigSourcesJSON: "{}", ConfigHash: "h", ConfigSchemaVersion: 3,
		AcceptedAt: queuedAt, QueuedAt: queuedAt,
	}).Error)
	require.NoError(t, db.db.Create(&ReviewStageAttemptModel{
		ReviewRunID: "r3", ExecutionAttempt: 1, Stage: "agent", InvocationNumber: 1, AttemptNumber: 1, Status: "failed", ErrorCode: "wall_clock_timeout", StartedAt: &start,
	}).Error)
	require.NoError(t, db.db.Create(&PollerLeaseModel{ID: "poller", Holder: "prism-00047", ExpiresAt: now.Add(time.Minute), Generation: 1}).Error)

	require.NoError(t, db.UpsertPublishedFinding(testPublished(func(p *PublishedFinding) { p.PublishedAt = now.Add(-time.Hour) })))
	require.NoError(t, db.UpsertPublishedFinding(testPublished(func(p *PublishedFinding) {
		p.Fingerprint, p.Kind, p.CommentID, p.PublishedAt = "summary", PublishedKindSummary, 77, now.Add(-time.Hour)
	})))
	_, err := db.RecordPublishedReply(&PublishedReply{RepoOwner: "acme", RepoName: "example", PRNumber: 1, RootCommentID: 1, AuthorCommentID: 10,
		Fingerprint: "f", AuthorID: 42, Class: "pushback", Action: "reacted", Body: "b", CreatedAt: now.Add(-3 * time.Hour)})
	require.NoError(t, err)
	require.NoError(t, db.db.Model(&PublishedReplyModel{}).Where("author_comment_id = 10").Update("processed_at", now.Add(-3*time.Hour)).Error)

	m, err := db.HealthMetrics(start, now, now)
	require.NoError(t, err)
	assert.Equal(t, 3, m.Runs.Total)
	assert.Equal(t, map[string]int{"completed": 2, "failed": 1}, m.Runs.ByStatus)
	assert.Equal(t, 1, m.Runs.ModelFallbacks)
	assert.ElementsMatch(t, []int64{300000, 500000}, m.Runs.DurationsMS)
	assert.Equal(t, map[string]int{"agent_error": 1}, m.Runs.ByTerminalCode)
	assert.Equal(t, 3, m.Runs.Criticals)
	assert.Equal(t, 1, m.Attempts["agent/wall_clock_timeout"])
	assert.Equal(t, 1, m.Queue.Queued)
	assert.InDelta(t, (12 * time.Minute).Seconds(), m.Queue.OldestQueuedAge.Seconds(), 1)
	assert.True(t, m.Lease.Present)
	assert.Equal(t, "prism-00047", m.Lease.Holder)
	assert.Equal(t, 1, m.Publish.Summaries)
	assert.Equal(t, 1, m.Publish.Inline)
	assert.Equal(t, 1, m.Replies.Handled)
	assert.Equal(t, 1, m.Replies.StuckPending)
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
