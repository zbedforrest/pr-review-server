package prioritization

import (
	"context"
	"fmt"
	"log"
	"sort"
	"time"

	"pr-review-server/db"
	"pr-review-server/github"
)

// PrioritizedPR represents a PR with its calculated priority score
type PrioritizedPR struct {
	Owner          string    `json:"owner"`
	Repo           string    `json:"repo"`
	Number         int       `json:"number"`
	Title          string    `json:"title"`
	Author         string    `json:"author"`
	Score          int       `json:"score"`
	Priority       string    `json:"priority"` // "HIGH", "MEDIUM", "LOW", "SKIP"
	PriorityEmoji  string    `json:"priority_emoji"`
	Reasons        []string  `json:"reasons"`
	AgeDays        int       `json:"age_days"`
	Additions      int       `json:"additions"`
	Deletions      int       `json:"deletions"`
	ChangedFiles   int       `json:"changed_files"`
	ReviewCount    int       `json:"review_count"`
	ApprovalCount  int       `json:"approval_count"`
	MyReviewStatus string    `json:"my_review_status"`
	GitHubURL      string    `json:"github_url"`
	ReviewURL      string    `json:"review_url"`
	CreatedAt      time.Time `json:"created_at"`
}

// Result contains the prioritization results
type Result struct {
	Timestamp       time.Time       `json:"timestamp"`
	TopPRs          []PrioritizedPR `json:"top_prs"`
	TotalPRsScored  int             `json:"total_prs_scored"`
	HighPriorityCount int           `json:"high_priority_count"`
	MediumPriorityCount int         `json:"medium_priority_count"`
	LowPriorityCount int            `json:"low_priority_count"`
}

// Prioritizer calculates priority scores for PRs
type Prioritizer struct {
	db       *db.DB
	ghClient *github.Client
	username string
}

// New creates a new Prioritizer
func New(database *db.DB, ghClient *github.Client, username string) *Prioritizer {
	return &Prioritizer{
		db:       database,
		ghClient: ghClient,
		username: username,
	}
}

// Calculate runs the prioritization algorithm and returns scored PRs
func (p *Prioritizer) Calculate(ctx context.Context) (*Result, error) {
	log.Println("[PRIORITIZATION] Starting PR prioritization calculation...")

	// Get all PRs from database
	dbPRs, err := p.db.GetAllPRs()
	if err != nil {
		return nil, fmt.Errorf("failed to get PRs from database: %w", err)
	}

	// Filter: only PRs that are not mine and not drafts
	var filteredPRs []*db.PR
	for i := range dbPRs {
		pr := &dbPRs[i]
		if !pr.IsMine && !pr.Draft {
			filteredPRs = append(filteredPRs, pr)
		}
	}

	if len(filteredPRs) == 0 {
		log.Println("[PRIORITIZATION] No PRs to prioritize (all are mine or drafts)")
		return &Result{
			Timestamp:       time.Now(),
			TopPRs:          []PrioritizedPR{},
			TotalPRsScored:  0,
		}, nil
	}

	log.Printf("[PRIORITIZATION] Analyzing %d PRs...", len(filteredPRs))

	// Convert to github.PullRequest format for batch fetching
	var ghPRs []github.PullRequest
	for _, pr := range filteredPRs {
		now := time.Now()
		ghPRs = append(ghPRs, github.PullRequest{
			Owner:     pr.RepoOwner,
			Repo:      pr.RepoName,
			Number:    pr.PRNumber,
			CreatedAt: &now, // Will be fetched from API
		})
	}

	// Batch fetch PR details using GraphQL (additions, deletions, review counts, etc.)
	prDetails, err := p.ghClient.BatchGetPRDetails(ctx, ghPRs)
	if err != nil {
		log.Printf("[PRIORITIZATION] Warning: Failed to fetch some PR details: %v", err)
		// Continue with what we have
	}

	// Score each PR
	var scoredPRs []PrioritizedPR
	for _, pr := range filteredPRs {
		key := fmt.Sprintf("%s/%s/%d", pr.RepoOwner, pr.RepoName, pr.PRNumber)
		details, hasDetails := prDetails[key]

		if !hasDetails {
			log.Printf("[PRIORITIZATION] Skipping PR %s (no details available)", key)
			continue
		}

		scored := p.scorePR(pr, details)
		scoredPRs = append(scoredPRs, scored)
	}

	// Sort by score (descending)
	sort.Slice(scoredPRs, func(i, j int) bool {
		return scoredPRs[i].Score > scoredPRs[j].Score
	})

	// Count by priority
	highCount, mediumCount, lowCount := 0, 0, 0
	for _, pr := range scoredPRs {
		switch pr.Priority {
		case "HIGH":
			highCount++
		case "MEDIUM":
			mediumCount++
		case "LOW":
			lowCount++
		}
	}

	log.Printf("[PRIORITIZATION] Complete: %d PRs scored (%d HIGH, %d MEDIUM, %d LOW)",
		len(scoredPRs), highCount, mediumCount, lowCount)

	return &Result{
		Timestamp:           time.Now(),
		TopPRs:              scoredPRs,
		TotalPRsScored:      len(scoredPRs),
		HighPriorityCount:   highCount,
		MediumPriorityCount: mediumCount,
		LowPriorityCount:    lowCount,
	}, nil
}

// scorePR calculates the priority score for a single PR
func (p *Prioritizer) scorePR(pr *db.PR, details *github.PRDetails) PrioritizedPR {
	score := 0
	var reasons []string

	// Calculate age in days
	ageDays := int(time.Since(details.CreatedAt).Hours() / 24)

	// 1. Age scoring (4+ days = +50, 3 days = +30, 2 days = +20, 1 day = +10)
	if ageDays >= 4 {
		score += 50
		reasons = append(reasons, fmt.Sprintf("Very old (%dd)", ageDays))
	} else if ageDays >= 3 {
		score += 30
		reasons = append(reasons, fmt.Sprintf("Old (%dd)", ageDays))
	} else if ageDays >= 2 {
		score += 20
		reasons = append(reasons, fmt.Sprintf("Aging (%dd)", ageDays))
	} else if ageDays >= 1 {
		score += 10
		reasons = append(reasons, fmt.Sprintf("Recent (%dd)", ageDays))
	}

	// 2. Approval gap (reviews but no approvals)
	if details.ReviewCount >= 3 && pr.ApprovalCount == 0 {
		score += 40
		reasons = append(reasons, fmt.Sprintf("%d reviews but no approvals", details.ReviewCount))
	}

	// 3. Size + attention gap
	if details.Additions >= 500 && details.ReviewCount < 2 {
		score += 30
		reasons = append(reasons, fmt.Sprintf("Large PR (%d+ lines) with few reviews", details.Additions))
	}

	// 4. Explicit request
	if details.RequestedMe {
		score += 25
		reasons = append(reasons, "You are explicitly requested")
	}

	// 5. Size factor
	if details.Additions >= 1000 {
		score += 20
		reasons = append(reasons, fmt.Sprintf("Very large (%d+ lines)", details.Additions))
	} else if details.Additions >= 500 {
		score += 10
		reasons = append(reasons, fmt.Sprintf("Large (%d+ lines)", details.Additions))
	}

	// 6. Already well-covered (penalty)
	if pr.ApprovalCount >= 1 && details.ReviewCount >= 5 {
		score -= 30
		reasons = append(reasons, fmt.Sprintf("Well-covered (%d approvals, %d reviews)", pr.ApprovalCount, details.ReviewCount))
	}

	// 7. Already reviewed by me (penalty)
	if pr.MyReviewStatus == "APPROVED" || pr.MyReviewStatus == "COMMENTED" {
		score -= 40
		reasons = append(reasons, fmt.Sprintf("You already reviewed (%s)", pr.MyReviewStatus))
	}

	// Determine priority level
	priority := "SKIP"
	priorityEmoji := "⚪"
	if score >= 60 {
		priority = "HIGH"
		priorityEmoji = "🔴"
	} else if score >= 30 {
		priority = "MEDIUM"
		priorityEmoji = "🟡"
	} else if score >= 0 {
		priority = "LOW"
		priorityEmoji = "🟢"
	}

	githubURL := fmt.Sprintf("https://github.com/%s/%s/pull/%d", pr.RepoOwner, pr.RepoName, pr.PRNumber)
	reviewURL := fmt.Sprintf("/reviews/%s", pr.ReviewHTMLPath)

	title := pr.Title
	if title == "" {
		title = fmt.Sprintf("PR #%d", pr.PRNumber)
	}

	author := pr.Author
	if author == "" {
		author = "Unknown"
	}

	return PrioritizedPR{
		Owner:          pr.RepoOwner,
		Repo:           pr.RepoName,
		Number:         pr.PRNumber,
		Title:          title,
		Author:         author,
		Score:          score,
		Priority:       priority,
		PriorityEmoji:  priorityEmoji,
		Reasons:        reasons,
		AgeDays:        ageDays,
		Additions:      details.Additions,
		Deletions:      details.Deletions,
		ChangedFiles:   details.ChangedFiles,
		ReviewCount:    details.ReviewCount,
		ApprovalCount:  pr.ApprovalCount,
		MyReviewStatus: pr.MyReviewStatus,
		GitHubURL:      githubURL,
		ReviewURL:      reviewURL,
		CreatedAt:      details.CreatedAt,
	}
}
