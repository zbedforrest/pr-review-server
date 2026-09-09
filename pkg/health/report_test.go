package health

import (
	"strings"
	"testing"
	"time"
)

func healthyMetrics() Metrics {
	now := time.Date(2026, 9, 10, 14, 0, 0, 0, time.UTC)
	return Metrics{
		WindowStart: now.Add(-24 * time.Hour), WindowEnd: now, Now: now,
		Runs: RunMetrics{
			Total: 20, ByStatus: map[string]int{"completed": 19, "failed": 1}, ByTrigger: map[string]int{"poller": 15, "api_v1": 5},
			ByTerminalCode: map[string]int{"agent_error": 1}, DurationsMS: []int64{300000, 400000, 500000, 600000, 700000},
			ModelFallbacks: 0, Verdicts: map[string]int{"approve": 12, "request_changes": 7}, Criticals: 3,
		},
		Attempts: map[string]int{"agent/wall_clock_timeout": 0},
		Queue:    QueueMetrics{Queued: 0, Running: 1, OldestQueuedAge: 0, OldestRunningAge: 5 * time.Minute},
		Publish:  PublishMetrics{Summaries: 6, Inline: 9, Annotations: 4, Dismissed: 1},
		Replies: ReplyMetrics{
			Handled: 4, ByClass: map[string]int{"pushback": 2, "resolution": 2}, ByAction: map[string]int{"reacted": 4},
			ByOutcome: map[string]int{"shadowed": 2}, ByDecision: map[string]int{"concede": 1, "hold": 1},
		},
		Telemetry: map[string]int{"reply_decision": 2, "reply_reacted": 2},
		Lease:     LeaseMetrics{Holder: "prism-00047-nsr-abc", ExpiresAt: now.Add(60 * time.Second), Present: true},
		PRErrors:  0,
		WallClock: 900 * time.Second,
	}
}

func TestEvaluateHealthyDayIsOK(t *testing.T) {
	r := Evaluate(healthyMetrics())
	if r.Overall != StatusOK {
		t.Fatalf("overall = %s, checks = %+v", r.Overall, r.Checks)
	}
	md := r.Markdown()
	for _, want := range []string{"# PRism daily health", "OK", "20 reviews", "95%", "p50", "p90", "6 summaries", "9 inline", "prism-00047-nsr-abc"} {
		if !strings.Contains(md, want) {
			t.Errorf("markdown missing %q:\n%s", want, md)
		}
	}
}

func TestEvaluateFlagsTheThingsThatHaveBrokenBefore(t *testing.T) {
	m := healthyMetrics()
	m.Runs.ByStatus = map[string]int{"completed": 14, "failed": 3, "timed_out": 3}
	m.Runs.ModelFallbacks = 2
	m.Queue.OldestQueuedAge = 45 * time.Minute
	m.Queue.OldestRunningAge = 20 * time.Minute
	m.Lease.ExpiresAt = m.Now.Add(-5 * time.Minute)
	m.Replies.StuckPending = 2
	m.Replies.Failed = 1
	m.Telemetry["reply_text_error"] = 3
	m.PRErrors = 2
	r := Evaluate(m)
	if r.Overall != StatusCritical {
		t.Fatalf("overall = %s", r.Overall)
	}
	got := map[string]Status{}
	for _, c := range r.Checks {
		got[c.Name] = c.Status
	}
	want := map[string]Status{
		"review success rate": StatusCritical, // 14/20
		"wall-clock timeouts": StatusWarn,     // 3 of 20 is 15%
		"model fallbacks":     StatusWarn,
		"queue age":           StatusCritical, // 45m queued
		"running reviews":     StatusWarn,     // 20m is past the 15m wall clock but not twice it
		"poller lease":        StatusCritical,
		"reply text errors":   StatusWarn,
		"stuck reply steps":   StatusWarn,
		"PR error messages":   StatusWarn,
	}
	for name, status := range want {
		if got[name] != status {
			t.Errorf("%s = %s, want %s", name, got[name], status)
		}
	}
}

func TestEvaluateQuietDayWarnsButIsNotCritical(t *testing.T) {
	m := healthyMetrics()
	m.Runs = RunMetrics{ByStatus: map[string]int{}, ByTrigger: map[string]int{}, ByTerminalCode: map[string]int{}, Verdicts: map[string]int{}}
	r := Evaluate(m)
	if r.Overall != StatusWarn {
		t.Fatalf("overall = %s, checks = %+v", r.Overall, r.Checks)
	}
	if !strings.Contains(r.Markdown(), "No reviews") {
		t.Errorf("markdown should say so:\n%s", r.Markdown())
	}
}

func TestEvaluateSkipsTheLeaseCheckWhenPollingIsDisabled(t *testing.T) {
	m := healthyMetrics()
	m.Lease = LeaseMetrics{}
	m.PollingDisabled = true
	r := Evaluate(m)
	if r.Overall != StatusOK {
		t.Fatalf("overall = %s, checks = %+v", r.Overall, r.Checks)
	}
}

func TestPercentiles(t *testing.T) {
	p50, p90, max := percentiles([]int64{100, 200, 300, 400, 500, 600, 700, 800, 900, 1000})
	if p50 != 500 || p90 != 900 || max != 1000 {
		t.Fatalf("p50=%d p90=%d max=%d", p50, p90, max)
	}
	if p50, p90, max := percentiles(nil); p50 != 0 || p90 != 0 || max != 0 {
		t.Fatal("empty input must be zero")
	}
}
