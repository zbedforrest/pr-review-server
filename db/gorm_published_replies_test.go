package db

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestGormDB_PublishedReply_RecordIsIdempotentPerAuthorComment(t *testing.T) {
	db := newTestDB(t)
	r := &PublishedReply{
		RepoOwner: "owner", RepoName: "repo", PRNumber: 7,
		RootCommentID: 9001, AuthorCommentID: 9010, Fingerprint: "a.go:1:abc",
		AuthorID: 42, Class: "resolution", Action: "reacted", Body: "Fixed",
		CreatedAt: time.Now().UTC().Truncate(time.Second),
	}
	created, err := db.RecordPublishedReply(r)
	require.NoError(t, err)
	assert.True(t, created)

	created, err = db.RecordPublishedReply(r)
	require.NoError(t, err)
	assert.False(t, created, "the same author comment must not be recorded twice")

	seen, err := db.GetPublishedReplyIDsForPR("owner", "repo", 7)
	require.NoError(t, err)
	assert.Equal(t, map[int64]bool{9010: true}, seen)
}

func TestGormDB_ListPublishedReplyTargets_ReturnsOpenNonDraftPRsWithInlineComments(t *testing.T) {
	db := newTestDB(t)
	for n, pr := range map[int]*PR{
		7:  {RepoOwner: "owner", RepoName: "repo", PRNumber: 7, PRState: "open"},
		8:  {RepoOwner: "owner", RepoName: "repo", PRNumber: 8, PRState: "open"},
		9:  {RepoOwner: "owner", RepoName: "repo", PRNumber: 9, PRState: "open"},
		10: {RepoOwner: "owner", RepoName: "repo", PRNumber: 10, PRState: "merged"},
		11: {RepoOwner: "owner", RepoName: "repo", PRNumber: 11, PRState: "open", Draft: true},
		13: {RepoOwner: "owner", RepoName: "repo", PRNumber: 13, PRState: ""},
	} {
		pr.LastCommitSHA = "abc"
		pr.Title = "t"
		pr.Author = "a"
		pr.Status = "completed"
		require.NoError(t, db.UpsertPR(pr), "pr %d", n)
	}
	require.NoError(t, db.UpsertPublishedFinding(testPublished(nil)))
	require.NoError(t, db.UpsertPublishedFinding(testPublished(func(p *PublishedFinding) { p.PRNumber = 8; p.CommentID = 0 })))
	require.NoError(t, db.UpsertPublishedFinding(testPublished(func(p *PublishedFinding) {
		p.PRNumber = 9
		p.Fingerprint = "b.go:2:feedface0000"
		p.State = PublishedStateResolved
	})))
	require.NoError(t, db.UpsertPublishedFinding(testPublished(func(p *PublishedFinding) { p.PRNumber = 10 })))
	require.NoError(t, db.UpsertPublishedFinding(testPublished(func(p *PublishedFinding) { p.PRNumber = 11 })))
	require.NoError(t, db.UpsertPublishedFinding(testPublished(func(p *PublishedFinding) { p.PRNumber = 12 })))
	require.NoError(t, db.UpsertPublishedFinding(testPublished(func(p *PublishedFinding) { p.PRNumber = 13 })))

	targets, err := db.ListPublishedReplyTargets()
	require.NoError(t, err)
	require.Len(t, targets, 3, "no comment id (8), merged (10), draft (11) and no PR row (12) are skipped; a legacy empty state (13) counts as open")
	assert.Equal(t, 7, targets[0].PRNumber)
	assert.Equal(t, map[int64]string{9001: "pkg/api/handler.go:4:deadbeef0123"}, targets[0].Roots)
	assert.Equal(t, 9, targets[1].PRNumber, "resolved findings still own their threads")
}

func TestGormDB_ListUnlinkedPublishedFindings_ReturnsReviewPostedRootsWithoutCommentIDsOnOpenPRs(t *testing.T) {
	db := newTestDB(t)
	for _, pr := range []*PR{
		{RepoOwner: "owner", RepoName: "repo", PRNumber: 7, PRState: "open"},
		{RepoOwner: "owner", RepoName: "repo", PRNumber: 8, PRState: "merged"},
		{RepoOwner: "owner", RepoName: "repo", PRNumber: 9, PRState: "open", Draft: true},
	} {
		pr.LastCommitSHA, pr.Title, pr.Author, pr.Status = "abc", "t", "a", "completed"
		require.NoError(t, db.UpsertPR(pr))
	}
	require.NoError(t, db.UpsertPublishedFinding(testPublished(func(p *PublishedFinding) { p.CommentID = 0 })))
	require.NoError(t, db.UpsertPublishedFinding(testPublished(func(p *PublishedFinding) {
		p.Fingerprint = "b.go:2:feedface0000"
		p.CommentID = 0
		p.ReviewID = 0
	})))
	require.NoError(t, db.UpsertPublishedFinding(testPublished(func(p *PublishedFinding) { p.Fingerprint = "c.go:3:cafe00000000" })))
	require.NoError(t, db.UpsertPublishedFinding(testPublished(func(p *PublishedFinding) {
		p.Fingerprint = "summary"
		p.Kind = PublishedKindSummary
		p.CommentID = 0
	})))
	require.NoError(t, db.UpsertPublishedFinding(testPublished(func(p *PublishedFinding) { p.PRNumber = 8; p.CommentID = 0 })))
	require.NoError(t, db.UpsertPublishedFinding(testPublished(func(p *PublishedFinding) { p.PRNumber = 9; p.CommentID = 0 })))

	rows, err := db.ListUnlinkedPublishedFindings()
	require.NoError(t, err)
	require.Len(t, rows, 1, "only inline findings posted through a review, still without a comment id, on open non-draft PRs")
	assert.Equal(t, "pkg/api/handler.go:4:deadbeef0123", rows[0].Fingerprint)
	assert.Equal(t, int64(4242), rows[0].ReviewID)
	assert.Equal(t, 7, rows[0].PRNumber)
	assert.NotZero(t, rows[0].ID)

	require.NoError(t, db.LinkPublishedFindingComment(rows[0].ID, 777))
	rows, err = db.ListUnlinkedPublishedFindings()
	require.NoError(t, err)
	assert.Empty(t, rows)
	targets, err := db.ListPublishedReplyTargets()
	require.NoError(t, err)
	require.Len(t, targets, 1)
	assert.Equal(t, "pkg/api/handler.go:4:deadbeef0123", targets[0].Roots[777])
}

func TestGormDB_LinkPublishedFindingComment_NeverOverwritesAnExistingCommentID(t *testing.T) {
	db := newTestDB(t)
	require.NoError(t, db.UpsertPublishedFinding(testPublished(nil)))
	rows, err := db.GetPublishedFindingsForPR("owner", "repo", 7)
	require.NoError(t, err)
	require.Len(t, rows, 1)

	require.NoError(t, db.LinkPublishedFindingComment(uint(rows[0].ID), 777))
	rows, err = db.GetPublishedFindingsForPR("owner", "repo", 7)
	require.NoError(t, err)
	assert.Equal(t, int64(9001), rows[0].CommentID)
}

func TestGormDB_ListRecentPublishedReplies_MostRecentlyHandledFirstWithinLimit(t *testing.T) {
	db := newTestDB(t)
	base := time.Date(2026, 9, 9, 12, 0, 0, 0, time.UTC)
	for i, age := range []time.Duration{0, time.Minute, -48 * time.Hour} {
		_, err := db.RecordPublishedReply(&PublishedReply{
			RepoOwner: "owner", RepoName: "repo", PRNumber: 7,
			RootCommentID: 9001, AuthorCommentID: int64(9010 + i), Fingerprint: "a.go:1:abc",
			AuthorID: 42, Class: "resolution", Action: "reacted", Body: "Fixed",
			CreatedAt: base.Add(age),
		})
		require.NoError(t, err)
		time.Sleep(2 * time.Millisecond)
	}
	rows, err := db.ListRecentPublishedReplies(2)
	require.NoError(t, err)
	require.Len(t, rows, 2)
	assert.Equal(t, int64(9012), rows[0].AuthorCommentID, "an old reply handled last is the newest news")
	assert.Equal(t, int64(9011), rows[1].AuthorCommentID)
	assert.False(t, rows[0].ProcessedAt.IsZero())
}

func TestGormDB_CountPublishedReplies_GroupsByActionAndClass(t *testing.T) {
	db := newTestDB(t)
	for i, class := range []string{"resolution", "resolution", "question"} {
		_, err := db.RecordPublishedReply(&PublishedReply{
			RepoOwner: "owner", RepoName: "repo", PRNumber: 7, RootCommentID: 9001, AuthorCommentID: int64(9010 + i),
			Fingerprint: "a.go:1:abc", AuthorID: 42, Class: class, Action: map[bool]string{true: "reacted", false: "observed"}[i < 2], Body: "x",
			CreatedAt: time.Now().UTC(),
		})
		require.NoError(t, err)
	}
	counts, err := db.CountPublishedReplies()
	require.NoError(t, err)
	assert.Equal(t, 3, counts.Total)
	assert.Equal(t, map[string]int{"reacted": 2, "observed": 1}, counts.ByAction)
	assert.Equal(t, map[string]int{"resolution": 2, "question": 1}, counts.ByClass)
}

func TestGormDB_PublishedReply_DecisionAndPostingRoundTrip(t *testing.T) {
	db := newTestDB(t)
	base := time.Date(2026, 9, 9, 12, 0, 0, 0, time.UTC)
	seed := func(id int64, root int64, class string) {
		_, err := db.RecordPublishedReply(&PublishedReply{
			RepoOwner: "owner", RepoName: "repo", PRNumber: 7, RootCommentID: root, AuthorCommentID: id,
			Fingerprint: "a.go:1:abc", AuthorID: 42, Class: class, Action: "reacted", Body: "b", CreatedAt: base,
		})
		require.NoError(t, err)
	}
	seed(9010, 9001, "pushback")
	seed(9011, 9001, "question")
	seed(9020, 9002, "pushback")

	require.NoError(t, db.SetPublishedReplyDecision("owner", "repo", 7, 9010, ReplyDecisionRecord{
		Decision: "hold", ReplyBody: "Still applies: see a.go:12.", Cited: `[{"file":"a.go","line":12}]`, Model: "claude-fable-5-1", DurationMS: 4200,
	}))
	require.NoError(t, db.MarkPublishedReplyPosted("owner", "repo", 7, 9010, 9500, base.Add(time.Minute)))
	require.NoError(t, db.SetPublishedReplyDecision("owner", "repo", 7, 9011, ReplyDecisionRecord{Decision: "abstain"}))

	rows, err := db.ListPublishedRepliesForRoot("owner", "repo", 7, 9001)
	require.NoError(t, err)
	require.Len(t, rows, 2)
	assert.Equal(t, "hold", rows[0].Decision)
	assert.Equal(t, int64(9500), rows[0].ReplyCommentID)
	assert.Equal(t, "Still applies: see a.go:12.", rows[0].ReplyBody)
	require.NotNil(t, rows[0].RepliedAt)
	assert.Equal(t, "abstain", rows[1].Decision)
	assert.Equal(t, int64(0), rows[1].ReplyCommentID)

	n, err := db.CountPublishedTextRepliesSince("owner", "repo", 7, base)
	require.NoError(t, err)
	assert.Equal(t, 1, n, "only posted text replies count against the per-PR budget")
	n, err = db.CountPublishedTextRepliesSince("owner", "repo", 7, base.Add(2*time.Minute))
	require.NoError(t, err)
	assert.Equal(t, 0, n)
}

func TestGormDB_SetPublishedFindingState_DismissesOneRow(t *testing.T) {
	db := newTestDB(t)
	require.NoError(t, db.UpsertPublishedFinding(testPublished(nil)))
	require.NoError(t, db.UpsertPublishedFinding(testPublished(func(p *PublishedFinding) { p.Fingerprint = "b.go:2:feedface0000" })))
	require.NoError(t, db.SetPublishedFindingState("owner", "repo", 7, "pkg/api/handler.go:4:deadbeef0123", PublishedStateDismissed))
	rows, err := db.GetPublishedFindingsForPR("owner", "repo", 7)
	require.NoError(t, err)
	states := map[string]string{}
	for _, r := range rows {
		states[r.Fingerprint] = r.State
	}
	assert.Equal(t, map[string]string{"pkg/api/handler.go:4:deadbeef0123": PublishedStateDismissed, "b.go:2:feedface0000": PublishedStateOpen}, states)
}

func TestGormDB_PublishedReply_OutcomeAndAttempts(t *testing.T) {
	db := newTestDB(t)
	_, err := db.RecordPublishedReply(&PublishedReply{
		RepoOwner: "owner", RepoName: "repo", PRNumber: 7, RootCommentID: 9001, AuthorCommentID: 9010,
		Fingerprint: "a.go:1:abc", AuthorID: 42, Class: "pushback", Action: "reacted", Body: "b", CreatedAt: time.Now().UTC(),
	})
	require.NoError(t, err)
	n, err := db.IncrementPublishedReplyAttempts("owner", "repo", 7, 9010)
	require.NoError(t, err)
	assert.Equal(t, 1, n)
	n, err = db.IncrementPublishedReplyAttempts("owner", "repo", 7, 9010)
	require.NoError(t, err)
	assert.Equal(t, 2, n)
	require.NoError(t, db.SetPublishedReplyOutcome("owner", "repo", 7, 9010, "posted"))
	rows, err := db.ListPublishedRepliesForPR("owner", "repo", 7)
	require.NoError(t, err)
	require.Len(t, rows, 1)
	assert.Equal(t, "posted", rows[0].Outcome)
	assert.Equal(t, 2, rows[0].Attempts)
}

func TestGormDB_EnsureIdempotentColumns_AddsReplyDecisionColumnsToAnOldTable(t *testing.T) {
	database := newTestDB(t)
	for _, col := range []string{"decision", "reply_body", "cited", "model", "duration_ms", "outcome", "attempts", "decision_head", "decision_thread", "replied_at", "claimed_by", "claimed_at"} {
		require.NoError(t, database.db.Migrator().DropColumn(&PublishedReplyModel{}, col), col)
	}
	require.NoError(t, database.ensureIdempotentColumns())
	_, err := database.RecordPublishedReply(&PublishedReply{
		RepoOwner: "owner", RepoName: "repo", PRNumber: 7, RootCommentID: 9001, AuthorCommentID: 9010,
		Fingerprint: "a.go:1:abc", AuthorID: 42, Class: "pushback", Action: "reacted", Body: "b", CreatedAt: time.Now().UTC(),
	})
	require.NoError(t, err)
	rows, err := database.ListPublishedRepliesForPR("owner", "repo", 7)
	require.NoError(t, err)
	require.Len(t, rows, 1)
}

func TestGormDB_ClaimPublishedReply_IsExclusiveUntilReleasedOrStale(t *testing.T) {
	db := newTestDB(t)
	_, err := db.RecordPublishedReply(&PublishedReply{
		RepoOwner: "owner", RepoName: "repo", PRNumber: 7, RootCommentID: 9001, AuthorCommentID: 9010,
		Fingerprint: "a.go:1:abc", AuthorID: 42, Class: "pushback", Action: "reacted", Body: "b", CreatedAt: time.Now().UTC(),
	})
	require.NoError(t, err)
	now := time.Date(2026, 9, 9, 19, 0, 0, 0, time.UTC)
	ok, err := db.ClaimPublishedReply("owner", "repo", 7, 9010, "a", now, 10*time.Minute)
	require.NoError(t, err)
	assert.True(t, ok)
	ok, err = db.ClaimPublishedReply("owner", "repo", 7, 9010, "b", now.Add(time.Minute), 10*time.Minute)
	require.NoError(t, err)
	assert.False(t, ok, "a live claim by another holder must be refused")
	ok, err = db.ClaimPublishedReply("owner", "repo", 7, 9010, "b", now.Add(11*time.Minute), 10*time.Minute)
	require.NoError(t, err)
	assert.True(t, ok, "a stale claim is taken over")
	require.NoError(t, db.ReleasePublishedReplyClaim("owner", "repo", 7, 9010, "b"))
	ok, err = db.ClaimPublishedReply("owner", "repo", 7, 9010, "a", now.Add(12*time.Minute), 10*time.Minute)
	require.NoError(t, err)
	assert.True(t, ok)
	require.NoError(t, db.SetPublishedReplyOutcome("owner", "repo", 7, 9010, "posted"))
	ok, err = db.ClaimPublishedReply("owner", "repo", 7, 9010, "c", now.Add(30*time.Minute), 10*time.Minute)
	require.NoError(t, err)
	assert.False(t, ok, "a finished step cannot be claimed")
}

func TestGormDB_MentionTriggers_ReserveFinalizeRelease(t *testing.T) {
	db := newTestDB(t)
	now := time.Date(2026, 9, 10, 12, 0, 0, 0, time.UTC)
	m := &MentionTrigger{CommentID: 500, RepoOwner: "owner", RepoName: "repo", PRNumber: 7, Author: "alice", CommitSHA: "abc", Publish: true, Holder: "a", CreatedAt: now, TriggeredAt: now}
	ok, err := db.ReserveMention(m)
	require.NoError(t, err)
	assert.True(t, ok)
	handled, err := db.MentionHandled(500, now.Add(time.Minute))
	require.NoError(t, err)
	assert.True(t, handled, "a live reservation counts as handled")
	other := *m
	other.Holder = "b"
	ok, err = db.ReserveMention(&other)
	require.NoError(t, err)
	assert.False(t, ok, "a live reservation cannot be taken by a second holder")

	require.NoError(t, db.ReleaseMention(500, "b"))
	handled, _ = db.MentionHandled(500, now.Add(time.Minute))
	assert.True(t, handled, "only the holder can release")
	require.NoError(t, db.ReleaseMention(500, "a"))
	handled, err = db.MentionHandled(500, now.Add(time.Minute))
	require.NoError(t, err)
	assert.False(t, handled, "a released request is retried")

	ok, err = db.ReserveMention(m)
	require.NoError(t, err)
	assert.True(t, ok)
	owned, err := db.FinalizeMention(500, "b")
	require.NoError(t, err)
	assert.False(t, owned, "only the holder can finalise")
	owned, err = db.FinalizeMention(500, "a")
	require.NoError(t, err)
	assert.True(t, owned)
	handled, err = db.MentionHandled(500, now.Add(48*time.Hour))
	require.NoError(t, err)
	assert.True(t, handled, "a finalised request stays handled")
	require.NoError(t, db.ReleaseMention(500, "a"))
	handled, _ = db.MentionHandled(500, now.Add(48*time.Hour))
	assert.True(t, handled, "release never removes a finalised row")

	stale := &MentionTrigger{CommentID: 600, RepoOwner: "owner", RepoName: "repo", PRNumber: 7, Author: "alice", Holder: "dead", CreatedAt: now, TriggeredAt: now.Add(-time.Hour)}
	_, err = db.ReserveMention(stale)
	require.NoError(t, err)
	handled, err = db.MentionHandled(600, now)
	require.NoError(t, err)
	assert.False(t, handled, "a reservation older than the TTL is a dead pass")
	ok, err = db.ReserveMention(&MentionTrigger{CommentID: 600, RepoOwner: "owner", RepoName: "repo", PRNumber: 7, Author: "alice", Holder: "b", CreatedAt: now, TriggeredAt: now})
	require.NoError(t, err)
	assert.True(t, ok, "and can be taken over")
}
