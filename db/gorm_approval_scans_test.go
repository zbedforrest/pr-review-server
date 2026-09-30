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

func TestApprovalBudgetUnsetCapAdmitsAndStillRecordsUsage(t *testing.T) {
	g := approvalTestStore(t)
	now := time.Date(2026, 9, 28, 12, 0, 0, 0, time.UTC)
	require.NoError(t, g.db.Create(&approvalDailyBudget{Day: "2026-09-28", InputTokens: 50000000, OutputTokens: 1000000}).Error)
	r := approvalTestAdmission("uncapped", 1, now, 1)
	r.DailyInputLimit, r.DailyOutputLimit = 0, 0
	_, _, err := g.AdmitApprovalScan(r)
	require.NoError(t, err)
	target, err := approvalTestClaim(g, now)
	require.NoError(t, err)
	require.NoError(t, g.ReserveApprovalBudget(target.ID, target.LeaseToken, now, 0, 0, 600000, 12000))
	var budget approvalDailyBudget
	require.NoError(t, g.db.First(&budget).Error)
	require.EqualValues(t, 50600000, budget.InputTokens)
	require.EqualValues(t, 1012000, budget.OutputTokens)
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
	require.Equal(t, "cancelling", scan.Status)
	require.Nil(t, scan.CompletedAt)
	targets, err := g.ListApprovalTargets(1, "cancel", 100, "")
	require.NoError(t, err)
	require.Equal(t, "completed", targets[0].ExecutionStatus)
	require.Equal(t, "cancelled", targets[1].ExecutionStatus)
	require.Equal(t, "cancelled", targets[2].ExecutionStatus)
	require.NoError(t, g.ReleaseApprovalTargetSlot(second.ID, second.LeaseToken))
	scan, err = g.GetApprovalScan(1, "cancel")
	require.NoError(t, err)
	require.Equal(t, "cancelled", scan.Status)
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
	var conflict *ApprovalActiveConflict
	require.ErrorAs(t, err, &conflict)
	require.Equal(t, "old", conflict.ScanID)
	next, err := approvalTestClaim(g, now)
	require.NoError(t, err)
	require.Nil(t, next)
	require.NoError(t, g.ReleaseApprovalTargetSlot(old.ID, "wrong"))
	_, _, err = g.AdmitApprovalScan(approvalTestAdmission("new", 1, now, 2))
	require.ErrorAs(t, err, &conflict)
	next, err = approvalTestClaim(g, now)
	require.NoError(t, err)
	require.Nil(t, next)
	require.NoError(t, g.ReleaseApprovalTargetSlot(old.ID, old.LeaseToken))
	_, _, err = g.AdmitApprovalScan(approvalTestAdmission("new", 1, now, 2))
	require.NoError(t, err)
	next, err = approvalTestClaim(g, now)
	require.NoError(t, err)
	require.NotNil(t, next)
	require.Equal(t, "new-2", next.ID)
}

func TestApprovalCancelledScanSettlesAfterLeaseExpiry(t *testing.T) {
	g := approvalTestStore(t)
	now := time.Now().UTC()
	_, _, err := g.AdmitApprovalScan(approvalTestAdmission("cancelled-worker", 1, now, 1))
	require.NoError(t, err)
	target, err := approvalTestClaim(g, now)
	require.NoError(t, err)
	require.NotNil(t, target)
	require.NoError(t, g.CancelApprovalScan(1, "cancelled-worker", now))
	scan, err := g.GetApprovalScan(1, "cancelled-worker")
	require.NoError(t, err)
	require.Equal(t, "cancelling", scan.Status)
	next, err := approvalTestClaim(g, now.Add(61*time.Second))
	require.NoError(t, err)
	require.Nil(t, next)
	scan, err = g.GetApprovalScan(1, "cancelled-worker")
	require.NoError(t, err)
	require.Equal(t, "cancelled", scan.Status)
	require.NotNil(t, scan.CompletedAt)
	_, _, err = g.AdmitApprovalScan(approvalTestAdmission("replacement", 1, now.Add(62*time.Second), 2))
	require.NoError(t, err)
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

func TestApprovalReportedUsageOverReservationDebitsDailyBudget(t *testing.T) {
	g := approvalTestStore(t)
	now := time.Now().UTC()
	_, _, err := g.AdmitApprovalScan(approvalTestAdmission("overage", 1, now, 1))
	require.NoError(t, err)
	target, err := approvalTestClaim(g, now)
	require.NoError(t, err)
	require.NoError(t, g.ReserveApprovalBudget(target.ID, target.LeaseToken, now, 600000, 12000, 600000, 12000))
	limits := ApprovalUsage{InputTokens: 600000, OutputTokens: 12000, Rounds: 16, ToolCalls: 40, ToolBytes: 1 << 20}
	require.NoError(t, g.ReserveApprovalUsage(target.ID, target.LeaseToken, now, ApprovalUsageReservation{ID: "call", Usage: ApprovalUsage{InputTokens: 100000, OutputTokens: 12000, Rounds: 1}, Limits: limits}))
	require.NoError(t, g.SettleApprovalUsage(target.ID, target.LeaseToken, "call", now, ApprovalUsage{InputTokens: 650000, OutputTokens: 13000, Rounds: 1}))
	saved, err := g.GetApprovalTarget(1, "overage", target.ID)
	require.NoError(t, err)
	require.EqualValues(t, 650000, saved.InputTokens)
	require.EqualValues(t, 13000, saved.OutputTokens)
	var budget approvalDailyBudget
	require.NoError(t, g.db.First(&budget).Error)
	require.EqualValues(t, 650000, budget.InputTokens)
	require.EqualValues(t, 13000, budget.OutputTokens)
	require.ErrorIs(t, g.ReserveApprovalUsage(target.ID, target.LeaseToken, now, ApprovalUsageReservation{ID: "next", Usage: ApprovalUsage{InputTokens: 1, OutputTokens: 1, Rounds: 1}, Limits: limits}), ErrApprovalBudget)
	require.ErrorIs(t, g.SettleApprovalUsage(target.ID, target.LeaseToken, "call", now, ApprovalUsage{InputTokens: 650000, OutputTokens: 13000, Rounds: 1}), ErrApprovalLeaseLost)
	require.NoError(t, g.FinalizeApprovalTarget(target.ID, target.LeaseToken, now, ApprovalFinalization{ExecutionStatus: "completed", Decision: "insufficient_evidence", Freshness: "expired", ReasonCodesJSON: `["budget_exhausted"]`}))
	require.NoError(t, g.db.First(&budget).Error)
	require.EqualValues(t, 650000, budget.InputTokens)
	require.EqualValues(t, 13000, budget.OutputTokens)
	r := approvalTestAdmission("blocked", 2, now, 2)
	r.DailyInputLimit = 600000
	r.DailyOutputLimit = 12000
	_, _, err = g.AdmitApprovalScan(r)
	require.ErrorIs(t, err, ErrApprovalBudget)
}

func TestApprovalSettlementRejectsHostileUsageAndReleasesOnlyUnusedTokens(t *testing.T) {
	g := approvalTestStore(t)
	now := time.Now().UTC()
	_, _, err := g.AdmitApprovalScan(approvalTestAdmission("bounded", 1, now, 1))
	require.NoError(t, err)
	target, err := approvalTestClaim(g, now)
	require.NoError(t, err)
	require.NoError(t, g.ReserveApprovalBudget(target.ID, target.LeaseToken, now, 600000, 12000, 600000, 12000))
	limits := ApprovalUsage{InputTokens: 600000, OutputTokens: 12000, Rounds: 16, ToolCalls: 40, ToolBytes: 1 << 20}
	require.NoError(t, g.ReserveApprovalUsage(target.ID, target.LeaseToken, now, ApprovalUsageReservation{ID: "call", Usage: ApprovalUsage{InputTokens: 100000, OutputTokens: 12000, Rounds: 1}, Limits: limits}))
	require.ErrorIs(t, g.SettleApprovalUsage(target.ID, target.LeaseToken, "call", now, ApprovalUsage{InputTokens: 1 << 62, OutputTokens: 1, Rounds: 1}), ErrApprovalBudget)
	require.NoError(t, g.SettleApprovalUsage(target.ID, target.LeaseToken, "call", now, ApprovalUsage{InputTokens: 150000, OutputTokens: 200, Rounds: 1}))
	require.NoError(t, g.FinalizeApprovalTarget(target.ID, target.LeaseToken, now, ApprovalFinalization{ExecutionStatus: "completed", Decision: "insufficient_evidence", Freshness: "expired"}))
	var budget approvalDailyBudget
	require.NoError(t, g.db.First(&budget).Error)
	require.EqualValues(t, 150000, budget.InputTokens)
	require.EqualValues(t, 200, budget.OutputTokens)
}

func TestApprovalObservedChangeFencesFinalizationAndValidation(t *testing.T) {
	g := approvalTestStore(t)
	now := time.Now().UTC()
	_, _, err := g.AdmitApprovalScan(approvalTestAdmission("observed", 1, now, 1))
	require.NoError(t, err)
	target, err := approvalTestClaim(g, now)
	require.NoError(t, err)
	require.NotNil(t, target)
	require.NoError(t, g.InvalidateApprovalTargets("acme", "example", 1, "changed", now.Add(time.Second)))
	require.NoError(t, approvalTestFinish(g, target, now.Add(2*time.Second)))
	result, err := g.GetApprovalTarget(1, "observed", target.ID)
	require.NoError(t, err)
	require.Equal(t, "stale", result.Freshness)
	_, err = g.ClaimApprovalValidation(1, target.ID, now.Add(3*time.Second), time.Minute)
	require.ErrorIs(t, err, ErrApprovalLeaseLost)
}

func approvalTestWaitBehindGate(t *testing.T, g *GormDB, operation func() error) error {
	t.Helper()
	locked := g.db.Begin()
	require.NoError(t, locked.Error)
	defer locked.Rollback()
	require.NoError(t, locked.Model(&approvalGate{}).Where("id = 1").UpdateColumn("version", gorm.Expr("version + 1")).Error)
	waiting := make(chan struct{})
	var once sync.Once
	require.NoError(t, g.db.Callback().Update().Before("gorm:update").Register("approval_test_gate_wait", func(tx *gorm.DB) {
		if tx.Statement.Table == "approval_mutation_gate" {
			once.Do(func() { close(waiting) })
		}
	}))
	defer g.db.Callback().Update().Remove("approval_test_gate_wait")
	done := make(chan error, 1)
	go func() { done <- operation() }()
	select {
	case <-waiting:
	case <-time.After(3 * time.Second):
		t.Fatal("operation did not reach mutation gate")
	}
	timer := time.NewTimer(150 * time.Millisecond)
	<-timer.C
	require.NoError(t, locked.Commit().Error)
	select {
	case err := <-done:
		return err
	case <-time.After(3 * time.Second):
		t.Fatal("operation did not finish after gate release")
		return nil
	}
}

func TestApprovalLeaseChecksAdvanceAcrossMutationGateWait(t *testing.T) {
	for _, operation := range []string{"heartbeat", "finalize", "validate", "claim"} {
		t.Run(operation, func(t *testing.T) {
			g := approvalTestStore(t)
			now := time.Date(2031, 4, 5, 12, 0, 0, 0, time.UTC)
			admission := approvalTestAdmission("clock", 1, now, 1)
			if operation == "claim" {
				admission.Scan.Deadline = now.Add(50 * time.Millisecond)
			}
			_, _, err := g.AdmitApprovalScan(admission)
			require.NoError(t, err)
			if operation == "claim" {
				var claimed *ApprovalTarget
				err = approvalTestWaitBehindGate(t, g, func() error { var claimErr error; claimed, claimErr = approvalTestClaim(g, now); return claimErr })
				require.NoError(t, err)
				require.Nil(t, claimed)
				target, readErr := g.GetApprovalTarget(1, "clock", "clock-1")
				require.NoError(t, readErr)
				require.Equal(t, "timed_out", target.ExecutionStatus)
				return
			}
			leaseDuration := 50 * time.Millisecond
			if operation == "validate" {
				leaseDuration = time.Minute
			}
			target, err := g.ClaimApprovalTarget(ApprovalClaim{Worker: "fixture", Now: now, LeaseDuration: leaseDuration, TargetDuration: time.Minute, MaxSlots: 2})
			require.NoError(t, err)
			require.NotNil(t, target)
			var guarded func() error
			switch operation {
			case "heartbeat":
				guarded = func() error { return g.HeartbeatApprovalTarget(target.ID, target.LeaseToken, now, time.Minute) }
			case "finalize":
				guarded = func() error { return approvalTestFinish(g, target, now) }
			case "validate":
				require.NoError(t, approvalTestFinish(g, target, now))
				validation, err := g.ClaimApprovalValidation(1, target.ID, now, 50*time.Millisecond)
				require.NoError(t, err)
				require.NotNil(t, validation)
				guarded = func() error { return g.FinishApprovalValidation(1, target.ID, validation.Token, now, "current", "") }
			}
			require.ErrorIs(t, approvalTestWaitBehindGate(t, g, guarded), ErrApprovalLeaseLost)
		})
	}
}

func TestApprovalQueueDeadlineDoesNotShortenStartedTarget(t *testing.T) {
	g := approvalTestStore(t)
	now := time.Now().UTC()
	req := approvalTestAdmission("queue-deadline", 1, now, 1, 2)
	_, _, err := g.AdmitApprovalScan(req)
	require.NoError(t, err)
	scan, err := g.GetApprovalScan(1, "queue-deadline")
	require.NoError(t, err)
	start := scan.Deadline.Add(-time.Second)
	target, err := approvalTestClaim(g, start)
	require.NoError(t, err)
	require.NotNil(t, target)
	require.NoError(t, g.HeartbeatApprovalTarget(target.ID, target.LeaseToken, scan.Deadline.Add(time.Second), time.Minute))
	require.NoError(t, approvalTestFinish(g, target, scan.Deadline.Add(2*time.Second)))
	_, err = approvalTestClaim(g, scan.Deadline.Add(3*time.Second))
	require.NoError(t, err)
	rows, err := g.ListApprovalTargets(1, "queue-deadline", 25, "")
	require.NoError(t, err)
	require.Equal(t, "completed", rows[0].ExecutionStatus)
	require.Equal(t, "timed_out", rows[1].ExecutionStatus)
}

func TestApprovalPrivateObservationDoesNotInvalidateOtherUsers(t *testing.T) {
	g := approvalTestStore(t)
	now := time.Now().UTC()
	for _, user := range []int{1, 2} {
		_, _, err := g.AdmitApprovalScan(approvalTestAdmission(fmt.Sprintf("user-%d", user), user, now, 1))
		require.NoError(t, err)
		target, err := approvalTestClaim(g, now)
		require.NoError(t, err)
		require.NotNil(t, target)
		require.NoError(t, approvalTestFinish(g, target, now))
	}
	require.NoError(t, g.InvalidateUserApprovalTargets(1, "acme", "example", 1, "private_update", now.Add(time.Second)))
	first, err := g.GetApprovalTarget(1, "user-1", "user-1-1")
	require.NoError(t, err)
	require.Equal(t, "stale", first.Freshness)
	second, err := g.GetApprovalTarget(2, "user-2", "user-2-1")
	require.NoError(t, err)
	require.Equal(t, "current", second.Freshness)
	require.NoError(t, g.InvalidateApprovalTargets("acme", "example", 1, "global_update", now.Add(2*time.Second)))
	second, err = g.GetApprovalTarget(2, "user-2", "user-2-1")
	require.NoError(t, err)
	require.Equal(t, "stale", second.Freshness)
}

func TestApprovalWorkerCanFinalizeOwnDeadlineWithoutLeaseTakeover(t *testing.T) {
	g := approvalTestStore(t)
	now := time.Now().UTC()
	_, _, err := g.AdmitApprovalScan(approvalTestAdmission("deadline-final", 1, now, 1))
	require.NoError(t, err)
	target, err := approvalTestClaim(g, now)
	require.NoError(t, err)
	require.NotNil(t, target)
	require.NoError(t, g.FinalizeApprovalTarget(target.ID, target.LeaseToken, target.Deadline.Add(time.Millisecond), ApprovalFinalization{ExecutionStatus: "timed_out", Freshness: "expired"}))
	result, err := g.GetApprovalTarget(1, "deadline-final", target.ID)
	require.NoError(t, err)
	require.Equal(t, "timed_out", result.ExecutionStatus)
	require.ErrorIs(t, g.FinalizeApprovalTarget(target.ID, "wrong-holder", target.Deadline.Add(time.Second), ApprovalFinalization{ExecutionStatus: "timed_out"}), ErrApprovalLeaseLost)
}

func TestApprovalAdmissionTargetLimit(t *testing.T) {
	for _, count := range []int{35, MaxApprovalTargetsPerScan, MaxApprovalTargetsPerScan + 1} {
		t.Run(fmt.Sprint(count), func(t *testing.T) {
			g := approvalTestStore(t)
			numbers := make([]int, count)
			for i := range numbers {
				numbers[i] = i + 1
			}
			scan, _, err := g.AdmitApprovalScan(approvalTestAdmission("limit", 1, time.Now(), numbers...))
			if count > MaxApprovalTargetsPerScan {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			require.Equal(t, count, scan.Total)
			targets, err := g.ListApprovalTargets(1, scan.ID, 100, "")
			require.NoError(t, err)
			require.Len(t, targets, count)
		})
	}
}
