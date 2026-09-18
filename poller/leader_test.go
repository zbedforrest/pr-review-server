package poller

import (
	"context"
	"errors"
	"testing"
	"time"

	"pr-review-server/config"
)

// Leadership gates the automatic poll loop: isLeader() drives whether a
// scheduled/initial poll runs. These tests cover the decision logic directly
// (the Start() loop gates on it, and poll re-verifies the lease itself).
func TestPoller_Leadership(t *testing.T) {
	mockGH := NewMockGitHubClient()
	mockDB := NewMockDatabase()
	p := newTestPoller(mockGH, mockDB)
	ctx := context.Background()

	// Election disabled (no holderID, as in direct unit-test construction):
	// always leader so a lone poller behaves normally.
	if !p.isLeader() {
		t.Fatal("expected isLeader()=true when election is disabled (empty holderID)")
	}

	// Enable election for this instance.
	p.holderID = "inst-1"

	// DB denies leadership -> not leader, scheduled poll would be skipped.
	mockDB.TryAcquireOrRenewLeadershipFunc = func(string, int64, time.Duration) (bool, error) { return false, nil }
	if p.updateLeadership(ctx) {
		t.Error("expected updateLeadership=false when DB denies the lease")
	}
	if p.isLeader() {
		t.Error("expected isLeader()=false after losing the lease")
	}

	// DB grants leadership -> leader.
	mockDB.TryAcquireOrRenewLeadershipFunc = func(string, int64, time.Duration) (bool, error) { return true, nil }
	if !p.updateLeadership(ctx) {
		t.Error("expected updateLeadership=true when DB grants the lease")
	}
	if !p.isLeader() {
		t.Error("expected isLeader()=true after acquiring the lease")
	}

	// DB error -> fail OPEN (assume leadership) so a transient DB hiccup doesn't
	// halt polling across the whole fleet.
	mockDB.TryAcquireOrRenewLeadershipFunc = func(string, int64, time.Duration) (bool, error) {
		return false, errors.New("transient db error")
	}
	if !p.updateLeadership(ctx) {
		t.Error("expected updateLeadership to fail open (true) on a lease query error")
	}
}

// The holder ID must be unique per instance so two instances of the same Cloud
// Run revision still contend for the lease correctly.
func TestNewHolderID_Unique(t *testing.T) {
	a, b := newHolderID(), newHolderID()
	if a == b {
		t.Errorf("expected distinct holder IDs, got %q twice", a)
	}
	if a == "" {
		t.Error("holder ID must not be empty")
	}
}

// The lease call must carry the instance's boot generation so a redeploy can
// preempt the zombie instance it replaces.
func TestUpdateLeadership_PassesBootGeneration(t *testing.T) {
	mockGH := NewMockGitHubClient()
	mockDB := NewMockDatabase()
	p := newTestPoller(mockGH, mockDB)
	p.holderID = "inst-1"
	p.generation = 1234

	var got int64
	mockDB.TryAcquireOrRenewLeadershipFunc = func(_ string, generation int64, _ time.Duration) (bool, error) {
		got = generation
		return true, nil
	}
	p.updateLeadership(context.Background())
	if got != 1234 {
		t.Fatalf("lease call generation = %d, want the poller's boot generation 1234", got)
	}
}

// A real poller gets a non-zero generation at construction.
func TestNew_SetsBootGeneration(t *testing.T) {
	p := New(&config.Config{}, NewMockDatabase(), nil, nil)
	if p.generation == 0 {
		t.Fatal("New must stamp a boot generation")
	}
}

// Instances that never poll (DISABLE_POLLING, e.g. local benchmark servers on
// the shared database) must not contend for the lease, or they starve prod.
func TestRun_DisabledPollingDoesNotContendForLeadership(t *testing.T) {
	mockGH := NewMockGitHubClient()
	mockDB := NewMockDatabase()
	calls := 0
	mockDB.TryAcquireOrRenewLeadershipFunc = func(string, int64, time.Duration) (bool, error) {
		calls++
		return true, nil
	}
	p := newTestPoller(mockGH, mockDB)
	p.holderID = "local-1"
	p.cfg.DisablePolling = true

	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	p.Start(ctx)

	if calls != 0 {
		t.Fatalf("polling-disabled instance made %d lease calls, want 0", calls)
	}
}

func waitForPollDone(t *testing.T, p *Poller) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		p.pollMutex.Lock()
		polling := p.polling
		p.pollMutex.Unlock()
		if !polling {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("timed out waiting for the poll to finish")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func githubCallLog(mockGH *MockGitHubClient) []string {
	mockGH.callLogMu.Lock()
	defer mockGH.callLogMu.Unlock()
	return append([]string(nil), mockGH.CallLog...)
}

// The tick can fire in the same instant the lease moves to a new revision,
// before the renew loop has flipped isLeaderFlag. The poll body must
// re-verify the lease itself instead of trusting the stale flag.
func TestStartPoll_LeaseLostAfterTickSkipsCycle(t *testing.T) {
	_, p, _ := mergeStatePollFixture(t, "CLEAN", "CLEAN")
	mockGH := p.ghClient.(*MockGitHubClient)
	mockDB := p.db.(*MockDatabase)
	p.holderID = "inst-old"
	p.isLeaderFlag.Store(true)
	mockDB.TryAcquireOrRenewLeadershipFunc = func(string, int64, time.Duration) (bool, error) { return false, nil }
	if !p.isLeader() {
		t.Fatal("precondition: the stale flag still reports leadership when the tick fires")
	}

	p.startPoll(context.Background(), "scheduled")
	waitForPollDone(t, p)

	if calls := githubCallLog(mockGH); len(calls) != 0 {
		t.Errorf("a non-leader ran poll work: GitHub calls %v", calls)
	}
	if n := len(mockGH.BatchGetCIStatusCalls); n != 0 {
		t.Errorf("BatchGetCIStatus calls = %d, want 0", n)
	}
	if p.isLeader() {
		t.Error("the poll-start lease check must record the loss")
	}
}

func TestPoll_LeaseLostMidCycleStopsBeforeGitHubFetch(t *testing.T) {
	_, p, _ := mergeStatePollFixture(t, "CLEAN", "CLEAN")
	mockGH := p.ghClient.(*MockGitHubClient)
	mockDB := p.db.(*MockDatabase)
	p.holderID = "inst-old"
	p.isLeaderFlag.Store(true)
	leaseCalls := 0
	mockDB.TryAcquireOrRenewLeadershipFunc = func(string, int64, time.Duration) (bool, error) {
		leaseCalls++
		return leaseCalls == 1, nil
	}

	p.poll(context.Background(), true)

	if leaseCalls != 2 {
		t.Errorf("lease calls = %d, want one at poll start and one before the fetch phase", leaseCalls)
	}
	if n := len(mockGH.BatchGetCIStatusCalls); n != 0 {
		t.Errorf("BatchGetCIStatus calls = %d, want 0 after losing the lease mid-cycle", n)
	}
	if n := len(mockGH.BatchGetPRReviewDataCalls); n != 0 {
		t.Errorf("BatchGetPRReviewData calls = %d, want 0 after losing the lease mid-cycle", n)
	}
}

func TestPoll_LeaderKeepsLeaseAndFetches(t *testing.T) {
	_, p, _ := mergeStatePollFixture(t, "CLEAN", "CLEAN")
	mockGH := p.ghClient.(*MockGitHubClient)
	mockDB := p.db.(*MockDatabase)
	p.holderID = "inst-1"
	leaseCalls := 0
	mockDB.TryAcquireOrRenewLeadershipFunc = func(string, int64, time.Duration) (bool, error) {
		leaseCalls++
		return true, nil
	}

	p.poll(context.Background(), true)
	waitForDetachedReviews(t, p)

	if leaseCalls != 2 {
		t.Errorf("lease calls = %d, want 2", leaseCalls)
	}
	if n := len(mockGH.BatchGetCIStatusCalls); n != 1 {
		t.Errorf("BatchGetCIStatus calls = %d, want 1 for a leader that kept the lease", n)
	}
}

func TestPoll_ManualCycleIgnoresLeadership(t *testing.T) {
	_, p, _ := mergeStatePollFixture(t, "CLEAN", "CLEAN")
	mockGH := p.ghClient.(*MockGitHubClient)
	mockDB := p.db.(*MockDatabase)
	p.holderID = "inst-old"
	leaseCalls := 0
	mockDB.TryAcquireOrRenewLeadershipFunc = func(string, int64, time.Duration) (bool, error) {
		leaseCalls++
		return false, nil
	}

	p.startPoll(context.Background(), "manual")
	waitForPollDone(t, p)
	waitForDetachedReviews(t, p)

	if leaseCalls != 0 {
		t.Errorf("lease calls = %d, want 0 for a manual trigger", leaseCalls)
	}
	if n := len(mockGH.BatchGetCIStatusCalls); n != 1 {
		t.Errorf("BatchGetCIStatus calls = %d, want 1 for a manual trigger on a non-leader", n)
	}
}
