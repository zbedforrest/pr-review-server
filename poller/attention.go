package poller

import (
	"log"
	"strings"

	"pr-review-server/db"
	"pr-review-server/github"
)

// attentionTransition records one user's needs_attention flip for a PR so it
// can be logged and broadcast once the batch write has succeeded.
type attentionTransition struct {
	userID  int
	login   string
	pr      github.PullRequest
	flagged bool
	head    string
}

// attentionForUser resolves the needs_attention write for one viewer. A PR that
// cannot need re-review (draft, closed or merged, or the viewer's own) forces
// false regardless of the reducer; otherwise the reducer's verdict is used and
// nil means it had none, so the stored value is preserved. Draft comes from the
// fetched review data: the DB row lags one cycle behind a draft/ready flip.
func attentionForUser(reviewData *github.PRReviewData, login string, prState string, isAuthor bool) *bool {
	if reviewData.IsDraft || isAuthor || (prState != "" && prState != "open") {
		return boolPtr(false)
	}
	for reviewer, flagged := range reviewData.AttentionByUser {
		if strings.EqualFold(reviewer, login) {
			return boolPtr(flagged)
		}
	}
	return nil
}

func boolPtr(v bool) *bool {
	return &v
}

// storedAttentionFlags reads the current needs_attention value of every view
// row on the PRs with fresh review data, keyed by (user, PR). Only rows present
// in the result exist. ok is false when the read failed, in which case the
// snapshot says nothing about which rows exist and callers must not derive
// transitions from it.
func (p *Poller) storedAttentionFlags(reviewDataMap map[string]*github.PRReviewData, dbPRMap map[string]*db.PR) (flags map[userPRViewKey]bool, ok bool) {
	prIDs := make([]int, 0, len(reviewDataMap))
	for key := range reviewDataMap {
		if dbPR, ok := dbPRMap[key]; ok {
			prIDs = append(prIDs, dbPR.ID)
		}
	}
	flags = make(map[userPRViewKey]bool)
	views, err := p.db.GetUserPRViewsForPRs(prIDs)
	if err != nil {
		log.Printf("[POLL] WARNING: Failed to read current attention flags, writing verdicts without transition detection this cycle: %v", err)
		return flags, false
	}
	for _, view := range views {
		flags[userPRViewKey{UserID: view.UserID, PRID: view.PRID}] = view.NeedsAttention
	}
	return flags, true
}

func (p *Poller) reportAttentionTransitions(transitions []attentionTransition) {
	for _, t := range transitions {
		verb := "cleared"
		if t.flagged {
			verb = "flagged"
		}
		log.Printf("[ATTENTION] user=%s pr=%s/%s#%d %s head=%s", t.login, t.pr.Owner, t.pr.Repo, t.pr.Number, verb, shortSHA(t.head))
		p.broadcastPRUpdateToUser(t.userID, t.pr.Owner, t.pr.Repo, t.pr.Number)
	}
}

func shortSHA(sha string) string {
	if len(sha) > 7 {
		return sha[:7]
	}
	return sha
}
