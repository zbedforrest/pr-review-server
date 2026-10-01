package poller

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"pr-review-server/pkg/reviewer/runconfig"
	"pr-review-server/pkg/reviewer/service"
	"pr-review-server/pkg/reviewer/types"
)

type fakeRun struct {
	delay     time.Duration
	err       error
	fallback  bool
	findings  []types.LineComment
	cancelled *int32
}

func withFakeRuns(t *testing.T, runs map[int]fakeRun) {
	t.Helper()
	origPrepare := prepareEnsembleCheckout
	prepareEnsembleCheckout = func(context.Context, service.AgentConfig, string, string, string, int, string) (string, func() error, error) {
		return "/shared/checkout", func() error { return nil }, nil
	}
	t.Cleanup(func() { prepareEnsembleCheckout = origPrepare })
	orig := runEnsembleAgent
	runEnsembleAgent = func(ctx context.Context, cfg service.AgentConfig, _ service.Spawner, _, _, _ string, _ int, _ string, _ []types.LineComment) (*service.AgentReview, error) {
		r := runs[cfg.Invocation]
		select {
		case <-time.After(r.delay):
		case <-ctx.Done():
			if r.cancelled != nil {
				atomic.AddInt32(r.cancelled, 1)
			}
			return nil, ctx.Err()
		}
		if r.err != nil {
			return nil, r.err
		}
		return &service.AgentReview{Comments: r.findings, ParseFallback: r.fallback, CostUSD: 0.03, RequestedModel: cfg.Model}, nil
	}
	t.Cleanup(func() { runEnsembleAgent = orig })
}

func finding(file string, line int) []types.LineComment {
	return []types.LineComment{{ID: "A-1", FilePath: file, LineNumber: line, Importance: "MEDIUM", CommentBody: "issue in " + file}}
}

func ensembleCfg() *runconfig.Ensemble {
	return &runconfig.Ensemble{Runs: 5, Quorum: 4, MinValid: 2, FallbackProfile: runconfig.ProfileLite}
}

func testPoller() *Poller {
	return &Poller{cfg: testConfig(), ensembleSlots: make(chan struct{}, 25)}
}

func TestEnsembleMergesTheFirstQuorumAndCancelsTheStraggler(t *testing.T) {
	var cancelled int32
	withFakeRuns(t, map[int]fakeRun{
		1: {delay: 10 * time.Millisecond, findings: finding("a.go", 10)},
		2: {delay: 20 * time.Millisecond, findings: finding("a.go", 12)},
		3: {delay: 30 * time.Millisecond, findings: finding("b.go", 5)},
		4: {delay: 40 * time.Millisecond, findings: finding("a.go", 11)},
		5: {delay: time.Hour, cancelled: &cancelled},
	})
	review, report, err := testPoller().runEnsembleAgents(context.Background(), ensembleCfg(), service.AgentConfig{}, "o", "r", "main", 1, "sha")
	if err != nil {
		t.Fatal(err)
	}
	if report.StopReason != "quorum" || report.Valid != 4 || report.Merged != 4 || atomic.LoadInt32(&cancelled) != 1 {
		t.Fatalf("report = %+v cancelled=%d", report, cancelled)
	}
	if len(review.Comments) != 3 || review.Comments[2].FilePath != "SUMMARY" {
		t.Fatalf("want two merged findings plus SUMMARY, got %+v", review.Comments)
	}
	if report.TotalCostUSD < 0.12 {
		t.Fatalf("cost of the merged runs not counted: %v", report.TotalCostUSD)
	}
}

func TestEnsembleRelaunchesAFastFailureOnce(t *testing.T) {
	withFakeRuns(t, map[int]fakeRun{
		1: {delay: time.Millisecond, err: errors.New("provider down")},
		2: {delay: 10 * time.Millisecond, findings: finding("a.go", 1)},
		3: {delay: 10 * time.Millisecond, findings: finding("a.go", 1)},
		4: {delay: 10 * time.Millisecond, findings: finding("a.go", 1)},
		5: {delay: 2 * time.Millisecond, err: errors.New("provider down")},
		6: {delay: 10 * time.Millisecond, findings: finding("a.go", 1)},
		7: {delay: 10 * time.Millisecond, findings: finding("a.go", 1)},
	})
	_, report, err := testPoller().runEnsembleAgents(context.Background(), ensembleCfg(), service.AgentConfig{}, "o", "r", "main", 1, "sha")
	if err != nil || report.Relaunches != 2 || report.Valid < 4 {
		t.Fatalf("err=%v report=%+v", err, report)
	}
}

func TestEnsembleReportsTooFewValidRunsForTheFallback(t *testing.T) {
	orig := ensembleRelaunchWindow
	ensembleRelaunchWindow = 10 * time.Millisecond
	t.Cleanup(func() { ensembleRelaunchWindow = orig })
	withFakeRuns(t, map[int]fakeRun{
		1: {delay: 70 * time.Millisecond, findings: finding("a.go", 1)},
		2: {delay: 70 * time.Millisecond, fallback: true},
		3: {delay: 70 * time.Millisecond, err: errors.New("timeout")},
		4: {delay: 70 * time.Millisecond, err: errors.New("timeout")},
		5: {delay: 70 * time.Millisecond, fallback: true},
	})
	_, report, err := testPoller().runEnsembleAgents(context.Background(), ensembleCfg(), service.AgentConfig{}, "o", "r", "main", 1, "sha")
	if !errors.Is(err, errEnsembleTooFewValid) || report.Valid != 1 {
		t.Fatalf("err=%v report=%+v", err, report)
	}
}

func TestEnsembleSidecarIsCompactAndAbsentWithoutAnEnsemble(t *testing.T) {
	var none *EnsembleReport
	if none.sidecar() != nil {
		t.Fatal("a run without an ensemble must add no sidecar field")
	}
	withFakeRuns(t, map[int]fakeRun{
		1: {delay: time.Millisecond, findings: finding("a.go", 1)}, 2: {delay: time.Millisecond, findings: finding("a.go", 2)},
		3: {delay: time.Millisecond, findings: finding("a.go", 3)}, 4: {delay: time.Millisecond, findings: finding("b.go", 1)},
		5: {delay: time.Hour},
	})
	_, report, err := testPoller().runEnsembleAgents(context.Background(), ensembleCfg(), service.AgentConfig{}, "o", "r", "main", 1, "sha")
	if err != nil {
		t.Fatal(err)
	}
	s, ok := report.sidecar().(ensembleSidecar)
	if !ok || s.StopReason != "quorum" || s.Merged != 4 || s.MergeMethod != "deterministic" || len(s.Support) != 2 {
		t.Fatalf("sidecar = %+v", report.sidecar())
	}
}

func TestEnsembleLabelsEveryRunAndKeepsTheirFindings(t *testing.T) {
	withFakeRuns(t, map[int]fakeRun{
		1: {delay: time.Millisecond, findings: finding("a.go", 1)}, 2: {delay: time.Millisecond, findings: finding("a.go", 2)},
		3: {delay: time.Millisecond, findings: finding("a.go", 3)}, 4: {delay: 2 * time.Millisecond, findings: finding("b.go", 1)},
		5: {delay: time.Hour},
	})
	_, report, err := testPoller().runEnsembleAgents(context.Background(), ensembleCfg(), service.AgentConfig{}, "o", "r", "main", 1, "sha")
	if err != nil {
		t.Fatal(err)
	}
	status := map[int]string{}
	for _, r := range report.Runs {
		status[r.Invocation] = r.Status
		if r.Status == "valid" && (len(r.Comments) != 1 || r.Comments[0].File == "") {
			t.Fatalf("run %d findings not kept: %+v", r.Invocation, r)
		}
	}
	if status[5] != "cancelled" || status[1] != "valid" || status[4] != "valid" {
		t.Fatalf("statuses = %v", status)
	}
}

func TestEnsembleReportsRunsEvenWhenFallingBack(t *testing.T) {
	orig := ensembleRelaunchWindow
	ensembleRelaunchWindow = time.Nanosecond
	t.Cleanup(func() { ensembleRelaunchWindow = orig })
	withFakeRuns(t, map[int]fakeRun{
		1: {delay: time.Millisecond, findings: finding("a.go", 1)}, 2: {delay: time.Millisecond, err: errors.New("boom")},
		3: {delay: time.Millisecond, err: errors.New("boom")}, 4: {delay: time.Millisecond, fallback: true}, 5: {delay: time.Millisecond, err: errors.New("boom")},
	})
	_, report, err := testPoller().runEnsembleAgents(context.Background(), ensembleCfg(), service.AgentConfig{}, "o", "r", "main", 1, "sha")
	if !errors.Is(err, errEnsembleTooFewValid) || len(report.Runs) != 5 {
		t.Fatalf("err=%v runs=%d", err, len(report.Runs))
	}
}

func TestEnsembleRunsShareOneCheckoutAndCleanItUpAfterTheDrain(t *testing.T) {
	var seen sync.Map
	var cleaned atomic.Bool
	withFakeRuns(t, map[int]fakeRun{})
	prepareEnsembleCheckout = func(context.Context, service.AgentConfig, string, string, string, int, string) (string, func() error, error) {
		return "/shared/pr-1", func() error { cleaned.Store(true); return nil }, nil
	}
	runEnsembleAgent = func(ctx context.Context, cfg service.AgentConfig, _ service.Spawner, _, _, _ string, _ int, _ string, _ []types.LineComment) (*service.AgentReview, error) {
		seen.Store(cfg.Invocation, cfg.SharedCheckout)
		if cfg.Invocation == 5 {
			<-ctx.Done()
			if cleaned.Load() {
				t.Error("the shared checkout was removed while a run was still using it")
			}
			return nil, ctx.Err()
		}
		return &service.AgentReview{Comments: finding("a.go", cfg.Invocation)}, nil
	}
	if _, _, err := testPoller().runEnsembleAgents(context.Background(), ensembleCfg(), service.AgentConfig{}, "o", "r", "main", 1, "sha"); err != nil {
		t.Fatal(err)
	}
	for i := 1; i <= 5; i++ {
		if dir, _ := seen.Load(i); dir != "/shared/pr-1" {
			t.Errorf("run %d used checkout %v", i, dir)
		}
	}
	if !cleaned.Load() {
		t.Fatal("shared checkout not cleaned up")
	}
}

func TestEnsembleFallsBackToPerRunCheckoutsWhenTheSharedOneFails(t *testing.T) {
	var shared sync.Map
	withFakeRuns(t, map[int]fakeRun{})
	prepareEnsembleCheckout = func(context.Context, service.AgentConfig, string, string, string, int, string) (string, func() error, error) {
		return "", nil, errors.New("clone failed")
	}
	runEnsembleAgent = func(_ context.Context, cfg service.AgentConfig, _ service.Spawner, _, _, _ string, _ int, _ string, _ []types.LineComment) (*service.AgentReview, error) {
		shared.Store(cfg.Invocation, cfg.SharedCheckout)
		return &service.AgentReview{Comments: finding("a.go", 1)}, nil
	}
	if _, _, err := testPoller().runEnsembleAgents(context.Background(), ensembleCfg(), service.AgentConfig{}, "o", "r", "main", 1, "sha"); err != nil {
		t.Fatal(err)
	}
	shared.Range(func(k, v any) bool {
		if v != "" {
			t.Errorf("run %v was given a shared checkout after the shared prep failed", k)
		}
		return true
	})
}
