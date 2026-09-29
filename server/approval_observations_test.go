package server

import (
	"github.com/stretchr/testify/require"
	"pr-review-server/db"
	"testing"
	"time"
)

func TestApprovalPollerEventInvalidatesCandidateBeforeBroadcast(t *testing.T) {
	s, store, target := approvalWorkerFixture(t)
	now := time.Now()
	until := now.Add(5 * time.Minute)
	require.NoError(t, store.FinalizeApprovalTarget(target.ID, target.LeaseToken, now, db.ApprovalFinalization{ExecutionStatus: "completed", Decision: "candidate", Freshness: "current", AssessmentJSON: `{"summary":"fixture"}`, ValidatedAt: &now, ValidUntil: &until}))
	s.BroadcastEvent(EventPRUpdated, map[string]interface{}{"owner": "acme", "repo": "example", "number": 123})
	result, err := store.GetApprovalTarget(target.UserID, target.ScanID, target.ID)
	require.NoError(t, err)
	require.Equal(t, "stale", result.Freshness)
	require.Contains(t, result.ReasonCodesJSON, "observed_pr_change")
}
