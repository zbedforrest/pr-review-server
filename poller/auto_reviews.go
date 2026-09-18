package poller

import (
	"context"
	"errors"
	"fmt"
	"log"
	"strconv"
	"strings"
	"time"

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
	// autoReviewPublicationGrace is how long after a run completes its
	// publication outcome may still be unrecorded before settlement reconciles
	// the intent against the publication ledger instead.
	autoReviewPublicationGrace = 10 * time.Minute
	// webhookDeliveryRetention keeps the dedup key well past GitHub's
	// redelivery window; done and superseded intents share it.
	webhookDeliveryRetention = 30 * 24 * time.Hour
	// autoReviewClaimGrace is how long a running intent may exist without its
	// run row: admission claims the intent first, then creates the run.
	autoReviewClaimGrace = 5 * time.Minute
)

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

// autoReviewTargetKey is the lowercased PR identity intents are indexed by.
func autoReviewTargetKey(owner, repo string, number int) string {
	return strings.ToLower(fmt.Sprintf("%s/%s/%d", owner, repo, number))
}

// autoReviewIntentIndex loads every intent that still says something about
// a head (all but superseded), keyed by PR, so the poll fallback can decide
// per PR without a query.
func (p *Poller) autoReviewIntentIndex() (map[string][]db.AutoReviewIntent, error) {
	intents, err := p.db.ListAutoReviewIntents(db.AutoReviewIntentFilter{Statuses: []string{
		db.AutoReviewIntentQueued, db.AutoReviewIntentRunning, db.AutoReviewIntentDone, db.AutoReviewIntentFailed,
	}})
	if err != nil {
		return nil, err
	}
	index := make(map[string][]db.AutoReviewIntent, len(intents))
	for _, intent := range intents {
		key := autoReviewTargetKey(intent.RepoOwner, intent.RepoName, intent.PRNumber)
		index[key] = append(index[key], intent)
	}
	return index, nil
}

func (p *Poller) pruneWebhookDeliveries() {
	cutoff := time.Now().Add(-webhookDeliveryRetention)
	n, err := p.db.DeleteWebhookDeliveriesBefore(cutoff)
	if err != nil {
		log.Printf("[WEBHOOK] prune deliveries: %v", err)
	} else if n > 0 {
		log.Printf("[WEBHOOK] pruned %d deliveries older than %s", n, webhookDeliveryRetention)
	}
	n, err = p.db.DeleteTerminalAutoReviewIntentsBefore(cutoff)
	if err != nil {
		log.Printf("[AUTO-REVIEW] prune terminal intents: %v", err)
	} else if n > 0 {
		log.Printf("[AUTO-REVIEW] pruned %d terminal intents older than %s", n, webhookDeliveryRetention)
	}
}

// HandleWebhookDelivery turns one accepted pull_request delivery into intent
// state before the delivery is acknowledged. Ready, opened-ready and pushed
// heads record an intent and admit its review; draft conversion and closing
// retire queued work for the PR. It returns an error only when the intent
// could not be recorded, so the caller can refuse the delivery and let a
// redelivery try again.
func (p *Poller) HandleWebhookDelivery(ctx context.Context, d db.WebhookDelivery) error {
	key := autoReviewKey(d.RepoOwner, d.RepoName, d.PRNumber, d.HeadSHA)
	switch d.Action {
	case "converted_to_draft", "closed":
		n, err := p.db.SupersedeQueuedAutoReviewIntents(d.RepoOwner, d.RepoName, d.PRNumber, "")
		if err != nil {
			return fmt.Errorf("delivery %s %s: supersede on %s: %w", d.DeliveryID, key, d.Action, err)
		}
		log.Printf("[AUTO-REVIEW] delivery=%s %s: %s superseded %d queued intent(s)", d.DeliveryID, key, d.Action, n)
		return nil
	case "ready_for_review", "opened", "synchronize":
	default:
		log.Printf("[AUTO-REVIEW] delivery=%s %s: ignoring action %q", d.DeliveryID, key, d.Action)
		return nil
	}
	if d.Draft || !strings.EqualFold(d.State, "open") {
		log.Printf("[AUTO-REVIEW] delivery=%s %s: %s on a %s draft=%t PR, nothing to review", d.DeliveryID, key, d.Action, d.State, d.Draft)
		return nil
	}
	if d.HeadSHA == "" {
		log.Printf("[AUTO-REVIEW] delivery=%s %s: %s without a head sha, ignoring", d.DeliveryID, key, d.Action)
		return nil
	}
	eligible, err := p.autoReviewEligible(d.Author)
	if err != nil {
		return fmt.Errorf("delivery %s %s: read author policy: %w", d.DeliveryID, key, err)
	}
	if !eligible {
		log.Printf("[AUTO-REVIEW] delivery=%s %s: author %s is not enabled for automatic reviews", d.DeliveryID, key, d.Author)
		return nil
	}
	if n, err := p.db.SupersedeQueuedAutoReviewIntents(d.RepoOwner, d.RepoName, d.PRNumber, d.HeadSHA); err != nil {
		return fmt.Errorf("delivery %s %s: supersede older heads: %w", d.DeliveryID, key, err)
	} else if n > 0 {
		log.Printf("[AUTO-REVIEW] delivery=%s %s: superseded %d queued intent(s) for older heads", d.DeliveryID, key, n)
	}
	intent := db.AutoReviewIntent{
		RepoOwner: d.RepoOwner, RepoName: d.RepoName, PRNumber: d.PRNumber, HeadSHA: d.HeadSHA,
		Trigger: d.Action, DeliveryID: d.DeliveryID,
	}
	// A head whose intent was retired (converted to draft, or its run failed)
	// is reviewed again when the author makes it ready once more.
	requeueFrom := []string{db.AutoReviewIntentSuperseded, db.AutoReviewIntentFailed}
	published, err := p.headPublished(d.RepoOwner, d.RepoName, d.PRNumber, d.HeadSHA)
	if err != nil {
		return fmt.Errorf("delivery %s %s: read publication ledger: %w", d.DeliveryID, key, err)
	}
	if published {
		// Another run (manual, the legacy poll path, or a delivery that
		// overtook this one) already commented on this head.
		intent.Status, intent.Publication = db.AutoReviewIntentDone, publicationPosted
		requeueFrom = append(requeueFrom, db.AutoReviewIntentQueued)
	}
	created, err := p.db.EnsureAutoReviewIntent(&intent, requeueFrom)
	if err != nil {
		return fmt.Errorf("delivery %s %s: record intent: %w", d.DeliveryID, key, err)
	}
	if published {
		log.Printf("[AUTO-REVIEW] delivery=%s %s: intent=%d done, the head was already published", d.DeliveryID, key, intent.ID)
		return nil
	}
	if !created && intent.Status != db.AutoReviewIntentQueued {
		log.Printf("[AUTO-REVIEW] delivery=%s %s: intent=%d already %s (run=%s), nothing to do", d.DeliveryID, key, intent.ID, intent.Status, intent.RunID)
		return nil
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
	return nil
}

// admitAutoReviewIntent runs the standard admission for one queued intent:
// force=true so a review cached while the PR was a draft cannot satisfy it,
// SkipPublish=false so the result reaches GitHub. The intent is linked to
// the run before the run is launched, so a crash or a failed write can never
// leave a live review without an owner. Nothing has run yet when admission
// fails, so every failure leaves the intent queued for the next poll: an
// active review on the PR, a transient database error, or deployment review
// defaults an operator still has to fix. It reports whether a run started.
// The resident-queue check shares the admission lock, so concurrent webhook
// deliveries cannot each see the last free place and all take it.
func (p *Poller) admitAutoReviewIntent(ctx context.Context, intent db.AutoReviewIntent, pr github.PullRequest) bool {
	key := autoReviewKey(pr.Owner, pr.Repo, pr.Number, pr.CommitSHA)
	p.autoReviewAdmitMutex.Lock()
	defer p.autoReviewAdmitMutex.Unlock()
	if p.trackedReviewCount() >= p.pollAdmissionLimit() {
		log.Printf("[AUTO-REVIEW] %s intent=%d: left queued, the resident review queue is full", key, intent.ID)
		return false
	}
	profile := p.autoReviewProfileFor(intent.Trigger, pr.Owner, pr.Repo, pr.Author)
	job, err := p.PrepareReviewJob(pr, runconfig.Overrides{Profile: &profile}, true, autoReviewTriggerSource, nil)
	var validationErr *runconfig.ValidationError
	if errors.As(err, &validationErr) && profile != runconfig.ProfileFull {
		// A lite profile the deployment policy rejects would otherwise leave the
		// intent queued and retried forever; the full pipeline still reviews it.
		log.Printf("[AUTO-REVIEW] %s intent=%d: %s profile rejected by policy (%v); reviewing with full", key, intent.ID, profile, err)
		profile = runconfig.ProfileFull
		job, err = p.PrepareReviewJob(pr, runconfig.Overrides{Profile: &profile}, true, autoReviewTriggerSource, nil)
	}
	if err != nil {
		log.Printf("[AUTO-REVIEW] %s intent=%d: cannot prepare %s review, left queued for the next poll: %v", key, intent.ID, profile, err)
		return false
	}
	job.SkipPublish = false
	claimed, err := p.db.UpdateAutoReviewIntentStatus(intent.ID, []string{db.AutoReviewIntentQueued}, db.AutoReviewIntentRunning, job.RunID)
	if err != nil {
		log.Printf("[AUTO-REVIEW] %s intent=%d run=%s: could not claim: %v", key, intent.ID, job.RunID, err)
		return false
	}
	if !claimed {
		log.Printf("[AUTO-REVIEW] %s intent=%d: no longer queued, not admitting", key, intent.ID)
		return false
	}
	if err := p.ProcessReviewJob(ctx, job); err != nil {
		if errors.Is(err, ErrReviewAlreadyTracked) {
			log.Printf("[AUTO-REVIEW] %s intent=%d: a review is already active, left queued for the next poll", key, intent.ID)
		} else {
			log.Printf("[AUTO-REVIEW] %s intent=%d run=%s: admission failed, left queued for the next poll: %v", key, intent.ID, job.RunID, err)
		}
		p.moveAutoReviewIntent(intent, db.AutoReviewIntentRunning, db.AutoReviewIntentQueued, "")
		return false
	}
	log.Printf("[AUTO-REVIEW] %s intent=%d run=%s: %s review admitted (trigger=%s delivery=%s)", key, intent.ID, job.RunID, profile, intent.Trigger, intent.DeliveryID)
	return true
}

func (p *Poller) moveAutoReviewIntent(intent db.AutoReviewIntent, from, to, runID string) {
	moved, err := p.db.UpdateAutoReviewIntentStatus(intent.ID, []string{from}, to, runID)
	if err != nil {
		log.Printf("[AUTO-REVIEW] intent=%d: could not move %s -> %s: %v", intent.ID, from, to, err)
	} else if !moved {
		log.Printf("[AUTO-REVIEW] intent=%d: not moved %s -> %s, the intent was no longer %s", intent.ID, from, to, from)
	}
}

// ensureFallbackAutoReviewIntent is the poll's safety net for a ready,
// allowlisted PR: queued intents for older heads are superseded and the
// current head gets a poll_fallback intent unless one already covers it. A
// head whose review already reached GitHub (by any run) is recorded as done,
// so switching the feature on does not re-review every open PR; a head whose
// intent was superseded (a draft round trip the webhook missed) is queued
// again; a failed head is not retried until a new push. existing is the PR's
// non-superseded intents from this cycle's index.
func (p *Poller) ensureFallbackAutoReviewIntent(pr db.PR, head string, existing []db.AutoReviewIntent) {
	key := autoReviewKey(pr.RepoOwner, pr.RepoName, pr.PRNumber, head)
	var current *db.AutoReviewIntent
	olderQueued := false
	for i := range existing {
		switch {
		case strings.EqualFold(existing[i].HeadSHA, head):
			current = &existing[i]
		case existing[i].Status == db.AutoReviewIntentQueued:
			olderQueued = true
		}
	}
	if olderQueued {
		if n, err := p.db.SupersedeQueuedAutoReviewIntents(pr.RepoOwner, pr.RepoName, pr.PRNumber, head); err != nil {
			log.Printf("[AUTO-REVIEW] %s: supersede older heads failed: %v", key, err)
			return
		} else if n > 0 {
			log.Printf("[AUTO-REVIEW] %s: poll superseded %d queued intent(s) for older heads", key, n)
		}
	}
	if current != nil && current.Status != db.AutoReviewIntentQueued {
		return
	}
	published, err := p.headPublished(pr.RepoOwner, pr.RepoName, pr.PRNumber, head)
	if err != nil {
		log.Printf("[AUTO-REVIEW] %s: read publication ledger failed: %v", key, err)
		return
	}
	if current != nil {
		// Another run (manual, or the legacy poll path) published this head
		// while the intent waited; nothing is owed any more.
		if published {
			p.moveAutoReviewIntent(*current, db.AutoReviewIntentQueued, db.AutoReviewIntentDone, "")
			log.Printf("[AUTO-REVIEW] %s: intent=%d done, the head was published by another run", key, current.ID)
		}
		return
	}
	intent := db.AutoReviewIntent{RepoOwner: pr.RepoOwner, RepoName: pr.RepoName, PRNumber: pr.PRNumber, HeadSHA: head, Trigger: db.AutoReviewTriggerPollFallback}
	if published {
		intent.Status = db.AutoReviewIntentDone
		intent.Publication = publicationPosted
	}
	created, err := p.db.EnsureAutoReviewIntent(&intent, []string{db.AutoReviewIntentSuperseded})
	if err != nil {
		log.Printf("[AUTO-REVIEW] %s: record fallback intent failed: %v", key, err)
		return
	}
	if created {
		log.Printf("[AUTO-REVIEW] %s: intent=%d %s by poll fallback", key, intent.ID, intent.Status)
	}
}

// dispatchAutoReviewIntents runs after the poll refreshed PR state: running
// intents are settled against their runs, and queued intents whose PR is
// still open, ready, allowlisted and on the same head are admitted within
// the poll's admission budget; the rest stay queued for the next cycle.
// Anything else queued is superseded so the backlog reflects real work.
func (p *Poller) dispatchAutoReviewIntents(ctx context.Context, prs []db.PR, states map[string]*github.PRState) {
	p.settleAutoReviewIntents()
	queued, err := p.db.ListAutoReviewIntents(db.AutoReviewIntentFilter{Statuses: []string{db.AutoReviewIntentQueued}})
	if err != nil {
		log.Printf("[AUTO-REVIEW] list queued intents: %v", err)
		return
	}
	if len(queued) == 0 {
		return
	}
	budget := p.pollAdmissionLimit() - p.trackedReviewCount()
	admitted := 0
	// Intents store lowercased targets; PR rows and state keys keep GitHub's
	// casing, so index both by the lowercased key.
	prByKey := make(map[string]*db.PR, len(prs))
	stateByKey := make(map[string]*github.PRState, len(states))
	for i := range prs {
		prByKey[strings.ToLower(fmt.Sprintf("%s/%s/%d", prs[i].RepoOwner, prs[i].RepoName, prs[i].PRNumber))] = &prs[i]
	}
	for k, state := range states {
		stateByKey[strings.ToLower(k)] = state
	}
	for _, intent := range queued {
		key := autoReviewKey(intent.RepoOwner, intent.RepoName, intent.PRNumber, intent.HeadSHA)
		lookup := fmt.Sprintf("%s/%s/%d", intent.RepoOwner, intent.RepoName, intent.PRNumber)
		pr, state := prByKey[lookup], stateByKey[lookup]
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
		if admitted >= budget {
			log.Printf("[AUTO-REVIEW] %s intent=%d: admission budget spent (%d this cycle), left queued", key, intent.ID, admitted)
			continue
		}
		target := buildPRFromDB(*pr)
		target.CommitSHA = intent.HeadSHA
		target.Draft = false
		if p.admitAutoReviewIntent(ctx, intent, target) {
			admitted++
		}
	}
}

func (p *Poller) trackedReviewCount() int {
	p.reviewsMutex.Lock()
	defer p.reviewsMutex.Unlock()
	return len(p.activeReviews)
}

// settleAutoReviewIntents moves running intents to their run's outcome. A
// completed run counts as done only when its review reached GitHub; a run
// that finished while the PR was a draft, closed or on another head is
// superseded so a later ready or push event can review the head again. The
// run row completes before the worker publishes and records the outcome, so
// a completed run with no outcome yet is left alone for a grace period. Past
// it (the process died mid-publication) the publication ledger decides: a
// recorded head is done; an unrecorded one is failed, not superseded, because
// the inline comments may have reached GitHub before the ledger did and a
// forced re-review would post them again.
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
			// Admission claims the intent before ProcessReviewJob writes the run
			// row. Past the grace nothing executed, so the head is owed again.
			if time.Since(intent.UpdatedAt) < autoReviewClaimGrace {
				continue
			}
			outcome = db.AutoReviewIntentQueued
		case run.Status == db.ReviewRunStatusCompleted:
			switch {
			case intent.Publication == publicationPosted:
				outcome = db.AutoReviewIntentDone
			case strings.HasPrefix(intent.Publication, publicationSkippedPrefix):
				outcome = db.AutoReviewIntentSuperseded
			case intent.Publication == "":
				if run.CompletedAt == nil || time.Since(*run.CompletedAt) < autoReviewPublicationGrace {
					continue
				}
				published, err := p.headPublished(intent.RepoOwner, intent.RepoName, intent.PRNumber, intent.HeadSHA)
				if err != nil {
					log.Printf("[AUTO-REVIEW] %s intent=%d run=%s: read publication ledger: %v", key, intent.ID, intent.RunID, err)
					continue
				}
				if published {
					outcome = db.AutoReviewIntentDone
				} else {
					outcome = db.AutoReviewIntentFailed
				}
				log.Printf("[AUTO-REVIEW] %s intent=%d run=%s: no publication outcome %s after completion, ledger says published=%t", key, intent.ID, intent.RunID, autoReviewPublicationGrace, published)
			default:
				outcome = db.AutoReviewIntentFailed
			}
		case run.Status == db.ReviewRunStatusCancelled:
			outcome = db.AutoReviewIntentSuperseded
		case run.Status == db.ReviewRunStatusFailed || run.Status == db.ReviewRunStatusTimedOut:
			outcome = db.AutoReviewIntentFailed
		}
		if outcome == "" {
			continue
		}
		runID := intent.RunID
		if outcome == db.AutoReviewIntentQueued {
			runID = ""
		}
		if _, err := p.db.UpdateAutoReviewIntentStatus(intent.ID, []string{db.AutoReviewIntentRunning}, outcome, runID); err != nil {
			log.Printf("[AUTO-REVIEW] %s intent=%d run=%s: could not settle: %v", key, intent.ID, intent.RunID, err)
			continue
		}
		log.Printf("[AUTO-REVIEW] %s intent=%d run=%s: %s (publication=%q)", key, intent.ID, intent.RunID, outcome, intent.Publication)
	}
}
