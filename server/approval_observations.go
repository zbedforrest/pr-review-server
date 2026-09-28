package server

import "time"

func (s *Server) observeApprovalEvent(eventType string, payload any) {
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
	_ = store.InvalidateApprovalTargets(owner, repo, number, "observed_pr_change", time.Now())
}
