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
	cfg       *config.Config
	db        *db.DB
	ghClient  *github.Client
	reviewDir string
}

func New(cfg *config.Config, database *db.DB, ghClient *github.Client) *Poller {
	return &Poller{
		cfg:       cfg,
		db:        database,
		ghClient:  ghClient,
		reviewDir: cfg.ReviewsDir,
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
		}
	}
}

func (p *Poller) poll(ctx context.Context) {
	log.Println("Polling for PRs...")

	prs, err := p.ghClient.GetPRsRequestingReview(ctx)
	if err != nil {
		log.Printf("Error fetching PRs: %v", err)
		return
	}

	log.Printf("Found %d PRs requesting review", len(prs))

	for _, pr := range prs {
		if err := p.processPR(ctx, pr); err != nil {
			log.Printf("Error processing PR %s/%s#%d: %v", pr.Owner, pr.Repo, pr.Number, err)
		}
	}
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
	outputPath := filepath.Join(p.reviewDir, filename)

	// Ensure reviews directory exists
	if err := os.MkdirAll(p.reviewDir, 0755); err != nil {
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
		"--html",
		"--fast",    // Development mode for faster iterations
		"--no-open", // Don't open in browser (for server use)
	)

	// Capture stdout (HTML) only, stderr goes to logs
	output, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("cbpr command failed: %w", err)
	}

	// Write HTML output to file
	if err := os.WriteFile(outputPath, output, 0644); err != nil {
		return "", fmt.Errorf("failed to write HTML file: %w", err)
	}

	return filename, nil
}
