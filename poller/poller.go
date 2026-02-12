package poller

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"pr-review-server/config"
	"pr-review-server/db"
	"pr-review-server/gcs"
	"pr-review-server/github"
	"pr-review-server/pkg/reviewer/llm"
	"pr-review-server/pkg/reviewer/service"
)

// Timeout constants for review process management.
// These values are coordinated to ensure consistent behavior:
// - ReviewProcessTimeout: Max time allowed for a review to complete before killing
// - StaleReviewResetTimeout: Time after which a "generating" PR is considered stale
// - ReviewProcessWarningThreshold: Time after which to warn about long-running reviews
// - ErrorPRRetryTimeout: Time after which an "error" PR is retried
const (
	// ReviewProcessTimeout is the maximum time allowed for a review process before killing it.
	// This is used by the process monitor to force-kill hung review processes.
	ReviewProcessTimeout = 5 * time.Minute

	// StaleReviewResetTimeout is how long a PR can be in "generating" status before being reset.
	// This should be >= ReviewProcessTimeout to avoid resetting while still running.
	// Using the same value ensures consistent behavior.
	StaleReviewResetTimeout = 5 * time.Minute

	// ReviewProcessWarningThreshold is when to start warning about long-running reviews.
	ReviewProcessWarningThreshold = 2 * time.Minute

	// ErrorPRRetryTimeout is how long to wait before retrying a PR in "error" state.
	ErrorPRRetryTimeout = 5 * time.Minute
)

type ProcessInfo struct {
	PID       int
	StartTime time.Time
}

type Poller struct {
	cfg              *config.Config
	db               db.Database
	ghClient         GitHubClient
	ghClientConcrete *github.Client // Concrete client for reviewer service (which uses a different interface)
	gcsClient        *gcs.Client
	storage          ReviewStorage   // Optional: for testing. If nil, uses gcsClient/local storage
	reviewGenerator  ReviewGenerator // Optional: for testing. If nil, uses real reviewer service
	reviewDir        string          // Local storage path (used when GCS is not configured)
	cacheUpdateFunc  func([]github.PullRequest)
	EventFunc        func(eventType string, payload interface{})
	triggerChan      chan struct{}
	polling          bool
	pollMutex        sync.Mutex
	// Track active review processes for cancellation and monitoring
	activeReviews map[string]ProcessInfo // prKey (owner/repo/number) -> ProcessInfo
	reviewsMutex  sync.Mutex
	// Track last poll time for countdown display
	lastPollTime  time.Time
	pollTimeMutex sync.RWMutex
	// Track ticker start time for accurate countdown
	tickerStartTime time.Time
	// Dev user for syncing user_pr_views in dev mode
	devUser *db.User
}

func New(cfg *config.Config, database db.Database, ghClient *github.Client, gcsClient *gcs.Client) *Poller {
	return &Poller{
		cfg:              cfg,
		db:               database,
		ghClient:         ghClient,
		ghClientConcrete: ghClient,
		gcsClient:        gcsClient,
		reviewDir:        cfg.ReviewsDir,
		triggerChan:      make(chan struct{}, 1), // Buffered to prevent blocking
		activeReviews:    make(map[string]ProcessInfo),
	}
}

// broadcastPRUpdate helper to send update events
func (p *Poller) broadcastPRUpdate(owner, repo string, number int) {
	if p.EventFunc != nil {
		p.EventFunc("pr_updated", map[string]interface{}{
			"owner":  owner,
			"repo":   repo,
			"number": number,
		})
	}
}

// upsertPRPreservingReviewData upserts a PR while preserving existing review data (doesn't fetch from GitHub)
// This is used when updating PR status/files but we want to keep approval counts unchanged
func (p *Poller) upsertPRPreservingReviewData(ctx context.Context, owner, repo string, prNumber int, commitSHA, htmlPath, status, title, author string, createdAt *time.Time, draft bool, criticalCount, mediumCount, lowCount int) error {
	// Get existing PR to preserve review data
	existingPR, err := p.db.GetPR(owner, repo, prNumber)
	if err != nil {
		log.Printf("[DB] Error: failed to get existing PR data for %s/%s#%d: %v", owner, repo, prNumber, err)
		return err // Propagate DB error to prevent data loss
	}

	// Default to existing values (or zero values if no existing PR)
	approvalCount := 0
	myReviewStatus := ""
	notes := ""
	if existingPR != nil {
		approvalCount = existingPR.ApprovalCount
		myReviewStatus = existingPR.MyReviewStatus
		notes = existingPR.Notes
	}

	pr := &db.PR{
		RepoOwner:      owner,
		RepoName:       repo,
		PRNumber:       prNumber,
		LastCommitSHA:  commitSHA,
		ReviewHTMLPath: htmlPath,
		Status:         status,
		Title:          title,
		Author:         author,
		ApprovalCount:  approvalCount,
		MyReviewStatus: myReviewStatus,
		CreatedAt:      createdAt,
		Draft:          draft,
		CriticalCount:  criticalCount,
		MediumCount:    mediumCount,
		LowCount:       lowCount,
		Notes:          notes,
	}

	// Set LastReviewedAt when marking as completed
	if status == "completed" {
		now := time.Now().UTC()
		pr.LastReviewedAt = &now
	}

	err = p.db.UpsertPR(pr)
	if err == nil {
		p.broadcastPRUpdate(owner, repo, prNumber)
	}
	return err
}

func (p *Poller) SetCacheUpdateFunc(f func([]github.PullRequest)) {
	p.cacheUpdateFunc = f
}

// SetDevUser sets the dev user for syncing user_pr_views in dev mode
func (p *Poller) SetDevUser(user *db.User) {
	p.devUser = user
}

func (p *Poller) Trigger() {
	// Non-blocking send to trigger channel
	select {
	case p.triggerChan <- struct{}{}:
		log.Println("Manual poll trigger requested")
	default:
		// Channel already has a pending trigger, skip
	}
}

func (p *Poller) Start(ctx context.Context) {
	tickerStartTime := time.Now()
	ticker := time.NewTicker(p.cfg.PollingInterval)
	defer ticker.Stop()

	// Store ticker start time for accurate countdown
	p.pollTimeMutex.Lock()
	p.tickerStartTime = tickerStartTime
	p.pollTimeMutex.Unlock()

	// Start reviewer process monitor
	monitorTicker := time.NewTicker(30 * time.Second)
	defer monitorTicker.Stop()
	go p.monitorReviewerProcesses(ctx, monitorTicker)

	log.Println("Starting poller...")
	log.Printf("Ticker created at %s, will fire every %v", tickerStartTime.Format("15:04:05.000"), p.cfg.PollingInterval)

	// Run immediately on start
	p.startPoll(ctx, "initial")

	for {
		select {
		case <-ctx.Done():
			log.Println("Poller stopped")
			return
		case tickTime := <-ticker.C:
			elapsed := tickTime.Sub(tickerStartTime)
			log.Printf("Ticker fired at %s (%.3fs since ticker start)", tickTime.Format("15:04:05.000"), elapsed.Seconds())
			p.startPoll(ctx, "scheduled")
		case <-p.triggerChan:
			p.startPoll(ctx, "manual")
		}
	}
}

func (p *Poller) monitorReviewerProcesses(ctx context.Context, ticker *time.Ticker) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			p.reviewsMutex.Lock()
			for key, info := range p.activeReviews {
				elapsed := time.Since(info.StartTime)
				if elapsed > ReviewProcessTimeout {
					log.Printf("[MONITOR] WARNING: review for %s has been running for %v (timeout), removing from tracking", key, elapsed)
					// Remove from tracking (goroutine will finish on its own)
					delete(p.activeReviews, key)
				} else if elapsed > ReviewProcessWarningThreshold {
					log.Printf("[MONITOR] WARNING: review for %s has been running for %v (threshold: 2m)", key, elapsed)
				} else {
					log.Printf("[MONITOR] review for %s running normally (%v elapsed)", key, elapsed)
				}
			}
			p.reviewsMutex.Unlock()
		}
	}
}

func (p *Poller) GetReviewerStatus() (running bool, duration time.Duration) {
	p.reviewsMutex.Lock()
	defer p.reviewsMutex.Unlock()

	if len(p.activeReviews) == 0 {
		return false, 0
	}

	// Verify processes are actually still running and find the longest duration
	count := 0
	var maxDuration time.Duration

	// Iterate safely over copy of keys if we need to modify, or just check
	// Since we might delete, it's safer to just check existence here, or rely on monitor to clean up
	// For status check, we'll just read.
	for _, info := range p.activeReviews {
		// Reviews run as in-process goroutines, just count active ones
		count++
		d := time.Since(info.StartTime)
		if d > maxDuration {
			maxDuration = d
		}
	}

	return count > 0, maxDuration
}

func (p *Poller) GetLastPollTime() time.Time {
	p.pollTimeMutex.RLock()
	defer p.pollTimeMutex.RUnlock()
	return p.lastPollTime
}

func (p *Poller) GetPollingInterval() time.Duration {
	return p.cfg.PollingInterval
}

// GetSecondsUntilNextPoll calculates accurate countdown based on ticker timing
func (p *Poller) GetSecondsUntilNextPoll() int {
	p.pollTimeMutex.RLock()
	tickerStart := p.tickerStartTime
	p.pollTimeMutex.RUnlock()

	if tickerStart.IsZero() {
		return 0
	}

	now := time.Now()
	interval := p.cfg.PollingInterval

	// Calculate how long since ticker started
	elapsed := now.Sub(tickerStart)

	// Calculate which tick number we're waiting for
	// Add 1 because we want the NEXT tick
	tickNumber := int(elapsed/interval) + 1

	// Calculate when that tick will fire
	nextTickTime := tickerStart.Add(time.Duration(tickNumber) * interval)

	// Calculate remaining time
	remaining := nextTickTime.Sub(now)

	if remaining < 0 {
		return 0
	}

	seconds := int(remaining.Seconds())

	return seconds
}

// prKey creates a unique key for tracking a PR
func prKey(owner, repo string, number int) string {
	return fmt.Sprintf("%s/%s#%d", owner, repo, number)
}

// trackReview adds a PR's review to the active reviews map
func (p *Poller) trackReview(owner, repo string, number, pid int) {
	p.reviewsMutex.Lock()
	defer p.reviewsMutex.Unlock()
	key := prKey(owner, repo, number)
	p.activeReviews[key] = ProcessInfo{
		PID:       pid,
		StartTime: time.Now(),
	}
	log.Printf("[TRACK] Tracking review for %s", key)
}

// untrackReview removes a PR's review process from the active reviews map
func (p *Poller) untrackReview(owner, repo string, number int) {
	p.reviewsMutex.Lock()
	defer p.reviewsMutex.Unlock()
	key := prKey(owner, repo, number)
	delete(p.activeReviews, key)
	log.Printf("[TRACK] Untracked review for %s", key)
}

// isTracked checks if a PR is currently being processed
func (p *Poller) isTracked(owner, repo string, number int) bool {
	p.reviewsMutex.Lock()
	defer p.reviewsMutex.Unlock()
	key := prKey(owner, repo, number)
	_, exists := p.activeReviews[key]
	return exists
}

// killReview removes an active review from tracking
// Note: Since reviews run as in-process goroutines, we cannot actually stop them.
// This only removes the review from tracking; the goroutine will complete on its own.
func (p *Poller) killReview(owner, repo string, number int) bool {
	p.reviewsMutex.Lock()
	key := prKey(owner, repo, number)
	_, exists := p.activeReviews[key]
	p.reviewsMutex.Unlock()

	if !exists {
		return false
	}

	log.Printf("[TRACK] Removing review for %s from tracking (goroutine will complete on its own)", key)
	p.untrackReview(owner, repo, number)
	return true
}

func (p *Poller) startPoll(ctx context.Context, trigger string) {
	p.pollMutex.Lock()
	if p.polling {
		log.Printf("Poll already in progress, skipping %s trigger", trigger)
		p.pollMutex.Unlock()
		return
	}
	p.polling = true
	p.pollMutex.Unlock()

	log.Printf("Starting %s poll", trigger)

	go func() {
		defer func() {
			p.pollMutex.Lock()
			p.polling = false
			p.pollMutex.Unlock()
			log.Printf("Completed %s poll", trigger)
		}()
		p.poll(ctx)
	}()
}

// cleanupClosedPRs removes PRs from the database and filesystem if they're closed on GitHub
func (p *Poller) cleanupClosedPRs(ctx context.Context) (int, error) {
	// Get all PRs from database
	allPRs, err := p.db.GetAllPRs()
	if err != nil {
		return 0, fmt.Errorf("failed to get PRs from database: %w", err)
	}

	removed := 0
	for _, pr := range allPRs {
		// Check if PR is still open on GitHub
		isOpen, err := p.ghClient.IsPROpen(ctx, pr.RepoOwner, pr.RepoName, pr.PRNumber)
		if err != nil {
			// If we can't fetch the PR, it might be deleted or we don't have access
			// Log but continue - we'll handle it on next poll
			log.Printf("[CLEANUP] Warning: Could not check status of PR %s/%s#%d: %v",
				pr.RepoOwner, pr.RepoName, pr.PRNumber, err)
			continue
		}

		// If PR is closed, remove it from the database
		// Note: Reviews are kept in GCS permanently for historical reference
		if !isOpen {
			log.Printf("[CLEANUP] PR %s/%s#%d is closed, removing from tracking (reviews kept in GCS)",
				pr.RepoOwner, pr.RepoName, pr.PRNumber)

			// Delete from database
			if err := p.db.DeletePR(pr.RepoOwner, pr.RepoName, pr.PRNumber); err != nil {
				log.Printf("[CLEANUP] ERROR: Failed to delete PR %s/%s#%d from database: %v",
					pr.RepoOwner, pr.RepoName, pr.PRNumber, err)
				continue
			}

			// Notify clients
			if p.EventFunc != nil {
				p.EventFunc("pr_deleted", map[string]interface{}{
					"owner":  pr.RepoOwner,
					"repo":   pr.RepoName,
					"number": pr.PRNumber,
				})
			}

			log.Printf("[CLEANUP] Successfully removed closed PR %s/%s#%d",
				pr.RepoOwner, pr.RepoName, pr.PRNumber)
			removed++
		}
	}

	return removed, nil
}

// reviewExists checks if a review already exists for the given PR+commit.
// If a storage interface is set (for testing), it uses that.
// Otherwise, checks GCS if bucket is configured, or local file storage.
func (p *Poller) reviewExists(ctx context.Context, owner, repo string, prNumber int, commitSHA string) (bool, error) {
	// Use storage interface if set (for testing)
	if p.storage != nil {
		return p.storage.ReviewExists(ctx, owner, repo, prNumber, commitSHA)
	}

	// Check GCS if bucket is configured
	if p.gcsClient != nil && p.gcsClient.BucketName() != "" {
		return p.gcsClient.ReviewExists(ctx, owner, repo, prNumber, commitSHA)
	}

	// Check local file storage
	filename := gcs.ReviewFileName(owner, repo, prNumber, commitSHA)
	localPath := filepath.Join(p.reviewDir, filename)
	_, err := os.Stat(localPath)
	if err == nil {
		return true, nil
	}
	if os.IsNotExist(err) {
		return false, nil
	}
	return false, err
}

// saveReview saves the review HTML content to storage.
// If a storage interface is set (for testing), it uses that.
// Otherwise, if GCS bucket is configured, uploads to GCS. Otherwise saves locally.
func (p *Poller) saveReview(ctx context.Context, owner, repo string, prNumber int, commitSHA string, htmlContent []byte) (string, error) {
	// Use storage interface if set (for testing)
	if p.storage != nil {
		return p.storage.SaveReview(ctx, owner, repo, prNumber, commitSHA, htmlContent)
	}

	// Check if GCS bucket is configured
	if p.gcsClient != nil && p.gcsClient.BucketName() != "" {
		return p.gcsClient.UploadReview(ctx, owner, repo, prNumber, commitSHA, htmlContent)
	}

	// Fall back to local file storage
	filename := gcs.ReviewFileName(owner, repo, prNumber, commitSHA)
	localPath := filepath.Join(p.reviewDir, filename)

	// Ensure reviews directory exists
	if err := os.MkdirAll(p.reviewDir, 0755); err != nil {
		return "", fmt.Errorf("failed to create reviews directory: %w", err)
	}

	// Write to local file
	if err := os.WriteFile(localPath, htmlContent, 0644); err != nil {
		return "", fmt.Errorf("failed to write review file: %w", err)
	}

	log.Printf("[LOCAL] Saved review to: %s", localPath)
	return filename, nil
}

// backfillPRMetadata fills in missing title/author for existing PRs by fetching from GitHub
func (p *Poller) backfillPRMetadata(ctx context.Context) (int, error) {
	// Get PRs with missing metadata
	prs, err := p.db.GetPRsWithMissingMetadata()
	if err != nil {
		return 0, fmt.Errorf("failed to get PRs with missing metadata: %w", err)
	}

	if len(prs) == 0 {
		return 0, nil
	}

	updated := 0
	for _, pr := range prs {
		// Fetch PR details from GitHub
		title, author, err := p.ghClient.GetPRDetails(ctx, pr.RepoOwner, pr.RepoName, pr.PRNumber)
		if err != nil {
			log.Printf("[BACKFILL] Warning: Could not fetch PR details for %s/%s#%d: %v",
				pr.RepoOwner, pr.RepoName, pr.PRNumber, err)
			continue
		}

		// Update database with metadata
		if err := p.db.UpdatePRMetadata(pr.RepoOwner, pr.RepoName, pr.PRNumber, title, author); err != nil {
			log.Printf("[BACKFILL] ERROR: Failed to update metadata for %s/%s#%d: %v",
				pr.RepoOwner, pr.RepoName, pr.PRNumber, err)
			continue
		}

		p.broadcastPRUpdate(pr.RepoOwner, pr.RepoName, pr.PRNumber)

		log.Printf("[BACKFILL] Updated metadata for PR %s/%s#%d: %s by %s",
			pr.RepoOwner, pr.RepoName, pr.PRNumber, title, author)
		updated++
	}

	return updated, nil
}

// backfillPRCreatedAt fills in missing created_at timestamps for existing PRs by fetching from GitHub
func (p *Poller) backfillPRCreatedAt(ctx context.Context) (int, error) {
	// Get PRs with missing created_at
	prs, err := p.db.GetPRsWithMissingCreatedAt()
	if err != nil {
		return 0, fmt.Errorf("failed to get PRs with missing created_at: %w", err)
	}

	if len(prs) == 0 {
		return 0, nil
	}

	updated := 0
	for _, pr := range prs {
		// Fetch PR details from GitHub
		ghPR, _, err := p.ghClient.GetPR(ctx, pr.RepoOwner, pr.RepoName, pr.PRNumber)
		if err != nil {
			log.Printf("[BACKFILL] Warning: Could not fetch PR for created_at %s/%s#%d: %v",
				pr.RepoOwner, pr.RepoName, pr.PRNumber, err)
			continue
		}

		createdAt := ghPR.GetCreatedAt().Time

		// Update database with created_at
		if err := p.db.UpdatePRCreatedAt(pr.RepoOwner, pr.RepoName, pr.PRNumber, createdAt); err != nil {
			log.Printf("[BACKFILL] ERROR: Failed to update created_at for %s/%s#%d: %v",
				pr.RepoOwner, pr.RepoName, pr.PRNumber, err)
			continue
		}

		p.broadcastPRUpdate(pr.RepoOwner, pr.RepoName, pr.PRNumber)

		log.Printf("[BACKFILL] Updated created_at for PR %s/%s#%d: %s",
			pr.RepoOwner, pr.RepoName, pr.PRNumber, createdAt.Format("2006-01-02 15:04:05"))
		updated++
	}

	return updated, nil
}

// checkForOutdatedReviews detects PRs with new commits and resets them to pending
func (p *Poller) checkForOutdatedReviews(ctx context.Context) (int, error) {
	// Get all PRs from database
	allPRs, err := p.db.GetAllPRs()
	if err != nil {
		return 0, fmt.Errorf("failed to get PRs from database: %w", err)
	}

	outdated := 0
	checkedCount := 0
	for _, pr := range allPRs {
		// Check ALL PRs regardless of status - pending/error PRs can also get new commits
		// and we want to ensure we always process the latest commit, not stale ones
		checkedCount++

		// Fetch current HEAD SHA from GitHub
		currentSHA, err := p.ghClient.GetPRHeadSHA(ctx, pr.RepoOwner, pr.RepoName, pr.PRNumber)
		if err != nil {
			log.Printf("[OUTDATED] Warning: Could not fetch current HEAD SHA for %s/%s#%d: %v",
				pr.RepoOwner, pr.RepoName, pr.PRNumber, err)
			continue
		}

		log.Printf("[OUTDATED] Checking %s/%s#%d: stored=%s current=%s status=%s",
			pr.RepoOwner, pr.RepoName, pr.PRNumber, pr.LastCommitSHA[:7], currentSHA[:7], pr.Status)

		// Compare commit SHAs
		if currentSHA != pr.LastCommitSHA {
			wasGenerating := pr.Status == "generating"
			statusMsg := pr.Status
			if wasGenerating {
				statusMsg = "generating (cancelling)"
			}
			log.Printf("[OUTDATED] PR %s/%s#%d (%s) has new commits (old: %s, new: %s), resetting to pending",
				pr.RepoOwner, pr.RepoName, pr.PRNumber, statusMsg, pr.LastCommitSHA[:7], currentSHA[:7])

			// Delete old HTML file if it exists
			if pr.ReviewHTMLPath != "" {
				oldHTMLPath := filepath.Join(p.reviewDir, pr.ReviewHTMLPath)
				if err := os.Remove(oldHTMLPath); err != nil && !os.IsNotExist(err) {
					log.Printf("[OUTDATED] Warning: Failed to delete old HTML file %s: %v", oldHTMLPath, err)
				} else if err == nil {
					log.Printf("[OUTDATED] Deleted old HTML file: %s", pr.ReviewHTMLPath)
				}
			}

			// If the PR was actively generating, kill the process
			if wasGenerating {
				if p.killReview(pr.RepoOwner, pr.RepoName, pr.PRNumber) {
					log.Printf("[OUTDATED] Killed active review process for %s/%s#%d",
						pr.RepoOwner, pr.RepoName, pr.PRNumber)
				}
			}

			// Reset PR to pending with new commit SHA and clear old review data
			if err := p.db.ResetPRToOutdated(pr.RepoOwner, pr.RepoName, pr.PRNumber, currentSHA); err != nil {
				log.Printf("[OUTDATED] ERROR: Failed to reset PR %s/%s#%d: %v",
					pr.RepoOwner, pr.RepoName, pr.PRNumber, err)
				continue
			}

			p.broadcastPRUpdate(pr.RepoOwner, pr.RepoName, pr.PRNumber)

			if wasGenerating {
				log.Printf("[OUTDATED] PR %d has a new commit while generating. Cancelled old review.", pr.PRNumber)
			} else if pr.Status == "completed" {
				log.Printf("[OUTDATED] PR %d has a new commit. Removed stale review.", pr.PRNumber)
			} else {
				log.Printf("[OUTDATED] Updated commit SHA for PR %d from %s to %s", pr.PRNumber, pr.LastCommitSHA[:7], currentSHA[:7])
			}

			outdated++
		}
	}

	if checkedCount > 0 {
		log.Printf("[OUTDATED] Checked %d PRs (out of %d total)", checkedCount, len(allPRs))
	}

	return outdated, nil
}

// syncUserPRViews ensures all PRs have user_pr_view records for the dev user
// This is called after fetching PRs to populate the user-PR relationships
func (p *Poller) syncUserPRViews(ctx context.Context, allPRs []github.PullRequest) {
	if p.devUser == nil {
		return // Not in dev mode, skip
	}

	userID := p.devUser.ID
	username := strings.ToLower(p.devUser.GitHubUsername)

	for _, pr := range allPRs {
		// Get PR from database to get its ID
		dbPR, err := p.db.GetPR(pr.Owner, pr.Repo, pr.Number)
		if err != nil || dbPR == nil {
			continue // PR not in database yet
		}

		// Determine if this is the user's own PR
		isAuthor := strings.EqualFold(pr.Author, username)

		// Ensure user_pr_view record exists
		if err := p.db.EnsureUserPRView(userID, dbPR.ID, isAuthor); err != nil {
			log.Printf("[POLL] Warning: Failed to ensure user_pr_view for user %d, PR %d: %v",
				userID, dbPR.ID, err)
		}
	}
}

func (p *Poller) poll(ctx context.Context) {
	startTime := time.Now()

	// Update last poll time for countdown display
	p.pollTimeMutex.Lock()
	p.lastPollTime = startTime
	p.pollTimeMutex.Unlock()

	log.Printf("[POLL] Starting poll at %s", startTime.Format("15:04:05"))

	// Reset any PRs stuck in "generating" for too long
	log.Printf("[POLL] Checking for stale PRs...")
	resetCount, err := p.db.ResetStaleGeneratingPRs(int(StaleReviewResetTimeout.Minutes()))
	if err != nil {
		log.Printf("[POLL] ERROR: Failed to reset stale PRs: %v", err)
	} else if resetCount > 0 {
		log.Printf("[POLL] Reset %d stale PRs from 'generating' to 'pending'", resetCount)
		// We don't easily know WHICH ones were reset without modifying ResetStaleGeneratingPRs to return them
		// For now, let the individual PR updates handle it or wait for the next full poll result
	} else {
		log.Printf("[POLL] No stale PRs found")
	}

	// Reset PRs in error state after timeout (self-healing)
	log.Printf("[POLL] Checking for error PRs to retry...")
	errorResetCount, err := p.db.ResetErrorPRs(int(ErrorPRRetryTimeout.Minutes()))
	if err != nil {
		log.Printf("[POLL] ERROR: Failed to reset error PRs: %v", err)
	} else if errorResetCount > 0 {
		log.Printf("[POLL] SELF-HEALING: Reset %d error PRs to 'pending' for retry", errorResetCount)
	} else {
		log.Printf("[POLL] No error PRs to retry")
	}

	// Clean up closed PRs (self-healing)
	log.Printf("[POLL] Checking for closed PRs to remove...")
	removedCount, err := p.cleanupClosedPRs(ctx)
	if err != nil {
		log.Printf("[POLL] ERROR: Failed to cleanup closed PRs: %v", err)
	} else if removedCount > 0 {
		log.Printf("[POLL] CLEANUP: Removed %d closed PRs from system", removedCount)
	} else {
		log.Printf("[POLL] No closed PRs to remove")
	}

	// Backfill missing PR metadata (self-healing)
	log.Printf("[POLL] Checking for PRs with missing metadata...")
	backfilledCount, err := p.backfillPRMetadata(ctx)
	if err != nil {
		log.Printf("[POLL] ERROR: Failed to backfill metadata: %v", err)
	} else if backfilledCount > 0 {
		log.Printf("[POLL] BACKFILL: Updated metadata for %d PRs", backfilledCount)
	} else {
		log.Printf("[POLL] No PRs need metadata backfill")
	}

	// Backfill missing created_at timestamps (self-healing)
	log.Printf("[POLL] Checking for PRs with missing created_at...")
	timestampBackfilledCount, err := p.backfillPRCreatedAt(ctx)
	if err != nil {
		log.Printf("[POLL] ERROR: Failed to backfill created_at: %v", err)
	} else if timestampBackfilledCount > 0 {
		log.Printf("[POLL] BACKFILL: Updated created_at for %d PRs", timestampBackfilledCount)
	} else {
		log.Printf("[POLL] No PRs need created_at backfill")
	}

	// Check for outdated reviews (PRs with new commits)
	log.Printf("[POLL] Checking for outdated reviews...")
	outdatedCount, err := p.checkForOutdatedReviews(ctx)
	if err != nil {
		log.Printf("[POLL] ERROR: Failed to check for outdated reviews: %v", err)
	} else if outdatedCount > 0 {
		log.Printf("[POLL] OUTDATED: Reset %d PRs with new commits to pending", outdatedCount)
	} else {
		log.Printf("[POLL] No outdated reviews found")
	}

	// Check auto-review setting to determine if we should process new PRs
	autoReviewEnabled, autoReviewErr := p.db.GetAutoReviewRequestedPRs()
	if autoReviewErr != nil {
		log.Printf("[POLL] Warning: Failed to get auto-review setting: %v", autoReviewErr)
		autoReviewEnabled = true // Default to enabled on error
	}

	log.Printf("[POLL] Fetching PRs requesting review from GitHub...")
	reviewPRs, err := p.ghClient.GetPRsRequestingReview(ctx)
	if err != nil {
		log.Printf("[POLL] ERROR: Failed to fetch PRs requesting review: %v", err)
		// Continue even if this fails - we can still process "my PRs"
		reviewPRs = []github.PullRequest{}
	} else {
		log.Printf("[POLL] Found %d PRs requesting review", len(reviewPRs))

		// Log new PRs (not in database yet)
		for _, pr := range reviewPRs {
			existingPR, err := p.db.GetPR(pr.Owner, pr.Repo, pr.Number)
			if err == nil && existingPR == nil {
				log.Printf("[POLL] New review request: PR #%d", pr.Number)
			}
		}
	}

	log.Printf("[POLL] Fetching my own open PRs from GitHub...")
	myPRs, err := p.ghClient.GetMyOpenPRs(ctx)
	if err != nil {
		log.Printf("[POLL] ERROR: Failed to fetch my open PRs: %v", err)
		// Continue even if this fails
		myPRs = []github.PullRequest{}
	}
	log.Printf("[POLL] Found %d of my own open PRs", len(myPRs))

	// Combine all PRs for cache
	allPRs := append(reviewPRs, myPRs...)

	// Update cache for fast dashboard loading
	if p.cacheUpdateFunc != nil {
		p.cacheUpdateFunc(allPRs)
	}

	// CRITICAL: Also add ALL database PRs to ensure we update review data even for PRs
	// that are no longer in GitHub search (e.g., you've already reviewed them)
	dbPRsForReviewUpdate, err := p.db.GetAllPRs()
	if err != nil {
		log.Printf("[POLL] WARNING: Failed to get database PRs for review update: %v", err)
	} else {
		// Create a map of PRs we already have to avoid duplicates
		prMap := make(map[string]github.PullRequest)
		for _, pr := range allPRs {
			key := fmt.Sprintf("%s/%s/%d", pr.Owner, pr.Repo, pr.Number)
			prMap[key] = pr
		}

		// Add database PRs that aren't already in our list
		// IMPORTANT: When auto-review is OFF, we already filtered out PRs that don't exist in DB
		// So we should NOT re-add them here, as that would bypass the filter
		for _, dbPR := range dbPRsForReviewUpdate {
			key := fmt.Sprintf("%s/%s/%d", dbPR.RepoOwner, dbPR.RepoName, dbPR.PRNumber)
			if _, exists := prMap[key]; !exists {
				// If auto-review is disabled, skip adding this database PR back to allPRs
				// This prevents deleted PRs from being re-created via batch updates
				// EXCEPTION: Always include PRs that are actively generating (manual triggers)
				if !autoReviewEnabled && dbPR.Status != "generating" {
					// Log that we're skipping this database PR
					log.Printf("[POLL] Skipping database PR %s/%s#%d (auto-review disabled, not in GitHub results)",
						dbPR.RepoOwner, dbPR.RepoName, dbPR.PRNumber)
					continue
				}

				// Add this PR from database
				allPRs = append(allPRs, github.PullRequest{
					Owner:     dbPR.RepoOwner,
					Repo:      dbPR.RepoName,
					Number:    dbPR.PRNumber,
					CommitSHA: dbPR.LastCommitSHA,
					Title:     dbPR.Title,
					Author:    dbPR.Author,
					URL:       fmt.Sprintf("https://github.com/%s/%s/pull/%d", dbPR.RepoOwner, dbPR.RepoName, dbPR.PRNumber),
					CreatedAt: dbPR.CreatedAt, // Preserve created_at from database
					Draft:     dbPR.Draft,
				})
			}
		}
		log.Printf("[POLL] Added %d database PRs to review update list (total: %d PRs)", len(allPRs)-len(prMap), len(allPRs))
	}

	// Sync user_pr_views for dev mode (ensure all PRs have user-PR relationship records)
	p.syncUserPRViews(ctx, allPRs)

	// Batch fetch review data for all PRs using GraphQL (much more efficient)
	log.Printf("[POLL] Batch fetching review data for %d PRs using GraphQL...", len(allPRs))
	if len(allPRs) > 0 {
		// Create a map of existing PRs from database to avoid N+1 queries in the update loop
		existingPRsMap := make(map[string]*db.PR)
		for i := range dbPRsForReviewUpdate {
			key := fmt.Sprintf("%s/%s/%d", dbPRsForReviewUpdate[i].RepoOwner, dbPRsForReviewUpdate[i].RepoName, dbPRsForReviewUpdate[i].PRNumber)
			existingPRsMap[key] = &dbPRsForReviewUpdate[i]
		}

		reviewDataMap, err := p.ghClient.BatchGetPRReviewData(ctx, allPRs)
		if err != nil {
			log.Printf("[POLL] WARNING: Failed to batch fetch review data: %v", err)
		} else {
			// Update database with batch review data
			updateCount := 0
			for _, pr := range allPRs {
				key := fmt.Sprintf("%s/%s/%d", pr.Owner, pr.Repo, pr.Number)
				if reviewData, exists := reviewDataMap[key]; exists {
					// Look up PR from the map instead of querying database (avoids N+1 queries)
					existingPR, existsInDB := existingPRsMap[key]
					if !existsInDB {
						continue
					}

					// Always sync user_pr_views with latest review status from GitHub
					if p.devUser != nil {
						if err := p.db.UpdateUserReviewStatus(p.devUser.ID, existingPR.ID, reviewData.MyReviewStatus); err != nil {
							log.Printf("[POLL] Warning: Failed to update user review status for PR %d: %v", existingPR.ID, err)
						}
					}

					// Check if anything actually changed in prs table
					approvalChanged := existingPR.ApprovalCount != reviewData.ApprovalCount
					reviewStatusChanged := existingPR.MyReviewStatus != reviewData.MyReviewStatus
					draftChanged := existingPR.Draft != pr.Draft

					if !approvalChanged && !reviewStatusChanged && !draftChanged {
						continue // Nothing changed in prs table, skip update
					}

					// Update approval count, my review status, and draft status (always use fresh values from GitHub)
					// IMPORTANT: Preserve all existing fields to avoid wiping CI data
					existingPR.ApprovalCount = reviewData.ApprovalCount
					existingPR.MyReviewStatus = reviewData.MyReviewStatus
					existingPR.Draft = pr.Draft
					if pr.CreatedAt != nil {
						existingPR.CreatedAt = pr.CreatedAt
					}

					err = p.db.UpsertPR(existingPR)
					if err != nil {
						log.Printf("[POLL] ERROR: Failed to update review data for %s/%s#%d: %v", pr.Owner, pr.Repo, pr.Number, err)
					} else {
						p.broadcastPRUpdate(pr.Owner, pr.Repo, pr.Number)
						updateCount++
					}
				}
			}
			log.Printf("[POLL] Updated review data for %d PRs (only those with changes)", updateCount)
		}

		// Batch fetch reviewer groups and sync to user_pr_views
		if p.devUser != nil {
			log.Printf("[POLL] Batch fetching reviewer groups for %d PRs...", len(allPRs))
			reviewerGroupsMap, err := p.ghClient.BatchGetReviewerGroups(ctx, allPRs)
			if err != nil {
				log.Printf("[POLL] WARNING: Failed to batch fetch reviewer groups: %v", err)
			} else {
				for _, pr := range allPRs {
					key := fmt.Sprintf("%s/%s/%d", pr.Owner, pr.Repo, pr.Number)
					if groupData, exists := reviewerGroupsMap[key]; exists {
						existingPR, existsInDB := existingPRsMap[key]
						if !existsInDB {
							continue
						}
						if err := p.db.UpdateUserViaTeams(p.devUser.ID, existingPR.ID, groupData.ReviewerGroups); err != nil {
							log.Printf("[POLL] Warning: Failed to update via_teams for PR %d: %v", existingPR.ID, err)
						}
					}
				}
				log.Printf("[POLL] Updated reviewer groups for %d PRs", len(reviewerGroupsMap))
			}
		}
	}

	// Batch fetch CI status for all PRs using GraphQL
	log.Printf("[POLL] Batch fetching CI status for %d PRs using GraphQL...", len(allPRs))
	if len(allPRs) > 0 {
		// Prepare PR list with commit SHAs for CI status check
		var prsWithSHA []struct {
			Owner, Repo string
			Number      int
			CommitSHA   string
		}
		for _, pr := range allPRs {
			prsWithSHA = append(prsWithSHA, struct {
				Owner, Repo string
				Number      int
				CommitSHA   string
			}{
				Owner:     pr.Owner,
				Repo:      pr.Repo,
				Number:    pr.Number,
				CommitSHA: pr.CommitSHA,
			})
		}

		ciStatusMap, err := p.ghClient.BatchGetCIStatus(ctx, prsWithSHA)
		if err != nil {
			log.Printf("[POLL] WARNING: Failed to batch fetch CI status: %v", err)
		} else {
			// Update database with CI status
			updateCount := 0
			for _, pr := range allPRs {
				key := fmt.Sprintf("%s/%s/%d", pr.Owner, pr.Repo, pr.Number)
				if ciStatus, exists := ciStatusMap[key]; exists {
					// Get existing PR data from database
					existingPR, err := p.db.GetPR(pr.Owner, pr.Repo, pr.Number)
					if err != nil || existingPR == nil {
						log.Printf("[POLL] ERROR: Could not get PR %s from database: %v", key, err)
						continue
					}

					// Serialize failed checks to JSON
					failedChecksJSON := "[]"
					if len(ciStatus.FailedChecks) > 0 {
						if jsonBytes, err := json.Marshal(ciStatus.FailedChecks); err == nil {
							failedChecksJSON = string(jsonBytes)
						}
					}

					// Check if anything actually changed
					stateChanged := existingPR.CIState != ciStatus.State
					checksChanged := existingPR.CIFailedChecks != failedChecksJSON

					if !stateChanged && !checksChanged {
						continue // Nothing changed, skip update
					}

					// Update CI status in database
					existingPR.CIState = ciStatus.State
					existingPR.CIFailedChecks = failedChecksJSON
					err = p.db.UpsertPR(existingPR)
					if err != nil {
						log.Printf("[POLL] ERROR: Failed to update CI status for %s/%s#%d: %v", pr.Owner, pr.Repo, pr.Number, err)
					} else {
						p.broadcastPRUpdate(pr.Owner, pr.Repo, pr.Number)
						updateCount++
					}
				}
			}
			log.Printf("[POLL] Updated CI status for %d PRs (only those with changes)", updateCount)
		}
	}

	// CRITICAL: Also check database for pending PRs that need processing
	// This ensures we process PRs even when GitHub API fails
	log.Printf("[POLL] Checking database for pending PRs...")

	// Re-use auto-review setting from earlier (already checked at line 656)

	dbPRs, err := p.db.GetAllPRs()
	if err != nil {
		log.Printf("[POLL] ERROR: Failed to get PRs from database: %v", err)
	} else {
		pendingCount := 0
		skippedCount := 0
		for _, dbPR := range dbPRs {
			// Check for pending OR generating PRs
			// We must pick up 'generating' PRs because they might be manual triggers that haven't started yet
			if dbPR.Status == "pending" || dbPR.Status == "generating" {
				// Skip pending PRs if auto-review is disabled
				// BUT: Always process 'generating' PRs (manual triggers)
				if dbPR.Status == "pending" && !autoReviewEnabled {
					skippedCount++
					continue
				}

				// Convert DB PR to GitHub PR format for processing
				ghPR := github.PullRequest{
					Owner:     dbPR.RepoOwner,
					Repo:      dbPR.RepoName,
					Number:    dbPR.PRNumber,
					CommitSHA: dbPR.LastCommitSHA,
					Title:     dbPR.Title,
					Author:    dbPR.Author,
					URL:       fmt.Sprintf("https://github.com/%s/%s/pull/%d", dbPR.RepoOwner, dbPR.RepoName, dbPR.PRNumber),
				}

				// In single-user mode, determine if the PR is "mine" to avoid self-review
				isMine := p.cfg.GitHubUsername != "" && strings.EqualFold(dbPR.Author, p.cfg.GitHubUsername)

				// Add to appropriate list based on ownership
				if isMine {
					myPRs = append(myPRs, ghPR)
				} else {
					reviewPRs = append(reviewPRs, ghPR)
				}
				pendingCount++
			}
		}
		if pendingCount > 0 {
			log.Printf("[POLL] Found %d pending PRs in database to process", pendingCount)
		}
		if skippedCount > 0 {
			log.Printf("[POLL] Skipped %d pending PRs (auto-review disabled)", skippedCount)
		}
	}

	// Combine myPRs and reviewPRs for processing - both get AI reviews
	allPRsToProcess := append(reviewPRs, myPRs...)

	// Group all PRs by repository for batch processing
	prsByRepo := make(map[string][]github.PullRequest)
	for _, pr := range allPRsToProcess {
		repoKey := fmt.Sprintf("%s/%s", pr.Owner, pr.Repo)
		prsByRepo[repoKey] = append(prsByRepo[repoKey], pr)
	}

	// Process PRs in smaller batches
	log.Printf("[POLL] Processing %d repositories for PRs", len(prsByRepo))
	for repoKey, repoPRs := range prsByRepo {
		log.Printf("[POLL] Processing PRs for repository %s with %d PRs", repoKey, len(repoPRs))
		// Split into smaller batches of 5 PRs to avoid timeout
		p.processInBatches(ctx, repoPRs, 5, autoReviewEnabled)
	}

	duration := time.Since(startTime)
	log.Printf("[POLL] Poll completed in %v", duration)
}

func (p *Poller) processInBatches(ctx context.Context, prs []github.PullRequest, batchSize int, autoReviewEnabled bool) {
	for i := 0; i < len(prs); i += batchSize {
		end := i + batchSize
		if end > len(prs) {
			end = len(prs)
		}
		batch := prs[i:end]
		log.Printf("[POLL] Processing batch %d-%d of %d PRs", i+1, end, len(prs))
		if err := p.processPRBatch(ctx, batch, autoReviewEnabled); err != nil {
			log.Printf("[POLL] ERROR: Batch %d-%d failed: %v", i+1, end, err)
		} else {
			log.Printf("[POLL] Successfully processed batch %d-%d", i+1, end)
		}
	}
}

func (p *Poller) processPRBatch(ctx context.Context, prs []github.PullRequest, autoReviewEnabled bool) error {
	if len(prs) == 0 {
		return nil
	}

	log.Printf("[BATCH] Processing %d PRs", len(prs))

	// Auto-review setting is now passed from poll() - no duplicate query needed

	var prsToReview []github.PullRequest

	for _, pr := range prs {
		existingPR, err := p.db.GetPR(pr.Owner, pr.Repo, pr.Number)
		if err != nil {
			log.Printf("[BATCH] WARNING: Could not get existing PR for %s/%s#%d: %v", pr.Owner, pr.Repo, pr.Number, err)
			// Continue to try and process if we can
		}

		isNew := existingPR == nil
		if isNew {
			existingPR = &db.PR{Status: "pending"}
		}

		// Determine if we should generate a review
		shouldGenerate := shouldReview(pr, existingPR, p.isTracked(pr.Owner, pr.Repo, pr.Number), autoReviewEnabled)

		if shouldGenerate && existingPR.Status == "generating" && !p.isTracked(pr.Owner, pr.Repo, pr.Number) {
			log.Printf("[BATCH] Manual trigger detected for %s/%s#%d", pr.Owner, pr.Repo, pr.Number)
		}

		// Always update metadata first
		existingPR.RepoOwner = pr.Owner
		existingPR.RepoName = pr.Repo
		existingPR.PRNumber = pr.Number
		existingPR.LastCommitSHA = pr.CommitSHA
		existingPR.Title = pr.Title
		existingPR.Author = pr.Author
		existingPR.CreatedAt = pr.CreatedAt
		existingPR.Draft = pr.Draft

		if err := p.db.UpsertPR(existingPR); err != nil {
			log.Printf("[BATCH] ERROR: Failed to upsert PR metadata for %s/%s#%d: %v", pr.Owner, pr.Repo, pr.Number, err)
		} else {
			if isNew {
				if p.EventFunc != nil {
					p.EventFunc("pr_created", map[string]interface{}{
						"owner":  pr.Owner,
						"repo":   pr.Repo,
						"number": pr.Number,
					})
				}
			} else {
				// Broadcast update to ensure metadata (title, author, etc) is fresh in UI
				p.broadcastPRUpdate(pr.Owner, pr.Repo, pr.Number)
			}
		}

		if shouldGenerate {
			// Add to queue
			prsToReview = append(prsToReview, pr)
		}
	}

	if len(prsToReview) == 0 {
		return nil
	}

	// Mark all PRs as generating (if not already)
	log.Printf("[BATCH] Queuing %d PRs for generation", len(prsToReview))

	owner := prsToReview[0].Owner
	repo := prsToReview[0].Repo
	prNumbers := getPRNumbers(prsToReview)
	log.Printf("[BATCH] Starting reviewer batch for %s/%s PRs: %v", owner, repo, prNumbers)

	startTime := time.Now()
	// Generate reviews using native reviewer (batch)
	batchErr := p.generateReviewsBatch(ctx, prsToReview)
	duration := time.Since(startTime)

	if batchErr != nil {
		log.Printf("[BATCH] ERROR: reviewer batch failed after %v: %v", duration, batchErr)
	} else {
		log.Printf("[BATCH] reviewer batch completed in %v", duration)
	}

	return nil
}

func getPRNumbers(prs []github.PullRequest) []int {
	nums := make([]int, len(prs))
	for i, pr := range prs {
		nums[i] = pr.Number
	}
	return nums
}

func (p *Poller) UpdatePRStatus(owner, repo string, prNumber int, status string) error {
	if err := p.db.UpdatePRStatus(owner, repo, prNumber, status); err != nil {
		return err
	}
	p.broadcastPRUpdate(owner, repo, prNumber)
	return nil
}

func (p *Poller) generateReviewsBatch(ctx context.Context, prs []github.PullRequest) error {
	if len(prs) == 0 {
		return nil
	}

	// If using mock generator (for testing), skip LLM client initialization
	var reviewSvc *service.Service
	if p.reviewGenerator == nil {
		// Initialize reviewer clients
		smartLlmClient := llm.NewClient(llm.ProviderGemini, p.cfg.GeminiAPIKey, false, false)
		fastLlmClient := llm.NewClient(llm.ProviderGemini, p.cfg.GeminiAPIKey, true, false)

		// Validate API Key once
		if err := smartLlmClient.ValidateAPIKey(); err != nil {
			return fmt.Errorf("Gemini API key validation failed: %w", err)
		}

		reviewSvc = service.NewService(p.ghClientConcrete, smartLlmClient, fastLlmClient)
	}

	// Concurrency limit: 5 parallel reviews
	concurrencyLimit := 5
	sem := make(chan struct{}, concurrencyLimit)
	var wg sync.WaitGroup

	// Process each PR concurrently
	for _, pr := range prs {
		wg.Add(1)
		sem <- struct{}{} // Acquire token

		go func(pr github.PullRequest) {
			defer wg.Done()
			defer func() { <-sem }() // Release token

			log.Printf("[REVIEWER] Processing PR: %s/%s#%d (commit: %s)", pr.Owner, pr.Repo, pr.Number, pr.CommitSHA[:7])

			// Check if review already exists (in GCS if configured, otherwise locally)
			exists, existsErr := p.reviewExists(ctx, pr.Owner, pr.Repo, pr.Number, pr.CommitSHA)
			if existsErr != nil {
				log.Printf("[REVIEWER] Warning: Failed to check for existing review: %v", existsErr)
				// Continue anyway - will regenerate if needed
			} else if exists {
				log.Printf("[REVIEWER] Review already exists for PR %d commit %s, skipping generation", pr.Number, pr.CommitSHA[:7])
				// Update database to point to existing review, preserving importance counts
				filename := gcs.ReviewFileName(pr.Owner, pr.Repo, pr.Number, pr.CommitSHA)
				// Get existing importance counts from database
				existingPR, _ := p.db.GetPR(pr.Owner, pr.Repo, pr.Number)
				criticalCount, mediumCount, lowCount := 0, 0, 0
				if existingPR != nil {
					criticalCount = existingPR.CriticalCount
					mediumCount = existingPR.MediumCount
					lowCount = existingPR.LowCount
				}
				if err := p.upsertPRPreservingReviewData(ctx, pr.Owner, pr.Repo, pr.Number, pr.CommitSHA, filename, "completed", pr.Title, pr.Author, pr.CreatedAt, pr.Draft, criticalCount, mediumCount, lowCount); err != nil {
					log.Printf("[REVIEWER] ERROR: Failed to update DB for existing review: %v", err)
				}
				return
			}

			// Set status to generating
			if err := p.db.SetPRGenerating(pr.Owner, pr.Repo, pr.Number, pr.CommitSHA, pr.Title, pr.Author, pr.CreatedAt, pr.Draft); err != nil {
				log.Printf("[BATCH] ERROR: Failed to set generating status for %s/%s#%d: %v", pr.Owner, pr.Repo, pr.Number, err)
				return
			}
			p.broadcastPRUpdate(pr.Owner, pr.Repo, pr.Number)

			// Track this review (using dummy PID since we're running in-process)
			p.trackReview(pr.Owner, pr.Repo, pr.Number, 0)

			execStart := time.Now()

			nRequests, _ := p.db.GetReviewNRequests()

			// Generate review using mock interface (testing) or real service
			var err error
			var reviewResult *ReviewResult
			if p.reviewGenerator != nil {
				// Use mock generator for testing
				genCfg := ReviewGeneratorConfig{
					Token:        "",
					Owner:        pr.Owner,
					RepoName:     pr.Repo,
					PRNumber:     pr.Number,
					CommitSHA:    pr.CommitSHA,
					WithComments: false,
					Verbose:      false,
					Fast:         false,
					NRequests:    nRequests,
				}
				reviewResult, err = p.reviewGenerator.GenerateReview(ctx, genCfg)
			} else {
				// Use real reviewer service
				reviewCfg := service.PerformReviewConfig{
					Token:        p.cfg.GitHubToken,
					Owner:        pr.Owner,
					RepoName:     pr.Repo,
					PRNumber:     pr.Number,
					WithComments: false,
					Verbose:      false,
					Fast:         false,
					NRequests:    nRequests,
				}

				result, svcErr := reviewSvc.PerformReview(reviewCfg)
				if svcErr != nil {
					err = svcErr
				} else {
					// Generate HTML Report content
					htmlContent := service.GenerateHTMLReportContent(result, pr.Number, pr.Owner, pr.Repo, pr.CommitSHA, llm.ProModel)
					if htmlContent == nil {
						err = fmt.Errorf("failed to generate HTML content")
					} else {
						reviewResult = &ReviewResult{
							HTMLContent:   htmlContent,
							CriticalCount: result.CriticalCount,
							MediumCount:   result.MediumCount,
							LowCount:      result.LowCount,
						}
					}
				}
			}
			execDuration := time.Since(execStart)

			if err != nil {
				log.Printf("[REVIEWER] ERROR: Review failed for PR %d after %v: %v", pr.Number, execDuration, err)

				// Check if outdated
				currentPR, dbErr := p.db.GetPR(pr.Owner, pr.Repo, pr.Number)
				if dbErr == nil && currentPR != nil && currentPR.Status == "pending" && currentPR.LastCommitSHA != pr.CommitSHA {
					log.Printf("[REVIEWER] Review for PR %d was cancelled because it became outdated.", pr.Number)
				} else {
					_ = p.UpdatePRStatus(pr.Owner, pr.Repo, pr.Number, "error") // nolint:errcheck
				}
				p.untrackReview(pr.Owner, pr.Repo, pr.Number)
				return
			}

			log.Printf("[REVIEWER] Review completed successfully for PR %d in %v", pr.Number, execDuration)

			// Save review (to GCS if configured, otherwise locally)
			filename, err := p.saveReview(ctx, pr.Owner, pr.Repo, pr.Number, pr.CommitSHA, reviewResult.HTMLContent)
			if err != nil {
				log.Printf("[REVIEWER] ERROR: Failed to save review for PR %d: %v", pr.Number, err)
				_ = p.UpdatePRStatus(pr.Owner, pr.Repo, pr.Number, "error") // nolint:errcheck
				p.untrackReview(pr.Owner, pr.Repo, pr.Number)
				return
			}

			log.Printf("[REVIEWER] Saved review: %s", filename)

			// Verify commit SHA matches (hasn't changed during generation)
			currentPR, err := p.db.GetPR(pr.Owner, pr.Repo, pr.Number)
			if err != nil {
				log.Printf("[REVIEWER] ERROR: Failed to fetch PR from DB: %v", err)
			} else if currentPR != nil && currentPR.LastCommitSHA != pr.CommitSHA {
				log.Printf("[REVIEWER] STALE REVIEW: PR %d commit changed during generation, but keeping in GCS for history", pr.Number)
				// Don't update DB - the next poll will generate a new review for the new commit
			} else {
				if err := p.upsertPRPreservingReviewData(ctx, pr.Owner, pr.Repo, pr.Number, pr.CommitSHA, filename, "completed", pr.Title, pr.Author, pr.CreatedAt, pr.Draft, reviewResult.CriticalCount, reviewResult.MediumCount, reviewResult.LowCount); err != nil {
					log.Printf("[REVIEWER] ERROR: Failed to update DB for PR %d: %v", pr.Number, err)
				} else {
					log.Printf("[REVIEWER] Marked PR %d as 'completed' (critical=%d, medium=%d, low=%d)", pr.Number, reviewResult.CriticalCount, reviewResult.MediumCount, reviewResult.LowCount)
				}
			}

			p.untrackReview(pr.Owner, pr.Repo, pr.Number)
		}(pr)
	}

	wg.Wait()
	return nil
}

// shouldReview determines if a PR should be processed for review generation
// based on its current state, database state, tracking status, and global settings.
func shouldReview(pr github.PullRequest, dbPR *db.PR, isTracked bool, autoReviewEnabled bool) bool {
	if dbPR == nil {
		return false // New PRs are not reviewed until they are persisted
	}

	// Condition 1: Manual Trigger
	// The PR is marked as 'generating' in DB (by user action), but we aren't tracking a process for it yet.
	isManualTrigger := dbPR.Status == "generating" && !isTracked

	if isManualTrigger {
		return true
	}

	// Condition 2: Auto Candidate
	// The PR is 'pending' AND auto-review is globally enabled.
	isAutoCandidate := dbPR.Status == "pending" && autoReviewEnabled

	if isAutoCandidate {
		// Additional check: Don't auto-generate if already completed for this commit
		if dbPR.LastCommitSHA == pr.CommitSHA && dbPR.Status == "completed" {
			return false
		}
		return true
	}

	return false
}
