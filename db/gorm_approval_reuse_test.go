package db

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func approvalReuseTarget(t *testing.T, g *GormDB, scan string, user int, now time.Time, number int) *ApprovalTarget {
	t.Helper()
	_, _, err := g.AdmitApprovalScan(approvalTestAdmission(scan, user, now, number))
	require.NoError(t, err)
	target, err := approvalTestClaim(g, now)
	require.NoError(t, err)
	require.NotNil(t, target)
	return target
}

func approvalReuseFinish(t *testing.T, g *GormDB, target *ApprovalTarget, now time.Time, assessment string) {
	t.Helper()
	require.NoError(t, g.FinalizeApprovalTarget(target.ID, target.LeaseToken, now, ApprovalFinalization{ExecutionStatus: "completed", Decision: "needs_attention", Freshness: "current", AssessmentJSON: assessment, ReasonCodesJSON: "[]"}))
}

func TestApprovalReuseLookupIsPerUserAndLatestWins(t *testing.T) {
	g := approvalTestStore(t)
	now := time.Now().UTC()
	first := approvalReuseTarget(t, g, "first", 1, now, 1)
	approvalReuseFinish(t, g, first, now, `{"n":1}`)
	require.NoError(t, g.RecordApprovalReuse(1, "key", first.ID, now))
	found, err := g.FindApprovalReuse(1, "key")
	require.NoError(t, err)
	require.Equal(t, first.ID, found.TargetID)
	require.JSONEq(t, `{"n":1}`, found.AssessmentJSON)

	_, err = g.FindApprovalReuse(2, "key")
	require.ErrorIs(t, err, ErrApprovalNotFound)
	require.NoError(t, g.RecordApprovalReuse(2, "key", first.ID, now))
	_, err = g.FindApprovalReuse(2, "key")
	require.ErrorIs(t, err, ErrApprovalNotFound, "another user's target must never be reused")

	second := approvalReuseTarget(t, g, "second", 1, now, 2)
	approvalReuseFinish(t, g, second, now, `{"n":2}`)
	require.NoError(t, g.RecordApprovalReuse(1, "key", second.ID, now.Add(time.Second)))
	found, err = g.FindApprovalReuse(1, "key")
	require.NoError(t, err)
	require.Equal(t, second.ID, found.TargetID)
	require.JSONEq(t, `{"n":2}`, found.AssessmentJSON)
}

func TestApprovalReuseMissesUnfinishedTargets(t *testing.T) {
	g := approvalTestStore(t)
	now := time.Now().UTC()
	running := approvalReuseTarget(t, g, "running", 1, now, 1)
	require.NoError(t, g.RecordApprovalReuse(1, "key", running.ID, now))
	_, err := g.FindApprovalReuse(1, "key")
	require.ErrorIs(t, err, ErrApprovalNotFound)
	require.NoError(t, g.FinalizeApprovalTarget(running.ID, running.LeaseToken, now, ApprovalFinalization{ExecutionStatus: "failed", Freshness: "expired", ReasonCodesJSON: "[]"}))
	_, err = g.FindApprovalReuse(1, "key")
	require.ErrorIs(t, err, ErrApprovalNotFound)
}

func TestApprovalReusePrunedWithItsScan(t *testing.T) {
	g := approvalTestStore(t)
	now := time.Now().UTC()
	target := approvalReuseTarget(t, g, "old", 1, now, 1)
	approvalReuseFinish(t, g, target, now, `{"n":1}`)
	require.NoError(t, g.RecordApprovalReuse(1, "key", target.ID, now))
	count, err := g.PruneApprovalScans(now.Add(time.Second))
	require.NoError(t, err)
	require.EqualValues(t, 1, count)
	var entries int64
	require.NoError(t, g.db.Model(&approvalReuseEntry{}).Count(&entries).Error)
	require.Zero(t, entries)
	_, err = g.FindApprovalReuse(1, "key")
	require.ErrorIs(t, err, ErrApprovalNotFound)
}
