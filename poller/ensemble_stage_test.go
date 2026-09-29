package poller

import (
	"context"
	"errors"
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
		5: {delay: 10 * time.Millisecond, err: errors.New("provider down")},
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
