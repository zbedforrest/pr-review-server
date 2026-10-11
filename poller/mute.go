package poller

import (
	"context"
	"log"
	"time"

	"pr-review-server/db"
	"pr-review-server/github"
	"pr-review-server/pkg/health"
)

// A PR author mutes PRism on one PR with "@<handle> stop" in the
// conversation or a thumbs-down on the sticky summary. The mute is a flag on
// the summary ledger row; every posting path (a round, the reply reactor, a
// summary refresh) asks prMuted before it reaches the PR, and "@<handle>
// review" from the author clears it. PUBLISH_PR_MUTE=false turns the whole
// mechanism off.

// muteLedger is the ledger slice the mute needs; *db.GormDB implements it.
type muteLedger interface {
	SetPublishedMute(owner, repo string, number int, muted bool) (bool, error)
	GetPublishedSummaryForPR(owner, repo string, number int) (db.PublishedFinding, bool, error)
}

// summaryReactionsGitHub reads the reactions on the summary comment and
// acknowledges a thumbs-down on it.
type summaryReactionsGitHub interface {
	ListIssueCommentReactions(ctx context.Context, owner, repo string, commentID int64) ([]github.CommentReaction, error)
	CreateIssueCommentReaction(ctx context.Context, owner, repo string, commentID int64, content string) error
}

// muteAcknowledgement is the reaction left on the summary once the author's
// thumbs-down has been seen.
const muteAcknowledgement = "eyes"

func (p *Poller) muteEnabled() bool {
	return p.cfg == nil || !p.cfg.PublishNoPRMute
}

func (p *Poller) muteLedger() (muteLedger, bool) {
	if !p.muteEnabled() {
		return nil, false
	}
	ledger, ok := p.db.(muteLedger)
	return ledger, ok
}

// prMuted is the one check every posting path makes before it reaches the
// PR: the ledger flag, or an author thumbs-down on the summary comment seen
// now, which is recorded and acknowledged with a reaction. A reactions read
// that fails leaves the PR unmuted; the next check looks again.
func (p *Poller) prMuted(ctx context.Context, owner, repo string, number int, authorID int64) (bool, error) {
	ledger, ok := p.muteLedger()
	if !ok {
		return false, nil
	}
	summary, found, err := ledger.GetPublishedSummaryForPR(owner, repo, number)
	if err != nil || !found {
		return false, err
	}
	if summary.Muted {
		return true, nil
	}
	var gh summaryReactionsGitHub
	if p.summaryGH != nil {
		gh = p.summaryGH
	} else if p.ghClientConcrete != nil {
		gh = p.ghClientConcrete
	}
	if gh == nil || summary.CommentID == 0 || authorID == 0 {
		return false, nil
	}
	reactions, err := gh.ListIssueCommentReactions(ctx, owner, repo, summary.CommentID)
	if err != nil {
		log.Printf("[PUBLISH] %s/%s#%d: summary reactions unreadable, not muted: %v", owner, repo, number, err)
		return false, nil
	}
	if !authorThumbsDown(reactions, authorID, summary.MuteLiftedAt) {
		return false, nil
	}
	changed, err := ledger.SetPublishedMute(owner, repo, number, true)
	if err != nil {
		log.Printf("[PUBLISH] %s/%s#%d: could not record the mute: %v", owner, repo, number, err)
		return false, nil
	}
	if !changed {
		return true, nil
	}
	p.recordHygieneEvent(github.PullRequest{Owner: owner, Repo: repo, Number: number}, health.ActionPRMuted, "via=thumbs_down")
	if err := gh.CreateIssueCommentReaction(ctx, owner, repo, summary.CommentID, muteAcknowledgement); err != nil {
		log.Printf("[PUBLISH] %s/%s#%d: could not acknowledge the mute: %v", owner, repo, number, err)
	}
	log.Printf("[PUBLISH] %s/%s#%d: muted by the author's thumbs-down on the summary", owner, repo, number)
	return true, nil
}

func authorThumbsDown(reactions []github.CommentReaction, authorID int64, liftedAt *time.Time) bool {
	for _, r := range reactions {
		if r.UserID != authorID || r.Content != "-1" {
			continue
		}
		if liftedAt != nil && !r.CreatedAt.After(*liftedAt) {
			continue
		}
		return true
	}
	return false
}

// prMuter is the mention scanner's handle on the mute flag.
type prMuter struct{ p *Poller }

func (m prMuter) Mute(owner, repo string, number int) (bool, error) {
	ledger, ok := m.p.muteLedger()
	if !ok {
		return false, nil
	}
	return ledger.SetPublishedMute(owner, repo, number, true)
}

func (m prMuter) Muted(owner, repo string, number int) (bool, error) {
	ledger, ok := m.p.muteLedger()
	if !ok {
		return false, nil
	}
	row, found, err := ledger.GetPublishedSummaryForPR(owner, repo, number)
	if err != nil || !found {
		return false, err
	}
	return row.Muted, nil
}

func (m prMuter) Unmute(owner, repo string, number int) (bool, error) {
	ledger, ok := m.p.muteLedger()
	if !ok {
		return false, nil
	}
	return ledger.SetPublishedMute(owner, repo, number, false)
}
