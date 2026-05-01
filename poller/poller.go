package poller

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strconv"
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
	StatusEventFunc  func()
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
	// Team member cache for resolving team review statuses
	teamMemberCache map[string][]string // team slug -> member logins
	teamCacheMutex  sync.RWMutex
	teamCacheExpiry time.Time
	// Poll economy: track cycles for periodic full refresh
	pollCount int
	// Agent-review subprocess spawner (nil-safe: defaults to the real claude CLI).
	agentSpawner service.Spawner
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
		agentSpawner:     service.DefaultSpawner{},
	}
}

// SetAgentSpawner overrides the subprocess spawner used for agent reviews.
// Tests inject a stub to avoid actually invoking the claude CLI.
func (p *Poller) SetAgentSpawner(s service.Spawner) { p.agentSpawner = s }

// isReviewInFlight reports whether a PR's status indicates an in-progress
// review (Gemini "generating" or Claude "agent_reviewing"). Used by the
// outdated-detection paths to decide whether to cancel the active review.
func isReviewInFlight(status string) bool {
	return status == "generating" || status == "agent_reviewing"
}

// runAgentStage runs the claude-agent pass on a Gemini ReviewResult: flips
// the PR status to agent_reviewing, spawns the agent against a clone of the
// PR head, replaces the comment set with the agent's refined output, and
// re-renders via the same HTML pipeline so the inline-comment UI is intact.
func (p *Poller) runAgentStage(ctx context.Context, pr github.PullRequest, result *service.ReviewResult) (*ReviewResult, error) {
	log.Printf("[REVIEWER] PR %d: Gemini pass done (comments=%d), entering agent stage",
		pr.Number, len(result.Comments))
	if setErr := p.db.SetPRAgentReviewing(pr.Owner, pr.Repo, pr.Number); setErr != nil {
		log.Printf("[REVIEWER] WARNING: could not set agent_reviewing status for PR %d: %v", pr.Number, setErr)
	}
	p.broadcastPRUpdate(pr.Owner, pr.Repo, pr.Number)

	agentCfg := service.AgentConfig{
		CloneRootDir: p.cfg.AgentCloneRootDir,
		LogsDir:      p.cfg.AgentLogsDir,
		WallClock:    time.Duration(p.cfg.AgentWallClockSec) * time.Second,
		MaxTurns:     p.cfg.AgentMaxTurns,
		GitHubToken:  p.cfg.GitHubToken,
	}
	agentOut, agentErr := service.RunAgentReview(ctx, agentCfg, p.agentSpawner,
		pr.Owner, pr.Repo, "", pr.Number, pr.CommitSHA, result.Comments)
	if agentErr != nil {
		log.Printf("[REVIEWER] ERROR: agent review failed for PR %d: %v", pr.Number, agentErr)
		return nil, fmt.Errorf("agent review: %w", agentErr)
	}

	result.Comments = agentOut.Comments
	result.ComputeImportanceCounts()
	log.Printf("[REVIEWER] PR %d: agent stage ok (clone=%s, log=%s, comments=%d, critical=%d, medium=%d, low=%d)",
		pr.Number, agentOut.CloneDir, agentOut.LogPath, len(agentOut.Comments),
		result.CriticalCount, result.MediumCount, result.LowCount)

	htmlContent := service.GenerateHTMLReportContent(result, pr.Number, pr.Owner, pr.Repo, pr.CommitSHA, llm.ProModel)
	if htmlContent == nil {
		return nil, fmt.Errorf("failed to generate HTML content from agent comments")
	}
	return &ReviewResult{
		HTMLContent:   htmlContent,
		CriticalCount: result.CriticalCount,
		MediumCount:   result.MediumCount,
		LowCount:      result.LowCount,
	}, nil
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

func (p *Poller) SetCacheUpdateFunc(f func([]github.PullRequest)) {
	p.cacheUpdateFunc = f
}

// SetDevUser sets the dev user for syncing user_pr_views in dev mode
func (p *Poller) SetDevUser(user *db.User) {
	p.devUser = user
}

// getTeamMembers returns team members with a 5-minute cache TTL.
func (p *Poller) getTeamMembers(ctx context.Context, orgName, teamSlug string) ([]string, error) {
	p.teamCacheMutex.RLock()
	if time.Now().Before(p.teamCacheExpiry) {
		if members, ok := p.teamMemberCache[teamSlug]; ok {
			p.teamCacheMutex.RUnlock()
			return members, nil
		}
	}
	p.teamCacheMutex.RUnlock()

	members, err := p.ghClient.GetOrgTeamMembers(ctx, orgName, teamSlug)
	if err != nil {
		return nil, err
	}

	p.teamCacheMutex.Lock()
	if p.teamMemberCache == nil {
		p.teamMemberCache = make(map[string][]string)
	}
	// If cache is expired, clear it and reset expiry
	if time.Now().After(p.teamCacheExpiry) {
		p.teamMemberCache = make(map[string][]string)
		p.teamCacheExpiry = time.Now().Add(5 * time.Minute)
	}
	p.teamMemberCache[teamSlug] = members
	p.teamCacheMutex.Unlock()

	return members, nil
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

// ProcessReviewImmediate starts review generation for a single PR immediately,
// bypassing the full poll cycle. The PR must already be in "generating" status
// in the database. trackReview is called synchronously before the goroutine
// launches so the poll cycle's shouldReview/isTracked guard sees it immediately.
//
// If force is true, the existing-review cache check is skipped — useful for
// the manual "Review" button so a click always regenerates (overwriting the
// previous review for the same commit).
func (p *Poller) ProcessReviewImmediate(ctx context.Context, owner, repo string, number int, commitSHA, title, author string, createdAt *time.Time, draft bool, force bool) {
	// Track synchronously BEFORE spawning goroutine so that any concurrent
	// poll cycle will see this PR as tracked and skip it.
	p.trackReview(owner, repo, number, 0)

	go func() {
		pr := github.PullRequest{
			Owner:     owner,
			Repo:      repo,
			Number:    number,
			CommitSHA: commitSHA,
			Title:     title,
			Author:    author,
			CreatedAt: createdAt,
			Draft:     draft,
		}
		// Run the single-PR review through generateReviewsBatch (reuses all
		// existing logic: existence check, LLM call, save, DB update, untrack).
		// Note: generateReviewsBatch calls trackReview again internally, but
		// since it's the same key the map entry is simply overwritten (harmless).
		if err := p.generateReviewsBatch(ctx, []github.PullRequest{pr}, force); err != nil {
			log.Printf("[IMMEDIATE] ERROR: Immediate review failed for %s/%s#%d: %v", owner, repo, number, err)
			// generateReviewsBatch handles per-PR error/untrack internally,
			// but if the batch-level error fires (e.g. API key validation),
			// we need to clean up.
			if p.isTracked(owner, repo, number) {
				_ = p.UpdatePRStatus(owner, repo, number, "error")
				p.untrackReview(owner, repo, number)
			}
		}
	}()
}

// IsReviewTracked returns whether a PR is currently being actively reviewed.
// Exported for use by the stale-reset guard in the poll cycle.
func (p *Poller) IsReviewTracked(owner, repo string, number int) bool {
	return p.isTracked(owner, repo, number)
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
	if p.StatusEventFunc != nil {
		p.StatusEventFunc()
	}

	go func() {
		defer func() {
			p.pollMutex.Lock()
			p.polling = false
			p.pollMutex.Unlock()
			log.Printf("Completed %s poll", trigger)
			if p.StatusEventFunc != nil {
				p.StatusEventFunc()
			}
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

// cleanupAndDetectOutdated combines closed-PR cleanup and outdated-review detection
// into a single pass using batched GraphQL, replacing the sequential REST calls.
func (p *Poller) cleanupAndDetectOutdated(ctx context.Context) (removed int, outdated int, err error) {
	allPRs, err := p.db.GetAllPRs()
	if err != nil {
		return 0, 0, fmt.Errorf("failed to get PRs from database: %w", err)
	}
	if len(allPRs) == 0 {
		return 0, 0, nil
	}

	// Build PRInfo slice for batch query
	prInfos := make([]github.PRInfo, len(allPRs))
	for i, pr := range allPRs {
		prInfos[i] = github.PRInfo{
			Owner:  pr.RepoOwner,
			Repo:   pr.RepoName,
			Number: pr.PRNumber,
		}
	}

	// Single batched GraphQL call for all PRs
	log.Printf("[POLL] Batch-fetching state for %d PRs...", len(prInfos))
	stateMap, err := p.ghClient.BatchGetPRState(ctx, prInfos)
	if err != nil {
		return 0, 0, fmt.Errorf("failed to batch-fetch PR state: %w", err)
	}

	// Single pass: handle closed PRs and outdated reviews
	for _, pr := range allPRs {
		key := fmt.Sprintf("%s/%s/%d", pr.RepoOwner, pr.RepoName, pr.PRNumber)
		state, ok := stateMap[key]
		if !ok {
			log.Printf("[POLL] Warning: No state data for PR %s, skipping", key)
			continue
		}

		// --- Closed PR cleanup ---
		if state.State != "OPEN" {
			log.Printf("[CLEANUP] PR %s is %s, removing from tracking (reviews kept in GCS)", key, state.State)
			if err := p.db.DeletePR(pr.RepoOwner, pr.RepoName, pr.PRNumber); err != nil {
				log.Printf("[CLEANUP] ERROR: Failed to delete PR %s: %v", key, err)
				continue
			}
			if p.EventFunc != nil {
				p.EventFunc("pr_deleted", map[string]interface{}{
					"owner":  pr.RepoOwner,
					"repo":   pr.RepoName,
					"number": pr.PRNumber,
				})
			}
			log.Printf("[CLEANUP] Successfully removed closed PR %s", key)
			removed++
			continue
		}

		// --- Outdated review detection ---
		if state.HeadRefOid != pr.LastCommitSHA {
			wasInFlight := isReviewInFlight(pr.Status)
			oldSHA, newSHA := pr.LastCommitSHA, state.HeadRefOid
			if len(oldSHA) > 7 {
				oldSHA = oldSHA[:7]
			}
			if len(newSHA) > 7 {
				newSHA = newSHA[:7]
			}
			log.Printf("[OUTDATED] PR %s has new commits (old: %s, new: %s), resetting to pending",
				key, oldSHA, newSHA)

			// Delete old HTML file if it exists
			if pr.ReviewHTMLPath != "" {
				oldHTMLPath := filepath.Join(p.reviewDir, pr.ReviewHTMLPath)
				if err := os.Remove(oldHTMLPath); err != nil && !os.IsNotExist(err) {
					log.Printf("[OUTDATED] Warning: Failed to delete old HTML file %s: %v", oldHTMLPath, err)
				}
			}

			// If the PR had an active Gemini or agent review, kill the process.
			if wasInFlight {
				if p.killReview(pr.RepoOwner, pr.RepoName, pr.PRNumber) {
					log.Printf("[OUTDATED] Killed active review process for %s", key)
				}
			}

			// Reset PR to pending with new commit SHA
			if err := p.db.ResetPRToOutdated(pr.RepoOwner, pr.RepoName, pr.PRNumber, state.HeadRefOid); err != nil {
				log.Printf("[OUTDATED] ERROR: Failed to reset PR %s: %v", key, err)
				continue
			}

			p.broadcastPRUpdate(pr.RepoOwner, pr.RepoName, pr.PRNumber)
			outdated++
		}
	}

	return removed, outdated, nil
}

// reviewExists checks if a review already exists for the given PR+commit.
// If a storage interface is set (for testing), it uses that.
// Otherwise, checks GCS if bucket is configured, or local file storage.
func (p *Poller) reviewExists(ctx context.Context, owner, repo string, prNumber int, commitSHA string) (bool, error) {
	if p.storage != nil {
		return p.storage.ReviewExists(ctx, owner, repo, prNumber, commitSHA)
	}

	if p.gcsClient != nil && p.gcsClient.BucketName() != "" {
		return p.gcsClient.ReviewExists(ctx, owner, repo, prNumber, commitSHA)
	}

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

// saveReview persists review content to storage (GCS or local disk).
func (p *Poller) saveReview(ctx context.Context, owner, repo string, prNumber int, commitSHA string, content []byte) (string, error) {
	if p.storage != nil {
		return p.storage.SaveReview(ctx, owner, repo, prNumber, commitSHA, content)
	}

	if p.gcsClient != nil && p.gcsClient.BucketName() != "" {
		return p.gcsClient.UploadReview(ctx, owner, repo, prNumber, commitSHA, content)
	}

	filename := gcs.ReviewFileName(owner, repo, prNumber, commitSHA)
	localPath := filepath.Join(p.reviewDir, filename)

	if err := os.MkdirAll(p.reviewDir, 0755); err != nil {
		return "", fmt.Errorf("failed to create reviews directory: %w", err)
	}

	if err := os.WriteFile(localPath, content, 0644); err != nil {
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
			wasInFlight := isReviewInFlight(pr.Status)
			statusMsg := pr.Status
			if wasInFlight {
				statusMsg = pr.Status + " (cancelling)"
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

			// If the PR had an active Gemini or agent review, kill the process.
			if wasInFlight {
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

			if wasInFlight {
				log.Printf("[OUTDATED] PR %d had new commit while in-flight (%s). Cancelled old review.", pr.PRNumber, pr.Status)
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

// syncUserPRViews creates user_pr_view records for PRs the user authored.
// Reviewer views are created separately in the reviewer groups phase.
// Uses the pre-built dbPRMap to avoid N+1 GetPR queries.
func (p *Poller) syncUserPRViews(allPRs []github.PullRequest, dbPRMap map[string]*db.PR, users []db.User) {
	batch := newViewBatch()
	for _, user := range users {
		for _, pr := range allPRs {
			if !strings.EqualFold(pr.Author, user.GitHubUsername) {
				continue
			}

			key := fmt.Sprintf("%s/%s/%d", pr.Owner, pr.Repo, pr.Number)
			dbPR, exists := dbPRMap[key]
			if !exists {
				continue
			}

			batch.EnsureView(user.ID, dbPR.ID, true)
		}
	}
	if batch.Len() > 0 {
		if err := batch.Flush(p.db); err != nil {
			log.Printf("[POLL] Warning: Failed to batch-upsert author views: %v", err)
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

	// --- Phase 1: Fast DB self-healing ---

	// Reset any PRs stuck in "generating" for too long
	log.Printf("[POLL] Checking for stale PRs...")
	resetCount, err := p.db.ResetStaleGeneratingPRs(int(StaleReviewResetTimeout.Minutes()))
	if err != nil {
		log.Printf("[POLL] ERROR: Failed to reset stale PRs: %v", err)
	} else if resetCount > 0 {
		log.Printf("[POLL] Reset %d stale PRs from 'generating' to 'pending'", resetCount)
		// Guard: if any actively-tracked reviews were reset (e.g. a long-running
		// immediate review), restore them to "generating" so the goroutine's
		// eventual DB write doesn't collide with a re-queued pending review.
		p.reviewsMutex.Lock()
		trackedKeys := make(map[string]bool, len(p.activeReviews))
		for k := range p.activeReviews {
			trackedKeys[k] = true
		}
		p.reviewsMutex.Unlock()
		if len(trackedKeys) > 0 {
			allPRsForCheck, checkErr := p.db.GetAllPRs()
			if checkErr == nil {
				for _, dbPR := range allPRsForCheck {
					key := prKey(dbPR.RepoOwner, dbPR.RepoName, dbPR.PRNumber)
					if trackedKeys[key] && dbPR.Status == "pending" {
						log.Printf("[POLL] Restoring actively-tracked PR %s from 'pending' back to 'generating'", key)
						_ = p.db.SetPRGenerating(dbPR.RepoOwner, dbPR.RepoName, dbPR.PRNumber, dbPR.LastCommitSHA, dbPR.Title, dbPR.Author, dbPR.CreatedAt, dbPR.Draft)
					}
				}
			}
		}
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

	// --- Phase 2: Fast metadata refresh (critical path for ≤60s dashboard staleness) ---
	// GitHub search + batched GraphQL metadata runs before slow self-healing operations
	// to minimize time between poll start and fresh dashboard data.

	// Check auto-review setting to determine if we should process new PRs
	autoReviewEnabled, autoReviewErr := p.db.GetAutoReviewRequestedPRs()
	if autoReviewErr != nil {
		log.Printf("[POLL] Warning: Failed to get auto-review setting: %v", autoReviewErr)
		autoReviewEnabled = true // Default to enabled on error
	}

	// Build the canonical DB state map ONCE, early. Used for:
	// 1. Skipping known PRs during fetch (multi-user optimization)
	// 2. Pre-insertion check for new PRs
	// 3. All downstream metadata phases (review data, CI, reviewer groups)
	dbPRMap := make(map[string]*db.PR)
	dbPRsAll, err := p.db.GetAllPRs()
	if err != nil {
		log.Printf("[POLL] WARNING: Failed to get database PRs: %v", err)
		dbPRsAll = nil
	} else {
		for i := range dbPRsAll {
			key := fmt.Sprintf("%s/%s/%d", dbPRsAll[i].RepoOwner, dbPRsAll[i].RepoName, dbPRsAll[i].PRNumber)
			dbPRMap[key] = &dbPRsAll[i]
		}
	}

	// Fetch PRs from GitHub.
	// If an org is configured, always use the org-wide search path so dev mode
	// sees the same universe of PRs as prod (including team-requested PRs).
	// In dev mode we still only sync user_pr_views for the dev user, so this
	// does not turn local development into a multi-user dashboard.
	var allPRs []github.PullRequest
	var prInfoMap map[string]*time.Time // PR key -> search updatedAt (for poll economy, multi-user only)
	if p.cfg.GitHubOrgName != "" {
		// Org-wide mode: fast search + skip known PRs + batch GraphQL for unknowns
		log.Printf("[POLL] Searching open PRs for org %s...", p.cfg.GitHubOrgName)
		prInfos, err := p.ghClient.SearchOpenPRs(ctx, p.cfg.GitHubOrgName)
		if err != nil {
			log.Printf("[POLL] ERROR: Failed to search org PRs: %v", err)
			prInfos = []github.PRInfo{}
		}
		log.Printf("[POLL] Found %d open PRs for org %s", len(prInfos), p.cfg.GitHubOrgName)

		// Build updatedAt map for poll economy change detection
		prInfoMap = make(map[string]*time.Time, len(prInfos))
		for _, info := range prInfos {
			key := fmt.Sprintf("%s/%s/%d", info.Owner, info.Repo, info.Number)
			prInfoMap[key] = info.UpdatedAt
		}

		// Partition into known (in DB) vs unknown PRs
		var unknownPRs []github.PRInfo
		for _, info := range prInfos {
			key := fmt.Sprintf("%s/%s/%d", info.Owner, info.Repo, info.Number)
			if dbPR, exists := dbPRMap[key]; exists {
				allPRs = append(allPRs, buildPRFromDB(*dbPR))
			} else {
				unknownPRs = append(unknownPRs, info)
			}
		}

		// Batch fetch details only for unknown PRs via GraphQL
		if len(unknownPRs) > 0 {
			detailsMap, err := p.ghClient.BatchGetPRDetails(ctx, unknownPRs)
			if err != nil {
				log.Printf("[POLL] ERROR: Failed to batch fetch PR details: %v", err)
			} else {
				for _, pr := range detailsMap {
					allPRs = append(allPRs, pr)
				}
			}
		}
		log.Printf("[POLL] %d from DB, %d fetched via GraphQL", len(prInfos)-len(unknownPRs), len(unknownPRs))
	} else {
		// Dev mode: user-specific fetches
		log.Printf("[POLL] Fetching PRs requesting review from GitHub...")
		reviewPRs, err := p.ghClient.GetPRsRequestingReview(ctx)
		if err != nil {
			log.Printf("[POLL] ERROR: Failed to fetch PRs requesting review: %v", err)
			reviewPRs = []github.PullRequest{}
		} else {
			log.Printf("[POLL] Found %d PRs requesting review", len(reviewPRs))
		}

		log.Printf("[POLL] Fetching my own open PRs from GitHub...")
		myPRs, err := p.ghClient.GetMyOpenPRs(ctx)
		if err != nil {
			log.Printf("[POLL] ERROR: Failed to fetch my open PRs: %v", err)
			myPRs = []github.PullRequest{}
		}
		log.Printf("[POLL] Found %d of my own open PRs", len(myPRs))

		allPRs = append(reviewPRs, myPRs...)
	}

	// Build the users list once for all per-user operations
	var allUsers []db.User
	if p.devUser != nil {
		allUsers = []db.User{*p.devUser}
	} else {
		allUsers, err = p.db.GetAllUsers()
		if err != nil {
			log.Printf("[POLL] WARNING: Failed to get all users: %v", err)
			allUsers = nil
		}
		if len(allUsers) > 0 {
			log.Printf("[POLL] Syncing PR views for %d users", len(allUsers))
		}
	}

	// Update cache for fast dashboard loading
	if p.cacheUpdateFunc != nil {
		p.cacheUpdateFunc(allPRs)
	}

	// Ensure all GitHub-fetched PRs exist in the database BEFORE syncUserPRViews.
	// Without this, new PRs discovered from GitHub won't have a prs row yet,
	// so syncUserPRViews can't create the user_pr_views record (FK constraint),
	// and the PR stays invisible on the dashboard until the NEXT poll cycle.
	for _, pr := range allPRs {
		key := fmt.Sprintf("%s/%s/%d", pr.Owner, pr.Repo, pr.Number)
		if _, exists := dbPRMap[key]; !exists {
			newPR := &db.PR{
				RepoOwner:     pr.Owner,
				RepoName:      pr.Repo,
				PRNumber:      pr.Number,
				LastCommitSHA: pr.CommitSHA,
				Title:         pr.Title,
				Author:        pr.Author,
				CreatedAt:     pr.CreatedAt,
				Draft:         pr.Draft,
				Status:        "pending",
			}
			if err := p.db.UpsertPR(newPR); err != nil {
				log.Printf("[POLL] Warning: Failed to pre-insert PR %s/%s#%d: %v", pr.Owner, pr.Repo, pr.Number, err)
			} else {
				log.Printf("[POLL] Pre-inserted new PR %s/%s#%d into database", pr.Owner, pr.Repo, pr.Number)
				p.broadcastPRUpdate(pr.Owner, pr.Repo, pr.Number)
				// Fetch the new PR's database ID and add to the map so downstream
				// phases (reviewer groups, CI, etc.) can find it immediately.
				if inserted, err := p.db.GetPR(pr.Owner, pr.Repo, pr.Number); err == nil && inserted != nil {
					dbPRMap[key] = inserted
				}
			}
		}
	}

	// Sync user_pr_views for all users (ensure all PRs have user-PR relationship records)
	p.syncUserPRViews(allPRs, dbPRMap, allUsers)

	// Add database PRs not in the GitHub fetch to allPRs so metadata still refreshes
	// for already-tracked PRs (for example, approval counts after a user reviews a PR).
	//
	// This happens AFTER syncUserPRViews so dev mode does not create spurious user_pr_views
	// for unrelated PRs from a shared database, while still keeping metadata current.
	ghKeys := make(map[string]bool, len(allPRs))
	for _, pr := range allPRs {
		ghKeys[fmt.Sprintf("%s/%s/%d", pr.Owner, pr.Repo, pr.Number)] = true
	}
	added := 0
	for key, dbPR := range dbPRMap {
		if !ghKeys[key] {
			allPRs = append(allPRs, buildPRFromDB(*dbPR))
			added++
		}
	}
	if added > 0 {
		log.Printf("[POLL] Added %d database PRs to metadata update list (total: %d PRs)", added, len(allPRs))
	}

	// Track which PRs changed across all metadata phases, then broadcast once at the end.
	// This ensures the frontend receives a consistent snapshot (approval count + via_teams + CI status
	// all current) rather than partial updates from individual phases.
	changedPRs := make(map[string]bool)

	// --- Poll economy: determine which PRs need metadata refresh ---
	// Compare GitHub's updated_at from search with stored values to skip unchanged PRs.
	p.pollCount++
	isFullRefresh := p.devUser != nil || prInfoMap == nil || p.pollCount%10 == 0
	if isFullRefresh && p.pollCount%10 == 0 {
		log.Printf("[POLL] Full refresh (cycle %d)", p.pollCount)
	}

	var metadataPRs []github.PullRequest
	if !isFullRefresh {
		changedPRKeys := make(map[string]bool)
		for key, searchUpdatedAt := range prInfoMap {
			dbPR, exists := dbPRMap[key]
			if !exists || dbPR.GitHubUpdatedAt == nil || searchUpdatedAt == nil ||
				!searchUpdatedAt.Truncate(time.Second).Equal(dbPR.GitHubUpdatedAt.Truncate(time.Second)) {
				changedPRKeys[key] = true
			}
		}
		// DB-only PRs (not in search) always included — unknown state
		for _, pr := range allPRs {
			key := fmt.Sprintf("%s/%s/%d", pr.Owner, pr.Repo, pr.Number)
			if _, inSearch := prInfoMap[key]; !inSearch {
				changedPRKeys[key] = true
			}
		}
		for _, pr := range allPRs {
			key := fmt.Sprintf("%s/%s/%d", pr.Owner, pr.Repo, pr.Number)
			if changedPRKeys[key] {
				metadataPRs = append(metadataPRs, pr)
			}
		}
		log.Printf("[POLL] %d/%d PRs changed, skipping %d unchanged", len(metadataPRs), len(allPRs), len(allPRs)-len(metadataPRs))
	} else {
		metadataPRs = allPRs
	}

	// --- Parallel GitHub API fetches ---
	// All three data sources (review data, reviewer groups, CI status) are independent.
	// Fetch them concurrently, then process results sequentially.
	var reviewDataMap map[string]*github.PRReviewData
	var reviewerGroupsMap map[string]*github.ReviewerGroupData
	var ciStatusMap map[string]*github.CIStatus

	if len(allPRs) > 0 {
		log.Printf("[POLL] Parallel-fetching review data + reviewer groups for %d changed PRs, CI status for all %d PRs...", len(metadataPRs), len(allPRs))

		var fetchWg sync.WaitGroup

		// Fetch review data (only changed PRs)
		if len(metadataPRs) > 0 {
			fetchWg.Add(1)
			go func() {
				defer fetchWg.Done()
				var err error
				reviewDataMap, err = p.ghClient.BatchGetPRReviewData(ctx, metadataPRs)
				if err != nil {
					log.Printf("[POLL] WARNING: Failed to batch fetch review data: %v", err)
					reviewDataMap = nil
				}
			}()
		}

		// Fetch reviewer groups (only changed PRs)
		if len(allUsers) > 0 && len(metadataPRs) > 0 {
			fetchWg.Add(1)
			go func() {
				defer fetchWg.Done()
				var err error
				reviewerGroupsMap, err = p.ghClient.BatchGetReviewerGroups(ctx, metadataPRs)
				if err != nil {
					log.Printf("[POLL] WARNING: Failed to batch fetch reviewer groups: %v", err)
					reviewerGroupsMap = nil
				}
			}()
		}

		// Fetch CI status for ALL PRs every cycle — CI check completions
		// don't update GitHub's PR updated_at, so we can't rely on change detection.
		fetchWg.Add(1)
		go func() {
			defer fetchWg.Done()
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
			var err error
			ciStatusMap, err = p.ghClient.BatchGetCIStatus(ctx, prsWithSHA)
			if err != nil {
				log.Printf("[POLL] WARNING: Failed to batch fetch CI status: %v", err)
				ciStatusMap = nil
			}
		}()

		fetchWg.Wait()
		log.Printf("[POLL] All parallel fetches complete")
	}

	// --- Process review data ---
	if reviewDataMap != nil {
		reviewViewBatch := newViewBatch()
		reviewPRBatch := newPRBatch()
		updateCount := 0
		for _, pr := range allPRs {
			key := fmt.Sprintf("%s/%s/%d", pr.Owner, pr.Repo, pr.Number)
			if reviewData, exists := reviewDataMap[key]; exists {
				existingPR, existsInDB := dbPRMap[key]
				if !existsInDB {
					continue
				}

				for _, user := range allUsers {
					userStatus := ""
					for login, status := range reviewData.UserReviews {
						if strings.EqualFold(login, user.GitHubUsername) {
							userStatus = status
							break
						}
					}
					if userStatus != "" {
						isAuthor := strings.EqualFold(existingPR.Author, user.GitHubUsername)
						reviewViewBatch.EnsureView(user.ID, existingPR.ID, isAuthor)
						reviewViewBatch.SetReviewStatus(user.ID, existingPR.ID, userStatus)
					}
				}

				approvalChanged := existingPR.ApprovalCount != reviewData.ApprovalCount
				reviewStatusChanged := existingPR.MyReviewStatus != reviewData.MyReviewStatus
				draftChanged := existingPR.Draft != pr.Draft

				if !approvalChanged && !reviewStatusChanged && !draftChanged {
					continue
				}

				existingPR.ApprovalCount = reviewData.ApprovalCount
				existingPR.MyReviewStatus = reviewData.MyReviewStatus
				existingPR.Draft = pr.Draft
				if pr.CreatedAt != nil {
					existingPR.CreatedAt = pr.CreatedAt
				}

				reviewPRBatch.Upsert(existingPR)
				changedPRs[key] = true
				updateCount++
			}
		}
		if err := reviewViewBatch.Flush(p.db); err != nil {
			log.Printf("[POLL] ERROR: Failed to batch-upsert review user views: %v", err)
		}
		if err := reviewPRBatch.Flush(p.db); err != nil {
			log.Printf("[POLL] ERROR: Failed to batch-upsert review PR data: %v", err)
		}
		log.Printf("[POLL] Updated review data for %d PRs (only those with changes)", updateCount)
	}

	// --- Process reviewer groups ---
	if reviewerGroupsMap != nil && len(allUsers) > 0 {
		orgName := p.cfg.GitHubOrgName
		if orgName == "" {
			for _, data := range reviewerGroupsMap {
				if data.OrgName != "" {
					orgName = data.OrgName
					break
				}
			}
		}

		teamMembersMap := map[string][]string{}
		if orgName != "" {
			uniqueSlugs := collectUniqueSlugs(reviewerGroupsMap)
			for slug := range uniqueSlugs {
				members, err := p.getTeamMembers(ctx, orgName, slug)
				if err != nil {
					log.Printf("[POLL] Warning: Failed to get members for team %s: %v", slug, err)
					continue
				}
				teamMembersMap[slug] = members
			}
		}

		groupsViewBatch := newViewBatch()
		for _, pr := range allPRs {
			key := fmt.Sprintf("%s/%s/%d", pr.Owner, pr.Repo, pr.Number)
			groupData, exists := reviewerGroupsMap[key]
			if !exists {
				continue
			}
			existingPR, existsInDB := dbPRMap[key]
			if !existsInDB {
				continue
			}

			var userReviews map[string]string
			if reviewDataMap != nil {
				if reviewData, exists := reviewDataMap[key]; exists {
					userReviews = reviewData.UserReviews
				}
			}

			for _, user := range allUsers {
				isOnTeam := false
				for _, team := range groupData.ReviewerGroups {
					slug, hasSlug := groupData.TeamSlugs[team]
					if !hasSlug {
						slug = slugifyTeamName(team)
					}
					for _, member := range teamMembersMap[slug] {
						if strings.EqualFold(member, user.GitHubUsername) {
							isOnTeam = true
							break
						}
					}
					if isOnTeam {
						break
					}
				}
				isPersonallyRequested := false
				for _, requestedUser := range groupData.RequestedUsers {
					if strings.EqualFold(requestedUser, user.GitHubUsername) {
						isPersonallyRequested = true
						break
					}
				}

				if !isOnTeam && !isPersonallyRequested {
					continue
				}

				teamStatuses := resolveTeamReviewStatuses(
					groupData.ReviewerGroups,
					nil,
					teamMembersMap,
					groupData.TeamSlugs,
					userReviews,
					user.GitHubUsername,
				)
				var viaTeams []string
				for _, team := range groupData.ReviewerGroups {
					if status, ok := teamStatuses[team]; ok {
						viaTeams = append(viaTeams, team+":"+status)
					}
				}

				if isPersonallyRequested {
					viaTeams = append(viaTeams, "__PERSONAL__")
				}

				if shouldUpdateViaTeams(viaTeams) {
					isAuthor := strings.EqualFold(pr.Author, user.GitHubUsername)
					groupsViewBatch.EnsureView(user.ID, existingPR.ID, isAuthor)
					groupsViewBatch.SetViaTeams(user.ID, existingPR.ID, viaTeams)
					changedPRs[key] = true
				}
			}
		}
		if err := groupsViewBatch.Flush(p.db); err != nil {
			log.Printf("[POLL] ERROR: Failed to batch-upsert reviewer group views: %v", err)
		}
		log.Printf("[POLL] Updated reviewer groups for %d PRs", len(reviewerGroupsMap))
	}

	// --- Process CI status ---
	if ciStatusMap != nil {
		ciPRBatch := newPRBatch()
		updateCount := 0
		for _, pr := range allPRs {
			key := fmt.Sprintf("%s/%s/%d", pr.Owner, pr.Repo, pr.Number)
			ciStatus, hasCIData := ciStatusMap[key]
			if !hasCIData {
				continue
			}
			existingPR, existsInDB := dbPRMap[key]
			if !existsInDB {
				continue
			}

			failedChecksJSON := "[]"
			if len(ciStatus.FailedChecks) > 0 {
				if jsonBytes, err := json.Marshal(ciStatus.FailedChecks); err == nil {
					failedChecksJSON = string(jsonBytes)
				}
			}

			if existingPR.CIState == ciStatus.State && existingPR.CIFailedChecks == failedChecksJSON {
				continue
			}

			existingPR.CIState = ciStatus.State
			existingPR.CIFailedChecks = failedChecksJSON
			ciPRBatch.Upsert(existingPR)
			changedPRs[key] = true
			updateCount++
		}
		if err := ciPRBatch.Flush(p.db); err != nil {
			log.Printf("[POLL] ERROR: Failed to batch-upsert CI status: %v", err)
		}
		log.Printf("[POLL] Updated CI status for %d PRs (only those with changes)", updateCount)
	}

	// Broadcast all changed PRs once, after all metadata phases are complete.
	// This ensures the frontend receives a consistent snapshot (approval count +
	// via_teams + CI status all current) rather than partial updates.
	for _, pr := range allPRs {
		key := fmt.Sprintf("%s/%s/%d", pr.Owner, pr.Repo, pr.Number)
		if changedPRs[key] {
			p.broadcastPRUpdate(pr.Owner, pr.Repo, pr.Number)
		}
	}

	// --- Poll economy: persist GitHub updatedAt for change detection next cycle ---
	if len(prInfoMap) > 0 {
		updated := 0
		for key, ts := range prInfoMap {
			if ts == nil {
				continue
			}
			dbPR, exists := dbPRMap[key]
			if !exists || dbPR.GitHubUpdatedAt == nil || !ts.Equal(*dbPR.GitHubUpdatedAt) {
				parts := strings.SplitN(key, "/", 3)
				if len(parts) == 3 {
					num, _ := strconv.Atoi(parts[2])
					if err := p.db.UpdatePRGitHubUpdatedAt(parts[0], parts[1], num, *ts); err != nil {
						log.Printf("[POLL] Warning: Failed to update GitHubUpdatedAt for %s: %v", key, err)
					} else {
						updated++
					}
				}
			}
		}
		if updated > 0 {
			log.Printf("[POLL] Updated GitHubUpdatedAt for %d PRs", updated)
		}
	}

	// --- Phase 3: Slow self-healing (runs after metadata is already fresh) ---

	// Batch cleanup + outdated detection (single GraphQL pass replaces 2N REST calls)
	log.Printf("[POLL] Batch-checking PR state (cleanup + outdated detection)...")
	removedCount, outdatedCount, err := p.cleanupAndDetectOutdated(ctx)
	if err != nil {
		log.Printf("[POLL] ERROR: Failed batch PR state check: %v", err)
	} else {
		if removedCount > 0 {
			log.Printf("[POLL] CLEANUP: Removed %d closed PRs from system", removedCount)
		} else {
			log.Printf("[POLL] No closed PRs to remove")
		}
		if outdatedCount > 0 {
			log.Printf("[POLL] OUTDATED: Reset %d PRs with new commits to pending", outdatedCount)
		} else {
			log.Printf("[POLL] No outdated reviews found")
		}
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

	// --- Phase 4: Review generation ---

	// These are used to split PRs into "mine" (skip self-review) vs "to review"
	var myPRs []github.PullRequest
	var reviewPRs []github.PullRequest

	// Re-query pending/generating PRs from database. We need fresh status data here
	// because Phase 3 (outdated review detection) may have reset PRs to "pending".
	log.Printf("[POLL] Checking database for pending PRs...")

	freshDBPRs, err := p.db.GetAllPRs()
	if err != nil {
		log.Printf("[POLL] ERROR: Failed to get PRs from database: %v", err)
	} else {
		pendingCount := 0
		skippedCount := 0
		for _, dbPR := range freshDBPRs {
			if dbPR.Status == "pending" || dbPR.Status == "generating" {
				// Skip pending PRs if auto-review is disabled
				// BUT: Always process 'generating' PRs (manual triggers)
				if dbPR.Status == "pending" && !autoReviewEnabled {
					skippedCount++
					continue
				}

				ghPR := buildPRFromDB(dbPR)

				// In single-user mode, determine if the PR is "mine" to avoid self-review
				isMine := p.cfg.GitHubUsername != "" && strings.EqualFold(dbPR.Author, p.cfg.GitHubUsername)

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
	// Generate reviews using native reviewer (batch). force=false on the
	// auto-poll path: respect the per-commit cache so we don't burn cycles
	// regenerating reviews for already-reviewed commits.
	batchErr := p.generateReviewsBatch(ctx, prsToReview, false)
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

// generateReviewsBatch runs Gemini (and optionally agent) review generation
// for one or more PRs. When force is true, the existing-review cache check
// is skipped — the caller wants a fresh review even if one already exists
// for the same commit. The auto-poll path uses force=false to avoid
// re-reviewing already-reviewed commits; the manual API trigger uses
// force=true so a click always regenerates.
func (p *Poller) generateReviewsBatch(ctx context.Context, prs []github.PullRequest, force bool) error {
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

			// Check if review already exists (in GCS if configured, otherwise locally).
			// Skipped when force=true so the manual trigger always regenerates.
			exists := false
			var existsErr error
			if !force {
				exists, existsErr = p.reviewExists(ctx, pr.Owner, pr.Repo, pr.Number, pr.CommitSHA)
			} else {
				log.Printf("[REVIEWER] PR %d: force=true, skipping existing-review cache check", pr.Number)
			}
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
				if err := p.db.MarkPRCompleted(pr.Owner, pr.Repo, pr.Number, pr.CommitSHA, filename, criticalCount, mediumCount, lowCount); err != nil {
					log.Printf("[REVIEWER] ERROR: Failed to update DB for existing review: %v", err)
				} else {
					p.broadcastPRUpdate(pr.Owner, pr.Repo, pr.Number)
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
				} else if p.cfg.AgenticReviews {
					reviewResult, err = p.runAgentStage(ctx, pr, result)
				} else {
					// Legacy HTML report path.
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
					if setErr := p.db.SetPRError(pr.Owner, pr.Repo, pr.Number, err.Error()); setErr != nil {
						log.Printf("[REVIEWER] WARNING: failed to persist error status for PR %d: %v", pr.Number, setErr)
					}
					p.broadcastPRUpdate(pr.Owner, pr.Repo, pr.Number)
				}
				p.untrackReview(pr.Owner, pr.Repo, pr.Number)
				return
			}

			log.Printf("[REVIEWER] Review completed successfully for PR %d in %v", pr.Number, execDuration)

			// Save review (to GCS if configured, otherwise locally)
			filename, err := p.saveReview(ctx, pr.Owner, pr.Repo, pr.Number, pr.CommitSHA, reviewResult.HTMLContent)
			if err != nil {
				log.Printf("[REVIEWER] ERROR: Failed to save review for PR %d: %v", pr.Number, err)
				if setErr := p.db.SetPRError(pr.Owner, pr.Repo, pr.Number, err.Error()); setErr != nil {
					log.Printf("[REVIEWER] WARNING: failed to persist error status for PR %d: %v", pr.Number, setErr)
				}
				p.broadcastPRUpdate(pr.Owner, pr.Repo, pr.Number)
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
				if err := p.db.MarkPRCompleted(pr.Owner, pr.Repo, pr.Number, pr.CommitSHA, filename, reviewResult.CriticalCount, reviewResult.MediumCount, reviewResult.LowCount); err != nil {
					log.Printf("[REVIEWER] ERROR: Failed to update DB for PR %d: %v", pr.Number, err)
				} else {
					p.broadcastPRUpdate(pr.Owner, pr.Repo, pr.Number)
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
