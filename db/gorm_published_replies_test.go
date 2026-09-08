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
