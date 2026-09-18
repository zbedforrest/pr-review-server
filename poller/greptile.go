package poller

import (
	"context"
	"log"

	gh "github.com/google/go-github/v57/github"

	"pr-review-server/db"
	"pr-review-server/github"
	"pr-review-server/pkg/reviewer/reconcile"
)

// Greptile's verdict for a head, stored on the PR row so the dashboard's
// draft-approve gate never calls GitHub: "green" when Greptile reviewed the
// head and posted no P0 or P1 finding, "red" when it posted one, "absent"
// when it has not reviewed the head.
const (
	GreptileStatusGreen  = "green"
	GreptileStatusRed    = "red"
	GreptileStatusAbsent = "absent"
)

// greptileRefreshesPerCycle bounds how many PRs one poll cycle catches up
// on; each refresh is at least two REST calls, more on PRs with over a
// hundred reviews or comments.
const greptileRefreshesPerCycle = 20

// greptileStatusStore is the narrow persistence capability behind the
// verdict, implemented by *db.GormDB and type-asserted so mocks stay untouched.
type greptileStatusStore interface {
	SetPRGreptileStatus(owner, repo string, prNumber int, headSHA, status string, reviewCount int) (bool, error)
}

// greptileReviewIDsForHead picks Greptile's live reviews of head; a review
// someone dismissed no longer vouches for anything.
func greptileReviewIDsForHead(reviews []*gh.PullRequestReview, head string) map[int64]bool {
	ids := map[int64]bool{}
	for _, r := range reviews {
		if r.GetState() == "DISMISSED" || r.GetState() == "PENDING" {
			continue
		}
		if reconcile.IsGreptileAuthor(r.GetUser().GetLogin()) && isSameCommit(r.GetCommitID(), head) {
			ids[r.GetID()] = true
		}
	}
	return ids
}

// computeGreptileStatus grades the head from Greptile's reviews of it and the
// inline comments those reviews carry. Comments from earlier heads do not
// count against a head Greptile has since reviewed clean.
func computeGreptileStatus(reviews []*gh.PullRequestReview, comments []github.ReviewCommentInfo, head string) string {
	headReviews := greptileReviewIDsForHead(reviews, head)
	if len(headReviews) == 0 {
		return GreptileStatusAbsent
	}
	external := make([]reconcile.ExternalComment, 0, len(comments))
	for _, c := range comments {
		if !headReviews[c.ReviewID] {
			continue
		}
		external = append(external, reconcile.ExternalComment{
			ID: c.ID, Author: c.Author, Body: c.Body, Path: c.Path,
			Line: c.Line, StartLine: c.StartLine, InReplyToID: c.InReplyToID,
		})
	}
	for _, f := range reconcile.ParseGreptileComments(external) {
		if f.Severity == "critical" || f.Severity == "medium" {
			return GreptileStatusRed
		}
	}
	return GreptileStatusGreen
}

// greptileHeadReviews counts Greptile's live reviews of the head in the
// batch review data.
func greptileHeadReviews(data *github.PRReviewData) int {
	n := 0
	for login, count := range data.HeadReviewCounts {
		if reconcile.IsGreptileAuthor(login) {
			n += count
		}
	}
	return n
}

// needsGreptileRefresh is the poll-cycle catch-up: Greptile usually posts
// after PRism finishes, so completion stored "absent" for this very head, and
// it may review the same head again later. Recompute whenever the stored
// verdict covers fewer Greptile reviews of the head than GitHub now shows.
func needsGreptileRefresh(pr *db.PR, data *github.PRReviewData) bool {
	reviews := greptileHeadReviews(data)
	if reviews == 0 {
		return false
	}
	if !isSameCommit(pr.GreptileStatusSHA, data.HeadOID) {
		return true
	}
	if pr.GreptileStatus != GreptileStatusGreen && pr.GreptileStatus != GreptileStatusRed {
		return true
	}
	return reviews > pr.GreptileReviewCount
}

// refreshGreptileStatus reads Greptile's reviews of head with the App client
// and stores the verdict, reporting whether the row changed. Best-effort: a
// failure leaves the previous verdict (or none) in place and the gate reads
// it as absent.
func (p *Poller) refreshGreptileStatus(ctx context.Context, owner, repo string, number int, head string) bool {
	store, ok := p.db.(greptileStatusStore)
	if !ok || p.ghClientConcrete == nil || head == "" {
		return false
	}
	reviews, err := p.ghClientConcrete.ListAllReviews(ctx, owner, repo, number)
	if err != nil {
		log.Printf("[GREPTILE] %s/%s#%d: list reviews: %v", owner, repo, number, err)
		return false
	}
	status := GreptileStatusAbsent
	headReviews := len(greptileReviewIDsForHead(reviews, head))
	if headReviews > 0 {
		comments, err := p.ghClientConcrete.ListReviewComments(ctx, owner, repo, number)
		if err != nil {
			log.Printf("[GREPTILE] %s/%s#%d: list review comments: %v", owner, repo, number, err)
			return false
		}
		status = computeGreptileStatus(reviews, comments, head)
	}
	written, err := store.SetPRGreptileStatus(owner, repo, number, head, status, headReviews)
	if err != nil {
		log.Printf("[GREPTILE] %s/%s#%d: store status: %v", owner, repo, number, err)
		return false
	}
	if !written {
		log.Printf("[GREPTILE] %s/%s#%d head=%s status=%s not stored, row is on a newer head", owner, repo, number, shortSHA(head), status)
		return false
	}
	log.Printf("[GREPTILE] %s/%s#%d head=%s status=%s reviews=%d", owner, repo, number, shortSHA(head), status, headReviews)
	return true
}
