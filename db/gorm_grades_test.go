package db

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func seedGradeLedger(t *testing.T, g *GormDB, posted time.Time) {
	t.Helper()
	rows := []PublishedFinding{
		{RepoOwner: "o", RepoName: "r", PRNumber: 1, Kind: PublishedKindSummary, Fingerprint: PublishedKindSummary, ReviewedSHA: "h1", LastSeenSHA: "h1", State: PublishedStateOpen, PublishedAt: posted, CommentID: 900, CommentText: "summary", MergeBase: "b1"},
		{RepoOwner: "o", RepoName: "r", PRNumber: 1, Kind: PublishedKindFinding, Fingerprint: "a.go:10:x", Severity: "medium", ReviewedSHA: "h1", LastSeenSHA: "h1", State: PublishedStateOpen, PublishedAt: posted, CommentID: 11, CommentText: "inline text", Line: 10},
		{RepoOwner: "o", RepoName: "r", PRNumber: 1, Kind: PublishedKindAnnotation, Fingerprint: "b.go:0:y", Severity: "low", ReviewedSHA: "h1", LastSeenSHA: "h1", State: PublishedStateOpen, PublishedAt: posted, CommentText: "note text"},
		{RepoOwner: "o", RepoName: "r", PRNumber: 1, Kind: PublishedKindFinding, Fingerprint: "c.go:5:z", ReviewedSHA: "h1", LastSeenSHA: "h1", State: PublishedStateOpen, PublishedAt: posted, CommentText: "never linked"},
		{RepoOwner: "o", RepoName: "r", PRNumber: 1, Kind: PublishedKindFinding, Fingerprint: "d.go:5:old", ReviewedSHA: "h0", LastSeenSHA: "h1", State: PublishedStateOpen, PublishedAt: posted.Add(-48 * time.Hour), CommentID: 12, CommentText: "old"},
	}
	for i := range rows {
		require.NoError(t, g.UpsertPublishedFinding(&rows[i]))
	}
	replied := posted
	require.NoError(t, g.db.Create(&PublishedReplyModel{
		RepoOwner: "o", RepoName: "r", PRNumber: 1, AuthorCommentID: 50, RootCommentID: 11, Fingerprint: "a.go:10:x",
		Class: "dispute", Action: "reply", Body: "author says no", ReplyCommentID: 51, ReplyBody: "prism answer",
		DecisionHead: "h2", RepliedAt: &replied, CreatedAt: posted, ProcessedAt: posted,
	}).Error)
}

func TestGradeCandidatesListsTheWindowsPostedCommentsOnce(t *testing.T) {
	g := newTestDB(t)
	now := time.Date(2026, 10, 22, 9, 0, 0, 0, time.UTC)
	seedGradeLedger(t, g, now.Add(-2*time.Hour))

	cands, err := g.GradeCandidates(now.Add(-24*time.Hour), now)
	require.NoError(t, err)
	require.Len(t, cands, 3)
	byKind := map[string]GradeCandidate{}
	for _, c := range cands {
		byKind[c.Kind] = c
	}
	assert.Equal(t, "inline text", byKind["inline"].Text)
	assert.Equal(t, "b1", byKind["inline"].MergeBase)
	assert.Equal(t, int64(900), byKind["summary_note"].SummaryCommentID)
	assert.Equal(t, "author says no", byKind["reply"].AuthorText)
	assert.Equal(t, "h2", byKind["reply"].HeadSHA)

	require.NoError(t, g.SaveCommentGrades([]CommentGradeModel{{Ref: byKind["inline"].Ref, RepoOwner: "o", RepoName: "r", PRNumber: 1, Kind: "inline", PostedAt: now, Grade: "BAD", GradedAt: now}}))
	require.NoError(t, g.SaveCommentGrades([]CommentGradeModel{{Ref: byKind["inline"].Ref, RepoOwner: "o", RepoName: "r", PRNumber: 1, Kind: "inline", PostedAt: now, Grade: "GOOD", GradedAt: now}}))
	cands, err = g.GradeCandidates(now.Add(-24*time.Hour), now)
	require.NoError(t, err)
	assert.Len(t, cands, 2)

	var stored []CommentGradeModel
	require.NoError(t, g.db.Find(&stored).Error)
	require.Len(t, stored, 1)
	assert.Equal(t, "BAD", stored[0].Grade)
}

func TestQualityMetricsSummarisesTheWindowAndTheWeek(t *testing.T) {
	g := newTestDB(t)
	end := time.Date(2026, 10, 22, 9, 0, 0, 0, time.UTC)
	grades := []CommentGradeModel{
		{Ref: "f:1", Kind: "inline", Severity: "medium", Grade: "BAD", Category: "wrong-claim", Reason: "x.env:2 SECRET_TOKEN=abcd1234wxyz", CommentURL: "u1", CostUSD: 0.5, GradedAt: end.Add(-time.Hour)},
		{Ref: "f:2", Kind: "inline", Severity: "low", Grade: "BAD", Category: "nitpick", Reason: "r2", CostUSD: 0.5, GradedAt: end.Add(-time.Hour)},
		{Ref: "f:3", Kind: "summary_note", Grade: "GOOD", CostUSD: 0.5, GradedAt: end.Add(-time.Hour)},
		{Ref: "f:4", Kind: "inline", Grade: "OK", GradedAt: end.Add(-50 * time.Hour)},
	}
	for i := range grades {
		grades[i].RepoOwner, grades[i].RepoName, grades[i].PRNumber, grades[i].PostedAt = "o", "r", 1, grades[i].GradedAt
	}
	require.NoError(t, g.SaveCommentGrades(grades))

	q, err := g.qualityMetrics(end.Add(-24*time.Hour), end)
	require.NoError(t, err)
	assert.Equal(t, 3, q.Graded)
	assert.Equal(t, map[string]int{"BAD": 2, "GOOD": 1}, q.ByGrade)
	assert.Equal(t, 2, q.ByKind["inline"]["BAD"])
	assert.Equal(t, map[string]int{"wrong-claim": 1, "nitpick": 1}, q.BadCategories)
	require.Len(t, q.Worst, 2)
	assert.Equal(t, "medium", q.Worst[0].Severity)
	assert.Equal(t, "x.env:2 SECRET_TOKEN=[redacted]", q.Worst[0].Reason, "rows stored before redaction are masked on the way out")
	assert.InDelta(t, 1.5, q.CostUSD, 1e-9)
	require.Len(t, q.Trend, 7)
	assert.Equal(t, 3, q.Trend[6].Graded)
	assert.Equal(t, 1, q.Trend[4].Graded)
}
