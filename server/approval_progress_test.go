package server

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"pr-review-server/db"
	"pr-review-server/pkg/approval"

	"github.com/stretchr/testify/require"
)

func TestApprovalProgressIsOwnedAndDoesNotReadGitHub(t *testing.T) {
	s, store, user, remote := newApprovalAPITestServer(t)
	scan := approvalAPIAdmit(t, s, user, remote.head, 1, 2)
	target, err := store.ClaimApprovalTarget(db.ApprovalClaim{Worker: "test", Now: time.Now(), LeaseDuration: time.Minute, TargetDuration: approvalTargetDuration, MaxSlots: 2})
	require.NoError(t, err)
	require.NoError(t, store.SetApprovalTargetProgress(target.ID, target.LeaseToken, "Reading review findings and discussion threads", time.Now()))
	remote.mu.Lock()
	reads := remote.reads
	remote.mu.Unlock()
	w := approvalAPICall(s, user, "GET", "/api/v1/approval-scans/"+scan.ID+"/progress", "", "")
	require.Equal(t, 200, w.Code)
	var progress approvalProgressResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &progress))
	require.Equal(t, 2, progress.Total)
	require.Equal(t, 1, progress.Running)
	require.Equal(t, 1, progress.Queued)
	require.Equal(t, "Reading review findings and discussion threads", progress.Summary)
	remote.mu.Lock()
	require.Equal(t, reads, remote.reads)
	remote.mu.Unlock()
	other := &db.User{ID: user.ID + 1, GitHubUsername: "another-user"}
	w = approvalAPICall(s, other, "GET", "/api/v1/approval-scans/"+scan.ID+"/progress", "", "")
	require.Equal(t, 404, w.Code)
	w = approvalAPICall(s, user, "POST", "/api/v1/approval-scans/"+scan.ID+"/progress", "{}", "fixture")
	require.Equal(t, 405, w.Code)
	require.NoError(t, store.FinalizeApprovalTarget(target.ID, target.LeaseToken, time.Now(), db.ApprovalFinalization{ExecutionStatus: "failed", Decision: "insufficient_evidence", Summary: "Final result"}))
	require.ErrorIs(t, store.SetApprovalTargetProgress(target.ID, target.LeaseToken, "Late progress", time.Now()), db.ErrApprovalLeaseLost)
	w = approvalAPICall(s, user, "GET", "/api/v1/approval-scans/"+scan.ID+"/progress", "", "")
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &progress))
	require.Equal(t, 1, progress.Finished)
	require.Equal(t, 0, progress.Running)
	require.NotEqual(t, "Final result", progress.Summary)
}

func TestApprovalProgressDiscardsSummaryWhenActivityChanges(t *testing.T) {
	s, store, target := approvalWorkerFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	events := make(chan approval.Activity, 1)
	ticks := make(chan time.Time)
	done := make(chan struct{})
	calls := make(chan struct{}, 2)
	summarize := func(context.Context, []approval.Activity) (string, error) {
		events <- approval.Activity{Stage: "validating"}
		calls <- struct{}{}
		return "A stale model summary", nil
	}
	go func() { defer close(done); s.runApprovalProgress(ctx, target, events, ticks, summarize) }()
	defer func() { cancel(); <-done }()
	ticks <- time.Now().Add(11 * time.Second)
	<-calls
	ticks <- time.Now().Add(12 * time.Second)
	require.Eventually(t, func() bool {
		row, err := store.GetApprovalTarget(target.UserID, target.ScanID, target.ID)
		return err == nil && row.Summary == "Rechecking current code and review evidence"
	}, 5*time.Second, time.Millisecond)
	select {
	case <-calls:
		t.Fatal("summaries must be spaced at least ten seconds apart")
	default:
	}
}

func TestApprovalProgressCancellationStopsSummaryRequest(t *testing.T) {
	s, _, target := approvalWorkerFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ticks := make(chan time.Time)
	started := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		s.runApprovalProgress(ctx, target, make(chan approval.Activity), ticks, func(ctx context.Context, _ []approval.Activity) (string, error) {
			close(started)
			<-ctx.Done()
			return "", ctx.Err()
		})
	}()
	ticks <- time.Now().Add(11 * time.Second)
	<-started
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("summary request outlived cancellation")
	}
}
