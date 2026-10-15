package db

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestGormDB_SettledPoints_RecordOncePerThreadAndListByAuthorWindow(t *testing.T) {
	db := newTestDB(t)
	defer db.Close()

	at := time.Date(2026, 10, 14, 15, 3, 0, 0, time.UTC)
	first := &SettledPoint{RepoOwner: "acme", RepoName: "example", PRNumber: 596, CommentID: 4001, Author: "Dev-Ana", Source: SettledSourceVerdict,
		Fingerprint: "store/models.py:24:ce55dc9e9abf", File: "store/models.py", FindingKind: "production_behavior", Subjects: "purchaser_read_swap,record_fanclub_bulk",
		Text: "fanout may write a row for a member who already bought the set", Reason: "intentional, the backfill is done", SettledAt: at}
	require.NoError(t, db.RecordSettledPoint(first))
	later := *first
	later.Reason, later.SettledAt = "a second answer", at.Add(time.Hour)
	require.NoError(t, db.RecordSettledPoint(&later))

	got, err := db.ListSettledPoints("acme", "example", "dev-ana", at.Add(-30*24*time.Hour), at.Add(24*time.Hour))
	require.NoError(t, err)
	require.Len(t, got, 1)
	require.Equal(t, "intentional, the backfill is done", got[0].Reason)
	require.Equal(t, "dev-ana", got[0].Author)
	require.True(t, got[0].SettledAt.Equal(at))

	got, err = db.ListSettledPoints("acme", "example", "dev-ana", at, at.Add(24*time.Hour))
	require.NoError(t, err)
	require.Empty(t, got, "a point settled at the window's open edge is outside it")
	got, err = db.ListSettledPoints("acme", "example", "someone-else", at.Add(-time.Hour), at.Add(time.Hour))
	require.NoError(t, err)
	require.Empty(t, got)
}

func TestGormDB_SettledPoints_DerivedFromDismissedRowsWithAVerdictReply(t *testing.T) {
	db := newTestDB(t)
	defer db.Close()

	posted := time.Date(2026, 10, 13, 22, 58, 0, 0, time.UTC)
	verdictAt := posted.Add(16 * time.Hour)
	require.NoError(t, db.UpsertPR(&PR{RepoOwner: "acme", RepoName: "example", PRNumber: 596, Author: "Dev-Ana", LastCommitSHA: "8c858a5", Status: "completed"}))
	row := testPublished(func(p *PublishedFinding) {
		p.RepoOwner, p.RepoName, p.PRNumber = "acme", "example", 596
		p.Fingerprint, p.CommentID, p.State, p.PublishedAt = "store/models.py:24:ce55dc9e9abf", 4001, PublishedStateDismissed, posted
		p.CommentText, p.FindingKind, p.Subjects = "The buyer lookup hardcodes purchaser_id.", "production_behavior", "purchaser_read_swap,record_fanclub_bulk"
	})
	require.NoError(t, db.UpsertPublishedFinding(row))
	_, err := db.RecordPublishedReply(&PublishedReply{RepoOwner: "acme", RepoName: "example", PRNumber: 596, RootCommentID: 4001, AuthorCommentID: 5001,
		Fingerprint: row.Fingerprint, AuthorID: 77, Class: "verdict", Action: "reacted", Body: "intentional, the backfill is done", CreatedAt: verdictAt})
	require.NoError(t, err)
	// A dismissed row of another author's PR is not this author's point.
	require.NoError(t, db.UpsertPR(&PR{RepoOwner: "acme", RepoName: "example", PRNumber: 600, Author: "dev-bo", LastCommitSHA: "1111111", Status: "completed"}))
	other := testPublished(func(p *PublishedFinding) {
		p.RepoOwner, p.RepoName, p.PRNumber = "acme", "example", 600
		p.Fingerprint, p.CommentID, p.State, p.PublishedAt = "store/other.py:1:aaaaaaaaaaaa", 4002, PublishedStateDismissed, posted
	})
	require.NoError(t, db.UpsertPublishedFinding(other))

	got, err := db.ListSettledPoints("acme", "example", "dev-ana", posted.Add(-time.Hour), verdictAt.Add(time.Hour))
	require.NoError(t, err)
	require.Len(t, got, 1)
	require.Equal(t, SettledSourceVerdict, got[0].Source)
	require.Equal(t, int64(4001), got[0].CommentID)
	require.Equal(t, "store/models.py", got[0].File)
	require.Equal(t, "intentional, the backfill is done", got[0].Reason)
	require.True(t, got[0].SettledAt.Equal(verdictAt))

	recorded := got[0]
	recorded.Reason = "recorded first"
	require.NoError(t, db.RecordSettledPoint(&recorded))
	got, err = db.ListSettledPoints("acme", "example", "dev-ana", posted.Add(-time.Hour), verdictAt.Add(time.Hour))
	require.NoError(t, err)
	require.Len(t, got, 1, "a recorded row replaces the derived one for the same thread")
	require.Equal(t, "recorded first", got[0].Reason)

	got, err = db.ListSettledPoints("acme", "example", "dev-ana", verdictAt.Add(-time.Hour), verdictAt.Add(time.Hour))
	require.NoError(t, err)
	require.Len(t, got, 1, "a row posted before the window whose verdict falls inside it is derived")
}
