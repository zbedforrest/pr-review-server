package poller

import (
	"fmt"

	"pr-review-server/db"
)

// userPRViewKey uniquely identifies a user-PR relationship.
type userPRViewKey struct {
	UserID int
	PRID   int
}

// userPRViewUpdate accumulates all changes to a single user_pr_view row.
// Pointer fields mean "update this column"; nil means "preserve existing value".
type userPRViewUpdate struct {
	IsAuthor     bool
	ReviewStatus *string
	ViaTeams     *[]string
}

// viewBatch accumulates user_pr_view changes across a phase.
type viewBatch struct {
	views map[userPRViewKey]*userPRViewUpdate
}

func newViewBatch() *viewBatch {
	return &viewBatch{views: make(map[userPRViewKey]*userPRViewUpdate)}
}

// EnsureView marks a (user, PR) pair as needing to exist, un-hidden.
func (b *viewBatch) EnsureView(userID, prID int, isAuthor bool) {
	key := userPRViewKey{UserID: userID, PRID: prID}
	if existing, ok := b.views[key]; ok {
		if isAuthor {
			existing.IsAuthor = true
		}
	} else {
		b.views[key] = &userPRViewUpdate{IsAuthor: isAuthor}
	}
}

// SetReviewStatus sets the review_status for a (user, PR) pair.
func (b *viewBatch) SetReviewStatus(userID, prID int, status string) {
	key := userPRViewKey{UserID: userID, PRID: prID}
	if existing, ok := b.views[key]; ok {
		existing.ReviewStatus = &status
	} else {
		b.views[key] = &userPRViewUpdate{ReviewStatus: &status}
	}
}

// SetViaTeams sets the via_teams for a (user, PR) pair.
func (b *viewBatch) SetViaTeams(userID, prID int, viaTeams []string) {
	key := userPRViewKey{UserID: userID, PRID: prID}
	if existing, ok := b.views[key]; ok {
		existing.ViaTeams = &viaTeams
	} else {
		b.views[key] = &userPRViewUpdate{ViaTeams: &viaTeams}
	}
}

// Len returns the number of accumulated entries.
func (b *viewBatch) Len() int {
	return len(b.views)
}

// Items converts the accumulated views into a slice of UserPRViewBatchItem for DB flush.
func (b *viewBatch) Items() []db.UserPRViewBatchItem {
	items := make([]db.UserPRViewBatchItem, 0, len(b.views))
	for key, update := range b.views {
		item := db.UserPRViewBatchItem{
			UserID:   key.UserID,
			PRID:     key.PRID,
			IsAuthor: update.IsAuthor,
		}
		if update.ReviewStatus != nil {
			item.ReviewStatus = update.ReviewStatus
		}
		if update.ViaTeams != nil {
			item.ViaTeams = update.ViaTeams
		}
		items = append(items, item)
	}
	return items
}

// Flush writes all accumulated view changes to the database in batch.
func (b *viewBatch) Flush(database db.Database) error {
	if len(b.views) == 0 {
		return nil
	}
	return database.BatchUpsertUserPRViews(b.Items())
}

// prBatch accumulates PR upserts, deduplicating by owner/repo/number.
type prBatch struct {
	prs map[string]*db.PR
}

func newPRBatch() *prBatch {
	return &prBatch{prs: make(map[string]*db.PR)}
}

// Upsert adds or updates a PR in the batch.
func (b *prBatch) Upsert(pr *db.PR) {
	key := fmt.Sprintf("%s/%s/%d", pr.RepoOwner, pr.RepoName, pr.PRNumber)
	b.prs[key] = pr
}

// Len returns the number of accumulated entries.
func (b *prBatch) Len() int {
	return len(b.prs)
}

// Flush writes all accumulated PR changes to the database in batch.
func (b *prBatch) Flush(database db.Database) error {
	if len(b.prs) == 0 {
		return nil
	}
	prs := make([]*db.PR, 0, len(b.prs))
	for _, pr := range b.prs {
		prs = append(prs, pr)
	}
	return database.BatchUpsertPRs(prs)
}
