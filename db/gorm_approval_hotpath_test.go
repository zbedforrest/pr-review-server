package db

import (
	"fmt"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

func approvalTestPostgresStore(t *testing.T) *GormDB {
	t.Helper()
	if os.Getenv("APPROVAL_TEST_POSTGRES_DSN") == "" {
		t.Skip("set APPROVAL_TEST_POSTGRES_DSN; SQLite's write lock is database-wide")
	}
	return approvalTestStore(t)
}

func approvalTestClaimAll(t *testing.T, g *GormDB, now time.Time, maxClaims int) []ApprovalTarget {
	t.Helper()
	claimed, err := g.ClaimApprovalTargets(ApprovalClaim{Worker: "test", Now: now, LeaseDuration: time.Minute, TargetDuration: 5 * time.Minute, MaxSlots: 50, MaxPerUser: 50, MaxClaims: maxClaims})
	require.NoError(t, err)
	return claimed
}

func TestApprovalHotPathWritesDoNotWaitForTheGate(t *testing.T) {
	g := approvalTestPostgresStore(t)
	now := time.Now().UTC()
	_, _, err := g.AdmitApprovalScan(approvalTestAdmission("hot", 1, now, 1, 2))
	require.NoError(t, err)
	claimed := approvalTestClaimAll(t, g, now, 2)
	require.Len(t, claimed, 2)
	target := claimed[0]
	limits := ApprovalUsage{InputTokens: 600000, OutputTokens: 12000, Rounds: 16, ToolCalls: 40, ToolBytes: 1 << 20}
	operations := []struct {
		name string
		run  func() error
	}{
		{"heartbeat", func() error { return g.HeartbeatApprovalTarget(target.ID, target.LeaseToken, now, time.Minute) }},
		{"stage", func() error { return g.SetApprovalTargetStage(target.ID, target.LeaseToken, "investigating", now) }},
		{"progress", func() error {
			return g.SetApprovalTargetProgress(target.ID, target.LeaseToken, "Reading review evidence", now)
		}},
		{"snapshot", func() error { return g.SaveApprovalSnapshot(target.ID, target.LeaseToken, `{"digest":"one"}`, now) }},
		{"budget", func() error {
			return g.ReserveApprovalBudget(target.ID, target.LeaseToken, now, 1200000, 24000, 600000, 12000)
		}},
		{"reserve", func() error {
			return g.ReserveApprovalUsage(target.ID, target.LeaseToken, now, ApprovalUsageReservation{ID: "model", Usage: ApprovalUsage{InputTokens: 1000, OutputTokens: 100, Rounds: 1}, Limits: limits})
		}},
		{"settle", func() error {
			return g.SettleApprovalUsage(target.ID, target.LeaseToken, "model", now, ApprovalUsage{InputTokens: 900, OutputTokens: 90, Rounds: 1})
		}},
		{"flush", func() error {
			return g.FlushApprovalUsage(target.ID, target.LeaseToken, now, ApprovalUsage{ToolCalls: 3, ToolBytes: 300})
		}},
		{"finalize", func() error {
			return g.FinalizeApprovalTarget(target.ID, target.LeaseToken, now, ApprovalFinalization{ExecutionStatus: "completed", Decision: "needs_attention", Freshness: "expired", ReasonCodesJSON: "[]"})
		}},
	}
	for _, operation := range operations {
		reached, beforeRelease, err := approvalTestBesideGate(t, g, operation.run)
		require.NoError(t, err, operation.name)
		require.False(t, reached, operation.name)
		require.True(t, beforeRelease, operation.name)
	}
	saved, err := g.GetApprovalTarget(1, "hot", target.ID)
	require.NoError(t, err)
	require.Equal(t, "completed", saved.ExecutionStatus)
	require.EqualValues(t, 900, saved.InputTokens)
	require.Equal(t, 3, saved.ToolCalls)
	require.EqualValues(t, 300, saved.ToolBytes)
}

func TestApprovalBatchClaimRespectsCapsOrderAndUniqueness(t *testing.T) {
	g := approvalTestStore(t)
	now := time.Date(2026, 9, 28, 12, 0, 0, 0, time.UTC)
	_, _, err := g.AdmitApprovalScan(approvalTestAdmission("first", 1, now, 1, 2, 3, 4, 5))
	require.NoError(t, err)
	_, _, err = g.AdmitApprovalScan(approvalTestAdmission("second", 2, now.Add(time.Second), 1, 2, 3))
	require.NoError(t, err)
	claim := ApprovalClaim{Worker: "batch", Now: now.Add(2 * time.Second), LeaseDuration: time.Minute, TargetDuration: 5 * time.Minute, MaxSlots: 6, MaxPerUser: 3, MaxClaims: 4}
	first, err := g.ClaimApprovalTargets(claim)
	require.NoError(t, err)
	ids := []string{}
	for _, target := range first {
		ids = append(ids, target.ID)
	}
	require.Equal(t, []string{"first-1", "first-2", "first-3", "second-1"}, ids)
	claim.MaxClaims = 10
	second, err := g.ClaimApprovalTargets(claim)
	require.NoError(t, err)
	ids = ids[:0]
	for _, target := range second {
		ids = append(ids, target.ID)
	}
	require.Equal(t, []string{"second-2", "second-3"}, ids)
	third, err := g.ClaimApprovalTargets(claim)
	require.NoError(t, err)
	require.Empty(t, third)
	tokens := map[string]bool{}
	for _, target := range append(first, second...) {
		require.Equal(t, "collecting", target.ExecutionStatus)
		require.Equal(t, 1, target.Attempts)
		require.True(t, strings.HasPrefix(target.LeaseToken, "batch:"))
		require.True(t, strings.HasSuffix(target.LeaseToken, ":"+target.ID))
		require.False(t, tokens[target.LeaseToken])
		tokens[target.LeaseToken] = true
		require.NoError(t, g.HeartbeatApprovalTarget(target.ID, target.LeaseToken, claim.Now, time.Minute))
	}
	for _, id := range []string{"first", "second"} {
		scan, err := g.GetApprovalScan(map[string]int{"first": 1, "second": 2}[id], id)
		require.NoError(t, err)
		require.Equal(t, "running", scan.Status)
	}
}

func TestApprovalConcurrentFinalizesSettleTheScan(t *testing.T) {
	g := approvalTestStore(t)
	now := time.Now().UTC()
	numbers := make([]int, 20)
	for i := range numbers {
		numbers[i] = i + 1
	}
	_, _, err := g.AdmitApprovalScan(approvalTestAdmission("many", 1, now, numbers...))
	require.NoError(t, err)
	claimed := approvalTestClaimAll(t, g, now, 50)
	require.Len(t, claimed, 20)
	var wg sync.WaitGroup
	errs := make(chan error, len(claimed))
	for _, target := range claimed {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs <- approvalTestFinish(g, &target, now)
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		require.NoError(t, err)
	}
	approvalTestClaimAll(t, g, now, 1)
	scan, err := g.GetApprovalScan(1, "many")
	require.NoError(t, err)
	require.Equal(t, "completed", scan.Status)
	require.NotNil(t, scan.CompletedAt)
}

func TestApprovalConcurrentBudgetTotalsAreExact(t *testing.T) {
	g := approvalTestStore(t)
	now := time.Date(2026, 9, 28, 12, 0, 0, 0, time.UTC)
	numbers := make([]int, 16)
	for i := range numbers {
		numbers[i] = i + 1
	}
	r := approvalTestAdmission("budget", 1, now, numbers...)
	r.DailyInputLimit, r.DailyOutputLimit = 0, 0
	_, _, err := g.AdmitApprovalScan(r)
	require.NoError(t, err)
	claimed := approvalTestClaimAll(t, g, now, 50)
	require.Len(t, claimed, 16)
	limits := ApprovalUsage{InputTokens: 600000, OutputTokens: 12000, Rounds: 16, ToolCalls: 40, ToolBytes: 1 << 20}
	var wg sync.WaitGroup
	errs := make(chan error, len(claimed))
	for i, target := range claimed {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs <- func() error {
				if err := g.ReserveApprovalBudget(target.ID, target.LeaseToken, now, 0, 0, 600000, 12000); err != nil {
					return err
				}
				if err := g.ReserveApprovalUsage(target.ID, target.LeaseToken, now, ApprovalUsageReservation{ID: target.ID + "-call", Usage: ApprovalUsage{InputTokens: 100000, OutputTokens: 4096, Rounds: 1}, Limits: limits}); err != nil {
					return err
				}
				if err := g.SettleApprovalUsage(target.ID, target.LeaseToken, target.ID+"-call", now, ApprovalUsage{InputTokens: int64(1000 + i), OutputTokens: int64(100 + i), Rounds: 1}); err != nil {
					return err
				}
				return g.FinalizeApprovalTarget(target.ID, target.LeaseToken, now, ApprovalFinalization{ExecutionStatus: "completed", Decision: "insufficient_evidence", Freshness: "expired"})
			}()
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		require.NoError(t, err)
	}
	var budget approvalDailyBudget
	require.NoError(t, g.db.First(&budget, "day = ?", "2026-09-28").Error)
	require.EqualValues(t, 16*1000+120, budget.InputTokens)
	require.EqualValues(t, 16*100+120, budget.OutputTokens)
}

func TestApprovalCarryAndFlushReachDurableCounters(t *testing.T) {
	g := approvalTestStore(t)
	now := time.Now().UTC()
	_, _, err := g.AdmitApprovalScan(approvalTestAdmission("carry", 1, now, 1))
	require.NoError(t, err)
	target, err := approvalTestClaim(g, now)
	require.NoError(t, err)
	require.NoError(t, g.ReserveApprovalBudget(target.ID, target.LeaseToken, now, 0, 0, 600000, 12000))
	limits := ApprovalUsage{InputTokens: 600000, OutputTokens: 12000, Rounds: 16, ToolCalls: 4, ToolBytes: 1 << 20}
	require.ErrorIs(t, g.ReserveApprovalUsage(target.ID, target.LeaseToken, now, ApprovalUsageReservation{ID: "over", Usage: ApprovalUsage{InputTokens: 10, OutputTokens: 10, Rounds: 1}, Limits: limits, Carry: ApprovalUsage{ToolCalls: 5}}), ErrApprovalBudget)
	require.NoError(t, g.ReserveApprovalUsage(target.ID, target.LeaseToken, now, ApprovalUsageReservation{ID: "call", Usage: ApprovalUsage{InputTokens: 10, OutputTokens: 10, Rounds: 1}, Limits: limits, Carry: ApprovalUsage{ToolCalls: 2, ToolBytes: 500}}))
	require.Error(t, g.FlushApprovalUsage(target.ID, target.LeaseToken, now, ApprovalUsage{InputTokens: 1}))
	require.NoError(t, g.FlushApprovalUsage(target.ID, target.LeaseToken, now, ApprovalUsage{ToolCalls: 1, ToolBytes: 40}))
	saved, err := g.GetApprovalTarget(1, "carry", target.ID)
	require.NoError(t, err)
	require.Equal(t, 3, saved.ToolCalls)
	require.EqualValues(t, 540, saved.ToolBytes)
	require.Equal(t, 1, saved.Rounds)
	require.ErrorIs(t, approvalTestFinish(g, target, now), ErrApprovalBudget)
	require.NoError(t, g.SettleApprovalUsage(target.ID, target.LeaseToken, "call", now, ApprovalUsage{InputTokens: 5, OutputTokens: 5, Rounds: 1}))
	require.NoError(t, approvalTestFinish(g, target, now))
	require.ErrorIs(t, g.FlushApprovalUsage(target.ID, target.LeaseToken, now, ApprovalUsage{ToolCalls: 1}), ErrApprovalLeaseLost)
}

// approvalTestOnFirstTargetQuery runs hook once, after the next query that
// reads approval targets.
func approvalTestOnFirstTargetQuery(t *testing.T, g *GormDB, hook func()) {
	t.Helper()
	var armed atomic.Bool
	armed.Store(true)
	name := fmt.Sprintf("approval_test_hook_%p", hook)
	require.NoError(t, g.db.Callback().Query().After("gorm:query").Register(name, func(tx *gorm.DB) {
		if tx.Statement.Table == "approval_targets" && armed.CompareAndSwap(true, false) {
			hook()
		}
	}))
	t.Cleanup(func() { _ = g.db.Callback().Query().Remove(name) })
}

func TestApprovalClaimHousekeepingNeverOverwritesAConcurrentFinalize(t *testing.T) {
	g := approvalTestPostgresStore(t)
	now := time.Date(2031, 4, 5, 12, 0, 0, 0, time.UTC)
	_, _, err := g.AdmitApprovalScan(approvalTestAdmission("race", 1, now, 1, 2))
	require.NoError(t, err)
	first, err := g.ClaimApprovalTarget(ApprovalClaim{Worker: "a", Now: now, LeaseDuration: time.Minute, TargetDuration: 10 * time.Minute, MaxSlots: 1})
	require.NoError(t, err)
	recoveredAt := now.Add(61 * time.Second)
	recovered, err := g.ClaimApprovalTarget(ApprovalClaim{Worker: "b", Now: recoveredAt, LeaseDuration: time.Minute, TargetDuration: 10 * time.Minute, MaxSlots: 1})
	require.NoError(t, err)
	require.Equal(t, first.ID, recovered.ID)
	require.Equal(t, 2, recovered.Attempts)
	finalized := make(chan error, 1)
	approvalTestOnFirstTargetQuery(t, g, func() {
		finalized <- approvalTestFinish(g, recovered, recoveredAt.Add(time.Second))
	})
	_, err = g.ClaimApprovalTargets(ApprovalClaim{Worker: "c", Now: recoveredAt.Add(61 * time.Second), LeaseDuration: time.Minute, TargetDuration: 10 * time.Minute, MaxSlots: 1, MaxClaims: 1})
	require.NoError(t, err)
	require.NoError(t, <-finalized)
	saved, err := g.GetApprovalTarget(1, "race", recovered.ID)
	require.NoError(t, err)
	require.Equal(t, "completed", saved.ExecutionStatus)
	require.Equal(t, "candidate", saved.Decision)
	require.NotEmpty(t, saved.AssessmentJSON)
}

func TestApprovalCancelRacingFinalizeNeverLeavesACandidateAfterCancellation(t *testing.T) {
	g := approvalTestPostgresStore(t)
	now := time.Now().UTC()
	_, _, err := g.AdmitApprovalScan(approvalTestAdmission("cancel-race", 1, now, 1, 2))
	require.NoError(t, err)
	claimed := approvalTestClaimAll(t, g, now, 2)
	require.Len(t, claimed, 2)
	finalized := make(chan error, 1)
	var calls atomic.Int32
	name := "approval_test_cancel_holds_row"
	require.NoError(t, g.db.Callback().Update().After("gorm:update").Register(name, func(tx *gorm.DB) {
		target, ok := tx.Statement.Dest.(*ApprovalTarget)
		if !ok || target.ID != claimed[0].ID || target.ExecutionStatus != "cancelled" || calls.Add(1) != 1 {
			return
		}
		go func() { finalized <- approvalTestFinish(g, &claimed[0], now) }()
		time.Sleep(300 * time.Millisecond)
	}))
	t.Cleanup(func() { _ = g.db.Callback().Update().Remove(name) })
	require.NoError(t, g.CancelApprovalScan(1, "cancel-race", now))
	require.ErrorIs(t, <-finalized, ErrApprovalLeaseLost)
	saved, err := g.GetApprovalTarget(1, "cancel-race", claimed[0].ID)
	require.NoError(t, err)
	require.Equal(t, "cancelled", saved.ExecutionStatus)
	require.Empty(t, saved.AssessmentJSON)
	require.ErrorIs(t, approvalTestFinish(g, &claimed[1], now), ErrApprovalLeaseLost)
}

func TestApprovalCancelAndFinalizeRaceStaysConsistent(t *testing.T) {
	g := approvalTestStore(t)
	now := time.Now().UTC()
	for round := range 10 {
		id := fmt.Sprintf("round-%d", round)
		_, _, err := g.AdmitApprovalScan(approvalTestAdmission(id, round+1, now, 1))
		require.NoError(t, err)
		claimed := approvalTestClaimAll(t, g, now, 1)
		require.Len(t, claimed, 1)
		var wg sync.WaitGroup
		var finalizeErr, cancelErr error
		wg.Add(2)
		go func() { defer wg.Done(); finalizeErr = approvalTestFinish(g, &claimed[0], now) }()
		go func() { defer wg.Done(); cancelErr = g.CancelApprovalScan(round+1, id, now) }()
		wg.Wait()
		require.NoError(t, cancelErr)
		saved, err := g.GetApprovalTarget(round+1, id, claimed[0].ID)
		require.NoError(t, err)
		if finalizeErr == nil {
			require.Equal(t, "completed", saved.ExecutionStatus)
			require.Equal(t, "candidate", saved.Decision)
		} else {
			require.ErrorIs(t, finalizeErr, ErrApprovalLeaseLost)
			require.Equal(t, "cancelled", saved.ExecutionStatus)
			require.Empty(t, saved.AssessmentJSON)
		}
	}
}

func TestApprovalCancelTerminatesATargetWhoseStageMovedAfterItsRead(t *testing.T) {
	g := approvalTestStore(t)
	now := time.Now().UTC()
	_, _, err := g.AdmitApprovalScan(approvalTestAdmission("moved", 1, now, 1))
	require.NoError(t, err)
	claimed := approvalTestClaimAll(t, g, now, 1)
	require.Len(t, claimed, 1)
	require.NoError(t, g.SetApprovalTargetStage(claimed[0].ID, claimed[0].LeaseToken, "collecting", now))
	var armed atomic.Bool
	armed.Store(true)
	name := "approval_test_stage_moves"
	require.NoError(t, g.db.Callback().Query().After("gorm:query").Register(name, func(tx *gorm.DB) {
		if tx.Statement.Table == "approval_targets" && armed.CompareAndSwap(true, false) {
			require.NoError(t, tx.Session(&gorm.Session{NewDB: true}).Exec("UPDATE approval_targets SET execution_status = ? WHERE id = ?", "investigating", claimed[0].ID).Error)
		}
	}))
	t.Cleanup(func() { _ = g.db.Callback().Query().Remove(name) })
	require.NoError(t, g.CancelApprovalScan(1, "moved", now))
	saved, err := g.GetApprovalTarget(1, "moved", claimed[0].ID)
	require.NoError(t, err)
	require.Equal(t, "cancelled", saved.ExecutionStatus)
}
