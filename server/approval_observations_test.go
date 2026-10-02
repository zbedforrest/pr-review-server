package server

import (
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"pr-review-server/db"
)

func finalizedCandidate(t *testing.T) (*Server, *db.GormDB, db.ApprovalTarget) {
	t.Helper()
	s, store, target := approvalWorkerFixture(t)
	now := time.Now()
	until := now.Add(5 * time.Minute)
	require.NoError(t, store.FinalizeApprovalTarget(target.ID, target.LeaseToken, now, db.ApprovalFinalization{ExecutionStatus: "completed", Decision: "candidate", Freshness: "current", AssessmentJSON: `{"summary":"fixture"}`, ValidatedAt: &now, ValidUntil: &until}))
	return s, store, target
}

func updatePR(t *testing.T, store *db.GormDB, change func(*db.PR)) {
	t.Helper()
	pr, err := store.GetPR("acme", "example", 123)
	require.NoError(t, err)
	change(pr)
	require.NoError(t, store.UpsertPR(pr))
}

func TestApprovalRoutinePRUpdateKeepsCandidateCurrent(t *testing.T) {
	s, store, target := finalizedCandidate(t)
	s.BroadcastEvent(EventPRUpdated, map[string]interface{}{"owner": "acme", "repo": "example", "number": 123})
	result, err := store.GetApprovalTarget(target.UserID, target.ScanID, target.ID)
	require.NoError(t, err)
	require.Equal(t, "current", result.Freshness)
}

func TestApprovalNewHeadInvalidatesCandidateBeforeBroadcast(t *testing.T) {
	s, store, target := finalizedCandidate(t)
	updatePR(t, store, func(pr *db.PR) { pr.LastCommitSHA = strings.Repeat("d", 40) })
	s.BroadcastEvent(EventPRUpdated, map[string]interface{}{"owner": "acme", "repo": "example", "number": 123})
	result, err := store.GetApprovalTarget(target.UserID, target.ScanID, target.ID)
	require.NoError(t, err)
	require.Equal(t, "stale", result.Freshness)
	require.Contains(t, result.ReasonCodesJSON, "head_changed")
}

func TestApprovalFailingCIInvalidatesCandidate(t *testing.T) {
	s, store, target := finalizedCandidate(t)
	updatePR(t, store, func(pr *db.PR) { pr.CIState = "failure" })
	s.BroadcastEvent(EventPRUpdated, map[string]interface{}{"owner": "acme", "repo": "example", "number": 123})
	result, err := store.GetApprovalTarget(target.UserID, target.ScanID, target.ID)
	require.NoError(t, err)
	require.Equal(t, "stale", result.Freshness)
	require.Contains(t, result.ReasonCodesJSON, "observed_ci_change")
}

func TestApprovalRunningReviewInvalidatesCandidate(t *testing.T) {
	s, store, target := finalizedCandidate(t)
	require.NoError(t, store.UpdatePRStatus("acme", "example", 123, "agent_reviewing"))
	s.BroadcastEvent(EventPRUpdated, map[string]interface{}{"owner": "acme", "repo": "example", "number": 123})
	result, err := store.GetApprovalTarget(target.UserID, target.ScanID, target.ID)
	require.NoError(t, err)
	require.Equal(t, "stale", result.Freshness)
	require.Contains(t, result.ReasonCodesJSON, "review_in_progress")
}
