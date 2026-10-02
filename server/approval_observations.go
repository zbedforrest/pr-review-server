package server

import (
	"time"

	"pr-review-server/db"
)

// recordQuickActionReview observes the dashboard update a quick review causes,
// then the review itself: the row has not seen the review yet, so observing it
// afterwards would clear the change request just recorded.
func (s *Server) recordQuickActionReview(user int, owner, repo string, number int, state string) {
	s.observeApprovalEvent(0, EventPRUpdated, map[string]interface{}{"owner": owner, "repo": repo, "number": number})
	s.observeOwnReview(user, owner, repo, number, state)
}

// observeOwnReview stales the user's candidates when they request changes from
// the dashboard; the pull request row only learns of that review on the next poll.
func (s *Server) observeOwnReview(user int, owner, repo string, number int, state string) {
	if state != "CHANGES_REQUESTED" || s.cfg == nil || !s.cfg.ApprovalCandidates().Enabled {
		return
	}
	store := s.approvalStore()
	if store == nil {
		return
	}
	_ = store.InvalidateMatchingApprovalTargets(user, owner, repo, number, db.ApprovalInvalidation{CandidatesOnly: true}, "human_changes_requested", time.Now())
}

func (s *Server) observeApprovalEvent(user int, eventType string, payload any) {
	if eventType != EventPRUpdated && eventType != EventPRDeleted {
		return
	}
	if s.cfg == nil || !s.cfg.ApprovalCandidates().Enabled {
		return
	}
	store := s.approvalStore()
	if store == nil {
		return
	}
	owner, repo, number, ok := extractPRIdentity(payload)
	if !ok {
		return
	}
	now := time.Now()
	for _, change := range s.approvalMaterialChanges(owner, repo, number) {
		_ = store.InvalidateMatchingApprovalTargets(user, owner, repo, number, change.match, change.reason, now)
	}
}

type approvalChange struct {
	reason string
	match  db.ApprovalInvalidation
}

// approvalMaterialChanges decides which approval results a dashboard update
// can change. Routine refreshes (reviewer groups, approval counts, review
// status) re-broadcast unchanged pull requests and must not mark results stale:
// a new head stales results for older heads, a blocking state (failing CI,
// requested changes, draft, running review) stales only candidates, and a
// cleared blocker re-stales the results it held back so they offer a recheck.

func (s *Server) approvalMaterialChanges(owner, repo string, number int) []approvalChange {
	if s.db == nil {
		return []approvalChange{{reason: "observed_pr_change"}}
	}
	pr, err := s.db.GetPR(owner, repo, number)
	if err != nil || pr == nil {
		return []approvalChange{{reason: "observed_pr_change"}}
	}
	if pr.PRState != "" && pr.PRState != "open" {
		return []approvalChange{{reason: "closed"}}
	}
	var changes []approvalChange
	if pr.LastCommitSHA != "" {
		changes = append(changes, approvalChange{"head_changed", db.ApprovalInvalidation{OffHead: pr.LastCommitSHA}})
	}
	candidates := db.ApprovalInvalidation{CandidatesOnly: true}
	if pr.CIState == "failure" || pr.CIState == "pending" {
		changes = append(changes, approvalChange{"observed_ci_change", candidates})
	}
	if pr.ReviewDecision == "CHANGES_REQUESTED" || pr.MyReviewStatus == "CHANGES_REQUESTED" {
		changes = append(changes, approvalChange{"human_changes_requested", candidates})
	}
	if pr.Draft {
		changes = append(changes, approvalChange{"pr_draft", candidates})
	}
	if reviewRunning(pr.Status) {
		changes = append(changes, approvalChange{"review_in_progress", candidates})
	}
	var cleared []string
	if pr.CIState == "success" {
		cleared = append(cleared, "ci_failed", "ci_pending", "observed_ci_change")
	}
	// An empty decision is also what unprotected repositories report, so it cannot prove a request was dismissed.
	if pr.ReviewDecision != "" && pr.ReviewDecision != "CHANGES_REQUESTED" && pr.MyReviewStatus != "CHANGES_REQUESTED" {
		cleared = append(cleared, "human_changes_requested", "observed_review_change")
	}
	if !pr.Draft {
		cleared = append(cleared, "pr_draft")
	}
	if !reviewRunning(pr.Status) {
		cleared = append(cleared, "review_in_progress")
	}
	if len(cleared) > 0 {
		changes = append(changes, approvalChange{"blocker_cleared", db.ApprovalInvalidation{Cleared: cleared}})
	}
	return changes
}

// reviewRunning matches the poller's in-flight states; "pending" is the idle default.
func reviewRunning(status string) bool {
	return status == "generating" || status == "agent_reviewing"
}
