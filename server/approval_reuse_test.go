package server

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"pr-review-server/db"
	"pr-review-server/pkg/approval"
)

type approvalReuseHarness struct {
	s                   *Server
	store               *db.GormDB
	first               db.ApprovalTarget
	collections, models int
	change              func(*approval.Snapshot)
	gaps                []string
}

func newApprovalReuseHarness(t *testing.T) *approvalReuseHarness {
	s, store, target := approvalWorkerFixture(t)
	h := &approvalReuseHarness{s: s, store: store, first: target}
	collector := s.approvalExecution.collector
	s.approvalExecution.collector = approvalCollectorFunc(func(ctx context.Context, target approval.Target, viewer approval.Viewer) (approval.Snapshot, error) {
		snapshot, err := collector.Collect(ctx, target, viewer)
		h.collections++
		if h.change != nil {
			h.change(&snapshot)
			approval.CanonicalizeSnapshot(&snapshot)
		}
		return snapshot, err
	})
	s.approvalExecution.investigator = func(db.ApprovalTarget) approval.Investigator {
		return approvalInvestigatorFunc(func(_ context.Context, snapshot approval.Snapshot, _ approval.Repository, _ approval.Budget) (approval.Assessment, error) {
			h.models++
			return approval.Evaluate(snapshot, approval.Assessment{SchemaVersion: "1", PolicyVersion: approval.PolicyVersion, PromptVersion: approval.PromptVersion, RuntimeVersion: approval.RuntimeVersion, Model: "fixture-model", SnapshotID: snapshot.ID, SnapshotDigest: snapshot.Digest, Summary: "The current review reports no concerns.", CoverageGaps: h.gaps, AssessedAt: time.Now().UTC(), Usage: approval.Usage{InputTokens: 100, OutputTokens: 10, Rounds: 1}}), nil
		})
	}
	return h
}

func (h *approvalReuseHarness) next(t *testing.T, scanID, kind string) db.ApprovalTarget {
	t.Helper()
	limits := approvalJSON(map[string]any{"provider": "anthropic", "model": "fixture-model", "policy_version": approval.PolicyVersion, "prompt_version": approval.PromptVersion, "runtime_version": approval.RuntimeVersion})
	_, _, err := h.store.AdmitApprovalScan(db.ApprovalAdmission{Scan: db.ApprovalScan{ID: scanID, UserID: h.first.UserID, Kind: kind, IdempotencyKey: scanID, RequestHash: scanID, LimitsJSON: limits}, Targets: []db.ApprovalTarget{{ID: scanID + "-target", Owner: "acme", Repo: "example", Number: 123, ExpectedHeadSHA: h.first.ExpectedHeadSHA, RepositoryID: 42}}, Now: time.Now(), DailyInputLimit: 6000000, DailyOutputLimit: 120000, TargetInputLimit: 600000, TargetOutputLimit: 12000})
	require.NoError(t, err)
	target, err := h.store.ClaimApprovalTarget(db.ApprovalClaim{Worker: "fixture", Now: time.Now(), LeaseDuration: time.Minute, TargetDuration: approvalTargetDuration, MaxSlots: 2})
	require.NoError(t, err)
	require.NotNil(t, target)
	return *target
}

func (h *approvalReuseHarness) run(t *testing.T, target db.ApprovalTarget) (*db.ApprovalTarget, approval.Assessment) {
	t.Helper()
	h.s.investigateApproval(context.Background(), target)
	result, err := h.store.GetApprovalTarget(target.UserID, target.ScanID, target.ID)
	require.NoError(t, err)
	require.Equal(t, "completed", result.ExecutionStatus, result.Summary)
	var assessment approval.Assessment
	require.NoError(t, json.Unmarshal([]byte(result.AssessmentJSON), &assessment))
	return result, assessment
}

func TestApprovalGateSkipsModelForFailingCI(t *testing.T) {
	h := newApprovalReuseHarness(t)
	h.change = func(s *approval.Snapshot) { s.Checks[0].State = "failure" }
	result, assessment := h.run(t, h.first)
	require.Zero(t, h.models)
	require.Equal(t, 1, h.collections)
	require.Equal(t, "needs_attention", result.Decision)
	require.Equal(t, "current", result.Freshness)
	require.Empty(t, result.BudgetDay)
	require.Zero(t, result.InputTokens)
	require.Equal(t, approval.OriginGate, assessment.Origin)
	require.Contains(t, assessment.ReasonCodes, "ci_failed")
	require.NotContains(t, assessment.ReasonCodes, "invalid_assessment")
	require.Equal(t, "CI is failing on the current head, so no investigation ran.", result.Summary)
	_, err := h.store.FindApprovalReuse(h.first.UserID, approvalDigestReuseKey(h.s.cfg.ApprovalCandidates(), assessment.SnapshotDigest))
	require.ErrorIs(t, err, db.ErrApprovalNotFound)
}

func TestApprovalReuseSkipsModelForIdenticalSnapshot(t *testing.T) {
	h := newApprovalReuseHarness(t)
	first, original := h.run(t, h.first)
	require.Equal(t, 1, h.models)
	require.Equal(t, 2, h.collections)
	require.Equal(t, "candidate", first.Decision)
	require.Equal(t, approval.OriginInvestigator, original.Origin)

	second, reused := h.run(t, h.next(t, "second-scan", "full"))
	require.Equal(t, 1, h.models)
	require.Equal(t, 3, h.collections)
	require.Equal(t, first.Decision, second.Decision)
	require.Equal(t, "current", second.Freshness)
	require.Empty(t, second.BudgetDay)
	require.Equal(t, approval.OriginReused, reused.Origin)
	require.Equal(t, h.first.ID, reused.ReusedFrom)
	require.Equal(t, approval.Usage{}, reused.Usage)
	require.True(t, reused.AssessedAt.Equal(original.AssessedAt))
}

func TestApprovalRecheckReinvestigates(t *testing.T) {
	h := newApprovalReuseHarness(t)
	h.run(t, h.first)
	_, assessment := h.run(t, h.next(t, "recheck-scan", "recheck"))
	require.Equal(t, 2, h.models)
	require.Equal(t, approval.OriginInvestigator, assessment.Origin)
}

func TestApprovalReuseMissesChangedEvidence(t *testing.T) {
	h := newApprovalReuseHarness(t)
	h.run(t, h.first)
	h.change = func(s *approval.Snapshot) {
		s.Checks = append(s.Checks, approval.Check{Name: "lint", State: "success", SHA: s.Revision.Head})
	}
	_, assessment := h.run(t, h.next(t, "changed-scan", "full"))
	require.Equal(t, 2, h.models)
	require.Equal(t, approval.OriginInvestigator, assessment.Origin)
}

func TestApprovalReuseMissesChangedReasoningBudget(t *testing.T) {
	h := newApprovalReuseHarness(t)
	h.run(t, h.first)
	t.Setenv("APPROVAL_CANDIDATES_REASONING_TOKENS", "3000")
	_, assessment := h.run(t, h.next(t, "effort-scan", "full"))
	require.Equal(t, 2, h.models)
	require.Equal(t, approval.OriginInvestigator, assessment.Origin)
}

func TestApprovalNonCandidateSkipsFinalCollection(t *testing.T) {
	h := newApprovalReuseHarness(t)
	h.gaps = []string{"The review did not cover every file."}
	result, assessment := h.run(t, h.first)
	require.Equal(t, 1, h.models)
	require.Equal(t, 1, h.collections)
	require.Equal(t, "insufficient_evidence", result.Decision)
	require.Equal(t, "current", result.Freshness)
	require.Equal(t, approval.OriginInvestigator, assessment.Origin)
	found, err := h.store.FindApprovalReuse(h.first.UserID, approvalDigestReuseKey(h.s.cfg.ApprovalCandidates(), assessment.SnapshotDigest))
	require.NoError(t, err)
	require.Equal(t, h.first.ID, found.TargetID)
	require.True(t, strings.Contains(found.AssessmentJSON, `"origin":"investigator"`))
}
