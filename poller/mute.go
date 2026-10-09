package poller

import (
	"context"
	"log"

	"pr-review-server/db"
	"pr-review-server/github"
	"pr-review-server/pkg/health"
)

// A PR author mutes PRism on one PR with "@<handle> stop" in the
// conversation or a thumbs-down on the sticky summary. The mute is a flag on
// the summary ledger row: no round, inline comment, reply or reaction reaches
// the PR while it holds, and "@<handle> review" from the author clears it.
// PUBLISH_PR_MUTE=false turns the whole mechanism off.

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

func summaryRow(rows []db.PublishedFinding) *db.PublishedFinding {
	for i := range rows {
		if rows[i].Kind == db.PublishedKindSummary {
			return &rows[i]
		}
	}
	return nil
}

// prMuted decides, right before a round posts, whether the PR is muted: by
// the ledger flag, or by an author thumbs-down on the summary comment seen
// now, which is recorded and acknowledged with a reaction. A reactions read
// that fails leaves the PR unmuted; the next round looks again.
func (p *Poller) prMuted(ctx context.Context, pr github.PullRequest, rows []db.PublishedFinding, authorID int64) bool {
	ledger, ok := p.muteLedger()
	if !ok {
		return false
	}
	summary := summaryRow(rows)
	if summary == nil {
		return false
	}
	if summary.Muted {
		return true
	}
	var gh summaryReactionsGitHub
	if p.summaryGH != nil {
		gh = p.summaryGH
	} else if p.ghClientConcrete != nil {
		gh = p.ghClientConcrete
	}
	if gh == nil || summary.CommentID == 0 || authorID == 0 {
		return false
	}
	reactions, err := gh.ListIssueCommentReactions(ctx, pr.Owner, pr.Repo, summary.CommentID)
	if err != nil {
		log.Printf("[PUBLISH] %s/%s#%d: summary reactions unreadable, not muted: %v", pr.Owner, pr.Repo, pr.Number, err)
		return false
	}
	if !authorThumbsDown(reactions, authorID) {
		return false
	}
	if _, err := ledger.SetPublishedMute(pr.Owner, pr.Repo, pr.Number, true); err != nil {
		log.Printf("[PUBLISH] %s/%s#%d: could not record the mute: %v", pr.Owner, pr.Repo, pr.Number, err)
		return false
	}
	p.recordHygieneEvent(pr, health.ActionPRMuted, "via=thumbs_down")
	if err := gh.CreateIssueCommentReaction(ctx, pr.Owner, pr.Repo, summary.CommentID, muteAcknowledgement); err != nil {
		log.Printf("[PUBLISH] %s/%s#%d: could not acknowledge the mute: %v", pr.Owner, pr.Repo, pr.Number, err)
	}
	log.Printf("[PUBLISH] %s/%s#%d: muted by the author's thumbs-down on the summary", pr.Owner, pr.Repo, pr.Number)
	return true
}

func authorThumbsDown(reactions []github.CommentReaction, authorID int64) bool {
	for _, r := range reactions {
		if r.UserID == authorID && r.Content == "-1" {
			return true
		}
	}
	return false
}

// prMutedInLedger is the reply scan's view: the flag alone, no GitHub read.
func (p *Poller) prMutedInLedger(owner, repo string, number int) (bool, error) {
	ledger, ok := p.muteLedger()
	if !ok {
		return false, nil
	}
	row, found, err := ledger.GetPublishedSummaryForPR(owner, repo, number)
	if err != nil || !found {
		return false, err
	}
	return row.Muted, nil
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

func (m prMuter) Unmute(owner, repo string, number int) (bool, error) {
	ledger, ok := m.p.muteLedger()
	if !ok {
		return false, nil
	}
	return ledger.SetPublishedMute(owner, repo, number, false)
}
