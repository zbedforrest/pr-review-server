package db

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestGormDB_SettledPoints_OneRowPerThreadAndListByAuthorWindow(t *testing.T) {
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
	require.Len(t, got, 1, "a thread is one row; the later record replaces it")
	require.Equal(t, "a second answer", got[0].Reason)
	require.Equal(t, "dev-ana", got[0].Author)
	require.True(t, got[0].SettledAt.Equal(at.Add(time.Hour)))

	require.NoError(t, db.RecordSettledPoint(first))
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

	conceded := testPublished(func(p *PublishedFinding) {
		p.RepoOwner, p.RepoName, p.PRNumber = "acme", "example", 596
		p.Fingerprint, p.CommentID, p.State, p.PublishedAt = "store/views.py:3:bbbbbbbbbbbb", 4003, PublishedStateDismissed, posted
	})
	require.NoError(t, db.UpsertPublishedFinding(conceded))
	_, err = db.RecordPublishedReply(&PublishedReply{RepoOwner: "acme", RepoName: "example", PRNumber: 596, RootCommentID: 4003, AuthorCommentID: 5003,
		Fingerprint: conceded.Fingerprint, AuthorID: 77, Class: "resolution", Action: "reacted", Decision: "concede", Body: "Fixed in a82796ac", CreatedAt: verdictAt})
	require.NoError(t, err)
	got, err = db.ListSettledPoints("acme", "example", "dev-ana", verdictAt.Add(-time.Hour), verdictAt.Add(time.Hour))
	require.NoError(t, err)
	require.Len(t, got, 1, "a conceded fix claim is a fix, not a stand")

	deferred := testPublished(func(p *PublishedFinding) {
		p.RepoOwner, p.RepoName, p.PRNumber = "acme", "example", 596
		p.Fingerprint, p.CommentID, p.State, p.PublishedAt = "store/cache.py:5:cccccccccccc", 4004, PublishedStateDismissed, posted
	})
	require.NoError(t, db.UpsertPublishedFinding(deferred))
	_, err = db.RecordPublishedReply(&PublishedReply{RepoOwner: "acme", RepoName: "example", PRNumber: 596, RootCommentID: 4004, AuthorCommentID: 5004,
		Fingerprint: deferred.Fingerprint, AuthorID: 77, Class: "verdict", Action: "reacted", Body: "Intentionally left to expire. This will be cleaned up in a followup PR.", CreatedAt: verdictAt})
	require.NoError(t, err)
	got, err = db.ListSettledPoints("acme", "example", "dev-ana", verdictAt.Add(-time.Hour), verdictAt.Add(time.Hour))
	require.NoError(t, err)
	require.Len(t, got, 1, "a verdict that defers the point to a follow-up settles nothing")

	stood := testPublished(func(p *PublishedFinding) {
		p.RepoOwner, p.RepoName, p.PRNumber = "acme", "example", 596
		p.Fingerprint, p.CommentID, p.State, p.PublishedAt = "store/feed.py:7:dddddddddddd", 4006, PublishedStateDismissed, posted
	})
	require.NoError(t, db.UpsertPublishedFinding(stood))
	for i, body := range []string{"intentional, the feed is paged on purpose", "Actually we'll rework this in a follow-up PR."} {
		_, err = db.RecordPublishedReply(&PublishedReply{RepoOwner: "acme", RepoName: "example", PRNumber: 596, RootCommentID: 4006, AuthorCommentID: int64(5006 + i),
			Fingerprint: stood.Fingerprint, AuthorID: 77, Class: "verdict", Action: "reacted", Body: body, CreatedAt: verdictAt.Add(time.Duration(i) * time.Minute)})
		require.NoError(t, err)
	}
	got, err = db.ListSettledPoints("acme", "example", "dev-ana", verdictAt.Add(-time.Hour), verdictAt.Add(time.Hour))
	require.NoError(t, err)
	require.Len(t, got, 1, "a later deferral supersedes the earlier stand")
	require.Equal(t, int64(4001), got[0].CommentID)

	deferredRow := recorded
	deferredRow.Source = SettledSourceDeferred
	require.NoError(t, db.RecordSettledPoint(&deferredRow))
	got, err = db.ListSettledPoints("acme", "example", "dev-ana", posted.Add(-time.Hour), verdictAt.Add(time.Hour))
	require.NoError(t, err)
	require.Len(t, got, 1, "recording a thread again replaces its row")
	require.Equal(t, SettledSourceDeferred, got[0].Source)
}

func TestGormDB_SettledPoints_DerivedFromAClosedPRWhoseRowCleanupDeleted(t *testing.T) {
	db := newTestDB(t)
	defer db.Close()

	posted := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	verdictAt := posted.Add(time.Hour)
	require.NoError(t, db.UpsertPR(&PR{RepoOwner: "acme", RepoName: "example", PRNumber: 596, Author: "dev-ana", LastCommitSHA: "8c858a5", Status: "completed"}))
	row := testPublished(func(p *PublishedFinding) {
		p.RepoOwner, p.RepoName, p.PRNumber = "acme", "example", 596
		p.Fingerprint, p.CommentID, p.State, p.PublishedAt = "store/models.py:24:ce55dc9e9abf", 4001, PublishedStateDismissed, posted
		p.Subjects = "purchaser_read_swap"
	})
	require.NoError(t, db.UpsertPublishedFinding(row))
	_, err := db.RecordPublishedReply(&PublishedReply{RepoOwner: "acme", RepoName: "example", PRNumber: 596, RootCommentID: 4001, AuthorCommentID: 5001,
		Fingerprint: row.Fingerprint, AuthorID: 77, Class: "verdict", Action: "reacted", Body: "intentional", CreatedAt: verdictAt})
	require.NoError(t, err)
	require.NoError(t, db.DeletePR("acme", "example", 596))

	got, err := db.ListSettledPoints("acme", "example", "dev-ana", posted.Add(-time.Hour), verdictAt.Add(time.Hour))
	require.NoError(t, err)
	require.Empty(t, got, "without any tracked PR of the author their replies cannot be told apart")

	require.NoError(t, db.UpsertPR(&PR{RepoOwner: "acme", RepoName: "example", PRNumber: 668, Author: "Dev-Ana", LastCommitSHA: "6c25465", Status: "pending"}))
	open := testPublished(func(p *PublishedFinding) {
		p.RepoOwner, p.RepoName, p.PRNumber = "acme", "example", 668
		p.Fingerprint, p.CommentID, p.PublishedAt = "store/feed.py:9:dddddddddddd", 4009, verdictAt
	})
	require.NoError(t, db.UpsertPublishedFinding(open))
	_, err = db.RecordPublishedReply(&PublishedReply{RepoOwner: "acme", RepoName: "example", PRNumber: 668, RootCommentID: 4009, AuthorCommentID: 5009,
		Fingerprint: open.Fingerprint, AuthorID: 77, Class: "pushback", Action: "reacted", Body: "The feed reads purchaser_id already.", CreatedAt: verdictAt.Add(time.Hour)})
	require.NoError(t, err)
	got, err = db.ListSettledPoints("acme", "example", "dev-ana", posted.Add(-time.Hour), verdictAt.Add(2*time.Hour))
	require.NoError(t, err)
	require.Len(t, got, 1, "the author's reply id on a tracked PR recovers the closed PR's dismissal")
	require.Equal(t, 596, got[0].PRNumber)
	require.Equal(t, "intentional", got[0].Reason)
}
