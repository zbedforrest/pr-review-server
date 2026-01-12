package prioritization

import (
	"testing"
	"time"

	"pr-review-server/db"
	"pr-review-server/github"
)

// TestScorePR_HighPriority tests that a high-priority PR gets a high score
func TestScorePR_HighPriority(t *testing.T) {
	p := &Prioritizer{username: "testuser"}

	// Old PR (10 days), no approvals, I'm requested as reviewer
	createdAt := time.Now().Add(-10 * 24 * time.Hour)
	pr := &db.PR{
		RepoOwner:      "owner",
		RepoName:       "repo",
		PRNumber:       123,
		ApprovalCount:  0,
		MyReviewStatus: "", // I haven't reviewed yet
	}
	details := &github.PRDetails{
		CreatedAt:    createdAt,
		Additions:    200,
		Deletions:    50,
		ChangedFiles: 10,
		ReviewCount:  0,
		RequestedMe:  true,
	}

	scored := p.scorePR(pr, details)

	// Verify high score and priority
	if scored.Priority != "HIGH" {
		t.Errorf("Expected HIGH priority, got %s (score: %d)", scored.Priority, scored.Score)
	}

	// Verify reasons include key factors
	reasons := scored.Reasons
	if len(reasons) == 0 {
		t.Error("Expected reasons to be populated")
	}

	// Check for specific expected reasons (actual format from implementation)
	hasAgeReason := false
	hasRequestedReason := false
	for _, reason := range reasons {
		if reason == "Very old (10d)" {
			hasAgeReason = true
		}
		if reason == "You are explicitly requested" {
			hasRequestedReason = true
		}
	}

	if !hasAgeReason {
		t.Errorf("Expected age reason in high-priority PR, got reasons: %v", reasons)
	}
	if !hasRequestedReason {
		t.Errorf("Expected 'requested as reviewer' reason in high-priority PR, got reasons: %v", reasons)
	}
}

// TestScorePR_LowPriority tests that a PR with low score gets LOW priority
func TestScorePR_LowPriority(t *testing.T) {
	p := &Prioritizer{username: "testuser"}

	// Recent PR, small changes, no reviews yet, not requested
	// This should get a small positive score (under 30) -> LOW priority
	createdAt := time.Now().Add(-1 * 24 * time.Hour)
	pr := &db.PR{
		RepoOwner:      "owner",
		RepoName:       "repo",
		PRNumber:       456,
		ApprovalCount:  0,
		MyReviewStatus: "",
	}
	details := &github.PRDetails{
		CreatedAt:    createdAt,
		Additions:    50,
		Deletions:    10,
		ChangedFiles: 3,
		ReviewCount:  0,
		RequestedMe:  false,
	}

	scored := p.scorePR(pr, details)

	// Verify low score and priority
	if scored.Priority != "LOW" {
		t.Errorf("Expected LOW priority, got %s (score: %d)", scored.Priority, scored.Score)
	}

	// Verify score is in LOW range (0-29)
	if scored.Score < 0 || scored.Score >= 30 {
		t.Errorf("Expected score in LOW range (0-29), got %d", scored.Score)
	}
}

// TestScorePR_AlreadyReviewed tests that an already-reviewed PR gets negative score (SKIP)
func TestScorePR_AlreadyReviewed(t *testing.T) {
	p := &Prioritizer{username: "testuser"}

	// PR that I've already reviewed - should get -40 penalty
	createdAt := time.Now().Add(-2 * 24 * time.Hour)
	pr := &db.PR{
		RepoOwner:      "owner",
		RepoName:       "repo",
		PRNumber:       457,
		ApprovalCount:  1,
		MyReviewStatus: "APPROVED",
	}
	details := &github.PRDetails{
		CreatedAt:    createdAt,
		Additions:    50,
		Deletions:    10,
		ChangedFiles: 3,
		ReviewCount:  1,
		RequestedMe:  false,
	}

	scored := p.scorePR(pr, details)

	// Verify SKIP status (negative score)
	if scored.Priority != "SKIP" {
		t.Errorf("Expected SKIP priority for already-reviewed PR, got %s (score: %d)", scored.Priority, scored.Score)
	}

	// Check for "already reviewed" reason
	hasReviewedReason := false
	for _, reason := range scored.Reasons {
		if reason == "You already reviewed (APPROVED)" {
			hasReviewedReason = true
		}
	}

	if !hasReviewedReason {
		t.Errorf("Expected 'already reviewed' reason, got reasons: %v", scored.Reasons)
	}
}

// TestScorePR_Skip tests that a PR with negative score gets skipped
func TestScorePR_Skip(t *testing.T) {
	p := &Prioritizer{username: "testuser"}

	// Very new PR, I've reviewed it with changes requested, multiple reviews, approved
	createdAt := time.Now().Add(-6 * time.Hour)
	pr := &db.PR{
		RepoOwner:      "owner",
		RepoName:       "repo",
		PRNumber:       789,
		ApprovalCount:  3,
		MyReviewStatus: "CHANGES_REQUESTED",
	}
	details := &github.PRDetails{
		CreatedAt:    createdAt,
		Additions:    10,
		Deletions:    5,
		ChangedFiles: 1,
		ReviewCount:  5,
		RequestedMe:  false,
	}

	scored := p.scorePR(pr, details)

	// Verify SKIP status
	if scored.Priority != "SKIP" {
		t.Errorf("Expected SKIP priority, got %s (score: %d)", scored.Priority, scored.Score)
	}

	// Verify negative or very low score
	if scored.Score >= 10 {
		t.Errorf("Expected very low score for SKIP PR, got %d", scored.Score)
	}
}

// TestScorePR_MediumPriority tests that a moderately important PR gets medium priority
func TestScorePR_MediumPriority(t *testing.T) {
	p := &Prioritizer{username: "testuser"}

	// Moderately old PR (5 days), some reviews, not approved yet
	createdAt := time.Now().Add(-5 * 24 * time.Hour)
	pr := &db.PR{
		RepoOwner:      "owner",
		RepoName:       "repo",
		PRNumber:       321,
		ApprovalCount:  0,
		MyReviewStatus: "",
	}
	details := &github.PRDetails{
		CreatedAt:    createdAt,
		Additions:    100,
		Deletions:    30,
		ChangedFiles: 5,
		ReviewCount:  2,
		RequestedMe:  false,
	}

	scored := p.scorePR(pr, details)

	// Verify medium priority
	if scored.Priority != "MEDIUM" {
		t.Errorf("Expected MEDIUM priority, got %s (score: %d)", scored.Priority, scored.Score)
	}

	// Verify score is in medium range (between 50 and 90)
	if scored.Score < 50 || scored.Score >= 90 {
		t.Errorf("Expected score in medium range (50-90), got %d", scored.Score)
	}
}

// TestScorePR_LargeChanges tests that large PRs get appropriate score adjustments
func TestScorePR_LargeChanges(t *testing.T) {
	p := &Prioritizer{username: "testuser"}

	// Very large PR with many changes (>1000 lines)
	createdAt := time.Now().Add(-3 * 24 * time.Hour)
	pr := &db.PR{
		RepoOwner:      "owner",
		RepoName:       "repo",
		PRNumber:       999,
		ApprovalCount:  0,
		MyReviewStatus: "",
	}
	details := &github.PRDetails{
		CreatedAt:    createdAt,
		Additions:    2000,
		Deletions:    500,
		ChangedFiles: 50,
		ReviewCount:  0,
		RequestedMe:  false,
	}

	scored := p.scorePR(pr, details)

	// Verify that size is mentioned in reasons (actual format from implementation)
	hasSizeReason := false
	for _, reason := range scored.Reasons {
		// Additions >= 1000 triggers "Very large (1000+ lines)"
		if reason == "Very large (2000+ lines)" {
			hasSizeReason = true
		}
	}

	if !hasSizeReason {
		t.Errorf("Expected size reason for large PR, got reasons: %v", scored.Reasons)
	}
}

// TestScorePR_EmptyDetails tests that scoring handles empty details gracefully
func TestScorePR_EmptyDetails(t *testing.T) {
	p := &Prioritizer{username: "testuser"}

	// PR with minimal data
	pr := &db.PR{
		RepoOwner:      "owner",
		RepoName:       "repo",
		PRNumber:       111,
		ApprovalCount:  0,
		MyReviewStatus: "",
	}
	details := &github.PRDetails{
		CreatedAt:    time.Now(),
		Additions:    0,
		Deletions:    0,
		ChangedFiles: 0,
		ReviewCount:  0,
		RequestedMe:  false,
	}

	scored := p.scorePR(pr, details)

	// Should not panic and should return a valid result
	if scored.Owner != "owner" || scored.Repo != "repo" || scored.Number != 111 {
		t.Error("Expected valid PR identification in scored result")
	}

	// Score should be calculated (even if low)
	if scored.Score < 0 {
		t.Errorf("Expected non-negative base score, got %d", scored.Score)
	}
}
