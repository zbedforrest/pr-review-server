package poller

import (
	"context"
	"errors"
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

// mentionCommand reports whether a comment addresses the handle and asks it
// for a review: the command word must follow the mention, so talking about a
// review ("why did the review flag X?") or forbidding one ("don't review
// this yet") does not trigger. Quoted lines are ignored so quoting someone's
// request is not one either.
func mentionCommand(body, handle string) bool {
	text := strings.ToLower(mentionQuoteRe.ReplaceAllString(body, ""))
	command := regexp.MustCompile(`(^|[^\w@/.-])@` + regexp.QuoteMeta(strings.ToLower(handle)) + `[\s,:]+(please\s+|can you\s+|could you\s+)?(re-?review|review)\b`)
	return command.MatchString(text)
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
	}
	since, err := p.mentionActivation()
	if err != nil {
		log.Printf("[MENTIONS] activation timestamp: %v", err)
		return
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
	triggered, deferred, errors := 0, 0, 0
	for _, pr := range candidates {
		key := mentionKey(pr.RepoOwner, pr.RepoName, pr.PRNumber)
		res, err := p.handleMentionsOnPR(ctx, ledger, pr, enabled, since)
		if err != nil {
			errors++
			log.Printf("[MENTIONS] %s: %v", key, err)
			continue
		}
		triggered += res.triggered
		deferred += res.deferred
		// A request deferred behind an active review is retried next cycle,
		// so the PR stays a candidate.
		if res.deferred == 0 && pr.GitHubUpdatedAt != nil {
			p.mentionLastScanned[key] = *pr.GitHubUpdatedAt
		}
	}
	if len(candidates) > 0 {
		log.Printf("[MENTIONS] cycle=%d full=%t handle=%s checked=%d triggered=%d deferred=%d errors=%d", cycle, full, p.cfg.MentionHandle, len(candidates), triggered, deferred, errors)
	}
}

type mentionLedger interface {
	MentionHandled(commentID int64) (bool, error)
	RecordMention(*db.MentionTrigger) error
}

const settingMentionEnabledAt = "mention_enabled_at"

// mentionActivation is the durable cutoff: commands older than it are never
// acted on, so enabling the feature does not answer every old mention, and a
// restart does not lose a command posted while the service was down.
func (p *Poller) mentionActivation() (time.Time, error) {
	raw, err := p.db.GetSetting(settingMentionEnabledAt)
	if err != nil {
		return time.Time{}, err
	}
	if strings.TrimSpace(raw) != "" {
		return time.Parse(time.RFC3339, strings.TrimSpace(raw))
	}
	now := time.Now().UTC().Truncate(time.Second)
	if err := p.db.SetSetting(settingMentionEnabledAt, now.Format(time.RFC3339)); err != nil {
		return time.Time{}, err
	}
	log.Printf("[MENTIONS] activated at %s; earlier mentions are ignored", now.Format(time.RFC3339))
	return now, nil
}

type mentionResult struct {
	triggered int
	deferred  int // requests waiting for an active review to finish
}

// handleMentionsOnPR acts on new review requests in one PR's conversation.
// The review is admitted first; only then is the request acknowledged and
// recorded, so a request that cannot be admitted right now (a review is
// already running) is retried rather than silently marked handled.
func (p *Poller) handleMentionsOnPR(ctx context.Context, ledger mentionLedger, pr *db.PR, enabledAuthors string, since time.Time) (mentionResult, error) {
	var res mentionResult
	comments, err := p.ghClientConcrete.ListIssueComments(ctx, pr.RepoOwner, pr.RepoName, pr.PRNumber)
	if err != nil {
		return res, err
	}
	key := mentionKey(pr.RepoOwner, pr.RepoName, pr.PRNumber)
	for _, c := range comments {
		if c.IsBot || c.CreatedAt.Before(since) || !mentionCommand(c.Body, p.cfg.MentionHandle) {
			continue
		}
		handled, err := ledger.MentionHandled(c.ID)
		if err != nil {
			return res, err
		}
		if handled {
			continue
		}
		// The cached row may trail a push by a poll cycle; review what the
		// requester is looking at.
		live, _, err := p.ghClientConcrete.GetPR(ctx, pr.RepoOwner, pr.RepoName, pr.PRNumber)
		if err != nil {
			return res, err
		}
		ghPR := github.PullRequest{Owner: pr.RepoOwner, Repo: pr.RepoName, Number: pr.PRNumber, CommitSHA: live.GetHead().GetSHA(),
			Title: live.GetTitle(), Author: live.GetUser().GetLogin(), CreatedAt: pr.CreatedAt, Draft: live.GetDraft()}
		publish := publishEnabledFor(ghPR.Author, enabledAuthors) && !ghPR.Draft
		job, err := p.defaultReviewJob(ghPR, true, "mention")
		if err == nil {
			job.SkipPublish = !publish
			err = p.ProcessReviewJob(ctx, job)
		}
		if errors.Is(err, ErrReviewAlreadyTracked) {
			res.deferred++
			continue
		}
		if err != nil {
			// Admission failed for a reason a retry will not fix; record it so
			// the same comment is not retried every cycle, and say so.
			log.Printf("[MENTIONS] %s: could not queue the review requested in comment %d: %v", key, c.ID, err)
			_ = ledger.RecordMention(&db.MentionTrigger{CommentID: c.ID, RepoOwner: pr.RepoOwner, RepoName: pr.RepoName, PRNumber: pr.PRNumber,
				Author: c.Author, CommitSHA: ghPR.CommitSHA, Publish: false, CreatedAt: c.CreatedAt, TriggeredAt: time.Now().UTC()})
			continue
		}
		res.triggered++
		if err := ledger.RecordMention(&db.MentionTrigger{
			CommentID: c.ID, RepoOwner: pr.RepoOwner, RepoName: pr.RepoName, PRNumber: pr.PRNumber,
			Author: c.Author, CommitSHA: ghPR.CommitSHA, Publish: publish, CreatedAt: c.CreatedAt, TriggeredAt: time.Now().UTC(),
		}); err != nil {
			return res, err
		}
		if err := p.ghClientConcrete.CreateIssueCommentReaction(ctx, pr.RepoOwner, pr.RepoName, c.ID, "eyes"); err != nil {
			log.Printf("[MENTIONS] %s: could not acknowledge comment %d: %v", key, c.ID, err)
		}
		if note := mentionPublishNote(publish, ghPR.Draft, ghPR.CommitSHA); note != "" {
			if _, err := p.ghClientConcrete.CreateIssueComment(ctx, pr.RepoOwner, pr.RepoName, pr.PRNumber, note); err != nil {
				log.Printf("[MENTIONS] %s: could not leave the dashboard-only note: %v", key, err)
			}
		}
		log.Printf("[MENTIONS] %s: review of %s requested by %s in comment %d (publish=%t)", key, ghPR.CommitSHA[:min(7, len(ghPR.CommitSHA))], c.Author, c.ID, publish)
		if userID := p.systemTelemetryUserID(); userID != 0 {
			if terr := p.db.CreateTelemetryEvents([]db.TelemetryEvent{mentionTelemetryEvent(ghPR, c.Author, publish, userID)}); terr != nil {
				log.Printf("[MENTIONS] WARN: could not record telemetry: %v", terr)
			}
		}
	}
	return res, nil
}
