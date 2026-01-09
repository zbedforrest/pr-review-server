package poller

import (
	"context"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sync"
	"syscall"
	"time"

	"pr-review-server/config"
	"pr-review-server/db"
	"pr-review-server/github"
)

type Poller struct {
	cfg             *config.Config
	db              *db.DB
	ghClient        *github.Client
	reviewDir       string
	cacheUpdateFunc func([]github.PullRequest)
	triggerChan     chan struct{}
	polling         bool
	pollMutex       sync.Mutex
	cbprPID         int
	cbprStartTime   time.Time
	cbprMutex       sync.Mutex
	// Track active review processes for cancellation
	activeReviews map[string]int // prKey (owner/repo/number) -> PID
	reviewsMutex  sync.Mutex
	// Track last poll time for countdown display
	lastPollTime time.Time
	pollTimeMutex sync.RWMutex
	// Track ticker start time for accurate countdown
	tickerStartTime time.Time
}

func New(cfg *config.Config, database *db.DB, ghClient *github.Client) *Poller {
	return &Poller{
		cfg:           cfg,
		db:            database,
		ghClient:      ghClient,
		reviewDir:     cfg.ReviewsDir,
		triggerChan:   make(chan struct{}, 1), // Buffered to prevent blocking
		activeReviews: make(map[string]int),
	}
}

// upsertPRPreservingReviewData upserts a PR while preserving existing review data (doesn't fetch from GitHub)
// This is used when updating PR status/files but we want to keep approval counts unchanged
func (p *Poller) upsertPRPreservingReviewData(ctx context.Context, owner, repo string, prNumber int, commitSHA, htmlPath, status, title, author string, isMine bool, createdAt time.Time, draft bool) error {
	// Get existing PR to preserve review data
	existingPR, err := p.db.GetPR(owner, repo, prNumber)
	if err != nil {
		log.Printf("[DB] Warning: failed to get existing PR data for %s/%s#%d: %v", owner, repo, prNumber, err)
	}

	// Default to existing values (or 0 if no existing PR)
	approvalCount := 0
	myReviewStatus := ""
	if existingPR != nil {
		approvalCount = existingPR.ApprovalCount
		myReviewStatus = existingPR.MyReviewStatus
	}

	return p.db.UpsertPR(owner, repo, prNumber, commitSHA, htmlPath, status, title, author, isMine, approvalCount, myReviewStatus, createdAt, draft)
}

// upsertPRWithReviewData fetches review data from GitHub and upserts the PR in the database
// DEPRECATED: Use batch GraphQL fetching at poll level instead of individual calls
func (p *Poller) upsertPRWithReviewData(ctx context.Context, owner, repo string, prNumber int, commitSHA, htmlPath, status, title, author string, isMine bool, createdAt time.Time, draft bool) error {
	// Get existing PR to preserve values if we're rate limited
	existingPR, err := p.db.GetPR(owner, repo, prNumber)
	if err != nil {
		log.Printf("[REVIEW_DATA] Warning: failed to get existing PR data for %s/%s#%d: %v", owner, repo, prNumber, err)
	}

	// Default to existing values (or 0 if no existing PR)
	approvalCount := 0
	myReviewStatus := ""
	if existingPR != nil {
		approvalCount = existingPR.ApprovalCount
		myReviewStatus = existingPR.MyReviewStatus
	}

	// Try to fetch fresh approval count
	if approvalCountVal, wasRateLimited, err := p.ghClient.GetApprovalCount(ctx, owner, repo, prNumber); err != nil {
		if wasRateLimited {
			log.Printf("[REVIEW_DATA] RATE LIMITED: Preserving existing approval count (%d) for %s/%s#%d", approvalCount, owner, repo, prNumber)
		} else {
			log.Printf("[REVIEW_DATA] Warning: failed to fetch approval count for %s/%s#%d: %v", owner, repo, prNumber, err)
		}
		// Keep existing approvalCount value
	} else {
		// Successfully fetched new value
		approvalCount = approvalCountVal
	}

	// Fetch my review status only for PRs to review (not my PRs)
	if !isMine {
		if reviewStatus, wasRateLimited, err := p.ghClient.GetMyReviewStatus(ctx, owner, repo, prNumber); err != nil {
			if wasRateLimited {
				log.Printf("[REVIEW_DATA] RATE LIMITED: Preserving existing review status (%s) for %s/%s#%d", myReviewStatus, owner, repo, prNumber)
			} else {
				log.Printf("[REVIEW_DATA] Warning: failed to fetch review status for %s/%s#%d: %v", owner, repo, prNumber, err)
			}
			// Keep existing myReviewStatus value
		} else {
			// Successfully fetched new value
			myReviewStatus = reviewStatus
		}
	}

	return p.db.UpsertPR(owner, repo, prNumber, commitSHA, htmlPath, status, title, author, isMine, approvalCount, myReviewStatus, createdAt, draft)
}

func (p *Poller) SetCacheUpdateFunc(f func([]github.PullRequest)) {
	p.cacheUpdateFunc = f
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

	// Start cbpr process monitor
	monitorTicker := time.NewTicker(30 * time.Second)
	defer monitorTicker.Stop()
	go p.monitorCbprProcesses(ctx, monitorTicker)

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

func (p *Poller) monitorCbprProcesses(ctx context.Context, ticker *time.Ticker) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			p.cbprMutex.Lock()
			if p.cbprPID != 0 {
				elapsed := time.Since(p.cbprStartTime)
				if elapsed > 5*time.Minute {
					log.Printf("[MONITOR] WARNING: cbpr process %d has been running for %v, killing it", p.cbprPID, elapsed)
					// Kill the process
					process, err := os.FindProcess(p.cbprPID)
					if err == nil {
						process.Kill()
					}
					p.cbprPID = 0
				} else if elapsed > 2*time.Minute {
					log.Printf("[MONITOR] WARNING: cbpr process %d has been running for %v (threshold: 2m)", p.cbprPID, elapsed)
				} else {
					log.Printf("[MONITOR] cbpr process %d running normally (%v elapsed)", p.cbprPID, elapsed)
				}
			}
			p.cbprMutex.Unlock()
		}
	}
}

func (p *Poller) GetCbprStatus() (running bool, duration time.Duration) {
	p.cbprMutex.Lock()
	defer p.cbprMutex.Unlock()
	if p.cbprPID != 0 {
		// Verify the process is actually still running
		if !p.isPIDRunning(p.cbprPID) {
			log.Printf("[MONITOR] WARNING: Tracked PID %d is no longer running, clearing", p.cbprPID)
			p.cbprPID = 0
			return false, 0
		}
		return true, time.Since(p.cbprStartTime)
	}
	return false, 0
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

func (p *Poller) isPIDRunning(pid int) bool {
	process, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	// Signal 0 doesn't actually send a signal, but checks if we can
	err = process.Signal(syscall.Signal(0))
	return err == nil
}

// prKey creates a unique key for tracking a PR
func prKey(owner, repo string, number int) string {
	return fmt.Sprintf("%s/%s#%d", owner, repo, number)
}

// trackReview adds a PR's review process to the active reviews map
func (p *Poller) trackReview(owner, repo string, number, pid int) {
	p.reviewsMutex.Lock()
	defer p.reviewsMutex.Unlock()
	key := prKey(owner, repo, number)
	p.activeReviews[key] = pid
	log.Printf("[TRACK] Tracking review for %s with PID %d", key, pid)
}

// untrackReview removes a PR's review process from the active reviews map
func (p *Poller) untrackReview(owner, repo string, number int) {
	p.reviewsMutex.Lock()
	defer p.reviewsMutex.Unlock()
	key := prKey(owner, repo, number)
	delete(p.activeReviews, key)
	log.Printf("[TRACK] Untracked review for %s", key)
}

// killReview kills an active review process if it exists
func (p *Poller) killReview(owner, repo string, number int) bool {
	p.reviewsMutex.Lock()
	key := prKey(owner, repo, number)
	pid, exists := p.activeReviews[key]
	p.reviewsMutex.Unlock()

	if !exists {
		return false
	}

	log.Printf("[KILL] Attempting to kill review process for %s (PID %d)", key, pid)
	process, err := os.FindProcess(pid)
	if err != nil {
		log.Printf("[KILL] Failed to find process %d: %v", pid, err)
		return false
	}

	if err := process.Kill(); err != nil {
		log.Printf("[KILL] Failed to kill process %d: %v", pid, err)
		return false
	}

	log.Printf("[KILL] Successfully killed process %d for %s", pid, key)
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

		// If PR is closed, remove it
		if !isOpen {
			log.Printf("[CLEANUP] PR %s/%s#%d is closed, removing from system",
				pr.RepoOwner, pr.RepoName, pr.PRNumber)

			// Delete HTML file if it exists
			if pr.ReviewHTMLPath != "" {
				htmlPath := filepath.Join(p.reviewDir, pr.ReviewHTMLPath)
				if err := os.Remove(htmlPath); err != nil && !os.IsNotExist(err) {
					log.Printf("[CLEANUP] Warning: Failed to delete HTML file %s: %v", htmlPath, err)
				} else if err == nil {
					log.Printf("[CLEANUP] Deleted HTML file: %s", htmlPath)
				}
			}

			// Delete from database
			if err := p.db.DeletePR(pr.RepoOwner, pr.RepoName, pr.PRNumber); err != nil {
				log.Printf("[CLEANUP] ERROR: Failed to delete PR %s/%s#%d from database: %v",
					pr.RepoOwner, pr.RepoName, pr.PRNumber, err)
				continue
			}

			log.Printf("[CLEANUP] Successfully removed closed PR %s/%s#%d",
				pr.RepoOwner, pr.RepoName, pr.PRNumber)
			removed++
		}
	}

	return removed, nil
}

// speak uses platform-appropriate TTS command for voice notifications
// macOS: say command, Linux: espeak-ng
func (p *Poller) speak(message string) {
	if !p.cfg.EnableVoiceNotifications {
		log.Printf("[VOICE] Skipped (disabled): %s", message)
		return
	}

	log.Printf("[VOICE] Speaking: %s", message)

	// Run TTS command in a goroutine to avoid blocking and prevent zombie processes
	go func() {
		var cmd *exec.Cmd

		switch runtime.GOOS {
		case "darwin":
			// macOS: use say command
			cmd = exec.Command("say", message)
		case "linux":
			// Linux: use espeak-ng with reasonable speed and voice
			cmd = exec.Command("espeak-ng", "-s", "175", message)
		default:
			log.Printf("[VOICE] ERROR: Unsupported OS %s", runtime.GOOS)
			return
		}

		if err := cmd.Run(); err != nil {
			log.Printf("[VOICE] ERROR: TTS command failed on %s: %v", runtime.GOOS, err)
		}
	}()
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

		log.Printf("[BACKFILL] Updated metadata for PR %s/%s#%d: %s by %s",
			pr.RepoOwner, pr.RepoName, pr.PRNumber, title, author)
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
		// Check PRs that are completed OR currently generating
		// If a PR is generating and gets a new commit, we need to cancel and restart
		if pr.Status != "completed" && pr.Status != "generating" {
			continue
		}

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
			statusMsg := "completed"
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

			// Voice notification for outdated review
			var message string
			if wasGenerating {
				message = fmt.Sprintf("PR number %d has a new commit while generating. Cancelling old review and starting fresh.", pr.PRNumber)
			} else {
				message = fmt.Sprintf("PR number %d has a new commit. Removing stale review and generating a new one.", pr.PRNumber)
			}
			p.speak(message)

			outdated++
		}
	}

	if checkedCount > 0 {
		log.Printf("[OUTDATED] Checked %d PRs (%d completed or generating)", checkedCount, len(allPRs))
	}

	return outdated, nil
}

func (p *Poller) poll(ctx context.Context) {
	startTime := time.Now()

	// Update last poll time for countdown display
	p.pollTimeMutex.Lock()
	p.lastPollTime = startTime
	p.pollTimeMutex.Unlock()

	log.Printf("[POLL] Starting poll at %s", startTime.Format("15:04:05"))

	// Reset any PRs stuck in "generating" for more than 2 minutes
	log.Printf("[POLL] Checking for stale PRs...")
	resetCount, err := p.db.ResetStaleGeneratingPRs(2)
	if err != nil {
		log.Printf("[POLL] ERROR: Failed to reset stale PRs: %v", err)
	} else if resetCount > 0 {
		log.Printf("[POLL] Reset %d stale PRs from 'generating' to 'pending'", resetCount)
	} else {
		log.Printf("[POLL] No stale PRs found")
	}

	// Reset PRs in error state that are older than 5 minutes (self-healing)
	log.Printf("[POLL] Checking for error PRs to retry...")
	errorResetCount, err := p.db.ResetErrorPRs(5)
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

	log.Printf("[POLL] Fetching PRs requesting review from GitHub...")
	reviewPRs, err := p.ghClient.GetPRsRequestingReview(ctx)
	if err != nil {
		log.Printf("[POLL] ERROR: Failed to fetch PRs requesting review: %v", err)
		// Continue even if this fails - we can still process "my PRs"
		reviewPRs = []github.PullRequest{}
	} else {
		log.Printf("[POLL] Found %d PRs requesting review", len(reviewPRs))

		// Check for new PRs (not in database yet) and announce them
		for _, pr := range reviewPRs {
			existingPR, err := p.db.GetPR(pr.Owner, pr.Repo, pr.Number)
			if err == nil && existingPR == nil {
				// This is a new PR
				message := fmt.Sprintf("Your review is newly requested on PR number %d", pr.Number)
				p.speak(message)
				log.Printf("[VOICE] New review request: PR #%d", pr.Number)
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
		for _, dbPR := range dbPRsForReviewUpdate {
			key := fmt.Sprintf("%s/%s/%d", dbPR.RepoOwner, dbPR.RepoName, dbPR.PRNumber)
			if _, exists := prMap[key]; !exists {
				// Add this PR from database
				allPRs = append(allPRs, github.PullRequest{
					Owner:     dbPR.RepoOwner,
					Repo:      dbPR.RepoName,
					Number:    dbPR.PRNumber,
					CommitSHA: dbPR.LastCommitSHA,
					Title:     dbPR.Title,
					Author:    dbPR.Author,
					URL:       fmt.Sprintf("https://github.com/%s/%s/pull/%d", dbPR.RepoOwner, dbPR.RepoName, dbPR.PRNumber),
				})
			}
		}
		log.Printf("[POLL] Added %d database PRs to review update list (total: %d PRs)", len(allPRs)-len(prMap), len(allPRs))
	}

	// Batch fetch review data for all PRs using GraphQL (much more efficient)
	log.Printf("[POLL] Batch fetching review data for %d PRs using GraphQL...", len(allPRs))
	if len(allPRs) > 0 {
		reviewDataMap, err := p.ghClient.BatchGetPRReviewData(ctx, allPRs)
		if err != nil {
			log.Printf("[POLL] WARNING: Failed to batch fetch review data: %v", err)
		} else {
			// Update database with batch review data
			updateCount := 0
			for _, pr := range allPRs {
				key := fmt.Sprintf("%s/%s/%d", pr.Owner, pr.Repo, pr.Number)
				if reviewData, exists := reviewDataMap[key]; exists {
					// Update PR with review data
					existingPR, err := p.db.GetPR(pr.Owner, pr.Repo, pr.Number)
					if err != nil || existingPR == nil {
						continue
					}

					// Determine if this is my PR
					isMine := existingPR.IsMine

					// Update approval count, my review status, and draft status (always use fresh value from GitHub)
					err = p.db.UpsertPR(
						pr.Owner, pr.Repo, pr.Number,
						existingPR.LastCommitSHA,
						existingPR.ReviewHTMLPath,
						existingPR.Status,
						existingPR.Title,
						existingPR.Author,
						isMine,
						reviewData.ApprovalCount,
						reviewData.MyReviewStatus,
						pr.CreatedAt,
						pr.Draft, // IMPORTANT: Always use fresh draft status from GitHub, never cached value
					)
					if err != nil {
						log.Printf("[POLL] ERROR: Failed to update review data for %s/%s#%d: %v", pr.Owner, pr.Repo, pr.Number, err)
					} else {
						updateCount++
					}
				}
			}
			log.Printf("[POLL] Successfully updated review data for %d/%d PRs", updateCount, len(allPRs))
		}
	}

	// CRITICAL: Also check database for pending PRs that need processing
	// This ensures we process PRs even when GitHub API fails
	log.Printf("[POLL] Checking database for pending PRs...")
	dbPRs, err := p.db.GetAllPRs()
	if err != nil {
		log.Printf("[POLL] ERROR: Failed to get PRs from database: %v", err)
	} else {
		pendingCount := 0
		for _, dbPR := range dbPRs {
			if dbPR.Status == "pending" {
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

				// Add to appropriate list based on is_mine flag
				if dbPR.IsMine {
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
	}

	// Group review PRs by repository for batch processing
	reviewPRsByRepo := make(map[string][]github.PullRequest)
	for _, pr := range reviewPRs {
		repoKey := fmt.Sprintf("%s/%s", pr.Owner, pr.Repo)
		reviewPRsByRepo[repoKey] = append(reviewPRsByRepo[repoKey], pr)
	}

	// Group my PRs by repository for batch processing
	myPRsByRepo := make(map[string][]github.PullRequest)
	for _, pr := range myPRs {
		repoKey := fmt.Sprintf("%s/%s", pr.Owner, pr.Repo)
		myPRsByRepo[repoKey] = append(myPRsByRepo[repoKey], pr)
	}

	// Process review PRs in smaller batches
	log.Printf("[POLL] Processing %d repositories for review PRs", len(reviewPRsByRepo))
	for repoKey, repoPRs := range reviewPRsByRepo {
		log.Printf("[POLL] Processing review PRs for repository %s with %d PRs", repoKey, len(repoPRs))
		// Split into smaller batches of 5 PRs to avoid timeout
		p.processInBatches(ctx, repoPRs, false, 5)
	}

	// Process my PRs in smaller batches
	log.Printf("[POLL] Processing %d repositories for my PRs", len(myPRsByRepo))
	for repoKey, repoPRs := range myPRsByRepo {
		log.Printf("[POLL] Processing my PRs for repository %s with %d PRs", repoKey, len(repoPRs))
		// Split into smaller batches of 5 PRs to avoid timeout
		p.processInBatches(ctx, repoPRs, true, 5)
	}

	duration := time.Since(startTime)
	log.Printf("[POLL] Poll completed in %v", duration)
}

func (p *Poller) processInBatches(ctx context.Context, prs []github.PullRequest, isMine bool, batchSize int) {
	for i := 0; i < len(prs); i += batchSize {
		end := i + batchSize
		if end > len(prs) {
			end = len(prs)
		}
		batch := prs[i:end]
		log.Printf("[POLL] Processing batch %d-%d of %d PRs", i+1, end, len(prs))
		if err := p.processPRBatch(ctx, batch, isMine); err != nil {
			log.Printf("[POLL] ERROR: Batch %d-%d failed: %v", i+1, end, err)
		} else {
			log.Printf("[POLL] Successfully processed batch %d-%d", i+1, end)
		}
	}
}

func (p *Poller) processPRBatch(ctx context.Context, prs []github.PullRequest, isMine bool) error {
	if len(prs) == 0 {
		return nil
	}

	prType := "review"
	if isMine {
		prType = "my"
	}
	log.Printf("[BATCH] Processing %d %s PRs", len(prs), prType)

	// Filter PRs that need review
	var prsToReview []github.PullRequest
	for _, pr := range prs {
		existingPR, err := p.db.GetPR(pr.Owner, pr.Repo, pr.Number)
		if err != nil {
			log.Printf("Error checking PR %s/%s#%d: %v", pr.Owner, pr.Repo, pr.Number, err)
			continue
		}

		// Check if this is a new commit for an existing PR (outdated review)
		// This is a safeguard against commits pushed after checkForOutdatedReviews() ran at poll start
		// but before this batch processing began. Ensures we don't regenerate stale reviews.
		if existingPR != nil && existingPR.LastCommitSHA != pr.CommitSHA && (existingPR.Status == "completed" || existingPR.Status == "generating") {
			log.Printf("[PROCESSING] PR %s/%s#%d has new commit (old: %s, new: %s), will regenerate",
				pr.Owner, pr.Repo, pr.Number, existingPR.LastCommitSHA[:7], pr.CommitSHA[:7])
			wasGenerating := existingPR.Status == "generating"
			var message string
			if wasGenerating {
				message = fmt.Sprintf("PR number %d has a new commit while generating. Cancelling old review and starting fresh.", pr.Number)
			} else {
				message = fmt.Sprintf("PR number %d has a new commit. Removing stale review and generating a new one.", pr.Number)
			}
			p.speak(message)
		}

		// Skip if already reviewed at this commit AND HTML file exists
		if existingPR != nil && existingPR.LastCommitSHA == pr.CommitSHA && existingPR.Status == "completed" {
			// Verify HTML file actually exists
			htmlExists := true
			if existingPR.ReviewHTMLPath != "" {
				absReviewDir, _ := filepath.Abs(p.reviewDir)
				htmlPath := filepath.Join(absReviewDir, existingPR.ReviewHTMLPath)
				if _, err := os.Stat(htmlPath); os.IsNotExist(err) {
					htmlExists = false
					log.Printf("PR %s/%s#%d marked as completed but HTML missing, will regenerate", pr.Owner, pr.Repo, pr.Number)
				}
			}
			if htmlExists {
				log.Printf("PR %s/%s#%d already reviewed at commit %s", pr.Owner, pr.Repo, pr.Number, pr.CommitSHA)
				continue
			}
		}

		// Skip if currently generating
		if existingPR != nil && existingPR.Status == "generating" {
			log.Printf("PR %s/%s#%d is currently being reviewed, skipping", pr.Owner, pr.Repo, pr.Number)
			continue
		}

		prsToReview = append(prsToReview, pr)
	}

	if len(prsToReview) == 0 {
		return nil
	}

	// Mark all PRs as generating
	log.Printf("[BATCH] Marking %d %s PRs as 'generating'", len(prsToReview), prType)
	for _, pr := range prsToReview {
		if err := p.db.SetPRGenerating(pr.Owner, pr.Repo, pr.Number, pr.CommitSHA, pr.Title, pr.Author, isMine, pr.CreatedAt, pr.Draft); err != nil {
			log.Printf("[BATCH] ERROR: Failed to set generating status for %s/%s#%d: %v", pr.Owner, pr.Repo, pr.Number, err)
		}
	}

	owner := prsToReview[0].Owner
	repo := prsToReview[0].Repo
	prNumbers := getPRNumbers(prsToReview)
	log.Printf("[BATCH] Starting cbpr batch for %s/%s PRs: %v", owner, repo, prNumbers)

	startTime := time.Now()
	// Generate reviews using cbpr (batch)
	batchErr := p.generateReviewsBatch(ctx, prsToReview, isMine)
	duration := time.Since(startTime)

	if batchErr != nil {
		log.Printf("[BATCH] ERROR: cbpr batch failed after %v: %v", duration, batchErr)
		// Don't mark all as error immediately - check which files were actually created
		// This provides resilience against partial failures
	} else {
		log.Printf("[BATCH] cbpr batch completed in %v", duration)
	}

	// Check each PR individually to see if its file exists
	// This allows partial success recovery when cbpr is killed mid-execution
	absReviewDir, _ := filepath.Abs(p.reviewDir)
	completedCount := 0
	errorCount := 0

	for _, pr := range prsToReview {
		filename := fmt.Sprintf("%s_%s_%d.html", pr.Owner, pr.Repo, pr.Number)
		htmlPath := filepath.Join(absReviewDir, filename)

		if _, err := os.Stat(htmlPath); err == nil {
			// File exists - mark as completed (review data will be updated in batch later)
			if err := p.upsertPRPreservingReviewData(ctx, pr.Owner, pr.Repo, pr.Number, pr.CommitSHA, filename, "completed", pr.Title, pr.Author, isMine, pr.CreatedAt, pr.Draft); err != nil {
				log.Printf("[BATCH] ERROR: Failed to update DB for %s/%s#%d: %v", pr.Owner, pr.Repo, pr.Number, err)
			} else {
				completedCount++
			}
		} else {
			// File doesn't exist - mark as error
			p.db.UpdatePRStatus(pr.Owner, pr.Repo, pr.Number, "error")
			errorCount++
		}
	}

	log.Printf("[BATCH] Results: %d completed, %d errors (out of %d %s PRs)", completedCount, errorCount, len(prsToReview), prType)

	if batchErr != nil && completedCount == 0 {
		return fmt.Errorf("failed to generate reviews: %w", batchErr)
	}

	log.Printf("[BATCH] Successfully generated reviews for %s/%s PRs: %v", owner, repo, prNumbers)
	return nil
}

func getPRNumbers(prs []github.PullRequest) []int {
	nums := make([]int, len(prs))
	for i, pr := range prs {
		nums[i] = pr.Number
	}
	return nums
}

func (p *Poller) processPR(ctx context.Context, pr github.PullRequest, isMine bool) error {
	// Check if we've already reviewed this commit
	existingPR, err := p.db.GetPR(pr.Owner, pr.Repo, pr.Number)
	if err != nil {
		return fmt.Errorf("failed to get PR from DB: %w", err)
	}

	// If we've already reviewed this commit SHA and it's completed, skip
	if existingPR != nil && existingPR.LastCommitSHA == pr.CommitSHA && existingPR.Status == "completed" {
		log.Printf("PR %s/%s#%d already reviewed at commit %s", pr.Owner, pr.Repo, pr.Number, pr.CommitSHA)
		return nil
	}

	// Skip if currently generating
	if existingPR != nil && existingPR.Status == "generating" {
		log.Printf("PR %s/%s#%d is currently being reviewed, skipping", pr.Owner, pr.Repo, pr.Number)
		return nil
	}

	log.Printf("Generating review for %s/%s#%d (commit: %s)", pr.Owner, pr.Repo, pr.Number, pr.CommitSHA)

	// Set status to generating
	if err := p.db.SetPRGenerating(pr.Owner, pr.Repo, pr.Number, pr.CommitSHA, pr.Title, pr.Author, isMine, pr.CreatedAt, pr.Draft); err != nil {
		return fmt.Errorf("failed to set PR generating status: %w", err)
	}

	// Generate review using cbpr
	htmlPath, err := p.generateReview(ctx, pr)
	if err != nil {
		p.db.UpdatePRStatus(pr.Owner, pr.Repo, pr.Number, "error")
		return fmt.Errorf("failed to generate review: %w", err)
	}

	// Update database with completed status (review data will be updated in batch later)
	if err := p.upsertPRPreservingReviewData(ctx, pr.Owner, pr.Repo, pr.Number, pr.CommitSHA, htmlPath, "completed", pr.Title, pr.Author, isMine, pr.CreatedAt, pr.Draft); err != nil {
		return fmt.Errorf("failed to update DB: %w", err)
	}

	log.Printf("Successfully generated review for %s/%s#%d", pr.Owner, pr.Repo, pr.Number)
	return nil
}

func (p *Poller) generateReview(ctx context.Context, pr github.PullRequest) (string, error) {
	// Create filename for the review
	filename := fmt.Sprintf("%s_%s_%d.html", pr.Owner, pr.Repo, pr.Number)

	// Use absolute path for output
	absReviewDir, err := filepath.Abs(p.reviewDir)
	if err != nil {
		return "", fmt.Errorf("failed to get absolute path: %w", err)
	}
	outputPath := filepath.Join(absReviewDir, filename)

	// Ensure reviews directory exists
	if err := os.MkdirAll(absReviewDir, 0755); err != nil {
		return "", fmt.Errorf("failed to create reviews directory: %w", err)
	}

	// Build cbpr command
	repoName := fmt.Sprintf("%s/%s", pr.Owner, pr.Repo)
	cmd := exec.CommandContext(ctx,
		p.cfg.CbprPath,
		"review",
		fmt.Sprintf("--repo-name=%s", repoName),
		"-n", "3",
		"-p", fmt.Sprintf("%d", pr.Number),
		fmt.Sprintf("--output=%s", outputPath), // Specify output file directly
	)

	log.Printf("Running cbpr: %s %v", p.cfg.CbprPath, cmd.Args)
	log.Printf("Output path: %s", outputPath)

	// Capture output for debugging
	output, err := cmd.CombinedOutput()
	if err != nil {
		log.Printf("cbpr command failed with error: %v", err)
		if len(output) > 0 {
			log.Printf("cbpr output: %s", string(output))
		}
		return "", fmt.Errorf("cbpr command failed: %w", err)
	}

	// Verify file was created
	if _, err := os.Stat(outputPath); os.IsNotExist(err) {
		return "", fmt.Errorf("cbpr succeeded but file not created at %s", outputPath)
	}

	return filename, nil
}

func (p *Poller) generateReviewsBatch(ctx context.Context, prs []github.PullRequest, isMine bool) error {
	if len(prs) == 0 {
		return nil
	}

	// Use absolute path for output directory
	absReviewDir, err := filepath.Abs(p.reviewDir)
	if err != nil {
		return fmt.Errorf("failed to get absolute path: %w", err)
	}

	// Ensure reviews directory exists
	if err := os.MkdirAll(absReviewDir, 0755); err != nil {
		return fmt.Errorf("failed to create reviews directory: %w", err)
	}

	// Process each PR individually since cbpr doesn't write to cwd in batch mode
	// cbpr writes to temp dir when using --html --no-open, so we must use --output
	for i, pr := range prs {
		log.Printf("[CBPR] Processing PR %d/%d: %s/%s#%d", i+1, len(prs), pr.Owner, pr.Repo, pr.Number)

		filename := fmt.Sprintf("%s_%s_%d.html", pr.Owner, pr.Repo, pr.Number)
		outputPath := filepath.Join(absReviewDir, filename)

		// Build cbpr command with --output flag
		repoName := fmt.Sprintf("%s/%s", pr.Owner, pr.Repo)
		cmd := exec.CommandContext(ctx,
			p.cfg.CbprPath,
			"review",
			fmt.Sprintf("--repo-name=%s", repoName),
			"-n", "3",
			"-p", fmt.Sprintf("%d", pr.Number),
			fmt.Sprintf("--output=%s", outputPath),
		)

		log.Printf("[CBPR] Executing: cbpr review --repo-name=%s -n 3 -p %d --output=%s", repoName, pr.Number, outputPath)

		execStart := time.Now()

		// Track cbpr process
		if err := cmd.Start(); err != nil {
			log.Printf("[CBPR] ERROR: Failed to start command for PR %d: %v", pr.Number, err)
			continue // Skip to next PR
		}

		pid := cmd.Process.Pid

		p.cbprMutex.Lock()
		p.cbprPID = pid
		p.cbprStartTime = execStart
		p.cbprMutex.Unlock()

		// Track this review for cancellation
		p.trackReview(pr.Owner, pr.Repo, pr.Number, pid)

		log.Printf("[CBPR] Process started with PID %d", pid)

		// Wait for command to complete
		err := cmd.Wait()
		execDuration := time.Since(execStart)

		// Clear tracked process
		p.cbprMutex.Lock()
		p.cbprPID = 0
		p.cbprMutex.Unlock()

		if err != nil {
			log.Printf("[CBPR] ERROR: Command failed for PR %d after %v: %v", pr.Number, execDuration, err)

			// Before marking as error, check if the PR was cancelled due to being outdated.
			// If so, another poll cycle has already handled it, and we should not overwrite the status.
			currentPR, dbErr := p.db.GetPR(pr.Owner, pr.Repo, pr.Number)
			if dbErr == nil && currentPR != nil && currentPR.Status == "pending" && currentPR.LastCommitSHA != pr.CommitSHA {
				log.Printf("[CBPR] Review for PR %d was cancelled because it became outdated. The PR is already re-queued.", pr.Number)
			} else {
				// Mark as error only for genuine failures
				p.db.UpdatePRStatus(pr.Owner, pr.Repo, pr.Number, "error")
				log.Printf("[CBPR] Marked PR %d as 'error' in database", pr.Number)
			}

			// Untrack after DB operation completes
			p.untrackReview(pr.Owner, pr.Repo, pr.Number)
			continue // Skip to next PR
		}

		log.Printf("[CBPR] Command completed successfully for PR %d in %v", pr.Number, execDuration)

		// Verify file was created and update status immediately
		if _, err := os.Stat(outputPath); os.IsNotExist(err) {
			log.Printf("[CBPR] ERROR: File not created for PR %d: %s", pr.Number, outputPath)
			// Mark as error immediately
			p.db.UpdatePRStatus(pr.Owner, pr.Repo, pr.Number, "error")
			log.Printf("[CBPR] Marked PR %d as 'error' in database", pr.Number)
		} else {
			log.Printf("[CBPR] Verified file exists: %s", filename)

			// Before marking as completed, verify the commit SHA hasn't changed
			// Protects against race condition where a new commit is pushed AFTER cbpr starts generating
			// but BEFORE it finishes. In this case, we discard the stale review and let the outdated
			// review detection on the next poll cycle regenerate with the latest commit.
			currentPR, err := p.db.GetPR(pr.Owner, pr.Repo, pr.Number)
			if err != nil {
				log.Printf("[CBPR] ERROR: Failed to fetch PR from DB: %v", err)
			} else if currentPR != nil && currentPR.LastCommitSHA != pr.CommitSHA {
				// Commit has changed since we started - discard this stale review
				log.Printf("[CBPR] STALE REVIEW: PR %d commit changed during generation (reviewed: %s, current: %s), discarding result and deleting file",
					pr.Number, pr.CommitSHA[:7], currentPR.LastCommitSHA[:7])
				os.Remove(outputPath) // Clean up the stale review file
			} else {
				// Commit matches - safe to mark as completed (review data updated in batch later)
				if err := p.upsertPRPreservingReviewData(ctx, pr.Owner, pr.Repo, pr.Number, pr.CommitSHA, filename, "completed", pr.Title, pr.Author, isMine, pr.CreatedAt, pr.Draft); err != nil {
					log.Printf("[CBPR] ERROR: Failed to update DB for PR %d: %v", pr.Number, err)
				} else {
					log.Printf("[CBPR] Marked PR %d as 'completed' in database", pr.Number)
				}
			}
		}

		// Untrack after all DB operations complete (prevents race with checkForOutdatedReviews)
		p.untrackReview(pr.Owner, pr.Repo, pr.Number)
	}

	return nil
}
