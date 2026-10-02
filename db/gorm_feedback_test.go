package db

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestGormDB_ListFeedbackTargets_OpenPRsAndRecentlyCommentedClosedOnes(t *testing.T) {
	db := newTestDB(t)
	now := time.Date(2026, 10, 2, 14, 0, 0, 0, time.UTC)
	for n, state := range map[int]string{7: "open", 8: "merged", 9: "closed"} {
		require.NoError(t, db.UpsertPR(&PR{RepoOwner: "acme", RepoName: "example", PRNumber: n, PRState: state, LastCommitSHA: "abc", Title: "t", Author: "a", Status: "completed"}))
	}
	seed := func(pr int, kind string, fp string, commentID int64, at time.Time) {
		require.NoError(t, db.UpsertPublishedFinding(testPublished(func(p *PublishedFinding) {
			p.RepoOwner, p.RepoName, p.PRNumber, p.Kind, p.Fingerprint, p.CommentID, p.PublishedAt = "acme", "example", pr, kind, fp, commentID, at
		})))
	}
	old, recent := now.Add(-30*24*time.Hour), now.Add(-2*24*time.Hour)
	seed(7, PublishedKindSummary, PublishedKindSummary, 700, old)
	seed(7, PublishedKindFinding, "a.go:1:aaaaaaaaaaaa", 701, old)
	seed(7, PublishedKindFinding, "a.go:2:bbbbbbbbbbbb", 0, old)
	seed(8, PublishedKindFinding, "b.go:1:cccccccccccc", 801, recent)
	seed(9, PublishedKindFinding, "c.go:1:dddddddddddd", 901, old)
	seed(10, PublishedKindFinding, "d.go:1:eeeeeeeeeeee", 1001, recent)
	_, err := db.RecordPublishedReply(&PublishedReply{RepoOwner: "acme", RepoName: "example", PRNumber: 7, RootCommentID: 701, AuthorCommentID: 7010, Fingerprint: "a.go:1:aaaaaaaaaaaa", AuthorID: 1, Class: "pushback", Action: "reacted", CreatedAt: recent})
	require.NoError(t, err)

	targets, err := db.ListFeedbackTargets(now.Add(-7 * 24 * time.Hour))
	require.NoError(t, err)
	require.Len(t, targets, 2, "%+v", targets)
	assert.Equal(t, 7, targets[0].PRNumber)
	assert.Equal(t, map[int64]bool{701: true}, targets[0].Roots)
	assert.Equal(t, map[int64]bool{700: true}, targets[0].Summaries)
	assert.Equal(t, map[int64]string{7010: "pushback"}, targets[0].ReplyClasses)
	assert.Equal(t, 8, targets[1].PRNumber, "merged but commented on this week")
	assert.Empty(t, targets[1].Summaries)
}

func TestGormDB_FeedbackItems_SaveOnceListByWindow(t *testing.T) {
	db := newTestDB(t)
	now := time.Date(2026, 10, 2, 14, 0, 0, 0, time.UTC)
	items := []FeedbackItem{
		{Source: "reply", ItemID: 101, CommentID: 100, RepoOwner: "acme", RepoName: "example", PRNumber: 42, Author: "dana-dev", Body: "Please stop.", ReplyClass: "pushback", Label: "very_frustrated", Classifier: "lexicon", URL: "u1", CreatedAt: now.Add(-time.Hour), ObservedAt: now},
		{Source: "reaction", ItemID: 101, CommentID: 100, RepoOwner: "acme", RepoName: "example", PRNumber: 42, Author: "lee-ops", Body: "-1", Reaction: "-1", Label: "frustrated", Classifier: "lexicon", URL: "u1", CreatedAt: now.Add(-3 * 24 * time.Hour), ObservedAt: now},
	}
	n, err := db.SaveFeedbackItems(items)
	require.NoError(t, err)
	assert.Equal(t, 2, n, "a reply and a reaction may share an id")

	items[0].Label = "happy"
	n, err = db.SaveFeedbackItems(items[:1])
	require.NoError(t, err)
	assert.Equal(t, 0, n)

	known, err := db.KnownFeedbackItems("acme", "example", 42)
	require.NoError(t, err)
	assert.Equal(t, map[FeedbackKey]bool{{"reply", 101}: true, {"reaction", 101}: true}, known)

	day, err := db.ListFeedbackItems(now.Add(-24*time.Hour), now)
	require.NoError(t, err)
	require.Len(t, day, 1)
	assert.Equal(t, "very_frustrated", day[0].Label, "the first classification stands")

	week, err := db.ListFeedbackItems(now.Add(-7*24*time.Hour), now)
	require.NoError(t, err)
	assert.Len(t, week, 2)
	assert.Equal(t, "reply", week[0].Source, "newest first")
}
