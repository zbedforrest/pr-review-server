package poller

import (
	"context"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
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
}

func New(cfg *config.Config, database *db.DB, ghClient *github.Client) *Poller {
	return &Poller{
		cfg:         cfg,
		db:          database,
		ghClient:    ghClient,
		reviewDir:   cfg.ReviewsDir,
		triggerChan: make(chan struct{}, 1), // Buffered to prevent blocking
	}
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
	ticker := time.NewTicker(p.cfg.PollingInterval)
	defer ticker.Stop()

	log.Println("Starting poller...")

	// Run immediately on start
	p.poll(ctx)

	for {
		select {
		case <-ctx.Done():
			log.Println("Poller stopped")
			return
		case <-ticker.C:
			p.poll(ctx)
		case <-p.triggerChan:
			log.Println("Manually triggered poll")
			p.poll(ctx)
		}
	}
}

func (p *Poller) poll(ctx context.Context) {
	log.Println("Polling for PRs...")

	// Reset any PRs stuck in "generating" for more than 5 minutes
	resetCount, err := p.db.ResetStaleGeneratingPRs(5)
	if err != nil {
		log.Printf("Error resetting stale PRs: %v", err)
	} else if resetCount > 0 {
		log.Printf("Reset %d stale PRs from 'generating' to 'pending'", resetCount)
	}

	prs, err := p.ghClient.GetPRsRequestingReview(ctx)
	if err != nil {
		log.Printf("Error fetching PRs: %v", err)
		return
	}

	log.Printf("Found %d PRs requesting review", len(prs))

	// Update cache for fast dashboard loading
	if p.cacheUpdateFunc != nil {
		p.cacheUpdateFunc(prs)
	}

	// Group PRs by repository for batch processing
	prsByRepo := make(map[string][]github.PullRequest)
	for _, pr := range prs {
		repoKey := fmt.Sprintf("%s/%s", pr.Owner, pr.Repo)
		prsByRepo[repoKey] = append(prsByRepo[repoKey], pr)
	}

	// Process each repository's PRs in batch
	for repoKey, repoPRs := range prsByRepo {
		if err := p.processPRBatch(ctx, repoPRs); err != nil {
			log.Printf("Error processing PRs for %s: %v", repoKey, err)
		}
	}
}

func (p *Poller) processPRBatch(ctx context.Context, prs []github.PullRequest) error {
	if len(prs) == 0 {
		return nil
	}

	// Filter PRs that need review
	var prsToReview []github.PullRequest
	for _, pr := range prs {
		existingPR, err := p.db.GetPR(pr.Owner, pr.Repo, pr.Number)
		if err != nil {
			log.Printf("Error checking PR %s/%s#%d: %v", pr.Owner, pr.Repo, pr.Number, err)
			continue
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
	for _, pr := range prsToReview {
		if err := p.db.SetPRGenerating(pr.Owner, pr.Repo, pr.Number, pr.CommitSHA); err != nil {
			log.Printf("Error setting generating status for %s/%s#%d: %v", pr.Owner, pr.Repo, pr.Number, err)
		}
	}

	owner := prsToReview[0].Owner
	repo := prsToReview[0].Repo
	log.Printf("Generating reviews for %s/%s PRs: %v", owner, repo, getPRNumbers(prsToReview))

	// Generate reviews using cbpr (batch)
	if err := p.generateReviewsBatch(ctx, prsToReview); err != nil {
		// Mark all as error
		for _, pr := range prsToReview {
			p.db.UpdatePRStatus(pr.Owner, pr.Repo, pr.Number, "error")
		}
		return fmt.Errorf("failed to generate reviews: %w", err)
	}

	// Update all as completed
	for _, pr := range prsToReview {
		filename := fmt.Sprintf("%s_%s_%d.html", pr.Owner, pr.Repo, pr.Number)
		if err := p.db.UpsertPR(pr.Owner, pr.Repo, pr.Number, pr.CommitSHA, filename, "completed"); err != nil {
			log.Printf("Error updating DB for %s/%s#%d: %v", pr.Owner, pr.Repo, pr.Number, err)
		}
	}

	log.Printf("Successfully generated reviews for %s/%s PRs: %v", owner, repo, getPRNumbers(prsToReview))
	return nil
}

func getPRNumbers(prs []github.PullRequest) []int {
	nums := make([]int, len(prs))
	for i, pr := range prs {
		nums[i] = pr.Number
	}
	return nums
}

func (p *Poller) processPR(ctx context.Context, pr github.PullRequest) error {
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
	if err := p.db.SetPRGenerating(pr.Owner, pr.Repo, pr.Number, pr.CommitSHA); err != nil {
		return fmt.Errorf("failed to set PR generating status: %w", err)
	}

	// Generate review using cbpr
	htmlPath, err := p.generateReview(ctx, pr)
	if err != nil {
		p.db.UpdatePRStatus(pr.Owner, pr.Repo, pr.Number, "error")
		return fmt.Errorf("failed to generate review: %w", err)
	}

	// Update database with completed status
	if err := p.db.UpsertPR(pr.Owner, pr.Repo, pr.Number, pr.CommitSHA, htmlPath, "completed"); err != nil {
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
		"--fast",                               // Development mode for faster iterations
		fmt.Sprintf("--output=%s", outputPath), // Specify output file directly
	)

	log.Printf("Running cbpr: %s %v", p.cfg.CbprPath, cmd.Args)
	log.Printf("Output path: %s", outputPath)

	// Run command, output goes to stderr (logs)
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("cbpr command failed: %w", err)
	}

	// Verify file was created
	if _, err := os.Stat(outputPath); os.IsNotExist(err) {
		return "", fmt.Errorf("cbpr succeeded but file not created at %s", outputPath)
	}

	return filename, nil
}

func (p *Poller) generateReviewsBatch(ctx context.Context, prs []github.PullRequest) error {
	if len(prs) == 0 {
		return nil
	}

	owner := prs[0].Owner
	repo := prs[0].Repo

	// Use absolute path for output directory
	absReviewDir, err := filepath.Abs(p.reviewDir)
	if err != nil {
		return fmt.Errorf("failed to get absolute path: %w", err)
	}

	// Ensure reviews directory exists
	if err := os.MkdirAll(absReviewDir, 0755); err != nil {
		return fmt.Errorf("failed to create reviews directory: %w", err)
	}

	// Build comma-separated list of PR numbers
	prNumbers := make([]string, len(prs))
	for i, pr := range prs {
		prNumbers[i] = fmt.Sprintf("%d", pr.Number)
	}
	prNumbersStr := fmt.Sprintf("%s", prNumbers[0])
	for i := 1; i < len(prNumbers); i++ {
		prNumbersStr += "," + prNumbers[i]
	}

	// Build cbpr command with multiple PRs
	repoName := fmt.Sprintf("%s/%s", owner, repo)
	cmd := exec.CommandContext(ctx,
		p.cfg.CbprPath,
		"review",
		fmt.Sprintf("--repo-name=%s", repoName),
		"-n", "3",
		"-p", prNumbersStr,
		"--fast",
		"--html", // Generate HTML output
	)

	// Set working directory to reviews directory so cbpr writes files there
	cmd.Dir = absReviewDir

	log.Printf("Running batch cbpr: %s %v", p.cfg.CbprPath, cmd.Args)
	log.Printf("Working directory: %s", absReviewDir)

	// Run command, output goes to stderr (logs)
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("cbpr command failed: %w", err)
	}

	// Verify files were created
	for _, pr := range prs {
		filename := fmt.Sprintf("%s_%s_%d.html", owner, repo, pr.Number)
		outputPath := filepath.Join(absReviewDir, filename)
		if _, err := os.Stat(outputPath); os.IsNotExist(err) {
			return fmt.Errorf("cbpr succeeded but file not created at %s", outputPath)
		}
	}

	return nil
}
