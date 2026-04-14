package poller

import (
	"testing"

	"pr-review-server/db"

	"github.com/stretchr/testify/assert"
)

func TestViewBatch_EnsureView_Basic(t *testing.T) {
	b := newViewBatch()
	b.EnsureView(1, 100, true)
	b.EnsureView(2, 200, false)

	assert.Equal(t, 2, b.Len())

	items := b.Items()
	assert.Len(t, items, 2)
}

func TestViewBatch_EnsureView_Deduplicates(t *testing.T) {
	b := newViewBatch()
	b.EnsureView(1, 100, false)
	b.EnsureView(1, 100, false) // duplicate

	assert.Equal(t, 1, b.Len())
}

func TestViewBatch_IsAuthor_TrueWins(t *testing.T) {
	b := newViewBatch()
	b.EnsureView(1, 100, false)
	b.EnsureView(1, 100, true) // author wins

	items := b.Items()
	assert.Len(t, items, 1)
	assert.True(t, items[0].IsAuthor)
}

func TestViewBatch_IsAuthor_TrueNotOverwrittenByFalse(t *testing.T) {
	b := newViewBatch()
	b.EnsureView(1, 100, true)
	b.EnsureView(1, 100, false) // should not downgrade

	items := b.Items()
	assert.Len(t, items, 1)
	assert.True(t, items[0].IsAuthor)
}

func TestViewBatch_SetReviewStatus_CreatesEntry(t *testing.T) {
	b := newViewBatch()
	b.SetReviewStatus(1, 100, "APPROVED")

	items := b.Items()
	assert.Len(t, items, 1)
	assert.NotNil(t, items[0].ReviewStatus)
	assert.Equal(t, "APPROVED", *items[0].ReviewStatus)
	assert.Nil(t, items[0].ViaTeams)
}

func TestViewBatch_SetViaTeams_CreatesEntry(t *testing.T) {
	b := newViewBatch()
	b.SetViaTeams(1, 100, []string{"team-a:pending", "team-b:approved"})

	items := b.Items()
	assert.Len(t, items, 1)
	assert.Nil(t, items[0].ReviewStatus)
	assert.NotNil(t, items[0].ViaTeams)
	assert.Equal(t, []string{"team-a:pending", "team-b:approved"}, *items[0].ViaTeams)
}

func TestViewBatch_MergesAllFields(t *testing.T) {
	b := newViewBatch()
	b.EnsureView(1, 100, true)
	b.SetReviewStatus(1, 100, "CHANGES_REQUESTED")
	b.SetViaTeams(1, 100, []string{"security:my_pending"})

	items := b.Items()
	assert.Len(t, items, 1)
	item := items[0]
	assert.Equal(t, 1, item.UserID)
	assert.Equal(t, 100, item.PRID)
	assert.True(t, item.IsAuthor)
	assert.NotNil(t, item.ReviewStatus)
	assert.Equal(t, "CHANGES_REQUESTED", *item.ReviewStatus)
	assert.NotNil(t, item.ViaTeams)
	assert.Equal(t, []string{"security:my_pending"}, *item.ViaTeams)
}

func TestViewBatch_MultipleUsers(t *testing.T) {
	b := newViewBatch()
	b.EnsureView(1, 100, true)
	b.EnsureView(2, 100, false)
	b.SetReviewStatus(1, 100, "APPROVED")
	b.SetViaTeams(2, 100, []string{"backend:pending"})

	assert.Equal(t, 2, b.Len())
	items := b.Items()
	assert.Len(t, items, 2)

	// Find each user's item
	byUser := map[int]int{}
	for i, item := range items {
		byUser[item.UserID] = i
	}

	user1 := items[byUser[1]]
	assert.True(t, user1.IsAuthor)
	assert.NotNil(t, user1.ReviewStatus)
	assert.Equal(t, "APPROVED", *user1.ReviewStatus)

	user2 := items[byUser[2]]
	assert.False(t, user2.IsAuthor)
	assert.NotNil(t, user2.ViaTeams)
	assert.Equal(t, []string{"backend:pending"}, *user2.ViaTeams)
}

func TestViewBatch_EmptyFlush(t *testing.T) {
	b := newViewBatch()
	// Flush on empty batch should not error
	err := b.Flush(nil) // nil db is fine since Flush returns early for empty
	assert.NoError(t, err)
}

func TestPRBatch_Upsert_Basic(t *testing.T) {
	b := newPRBatch()
	b.Upsert(&db.PR{RepoOwner: "org", RepoName: "repo", PRNumber: 1})
	b.Upsert(&db.PR{RepoOwner: "org", RepoName: "repo", PRNumber: 2})

	assert.Equal(t, 2, b.Len())
}

func TestPRBatch_Upsert_Deduplicates(t *testing.T) {
	b := newPRBatch()
	b.Upsert(&db.PR{RepoOwner: "org", RepoName: "repo", PRNumber: 1, ApprovalCount: 1})
	b.Upsert(&db.PR{RepoOwner: "org", RepoName: "repo", PRNumber: 1, ApprovalCount: 3})

	assert.Equal(t, 1, b.Len())
	// Last write wins
	for _, pr := range b.prs {
		assert.Equal(t, 3, pr.ApprovalCount)
	}
}

func TestPRBatch_EmptyFlush(t *testing.T) {
	b := newPRBatch()
	err := b.Flush(nil)
	assert.NoError(t, err)
}
