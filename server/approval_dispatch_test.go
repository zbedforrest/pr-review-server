package server

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"pr-review-server/config"
	"pr-review-server/db"
	gh "pr-review-server/github"
	"pr-review-server/pkg/approval"
)

type approvalDispatchFixture struct {
	s     *Server
	store *db.GormDB
	user  *db.User
	head  string
}

func newApprovalDispatchFixture(t *testing.T, prs int, investigate func(context.Context) error) *approvalDispatchFixture {
	t.Helper()
	t.Setenv("APPROVAL_CANDIDATES_ENABLED", "true")
	t.Setenv("APPROVAL_CANDIDATES_PROVIDER", "anthropic")
	t.Setenv("APPROVAL_CANDIDATES_MODEL", "fixture-model")
	t.Setenv("APPROVAL_CANDIDATES_PROVIDER_IDENTITIES", "")
	t.Setenv("APPROVAL_CANDIDATES_PROGRESS_SUMMARIES", "false")
	head, base := strings.Repeat("a", 40), strings.Repeat("b", 40)
	remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if strings.HasSuffix(r.URL.Path, "/reviews") {
			fmt.Fprint(w, "[]")
			return
		}
		number, _ := strconv.Atoi(r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:])
		fmt.Fprintf(w, `{"number":%d,"state":"open","user":{"login":"contributor"},"head":{"sha":%q},"base":{"sha":%q,"repo":{"id":100}}}`, number, head, base)
	}))
	t.Cleanup(remote.Close)
	store, err := db.NewGormSQLite(filepath.Join(t.TempDir(), "dispatch.db") + "?_busy_timeout=10000&_journal_mode=WAL")
	require.NoError(t, err)
	t.Cleanup(func() { _ = store.Close() })
	s := New(&config.Config{GitHubUsername: "reviewer", BaseURL: "https://reviews.example", AnthropicAPIKey: "fixture-key"}, store, gh.NewTestClient(remote.URL, "reviewer"), nil)
	user := createTestUser(t, store, "reviewer")
	for number := 1; number <= prs; number++ {
		require.NoError(t, store.UpsertPR(&db.PR{RepoOwner: "acme", RepoName: "example", PRNumber: number, LastCommitSHA: head, Author: "contributor", Title: "Example change", Status: "completed"}))
		pr, err := store.GetPR("acme", "example", number)
		require.NoError(t, err)
		ensureUserPRView(t, store, user.ID, pr.ID, false)
	}
	s.approvalExecution = &approvalExecution{
		repository: func(context.Context, approval.Snapshot) (approval.Repository, func(), error) {
			return nil, func() {}, nil
		},
		collector: approvalCollectorFunc(func(ctx context.Context, target approval.Target, viewer approval.Viewer) (approval.Snapshot, error) {
			snapshot := approval.Snapshot{Target: target, Viewer: viewer, RepositoryID: 100, Revision: approval.Revision{Head: head, Base: base, MergeBase: strings.Repeat("c", 40)}, Eligible: true, Manifest: approval.Manifest{Complete: true, Endpoints: []approval.Endpoint{{Name: "fixture", Complete: true}}}, Checks: []approval.Check{{Name: "test", State: "success", SHA: head}}}
			approval.CanonicalizeSnapshot(&snapshot)
			return snapshot, nil
		}),
		investigator: func(db.ApprovalTarget) approval.Investigator {
			return approvalInvestigatorFunc(func(ctx context.Context, snapshot approval.Snapshot, _ approval.Repository, _ approval.Budget) (approval.Assessment, error) {
				if err := investigate(ctx); err != nil {
					return approval.Assessment{}, err
				}
				return approval.Assessment{SchemaVersion: "1", PolicyVersion: approval.PolicyVersion, PromptVersion: approval.PromptVersion, RuntimeVersion: approval.RuntimeVersion, Model: "fixture-model", Decision: "insufficient_evidence", Summary: "Fixture assessment", SnapshotID: snapshot.ID, SnapshotDigest: snapshot.Digest}, nil
			})
		},
	}
	return &approvalDispatchFixture{s: s, store: store, user: user, head: head}
}

func (f *approvalDispatchFixture) waitForScan(t *testing.T, id string, within time.Duration) *db.ApprovalScan {
	t.Helper()
	var scan *db.ApprovalScan
	require.Eventually(t, func() bool {
		var err error
		scan, err = f.store.GetApprovalScan(f.user.ID, id)
		return err == nil && scan.Status == "completed"
	}, within, 50*time.Millisecond)
	return scan
}

func TestApprovalDispatcherInvestigatesAWholeScanAtOnce(t *testing.T) {
	const prs = 50
	var entered, peak, inFlight atomic.Int32
	all := make(chan struct{})
	f := newApprovalDispatchFixture(t, prs, func(ctx context.Context) error {
		now := inFlight.Add(1)
		defer inFlight.Add(-1)
		for {
			seen := peak.Load()
			if now <= seen || peak.CompareAndSwap(seen, now) {
				break
			}
		}
		if entered.Add(1) == prs {
			close(all)
		}
		select {
		case <-all:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	})
	numbers := make([]int, prs)
	for i := range numbers {
		numbers[i] = i + 1
	}
	scan := approvalAPIAdmit(t, f.s, f.user, f.head, numbers...)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	f.s.runApprovalWorkers(ctx)
	f.waitForScan(t, scan.ID, 60*time.Second)
	require.EqualValues(t, prs, peak.Load())
	targets, err := f.store.ListApprovalTargets(f.user.ID, scan.ID, 100, "")
	require.NoError(t, err)
	require.Len(t, targets, prs)
	for _, target := range targets {
		require.Equal(t, "completed", target.ExecutionStatus)
		require.Equal(t, "insufficient_evidence", target.Decision)
	}
}

func TestApprovalAdmissionWakesTheDispatcher(t *testing.T) {
	started := make(chan time.Time, 1)
	f := newApprovalDispatchFixture(t, 1, func(context.Context) error {
		select {
		case started <- time.Now():
		default:
		}
		return nil
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	f.s.runApprovalWorkers(ctx)
	time.Sleep(1500 * time.Millisecond)
	admitted := time.Now()
	scan := approvalAPIAdmit(t, f.s, f.user, f.head, 1)
	select {
	case at := <-started:
		require.Less(t, at.Sub(admitted), approvalIdleClaimEvery-time.Second)
	case <-time.After(approvalIdleClaimEvery):
		t.Fatal("admission did not wake the dispatcher")
	}
	f.waitForScan(t, scan.ID, 10*time.Second)
}

type approvalUsageCountingStore struct {
	db.ApprovalStore
	calls atomic.Int32
}

func (s *approvalUsageCountingStore) ReserveApprovalUsage(id, token string, now time.Time, r db.ApprovalUsageReservation) error {
	s.calls.Add(1)
	return s.ApprovalStore.ReserveApprovalUsage(id, token, now, r)
}
func (s *approvalUsageCountingStore) SettleApprovalUsage(id, token, reservation string, now time.Time, u db.ApprovalUsage) error {
	s.calls.Add(1)
	return s.ApprovalStore.SettleApprovalUsage(id, token, reservation, now, u)
}
func (s *approvalUsageCountingStore) FlushApprovalUsage(id, token string, now time.Time, u db.ApprovalUsage) error {
	s.calls.Add(1)
	return s.ApprovalStore.FlushApprovalUsage(id, token, now, u)
}

func TestApprovalBudgetAccountsToolReadsInProcess(t *testing.T) {
	_, store, target := approvalWorkerFixture(t)
	require.NoError(t, store.ReserveApprovalBudget(target.ID, target.LeaseToken, time.Now(), 0, 0, approval.TargetInputTokens, approval.TargetOutputTokens))
	counting := &approvalUsageCountingStore{ApprovalStore: store}
	budget := newApprovalBudget(counting, target)
	ctx := context.Background()
	tool := approval.Usage{ToolCalls: 1, ToolBytes: 65536}
	read := func(bytes int) {
		require.NoError(t, budget.Reserve(ctx, tool))
		require.NoError(t, budget.Settle(ctx, tool, approval.Usage{ToolCalls: 1, ToolBytes: bytes}))
	}
	for range 3 {
		read(100)
	}
	require.Zero(t, counting.calls.Load())
	call := approval.Usage{InputTokens: 1000, OutputTokens: 100, Rounds: 1}
	require.NoError(t, budget.Reserve(ctx, call))
	require.NoError(t, budget.Settle(ctx, call, approval.Usage{InputTokens: 500, OutputTokens: 50, Rounds: 1}))
	require.EqualValues(t, 2, counting.calls.Load())
	saved, err := store.GetApprovalTarget(target.UserID, target.ScanID, target.ID)
	require.NoError(t, err)
	require.Equal(t, 3, saved.ToolCalls)
	require.EqualValues(t, 300, saved.ToolBytes)
	read(40)
	require.NoError(t, budget.Reserve(ctx, tool))
	budget.flush()
	require.EqualValues(t, 3, counting.calls.Load())
	saved, err = store.GetApprovalTarget(target.UserID, target.ScanID, target.ID)
	require.NoError(t, err)
	require.Equal(t, 5, saved.ToolCalls)
	require.EqualValues(t, 300+40+65536, saved.ToolBytes)
	require.EqualValues(t, 500, saved.InputTokens)
	require.Equal(t, 1, saved.Rounds)
}

func TestApprovalBudgetEnforcesToolLimitsLocally(t *testing.T) {
	_, store, target := approvalWorkerFixture(t)
	target.ToolCalls = approval.MaxToolCalls + approval.MaxCitationReads
	budget := newApprovalBudget(store, target)
	require.ErrorIs(t, budget.Reserve(context.Background(), approval.Usage{ToolCalls: 1, ToolBytes: 10}), db.ErrApprovalBudget)
	target.ToolCalls = 0
	target.ToolBytes = approval.MaxToolBytes + approval.MaxCitationBytes - 10
	budget = newApprovalBudget(store, target)
	require.ErrorIs(t, budget.Reserve(context.Background(), approval.Usage{ToolCalls: 1, ToolBytes: 65536}), db.ErrApprovalBudget)
	require.ErrorIs(t, budget.Settle(context.Background(), approval.Usage{}, approval.Usage{}), db.ErrApprovalLeaseLost)
}

func TestApprovalScanDeadlineCoversEveryWaveOfSlots(t *testing.T) {
	now := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	for _, c := range []struct {
		targets, slots int
		want           time.Duration
	}{
		{50, 50, 20 * time.Minute},
		{1, 50, 20 * time.Minute},
		{51, 50, 25 * time.Minute},
		{50, 4, 13*approvalTargetDuration + 15*time.Minute},
	} {
		require.Equal(t, now.Add(c.want), approvalScanDeadline(now, c.targets, c.slots), "%d targets, %d slots", c.targets, c.slots)
	}
}

func TestApprovalSlotsComeFromTheEnvironment(t *testing.T) {
	for value, want := range map[string]int{"": 50, "8": 8, "0": 1, "500": 100, "many": 50} {
		t.Setenv("APPROVAL_CANDIDATES_SLOTS", value)
		require.Equal(t, want, approvalSlots(), value)
	}
	t.Setenv("APPROVAL_CANDIDATES_SLOTS", "12")
	t.Setenv("APPROVAL_CANDIDATES_MODEL_CONCURRENCY", "")
	require.Equal(t, 12, approvalModelConcurrency())
	t.Setenv("APPROVAL_CANDIDATES_MODEL_CONCURRENCY", "3")
	require.Equal(t, 3, approvalModelConcurrency())
}

type approvalClaimCountingDB struct {
	*db.GormDB
	claims atomic.Int32
}

func (d *approvalClaimCountingDB) ClaimApprovalTargets(req db.ApprovalClaim) ([]db.ApprovalTarget, error) {
	d.claims.Add(1)
	return d.GormDB.ClaimApprovalTargets(req)
}

func TestApprovalFinishedTargetsClaimAgainOnlyWhileWorkMayBeQueued(t *testing.T) {
	for _, slots := range []int{2, 50} {
		t.Run(fmt.Sprint("slots=", slots), func(t *testing.T) {
			t.Setenv("APPROVAL_CANDIDATES_SLOTS", strconv.Itoa(slots))
			f := newApprovalDispatchFixture(t, 6, func(context.Context) error {
				time.Sleep(100 * time.Millisecond)
				return nil
			})
			counting := &approvalClaimCountingDB{GormDB: f.store}
			f.s.db = counting
			scan := approvalAPIAdmit(t, f.s, f.user, f.head, 1, 2, 3, 4, 5, 6)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			started := time.Now()
			f.s.runApprovalWorkers(ctx)
			f.waitForScan(t, scan.ID, 30*time.Second)
			require.Less(t, time.Since(started), approvalIdleClaimEvery)
			if slots == 50 {
				require.LessOrEqual(t, counting.claims.Load(), int32(2))
			}
		})
	}
}
