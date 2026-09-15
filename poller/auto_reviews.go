package poller

import (
	"context"
	"errors"
	"fmt"
	"log"
	"strconv"
	"strings"

	"pr-review-server/db"
	"pr-review-server/github"
	"pr-review-server/pkg/reviewer/runconfig"
)

// Automatic reviews for allowlisted authors: a GitHub webhook (or the poll
// fallback) records an intent per PR head, and admission runs a forced,
// published review for it. The intent table is the dedup key shared by both
// paths; the review-run ledger stays the source of truth for execution.
const (
	settingAutoReviewReadyPRs = "auto_review_ready_prs"
	autoReviewTriggerSource   = "auto_ready"
)

var autoReviewSatisfiedStatuses = []string{db.AutoReviewIntentQueued, db.AutoReviewIntentRunning, db.AutoReviewIntentDone}

// publishAllowedFor reports whether the author is on the publication
// allowlist. A read error denies, like the publish gate always has.
func (p *Poller) publishAllowedFor(author string) (bool, error) {
	enabled, err := p.db.GetSetting(settingPublishEnabledAuthors)
	if err != nil {
		return false, err
	}
	return publishEnabledFor(author, enabled), nil
}

// autoReviewReadyEnabled reads the feature switch; a missing value is off.
func (p *Poller) autoReviewReadyEnabled() (bool, error) {
	raw, err := p.db.GetSetting(settingAutoReviewReadyPRs)
	if err != nil {
		return false, err
	}
	enabled, parseErr := strconv.ParseBool(strings.TrimSpace(raw))
	return parseErr == nil && enabled, nil
}

// autoReviewEligible is the one author policy for automatic reviews: the
// switch must be on and the PR author must be publish-enabled.
func (p *Poller) autoReviewEligible(author string) (bool, error) {
	enabled, err := p.autoReviewReadyEnabled()
	if err != nil || !enabled {
		return false, err
	}
	return p.publishAllowedFor(author)
}

func autoReviewKey(owner, repo string, number int, head string) string {
	return fmt.Sprintf("%s/%s#%d@%s", owner, repo, number, head[:min(7, len(head))])
}

// HandleWebhookDelivery turns one accepted pull_request delivery into intent
// state. Ready, opened-ready and pushed heads queue a review; draft
// conversion and closing retire queued work for the PR. It is safe to call
// from a goroutine after the HTTP response is written.
func (p *Poller) HandleWebhookDelivery(ctx context.Context, d db.WebhookDelivery) {
	key := autoReviewKey(d.RepoOwner, d.RepoName, d.PRNumber, d.HeadSHA)
	switch d.Action {
	case "converted_to_draft", "closed":
		n, err := p.db.SupersedeQueuedAutoReviewIntents(d.RepoOwner, d.RepoName, d.PRNumber, "")
		if err != nil {
			log.Printf("[AUTO-REVIEW] delivery=%s %s: supersede on %s failed: %v", d.DeliveryID, key, d.Action, err)
			return
		}
		log.Printf("[AUTO-REVIEW] delivery=%s %s: %s superseded %d queued intent(s)", d.DeliveryID, key, d.Action, n)
		return
	case "ready_for_review", "opened", "synchronize":
	default:
		log.Printf("[AUTO-REVIEW] delivery=%s %s: ignoring action %q", d.DeliveryID, key, d.Action)
		return
	}
	if d.Draft || !strings.EqualFold(d.State, "open") {
		log.Printf("[AUTO-REVIEW] delivery=%s %s: %s on a %s draft=%t PR, nothing to review", d.DeliveryID, key, d.Action, d.State, d.Draft)
		return
	}
	if d.HeadSHA == "" {
		log.Printf("[AUTO-REVIEW] delivery=%s %s: %s without a head sha, ignoring", d.DeliveryID, key, d.Action)
		return
	}
	eligible, err := p.autoReviewEligible(d.Author)
	if err != nil {
		log.Printf("[AUTO-REVIEW] delivery=%s %s: policy read failed: %v", d.DeliveryID, key, err)
		return
	}
	if !eligible {
		log.Printf("[AUTO-REVIEW] delivery=%s %s: author %s is not enabled for automatic reviews", d.DeliveryID, key, d.Author)
		return
	}
	if n, err := p.db.SupersedeQueuedAutoReviewIntents(d.RepoOwner, d.RepoName, d.PRNumber, d.HeadSHA); err != nil {
		log.Printf("[AUTO-REVIEW] delivery=%s %s: supersede older heads failed: %v", d.DeliveryID, key, err)
	} else if n > 0 {
		log.Printf("[AUTO-REVIEW] delivery=%s %s: superseded %d queued intent(s) for older heads", d.DeliveryID, key, n)
	}
	intent := db.AutoReviewIntent{
		RepoOwner: d.RepoOwner, RepoName: d.RepoName, PRNumber: d.PRNumber, HeadSHA: d.HeadSHA,
		Trigger: d.Action, DeliveryID: d.DeliveryID,
	}
	// A head whose intent was retired (converted to draft, or its run failed)
	// is reviewed again when the author makes it ready once more.
	created, err := p.db.EnsureAutoReviewIntent(&intent, []string{db.AutoReviewIntentSuperseded, db.AutoReviewIntentFailed})
	if err != nil {
		log.Printf("[AUTO-REVIEW] delivery=%s %s: record intent failed: %v", d.DeliveryID, key, err)
		return
	}
	if !created && intent.Status != db.AutoReviewIntentQueued {
		log.Printf("[AUTO-REVIEW] delivery=%s %s: intent=%d already %s (run=%s), nothing to do", d.DeliveryID, key, intent.ID, intent.Status, intent.RunID)
		return
	}
	log.Printf("[AUTO-REVIEW] delivery=%s %s: intent=%d queued by %s", d.DeliveryID, key, intent.ID, d.Action)
	pr := github.PullRequest{
		Owner: d.RepoOwner, Repo: d.RepoName, Number: d.PRNumber, CommitSHA: d.HeadSHA,
		Title: d.Title, Author: d.Author, Draft: false,
	}
	if existing, getErr := p.db.GetPR(d.RepoOwner, d.RepoName, d.PRNumber); getErr == nil && existing != nil {
		pr.CreatedAt = existing.CreatedAt
		if pr.Title == "" {
			pr.Title = existing.Title
		}
	}
	p.admitAutoReviewIntent(ctx, intent, pr)
}

// admitAutoReviewIntent runs the standard admission for one queued intent:
// force=true so a review cached while the PR was a draft cannot satisfy it,
// SkipPublish=false so the result reaches GitHub. A PR already under review
// leaves the intent queued for the next poll to retry.
func (p *Poller) admitAutoReviewIntent(ctx context.Context, intent db.AutoReviewIntent, pr github.PullRequest) {
	key := autoReviewKey(pr.Owner, pr.Repo, pr.Number, pr.CommitSHA)
	p.autoReviewAdmitMutex.Lock()
	defer p.autoReviewAdmitMutex.Unlock()
	job, err := p.PrepareReviewJob(pr, runconfig.Overrides{}, true, autoReviewTriggerSource, nil)
	if err != nil {
		log.Printf("[AUTO-REVIEW] %s intent=%d: cannot prepare review: %v", key, intent.ID, err)
		p.failAutoReviewIntent(intent)
		return
	}
	job.SkipPublish = false
	if err := p.ProcessReviewJob(ctx, job); err != nil {
		if errors.Is(err, ErrReviewAlreadyTracked) {
			log.Printf("[AUTO-REVIEW] %s intent=%d: a review is already active, left queued for the next poll", key, intent.ID)
			return
		}
		log.Printf("[AUTO-REVIEW] %s intent=%d: admission failed: %v", key, intent.ID, err)
		p.failAutoReviewIntent(intent)
		return
	}
	moved, err := p.db.UpdateAutoReviewIntentStatus(intent.ID, []string{db.AutoReviewIntentQueued}, db.AutoReviewIntentRunning, job.RunID)
	switch {
	case err != nil:
		log.Printf("[AUTO-REVIEW] %s intent=%d run=%s: could not mark running: %v", key, intent.ID, job.RunID, err)
	case !moved:
		log.Printf("[AUTO-REVIEW] %s intent=%d run=%s: admitted but the intent was no longer queued", key, intent.ID, job.RunID)
	default:
		log.Printf("[AUTO-REVIEW] %s intent=%d run=%s: review admitted (trigger=%s delivery=%s)", key, intent.ID, job.RunID, intent.Trigger, intent.DeliveryID)
	}
}

func (p *Poller) failAutoReviewIntent(intent db.AutoReviewIntent) {
	if _, err := p.db.UpdateAutoReviewIntentStatus(intent.ID, []string{db.AutoReviewIntentQueued}, db.AutoReviewIntentFailed, ""); err != nil {
		log.Printf("[AUTO-REVIEW] intent=%d: could not mark failed: %v", intent.ID, err)
	}
}

// ensureFallbackAutoReviewIntent is the poll's safety net for a ready,
// allowlisted PR: queued intents for older heads are superseded and the
// current head gets a poll_fallback intent unless one already covers it. A
// head whose intent was superseded (a draft round trip the webhook missed) is
// queued again; a failed head is not retried until a new push.
func (p *Poller) ensureFallbackAutoReviewIntent(owner, repo string, number int, head string) {
	key := autoReviewKey(owner, repo, number, head)
	if n, err := p.db.SupersedeQueuedAutoReviewIntents(owner, repo, number, head); err != nil {
		log.Printf("[AUTO-REVIEW] %s: supersede older heads failed: %v", key, err)
		return
	} else if n > 0 {
		log.Printf("[AUTO-REVIEW] %s: poll superseded %d queued intent(s) for older heads", key, n)
	}
	intent := db.AutoReviewIntent{RepoOwner: owner, RepoName: repo, PRNumber: number, HeadSHA: head, Trigger: db.AutoReviewTriggerPollFallback}
	created, err := p.db.EnsureAutoReviewIntent(&intent, []string{db.AutoReviewIntentSuperseded})
	if err != nil {
		log.Printf("[AUTO-REVIEW] %s: record fallback intent failed: %v", key, err)
		return
	}
	if created {
		log.Printf("[AUTO-REVIEW] %s: intent=%d queued by poll fallback", key, intent.ID)
	}
}

// dispatchAutoReviewIntents runs after the poll refreshed PR state: running
// intents are settled against their runs, and queued intents whose PR is
// still open, ready, allowlisted and on the same head are admitted. Anything
// else queued is superseded so the backlog reflects real work.
func (p *Poller) dispatchAutoReviewIntents(ctx context.Context, states map[string]*github.PRState) {
	p.settleAutoReviewIntents()
	queued, err := p.db.ListAutoReviewIntents(db.AutoReviewIntentFilter{Statuses: []string{db.AutoReviewIntentQueued}})
	if err != nil {
		log.Printf("[AUTO-REVIEW] list queued intents: %v", err)
		return
	}
	for _, intent := range queued {
		key := autoReviewKey(intent.RepoOwner, intent.RepoName, intent.PRNumber, intent.HeadSHA)
		state := states[fmt.Sprintf("%s/%s/%d", intent.RepoOwner, intent.RepoName, intent.PRNumber)]
		pr, err := p.db.GetPR(intent.RepoOwner, intent.RepoName, intent.PRNumber)
		if err != nil {
			log.Printf("[AUTO-REVIEW] %s intent=%d: load PR: %v", key, intent.ID, err)
			continue
		}
		reason := ""
		switch {
		case pr == nil || state == nil:
			reason = "the PR is no longer tracked"
		case state.State != "OPEN":
			reason = "the PR is " + strings.ToLower(state.State)
		case state.IsDraft:
			reason = "the PR is a draft"
		case !strings.EqualFold(state.HeadRefOid, intent.HeadSHA):
			reason = "the head moved to " + state.HeadRefOid[:min(7, len(state.HeadRefOid))]
		}
		if reason == "" {
			eligible, err := p.autoReviewEligible(pr.Author)
			if err != nil {
				log.Printf("[AUTO-REVIEW] %s intent=%d: policy read failed: %v", key, intent.ID, err)
				continue
			}
			if !eligible {
				reason = "author " + pr.Author + " is no longer enabled"
			}
		}
		if reason != "" {
			if _, err := p.db.UpdateAutoReviewIntentStatus(intent.ID, []string{db.AutoReviewIntentQueued}, db.AutoReviewIntentSuperseded, ""); err != nil {
				log.Printf("[AUTO-REVIEW] %s intent=%d: could not supersede: %v", key, intent.ID, err)
			} else {
				log.Printf("[AUTO-REVIEW] %s intent=%d: superseded, %s", key, intent.ID, reason)
			}
			continue
		}
		target := buildPRFromDB(*pr)
		target.CommitSHA = intent.HeadSHA
		target.Draft = false
		p.admitAutoReviewIntent(ctx, intent, target)
	}
}

// settleAutoReviewIntents moves running intents to their run's outcome.
func (p *Poller) settleAutoReviewIntents() {
	running, err := p.db.ListAutoReviewIntents(db.AutoReviewIntentFilter{Statuses: []string{db.AutoReviewIntentRunning}})
	if err != nil {
		log.Printf("[AUTO-REVIEW] list running intents: %v", err)
		return
	}
	for _, intent := range running {
		key := autoReviewKey(intent.RepoOwner, intent.RepoName, intent.PRNumber, intent.HeadSHA)
		run, err := p.db.GetReviewRun(intent.RunID)
		if err != nil {
			log.Printf("[AUTO-REVIEW] %s intent=%d run=%s: load run: %v", key, intent.ID, intent.RunID, err)
			continue
		}
		outcome := ""
		switch {
		case run == nil:
			outcome = db.AutoReviewIntentFailed
		case run.Status == db.ReviewRunStatusCompleted:
			outcome = db.AutoReviewIntentDone
		case run.Status == db.ReviewRunStatusCancelled:
			outcome = db.AutoReviewIntentSuperseded
		case run.Status == db.ReviewRunStatusFailed || run.Status == db.ReviewRunStatusTimedOut:
			outcome = db.AutoReviewIntentFailed
		}
		if outcome == "" {
			continue
		}
		if _, err := p.db.UpdateAutoReviewIntentStatus(intent.ID, []string{db.AutoReviewIntentRunning}, outcome, ""); err != nil {
			log.Printf("[AUTO-REVIEW] %s intent=%d run=%s: could not settle: %v", key, intent.ID, intent.RunID, err)
			continue
		}
		log.Printf("[AUTO-REVIEW] %s intent=%d run=%s: %s", key, intent.ID, intent.RunID, outcome)
	}
}
