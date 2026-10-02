package server

import (
	"time"

	"pr-review-server/db"
)

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
// a new head stales results for older heads, and a blocking state (failing CI,
// requested changes, draft) stales only candidates.
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
	switch {
	case pr.CIState == "failure":
		changes = append(changes, approvalChange{"observed_ci_change", candidates})
	case pr.ReviewDecision == "CHANGES_REQUESTED":
		changes = append(changes, approvalChange{"human_changes_requested", candidates})
	case pr.Draft:
		changes = append(changes, approvalChange{"draft", candidates})
	}
	return changes
}
