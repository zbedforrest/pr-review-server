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

func TestGormDB_ListPublishedReplyTargets_ReturnsPRsWithOpenInlineComments(t *testing.T) {
	db := newTestDB(t)
	require.NoError(t, db.UpsertPublishedFinding(testPublished(nil)))
	require.NoError(t, db.UpsertPublishedFinding(testPublished(func(p *PublishedFinding) {
		p.PRNumber = 8
		p.CommentID = 0
	})))
	require.NoError(t, db.UpsertPublishedFinding(testPublished(func(p *PublishedFinding) {
		p.PRNumber = 9
		p.Fingerprint = "b.go:2:feedface0000"
		p.State = PublishedStateResolved
	})))

	targets, err := db.ListPublishedReplyTargets()
	require.NoError(t, err)
	require.Len(t, targets, 2, "PR 8 has no comment id and must be skipped")
	assert.Equal(t, 7, targets[0].PRNumber)
	assert.Equal(t, map[int64]string{9001: "pkg/api/handler.go:4:deadbeef0123"}, targets[0].Roots)
	assert.Equal(t, 9, targets[1].PRNumber, "resolved findings still own their threads")
}
