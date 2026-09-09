package poller

import (
	"context"
	"fmt"
	"log"
	"regexp"
	"strings"
	"time"

	"pr-review-server/db"
	"pr-review-server/github"
)

// Authors ask for a review by mentioning the App in a PR comment, the way
// they would with any review bot: "@<handle> review". The poller already
// tracks every open PR's updated_at, and a new comment bumps it, so each cycle
// lists issue comments only on PRs that moved and looks for the command.
// Handled comment ids are recorded so a restart or a re-scan never triggers
// twice.
const mentionFullScanEvery = 10

func mentionKey(owner, repo string, number int) string {
	return fmt.Sprintf("%s/%s#%d", owner, repo, number)
}

var mentionQuoteRe = regexp.MustCompile(`(?m)^[ \t]*>.*$`)

// mentionCommand reports whether a comment addresses the handle and asks for
// a review. Quoted lines are ignored so quoting someone's request is not one.
func mentionCommand(body, handle string) bool {
	text := strings.ToLower(mentionQuoteRe.ReplaceAllString(body, ""))
	mention := regexp.MustCompile(`(^|[^\w@/.-])@` + regexp.QuoteMeta(strings.ToLower(handle)) + `\b([^\w-]|$)`)
	if !mention.MatchString(text) {
		return false
	}
	// The handle may itself contain "review"; only the rest of the comment
	// carries the command.
	rest := mention.ReplaceAllString(text, " ")
	return regexp.MustCompile(`\bre-?review\b|\breview\b`).MatchString(rest)
}

// mentionCandidates picks the open PRs whose cached updated_at moved since the
// last scan (every open PR on a full scan).
func mentionCandidates(prs []*db.PR, lastScanned map[string]time.Time, full bool) []*db.PR {
	var out []*db.PR
	for _, pr := range prs {
		if !strings.EqualFold(pr.PRState, "open") {
			continue
		}
		if full || pr.GitHubUpdatedAt == nil || pr.GitHubUpdatedAt.After(lastScanned[mentionKey(pr.RepoOwner, pr.RepoName, pr.PRNumber)]) {
			out = append(out, pr)
		}
	}
	return out
}

// mentionPublishNote is the short reply left when a requested review cannot
// be posted to the PR, so the requester knows where to look and why.
func mentionPublishNote(publishable, draft bool, headSHA string) string {
	if publishable {
		return ""
	}
	if len(headSHA) > 7 {
		headSHA = headSHA[:7]
	}
	reason := "the author is not in the comment pilot"
	if draft {
		reason = "the pull request is a draft"
	}
	return fmt.Sprintf("Reviewing %s. The result will be on the PRism dashboard only, because %s.", headSHA, reason)
}

func mentionTelemetryEvent(pr github.PullRequest, by string, publish bool, userID int) db.TelemetryEvent {
	return db.TelemetryEvent{UserID: userID, Action: "mention_trigger", Label: fmt.Sprintf("by=%s publish=%t", by, publish),
		PROwner: pr.Owner, PRRepo: pr.Repo, PRNumber: pr.Number}
}

// scanMentions runs once per poll cycle on the leader.
func (p *Poller) scanMentions(ctx context.Context) {
	if p.cfg.MentionHandle == "" || !p.mentionScanRunning.CompareAndSwap(false, true) {
		return
	}
	defer p.mentionScanRunning.Store(false)
	ledger, ok := p.db.(mentionLedger)
	if !ok || p.ghClientConcrete == nil {
		return
	}
	if p.mentionLastScanned == nil {
		p.mentionLastScanned = map[string]time.Time{}
		p.mentionActivatedAt = time.Now().UTC()
	}
	cycle := p.mentionScanCycle.Add(1)
	full := cycle%mentionFullScanEvery == 1
	all, err := p.db.GetAllPRs()
	if err != nil {
		log.Printf("[MENTIONS] list PRs: %v", err)
		return
	}
	prs := make([]*db.PR, 0, len(all))
	for i := range all {
		prs = append(prs, &all[i])
	}
	candidates := mentionCandidates(prs, p.mentionLastScanned, full)
	enabled, _ := p.db.GetSetting(settingPublishEnabledAuthors)
	triggered, errors := 0, 0
	for _, pr := range candidates {
		key := mentionKey(pr.RepoOwner, pr.RepoName, pr.PRNumber)
		n, err := p.handleMentionsOnPR(ctx, ledger, pr, enabled)
		if err != nil {
			errors++
			log.Printf("[MENTIONS] %s: %v", key, err)
			continue
		}
		triggered += n
		if pr.GitHubUpdatedAt != nil {
			p.mentionLastScanned[key] = *pr.GitHubUpdatedAt
		}
	}
	if len(candidates) > 0 {
		log.Printf("[MENTIONS] cycle=%d full=%t handle=%s checked=%d triggered=%d errors=%d", cycle, full, p.cfg.MentionHandle, len(candidates), triggered, errors)
	}
}

type mentionLedger interface {
	MentionHandled(commentID int64) (bool, error)
	RecordMention(*db.MentionTrigger) error
}

func (p *Poller) handleMentionsOnPR(ctx context.Context, ledger mentionLedger, pr *db.PR, enabledAuthors string) (int, error) {
	comments, err := p.ghClientConcrete.ListIssueComments(ctx, pr.RepoOwner, pr.RepoName, pr.PRNumber)
	if err != nil {
		return 0, err
	}
	triggered := 0
	for _, c := range comments {
		if c.IsBot || c.CreatedAt.Before(p.mentionActivatedAt) || !mentionCommand(c.Body, p.cfg.MentionHandle) {
			continue
		}
		handled, err := ledger.MentionHandled(c.ID)
		if err != nil {
			return triggered, err
		}
		if handled {
			continue
		}
		// Acknowledge before the review is queued, so the requester sees the
		// request landed even if the queue is deep.
		if err := p.ghClientConcrete.CreateIssueCommentReaction(ctx, pr.RepoOwner, pr.RepoName, c.ID, "eyes"); err != nil {
			return triggered, err
		}
		publish := publishEnabledFor(pr.Author, enabledAuthors) && !pr.Draft
		if err := ledger.RecordMention(&db.MentionTrigger{
			CommentID: c.ID, RepoOwner: pr.RepoOwner, RepoName: pr.RepoName, PRNumber: pr.PRNumber,
			Author: c.Author, CommitSHA: pr.LastCommitSHA, Publish: publish, CreatedAt: c.CreatedAt, TriggeredAt: time.Now().UTC(),
		}); err != nil {
			return triggered, err
		}
		if note := mentionPublishNote(publish, pr.Draft, pr.LastCommitSHA); note != "" {
			if _, err := p.ghClientConcrete.CreateIssueComment(ctx, pr.RepoOwner, pr.RepoName, pr.PRNumber, note); err != nil {
				log.Printf("[MENTIONS] %s: could not leave the dashboard-only note: %v", mentionKey(pr.RepoOwner, pr.RepoName, pr.PRNumber), err)
			}
		}
		ghPR := github.PullRequest{Owner: pr.RepoOwner, Repo: pr.RepoName, Number: pr.PRNumber, CommitSHA: pr.LastCommitSHA,
			Title: pr.Title, Author: pr.Author, CreatedAt: pr.CreatedAt, Draft: pr.Draft}
		job, err := p.defaultReviewJob(ghPR, true, "mention")
		if err == nil {
			job.SkipPublish = !publish
			err = p.ProcessReviewJob(ctx, job)
		}
		if err != nil {
			log.Printf("[MENTIONS] %s: queue review: %v", mentionKey(pr.RepoOwner, pr.RepoName, pr.PRNumber), err)
		} else {
			triggered++
		}
		log.Printf("[MENTIONS] %s: review requested by %s in comment %d (publish=%t)", mentionKey(pr.RepoOwner, pr.RepoName, pr.PRNumber), c.Author, c.ID, publish)
		if userID := p.systemTelemetryUserID(); userID != 0 {
			if terr := p.db.CreateTelemetryEvents([]db.TelemetryEvent{mentionTelemetryEvent(ghPR, c.Author, publish, userID)}); terr != nil {
				log.Printf("[MENTIONS] WARN: could not record telemetry: %v", terr)
			}
		}
	}
	return triggered, nil
}
