package server

import (
	"encoding/json"
	"errors"
	"log"

	"pr-review-server/db"
	"pr-review-server/pkg/approval"
)

// approvalReuseKey is the key an assessment is recorded under after it is
// finalized; only investigator assessments are worth reusing.
func (s *Server) approvalReuseKey(a *approval.Assessment) string {
	if a == nil || a.Origin != approval.OriginInvestigator || s.cfg == nil {
		return ""
	}
	cfg := s.cfg.ApprovalCandidates()
	if a.Model != cfg.Model {
		return ""
	}
	return approval.ReuseKey(a.SnapshotDigest, cfg.Provider, cfg.Model)
}

// approvalPrejudged settles a target without the model: a full scan first
// reuses an identical earlier investigation, which explains more than a gate,
// then any scan applies the deterministic gates.
func (s *Server) approvalPrejudged(scan db.ApprovalScan, user int, snapshot approval.Snapshot) (*approval.Assessment, bool) {
	if s.cfg == nil {
		return nil, false
	}
	cfg := s.cfg.ApprovalCandidates()
	if scan.Kind != "recheck" {
		found, err := s.approvalStore().FindApprovalReuse(user, approval.ReuseKey(snapshot.Digest, cfg.Provider, cfg.Model))
		if err != nil && !errors.Is(err, db.ErrApprovalNotFound) {
			log.Printf("[APPROVAL] reuse lookup for scan %s: %v", scan.ID, err)
		}
		var prior approval.Assessment
		if err == nil && json.Unmarshal([]byte(found.AssessmentJSON), &prior) == nil && prior.Model == cfg.Model {
			if a, ok := approval.Reuse(snapshot, prior, found.TargetID); ok {
				return &a, true
			}
		}
	}
	a, ok := approval.Gate(snapshot)
	if !ok {
		return nil, false
	}
	// The admitted model is part of the configuration every finalized assessment must match.
	a.Model = cfg.Model
	return &a, true
}
