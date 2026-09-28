package db

import (
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
)

func approvalTestStore(t *testing.T) *GormDB {
	t.Helper()
	if dsn := os.Getenv("APPROVAL_TEST_POSTGRES_DSN"); dsn != "" {
		root, err := gorm.Open(postgres.Open(dsn), &gorm.Config{})
		require.NoError(t, err)
		schema := "approval_test_" + approvalToken()[:12]
		require.NoError(t, root.Exec("CREATE SCHEMA "+schema).Error)
		t.Cleanup(func() {
			require.NoError(t, root.Exec("DROP SCHEMA "+schema+" CASCADE").Error)
			sql, err := root.DB()
			require.NoError(t, err)
			require.NoError(t, sql.Close())
		})
		u, err := url.Parse(dsn)
		require.NoError(t, err)
		q := u.Query()
		q.Set("search_path", schema)
		u.RawQuery = q.Encode()
		g, err := NewGormPostgres(u.String())
		require.NoError(t, err)
		t.Cleanup(func() { require.NoError(t, g.Close()) })
		return g
	}
	g, err := NewGormSQLite(filepath.Join(t.TempDir(), "approval.db") + "?_busy_timeout=5000&_journal_mode=WAL")
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, g.Close()) })
	return g
}

func approvalTestAdmission(id string, user int, now time.Time, numbers ...int) ApprovalAdmission {
	r := ApprovalAdmission{Scan: ApprovalScan{ID: id, UserID: user, Kind: "full", IdempotencyKey: id, RequestHash: "hash-" + id, ScopeJSON: "{}", LimitsJSON: "{}"}, Now: now, DailyInputLimit: 1200000, DailyOutputLimit: 24000, TargetInputLimit: 600000, TargetOutputLimit: 12000}
	for _, number := range numbers {
		r.Targets = append(r.Targets, ApprovalTarget{ID: fmt.Sprintf("%s-%d", id, number), Owner: "acme", Repo: "example", Number: number, ExpectedHeadSHA: strings.Repeat("a", 40)})
	}
	return r
}
func approvalTestClaim(g *GormDB, now time.Time) (*ApprovalTarget, error) {
	return g.ClaimApprovalTarget(ApprovalClaim{Worker: "test", Now: now, LeaseDuration: 60 * time.Second, TargetDuration: 180 * time.Second, MaxSlots: 2})
}
func approvalTestFinish(g *GormDB, target *ApprovalTarget, now time.Time) error {
	until := now.Add(5 * time.Minute)
	return g.FinalizeApprovalTarget(target.ID, target.LeaseToken, now, ApprovalFinalization{ExecutionStatus: "completed", Decision: "candidate", Freshness: "current", AssessmentJSON: `{"decision":"candidate"}`, ReasonCodesJSON: "[]", ValidatedAt: &now, ValidUntil: &until})
}

func TestApprovalAdmissionOwnershipAndIdempotency(t *testing.T) {
	g := approvalTestStore(t)
	now := time.Now().UTC()
	r := approvalTestAdmission("one", 1, now, 1, 2)
	s, replay, err := g.AdmitApprovalScan(r)
	require.NoError(t, err)
	require.False(t, replay)
	require.Equal(t, 2, s.Total)
	s, replay, err = g.AdmitApprovalScan(r)
	require.NoError(t, err)
	require.True(t, replay)
	require.Equal(t, "one", s.ID)
	r.Scan.RequestHash = "different"
	_, _, err = g.AdmitApprovalScan(r)
	require.ErrorIs(t, err, ErrApprovalIdempotency)
	_, _, err = g.AdmitApprovalScan(approvalTestAdmission("two", 1, now, 3))
	var conflict *ApprovalActiveConflict
	require.ErrorAs(t, err, &conflict)
	require.Equal(t, "one", conflict.ScanID)
	_, err = g.GetApprovalScan(2, "one")
	require.ErrorIs(t, err, ErrApprovalNotFound)
	_, err = g.GetApprovalTarget(2, "one", "one-1")
	require.ErrorIs(t, err, ErrApprovalNotFound)
	require.ErrorIs(t, g.CancelApprovalScan(2, "one", now), ErrApprovalNotFound)
	_, err = g.GetApprovalTarget(1, "wrong", "one-1")
	require.ErrorIs(t, err, ErrApprovalNotFound)
}

func TestApprovalConcurrentAdmissionAndClaims(t *testing.T) {
	g := approvalTestStore(t)
	now := time.Now().UTC()
	var wg sync.WaitGroup
	errs := make(chan error, 2)
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, _, err := g.AdmitApprovalScan(approvalTestAdmission(fmt.Sprintf("scan-%d", i), 1, now, 1, 2))
			errs <- err
		}(i)
	}
	wg.Wait()
	close(errs)
	ok, conflicts := 0, 0
	for err := range errs {
		if err == nil {
			ok++
		} else {
			var conflict *ApprovalActiveConflict
			require.ErrorAs(t, err, &conflict)
			conflicts++
		}
	}
	require.Equal(t, 1, ok)
	require.Equal(t, 1, conflicts)
	_, _, err := g.AdmitApprovalScan(approvalTestAdmission("user-two", 2, now, 3))
	require.NoError(t, err)
	_, _, err = g.AdmitApprovalScan(approvalTestAdmission("user-three", 3, now, 4))
	require.NoError(t, err)
	claims := make(chan *ApprovalTarget, 8)
	errs = make(chan error, 8)
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); target, err := approvalTestClaim(g, now); claims <- target; errs <- err }()
	}
	wg.Wait()
	close(claims)
	close(errs)
	for err := range errs {
		require.NoError(t, err)
	}
	users := map[int]bool{}
	ids := map[string]bool{}
	for claim := range claims {
		if claim != nil {
			require.False(t, users[claim.UserID])
			require.False(t, ids[claim.ID])
			users[claim.UserID] = true
			ids[claim.ID] = true
		}
	}
	require.Len(t, ids, 2)
}

func TestApprovalRecoveryPreservesDeadlineAndCharges(t *testing.T) {
	g := approvalTestStore(t)
	now := time.Now().UTC()
	_, _, err := g.AdmitApprovalScan(approvalTestAdmission("recover", 1, now, 1))
	require.NoError(t, err)
	target, err := approvalTestClaim(g, now)
	require.NoError(t, err)
	require.NotNil(t, target)
	require.NoError(t, g.ReserveApprovalBudget(target.ID, target.LeaseToken, now, 1200000, 24000, 600000, 12000))
	reservation := ApprovalUsageReservation{ID: "call", Usage: ApprovalUsage{InputTokens: 100000, OutputTokens: 4096, Rounds: 1}, Limits: ApprovalUsage{InputTokens: 600000, OutputTokens: 12000, Rounds: 16, ToolCalls: 40, ToolBytes: 1 << 20}}
	require.NoError(t, g.ReserveApprovalUsage(target.ID, target.LeaseToken, now, reservation))
	recovered, err := approvalTestClaim(g, now.Add(61*time.Second))
	require.NoError(t, err)
	require.NotNil(t, recovered)
	require.Equal(t, 2, recovered.Attempts)
	require.Equal(t, target.Deadline.Unix(), recovered.Deadline.Unix())
	require.EqualValues(t, 100000, recovered.InputTokens)
	require.EqualValues(t, 4096, recovered.OutputTokens)
	require.ErrorIs(t, approvalTestFinish(g, target, now.Add(62*time.Second)), ErrApprovalLeaseLost)
	require.ErrorIs(t, g.SettleApprovalUsage(target.ID, recovered.LeaseToken, "call", now.Add(62*time.Second), ApprovalUsage{}), ErrApprovalLeaseLost)
	next, err := approvalTestClaim(g, now.Add(122*time.Second))
	require.NoError(t, err)
	require.Nil(t, next)
	saved, err := g.GetApprovalTarget(1, "recover", target.ID)
	require.NoError(t, err)
	require.Equal(t, "failed", saved.ExecutionStatus)
	require.Contains(t, saved.ReasonCodesJSON, "recovery_exhausted")
	var budget approvalDailyBudget
	require.NoError(t, g.db.First(&budget).Error)
	require.EqualValues(t, 100000, budget.InputTokens)
	require.EqualValues(t, 4096, budget.OutputTokens)
}

func TestApprovalBudgetSettlementAndMidnight(t *testing.T) {
	g := approvalTestStore(t)
	now := time.Date(2026, 9, 28, 23, 59, 30, 0, time.UTC)
	_, _, err := g.AdmitApprovalScan(approvalTestAdmission("budget", 1, now, 1))
	require.NoError(t, err)
	target, err := approvalTestClaim(g, now)
	require.NoError(t, err)
	require.NoError(t, g.ReserveApprovalBudget(target.ID, target.LeaseToken, now, 600000, 12000, 600000, 12000))
	limits := ApprovalUsage{InputTokens: 600000, OutputTokens: 12000, Rounds: 16, ToolCalls: 40, ToolBytes: 1 << 20}
	require.NoError(t, g.ReserveApprovalUsage(target.ID, target.LeaseToken, now, ApprovalUsageReservation{ID: "model", Usage: ApprovalUsage{InputTokens: 100000, OutputTokens: 4096, Rounds: 1}, Limits: limits}))
	require.NoError(t, g.SettleApprovalUsage(target.ID, target.LeaseToken, "model", now, ApprovalUsage{InputTokens: 1000, OutputTokens: 200}))
	require.ErrorIs(t, g.SettleApprovalUsage(target.ID, target.LeaseToken, "model", now, ApprovalUsage{}), ErrApprovalLeaseLost)
	require.NoError(t, g.ReserveApprovalUsage(target.ID, target.LeaseToken, now, ApprovalUsageReservation{ID: "tool", Usage: ApprovalUsage{ToolCalls: 1, ToolBytes: 65536}, Limits: limits}))
	require.NoError(t, g.SettleApprovalUsage(target.ID, target.LeaseToken, "tool", now, ApprovalUsage{ToolCalls: 1, ToolBytes: 200}))
	r := approvalTestAdmission("blocked", 2, now, 2)
	r.DailyInputLimit = 600000
	r.DailyOutputLimit = 12000
	_, _, err = g.AdmitApprovalScan(r)
	require.ErrorIs(t, err, ErrApprovalBudget)
	later := now.Add(40 * time.Second)
	require.NoError(t, g.ReserveApprovalBudget(target.ID, target.LeaseToken, later, 600000, 12000, 600000, 12000))
	require.NoError(t, approvalTestFinish(g, target, later))
	var budgets []approvalDailyBudget
	require.NoError(t, g.db.Find(&budgets).Error)
	require.Len(t, budgets, 1)
	require.Equal(t, "2026-09-28", budgets[0].Day)
	require.EqualValues(t, 1000, budgets[0].InputTokens)
	require.EqualValues(t, 200, budgets[0].OutputTokens)
	r.Now = later
	_, _, err = g.AdmitApprovalScan(r)
	require.NoError(t, err)
}

func TestApprovalCancelPreservesCompletedAndFencesRemaining(t *testing.T) {
	g := approvalTestStore(t)
	now := time.Now().UTC()
	_, _, err := g.AdmitApprovalScan(approvalTestAdmission("cancel", 1, now, 1, 2, 3))
	require.NoError(t, err)
	first, err := approvalTestClaim(g, now)
	require.NoError(t, err)
	require.NoError(t, approvalTestFinish(g, first, now))
	second, err := approvalTestClaim(g, now)
	require.NoError(t, err)
	require.NotNil(t, second)
	require.NoError(t, g.CancelApprovalScan(1, "cancel", now))
	require.NoError(t, g.CancelApprovalScan(1, "cancel", now))
	require.ErrorIs(t, approvalTestFinish(g, second, now), ErrApprovalLeaseLost)
	require.ErrorIs(t, g.HeartbeatApprovalTarget(second.ID, second.LeaseToken, now, time.Minute), ErrApprovalLeaseLost)
	scan, err := g.GetApprovalScan(1, "cancel")
	require.NoError(t, err)
	require.Equal(t, "cancelled", scan.Status)
	targets, err := g.ListApprovalTargets(1, "cancel", 100, "")
	require.NoError(t, err)
	require.Equal(t, "completed", targets[0].ExecutionStatus)
	require.Equal(t, "cancelled", targets[1].ExecutionStatus)
	require.Equal(t, "cancelled", targets[2].ExecutionStatus)
}

func TestApprovalGenerationFencesValidationAndPreservesOtherPRs(t *testing.T) {
	g := approvalTestStore(t)
	now := time.Now().UTC()
	_, _, err := g.AdmitApprovalScan(approvalTestAdmission("old", 1, now, 1, 2))
	require.NoError(t, err)
	first, err := approvalTestClaim(g, now)
	require.NoError(t, err)
	require.NoError(t, approvalTestFinish(g, first, now))
	second, err := approvalTestClaim(g, now)
	require.NoError(t, err)
	require.NoError(t, approvalTestFinish(g, second, now))
	v, err := g.ClaimApprovalValidation(1, first.ID, now, time.Minute)
	require.NoError(t, err)
	require.NotNil(t, v)
	r := approvalTestAdmission("new", 1, now.Add(time.Second), 1)
	r.Scan.Kind = "recheck"
	_, _, err = g.AdmitApprovalScan(r)
	require.NoError(t, err)
	require.ErrorIs(t, g.FinishApprovalValidation(1, first.ID, v.Token, now.Add(2*time.Second), "current", ""), ErrApprovalLeaseLost)
	current, err := g.ListCurrentApprovalTargets(1, 100, "")
	require.NoError(t, err)
	require.Len(t, current, 2)
	byNumber := map[int]ApprovalTarget{}
	for _, target := range current {
		byNumber[target.Number] = target
	}
	require.Equal(t, "new-1", byNumber[1].ID)
	require.EqualValues(t, 2, byNumber[1].Generation)
	require.Equal(t, "queued", byNumber[1].ExecutionStatus)
	require.Equal(t, "old-2", byNumber[2].ID)
}

func TestApprovalImmutableArtifactsAndRetention(t *testing.T) {
	g := approvalTestStore(t)
	now := time.Now().UTC()
	_, _, err := g.AdmitApprovalScan(approvalTestAdmission("artifact", 1, now, 1))
	require.NoError(t, err)
	target, err := approvalTestClaim(g, now)
	require.NoError(t, err)
	require.NoError(t, g.SaveApprovalSnapshot(target.ID, target.LeaseToken, `{"digest":"one"}`, now))
	require.NoError(t, g.SaveApprovalSnapshot(target.ID, target.LeaseToken, `{"digest":"one"}`, now))
	require.Error(t, g.SaveApprovalSnapshot(target.ID, target.LeaseToken, `{"digest":"two"}`, now))
	require.NoError(t, approvalTestFinish(g, target, now))
	require.ErrorIs(t, approvalTestFinish(g, target, now), ErrApprovalLeaseLost)
	result, err := g.GetApprovalTarget(1, "artifact", target.ID)
	require.NoError(t, err)
	require.JSONEq(t, `{"digest":"one"}`, result.SnapshotJSON)
	require.NotEmpty(t, result.AssessmentJSON)
	_, _, err = g.AdmitApprovalScan(approvalTestAdmission("active", 2, now.Add(-40*24*time.Hour), 2))
	require.NoError(t, err)
	count, err := g.PruneApprovalScans(now.Add(time.Second))
	require.NoError(t, err)
	require.EqualValues(t, 1, count)
	_, err = g.GetApprovalScan(1, "artifact")
	require.ErrorIs(t, err, ErrApprovalNotFound)
	_, err = g.GetApprovalScan(2, "active")
	require.NoError(t, err)
	current, err := g.ListCurrentApprovalTargets(1, 100, "")
	require.NoError(t, err)
	require.Empty(t, current)
}

func TestApprovalDeadlineAndValidationSlots(t *testing.T) {
	g := approvalTestStore(t)
	now := time.Now().UTC()
	for i := 1; i <= 3; i++ {
		_, _, err := g.AdmitApprovalScan(approvalTestAdmission(fmt.Sprint(i), i, now, i))
		require.NoError(t, err)
		target, err := approvalTestClaim(g, now)
		require.NoError(t, err)
		require.NoError(t, approvalTestFinish(g, target, now))
	}
	first, err := g.ClaimApprovalValidation(1, "1-1", now, time.Minute)
	require.NoError(t, err)
	require.NotNil(t, first)
	duplicate, err := g.ClaimApprovalValidation(1, "1-1", now, time.Minute)
	require.NoError(t, err)
	require.Nil(t, duplicate)
	second, err := g.ClaimApprovalValidation(2, "2-2", now, time.Minute)
	require.NoError(t, err)
	require.NotNil(t, second)
	third, err := g.ClaimApprovalValidation(3, "3-3", now, time.Minute)
	require.NoError(t, err)
	require.Nil(t, third)
	require.NoError(t, g.FinishApprovalValidation(1, "1-1", first.Token, now, "expired", "validation_failed"))
	third, err = g.ClaimApprovalValidation(3, "3-3", now, time.Minute)
	require.NoError(t, err)
	require.NotNil(t, third)
	require.NoError(t, g.InvalidateApprovalTargets("ACME", "EXAMPLE", 3, "evidence_changed", now))
	require.ErrorIs(t, g.FinishApprovalValidation(3, "3-3", third.Token, now, "current", ""), ErrApprovalLeaseLost)
	r := approvalTestAdmission("deadline", 4, now, 4)
	r.Scan.Deadline = now.Add(time.Second)
	_, _, err = g.AdmitApprovalScan(r)
	require.NoError(t, err)
	target, err := approvalTestClaim(g, now.Add(2*time.Second))
	require.NoError(t, err)
	require.Nil(t, target)
	result, err := g.GetApprovalTarget(4, "deadline", "deadline-4")
	require.NoError(t, err)
	require.Equal(t, "timed_out", result.ExecutionStatus)
	require.Contains(t, result.ReasonCodesJSON, "queue_deadline")
}

func TestApprovalCancelledSlotHeldUntilWorkerAcknowledges(t *testing.T) {
	g := approvalTestStore(t)
	now := time.Now().UTC()
	_, _, err := g.AdmitApprovalScan(approvalTestAdmission("old", 1, now, 1))
	require.NoError(t, err)
	old, err := approvalTestClaim(g, now)
	require.NoError(t, err)
	require.NotNil(t, old)
	require.NoError(t, g.CancelApprovalScan(1, "old", now))
	_, _, err = g.AdmitApprovalScan(approvalTestAdmission("new", 1, now, 2))
	require.NoError(t, err)
	next, err := approvalTestClaim(g, now)
	require.NoError(t, err)
	require.Nil(t, next)
	require.NoError(t, g.ReleaseApprovalTargetSlot(old.ID, "wrong"))
	next, err = approvalTestClaim(g, now)
	require.NoError(t, err)
	require.Nil(t, next)
	require.NoError(t, g.ReleaseApprovalTargetSlot(old.ID, old.LeaseToken))
	next, err = approvalTestClaim(g, now)
	require.NoError(t, err)
	require.NotNil(t, next)
	require.Equal(t, "new-2", next.ID)
}

func TestApprovalScanPaginationSentinelAndOwnership(t *testing.T) {
	g := approvalTestStore(t)
	now := time.Now().UTC()
	for i := 0; i < 103; i++ {
		require.NoError(t, g.db.Create(&ApprovalScan{ID: fmt.Sprintf("%03d", i), UserID: 1, Kind: "full", Status: "completed", IdempotencyKey: fmt.Sprint(i), CreatedAt: now}).Error)
	}
	require.NoError(t, g.db.Create(&ApprovalScan{ID: "other", UserID: 2, Kind: "full", Status: "completed", IdempotencyKey: "other", CreatedAt: now.Add(-time.Hour)}).Error)
	page, err := g.ListApprovalScans(1, "full", 101, "")
	require.NoError(t, err)
	require.Len(t, page, 101)
	rest, err := g.ListApprovalScans(1, "full", 101, page[99].ID)
	require.NoError(t, err)
	require.Len(t, rest, 3)
	for _, scan := range rest {
		require.Equal(t, 1, scan.UserID)
	}
}
