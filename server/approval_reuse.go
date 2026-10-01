package server

import (
	"encoding/json"
	"errors"
	"log"
	"strconv"

	"pr-review-server/config"
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
	return approvalDigestReuseKey(cfg, a.SnapshotDigest)
}

// approvalDigestReuseKey folds the reasoning effort into the model so an
// effort change does not reuse answers given under another effort.
func approvalDigestReuseKey(cfg config.ApprovalConfig, digest string) string {
	return approval.ReuseKey(digest, cfg.Provider, cfg.Provider+"/"+cfg.Model+"/"+strconv.Itoa(cfg.ReasoningTokens))
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
		found, err := s.approvalStore().FindApprovalReuse(user, approvalDigestReuseKey(cfg, snapshot.Digest))
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
	if cfg.Provider == approval.ProviderJev {
		// The scorer reports blockers itself so every pull request gets a score.
		return nil, false
	}
	a, ok := approval.Gate(snapshot)
	if !ok {
		return nil, false
	}
	// The admitted model is part of the configuration every finalized assessment must match.
	a.Model = cfg.Model
	return &a, true
}
