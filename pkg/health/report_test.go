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
	for _, want := range []string{"# PRism daily health", "OK", "20 reviews", "95%", "p50", "p90", "6 PRs got their first summary", "9 inline", "prism-00047-nsr-abc"} {
		if !strings.Contains(md, want) {
			t.Errorf("markdown missing %q:\n%s", want, md)
		}
	}
}

func TestEvaluateFlagsTheThingsThatHaveBrokenBefore(t *testing.T) {
	m := healthyMetrics()
	m.Runs.ByStatus = map[string]int{"completed": 14, "failed": 3, "timed_out": 3}
	m.Runs.ByTerminalCode = map[string]int{"run_timeout": 3, "agent_error": 3}
	m.Runs.ModelFallbacks = 2
	m.Queue.OldestQueuedAge = 45 * time.Minute
	m.Queue.OldestRunningAge = 20 * time.Minute
	m.Queue.RunningOverBudget = 1
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
		"running reviews":     StatusWarn,     // one run past its budget, none past twice
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

func TestEvaluateCancelledRunsAreNotFailures(t *testing.T) {
	m := healthyMetrics()
	m.Runs.ByStatus = map[string]int{"completed": 10, "cancelled": 9, "failed": 0}
	r := Evaluate(m)
	if r.Overall != StatusOK {
		t.Fatalf("superseded runs must not sink the success rate: %s %+v", r.Overall, r.Checks)
	}
	if !strings.Contains(r.Markdown(), "9 runs superseded") {
		t.Errorf("cancellations stay visible:\n%s", r.Markdown())
	}
}

func TestEvaluateAllCancelledDayIsNotAQuietDay(t *testing.T) {
	m := healthyMetrics()
	m.Runs.ByStatus = map[string]int{"cancelled": 6}
	r := Evaluate(m)
	md := r.Markdown()
	if !strings.Contains(md, "No reviews attempted in the window (6 cancelled)") || !strings.Contains(md, "6 runs superseded") || !strings.Contains(r.Headline, "6 cancelled") {
		t.Fatalf("cancellations must stay visible on a day with no attempts:\n%s", md)
	}
}

func TestEvaluateDoesNotBlameTheWallClockForAbandonedRuns(t *testing.T) {
	m := healthyMetrics()
	m.Runs.ByStatus = map[string]int{"completed": 19, "timed_out": 1}
	m.Runs.ByTerminalCode = map[string]int{"lease_abandoned": 1}
	for _, c := range Evaluate(m).Checks {
		if c.Name == "wall-clock timeouts" {
			t.Fatalf("an abandoned lease is not a wall-clock timeout: %+v", c)
		}
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

func TestEvaluateEscalatesAnAgingAutoReviewBacklog(t *testing.T) {
	for age, want := range map[time.Duration]Status{
		5 * time.Minute:  StatusOK,
		45 * time.Minute: StatusWarn,
		3 * time.Hour:    StatusCritical,
	} {
		m := healthyMetrics()
		m.AutoReview = AutoReviewMetrics{Queued: 4, OldestQueuedAge: age}
		var got *Check
		for _, c := range Evaluate(m).Checks {
			if c.Name == "auto-review backlog" {
				c := c
				got = &c
			}
		}
		if got == nil || got.Status != want {
			t.Errorf("age %s: check = %+v, want %s", age, got, want)
		}
	}
}

func TestEvaluateCountsAuthorVerdictsEvenAtZero(t *testing.T) {
	r := Evaluate(healthyMetrics())
	for _, c := range r.Checks {
		if c.Name == "publication hygiene" {
			if !strings.Contains(c.Detail, "0 author verdicts settled") || strings.Contains(c.Detail, "measured: author verdicts") {
				t.Fatalf("the verdict counter is wired and reads as measured at zero: %q", c.Detail)
			}
			return
		}
	}
	t.Fatalf("no publication hygiene check in %+v", r.Checks)
}

func TestEvaluateReportsPublicationHygieneWithEveryCounter(t *testing.T) {
	m := healthyMetrics()
	m.Telemetry[ActionRepeatedPost] = 4
	m.Telemetry[ActionSameCommitResolve] = 2
	m.Telemetry[ActionVerdictSettled] = 1
	r := Evaluate(m)
	var line *Check
	for i := range r.Checks {
		if r.Checks[i].Name == "publication hygiene" {
			line = &r.Checks[i]
		}
	}
	if line == nil {
		t.Fatalf("no publication hygiene check in %+v", r.Checks)
	}
	want := "4 repeated posts, 0 repeats after dismiss, 0 fixed without a file change, 0 fixed without a hunk change, 2 same-commit resolves, 0 severity escalations, 1 author verdicts settled, 0 posts stopped by the publish gate, 0 of which stopped by an opt-out, 0 pushes skipped for an unchanged diff, 0 pushes skipped by the daily round cap, 0 PRs muted by their author, 0 findings held by the output hygiene gate"
	if line.Detail != want || line.Status != StatusOK {
		t.Fatalf("line = %+v\nwant %q", *line, want)
	}
	if r.Overall != StatusOK {
		t.Fatalf("hygiene counters inform, they do not alarm: overall = %s", r.Overall)
	}
	md := r.Markdown()
	if !strings.Contains(md, "**publication hygiene**: "+want) {
		t.Fatalf("markdown missing the hygiene line:\n%s", md)
	}
	if strings.Contains(md, "Telemetry: ") && strings.Contains(md[strings.Index(md, "Telemetry: "):], ActionRepeatedPost) {
		t.Fatalf("hygiene actions must not repeat in the generic telemetry line:\n%s", md)
	}
}

func TestEvaluateShowsZeroHygieneWhenNoEventsExist(t *testing.T) {
	m := healthyMetrics()
	m.Telemetry = nil
	r := Evaluate(m)
	for _, c := range r.Checks {
		if c.Name == "publication hygiene" {
			if !strings.HasPrefix(c.Detail, "0 repeated posts, 0 repeats after dismiss, 0 fixed without a file change, 0 fixed without a hunk change, 0 same-commit resolves") {
				t.Fatalf("detail = %q", c.Detail)
			}
			return
		}
	}
	t.Fatalf("no publication hygiene check in %+v", r.Checks)
}

func feedbackFixture() FeedbackMetrics {
	return FeedbackMetrics{
		Scanned: true,
		ByLabel: map[string]int{FeedbackHappy: 2, FeedbackNeutral: 5, FeedbackFrustrated: 1, FeedbackVeryFrustrated: 1},
		Frustrated: []FeedbackQuote{
			{Label: FeedbackVeryFrustrated, Author: "dana-dev", Quote: "This is the third time it flagged the same line after I explained it. Please stop.", URL: "https://github.com/acme/example/pull/42#discussion_r101"},
			{Label: FeedbackFrustrated, Author: "lee-ops", Quote: "reacted -1", URL: "https://github.com/acme/example/pull/43#discussion_r500"},
		},
		Happy: &FeedbackQuote{Label: FeedbackHappy, Author: "sam-q", Quote: "thanks, the summary was helpful today", URL: "https://github.com/acme/example/pull/42#issuecomment-201"},
	}
}

func TestEvaluateFeedbackLineQuotesTheUnhappyAndWarnsOnVeryFrustrated(t *testing.T) {
	m := healthyMetrics()
	m.Feedback = feedbackFixture()
	r := Evaluate(m)
	var line Check
	for _, c := range r.Checks {
		if c.Name == "author feedback" {
			line = c
		}
	}
	if line.Status != StatusWarn {
		t.Fatalf("status = %s, detail = %s", line.Status, line.Detail)
	}
	want := `2 happy, 5 neutral, 1 frustrated, 1 very frustrated; @dana-dev (very): "This is the third time it flagged the same line after I explained it. Please stop." https://github.com/acme/example/pull/42#discussion_r101; @lee-ops: "reacted -1" https://github.com/acme/example/pull/43#discussion_r500; happy: @sam-q: "thanks, the summary was helpful today" https://github.com/acme/example/pull/42#issuecomment-201`
	if line.Detail != want {
		t.Errorf("detail =\n%s\nwant\n%s", line.Detail, want)
	}
	if r.Overall != StatusWarn || !strings.Contains(r.Headline, "author feedback") {
		t.Errorf("overall = %s, headline = %s", r.Overall, r.Headline)
	}
	if !strings.Contains(r.Markdown(), "🟡 **author feedback**") {
		t.Errorf("markdown:\n%s", r.Markdown())
	}
}

func TestEvaluateFeedbackStatusThresholds(t *testing.T) {
	for n, want := range map[int]Status{0: StatusOK, 1: StatusWarn, 2: StatusWarn, 3: StatusCritical, 7: StatusCritical} {
		m := healthyMetrics()
		m.Feedback = FeedbackMetrics{Scanned: true, ByLabel: map[string]int{FeedbackVeryFrustrated: n, FeedbackHappy: 1}}
		if got := feedbackStatus(m.Feedback); got != want {
			t.Errorf("%d very frustrated: status = %s, want %s", n, got, want)
		}
	}
}

func TestEvaluateFeedbackLineSaysWhenNothingWasScanned(t *testing.T) {
	for _, tc := range []struct {
		f    FeedbackMetrics
		want string
	}{
		{FeedbackMetrics{}, "not scanned"},
		{FeedbackMetrics{Note: "no GitHub client"}, "not scanned (no GitHub client)"},
		{FeedbackMetrics{Scanned: true}, "no author feedback in the window"},
		{FeedbackMetrics{Scanned: true, Note: "partial (3 of 40 targets)"}, "no author feedback in the window (partial scan: partial (3 of 40 targets))"},
		{FeedbackMetrics{ByLabel: map[string]int{FeedbackHappy: 1}, Note: "scan failed"}, "1 happy, 0 neutral, 0 frustrated, 0 very frustrated (stored items only; not scanned: scan failed)"},
		{FeedbackMetrics{Scanned: true, ByLabel: map[string]int{FeedbackHappy: 1}, Note: "2 of 30 PRs could not be read"}, "1 happy, 0 neutral, 0 frustrated, 0 very frustrated (partial scan: 2 of 30 PRs could not be read)"},
	} {
		if got := feedbackDetail(tc.f); got != tc.want {
			t.Errorf("%+v: %q, want %q", tc.f, got, tc.want)
		}
	}
	if telemetry := TelemetryActions[len(TelemetryActions)-1]; telemetry != ActionFeedbackFrustrated {
		t.Errorf("feedback counter missing from TelemetryActions: %v", TelemetryActions)
	}
	if other := withoutHygiene(map[string]int{ActionFeedbackFrustrated: 3, "reply_decision": 1}); len(other) != 1 || other["reply_decision"] != 1 {
		t.Errorf("feedback counter leaked into the generic telemetry line: %v", other)
	}
}

func TestSummariseDailyRounds(t *testing.T) {
	got := SummariseDailyRounds([]int{1, 4, 2, 5, 4, 1}, 4)
	want := DailyRoundStats{PRDays: 6, P50: 3, Max: 5, Cap: 4, AtCap: 2, OverCap: 1}
	if got != want {
		t.Fatalf("got %+v want %+v", got, want)
	}
	if got := SummariseDailyRounds([]int{3, 1, 2}, 0); got != (DailyRoundStats{PRDays: 3, P50: 2, Max: 3}) {
		t.Fatalf("no cap means nothing is at or over it: %+v", got)
	}
	if got := SummariseDailyRounds(nil, 4); got != (DailyRoundStats{Cap: 4}) {
		t.Fatalf("no rounds: %+v", got)
	}
}

func TestEvaluateReportsRoundsPerPRPerDay(t *testing.T) {
	m := healthyMetrics()
	m.AutoReview.RoundsPerPRDay = []int{1, 1, 4, 6}
	m.AutoReview.DailyRoundCap = 4
	m.Telemetry[ActionReviewSkippedDailyCap] = 3
	r := Evaluate(m)
	var line *Check
	for i := range r.Checks {
		if r.Checks[i].Name == "auto-review rounds per PR per day" {
			line = &r.Checks[i]
		}
	}
	if line == nil {
		t.Fatalf("no rounds line in %+v", r.Checks)
	}
	want := "p50 2.5, max 6 over 4 PR-days; cap 4: 1 at it, 1 over it, 3 pushes skipped by it"
	if line.Detail != want || line.Status != StatusOK {
		t.Fatalf("line = %+v\nwant %q", *line, want)
	}
	if got := r.Metrics.AutoReview.DailyRounds; got != (DailyRoundStats{PRDays: 4, P50: 2.5, Max: 6, Cap: 4, AtCap: 1, OverCap: 1}) {
		t.Fatalf("stored stats = %+v", got)
	}
	if r.Overall != StatusOK {
		t.Fatalf("the rounds line informs, it does not alarm: %s", r.Overall)
	}

	m.AutoReview.RoundsPerPRDay = nil
	for _, c := range Evaluate(m).Checks {
		if c.Name == "auto-review rounds per PR per day" && c.Detail != "no automatic rounds in the window" {
			t.Fatalf("empty window: %q", c.Detail)
		}
	}
	m.AutoReview.RoundsPerPRDay, m.AutoReview.DailyRoundCap = []int{2, 7}, 0
	for _, c := range Evaluate(m).Checks {
		if c.Name == "auto-review rounds per PR per day" && c.Detail != "p50 4.5, max 7 over 2 PR-days; no daily cap" {
			t.Fatalf("cap off: %q", c.Detail)
		}
	}
}

func TestEvaluateBacklogLineNamesTheDebounceWindow(t *testing.T) {
	m := healthyMetrics()
	m.AutoReview = AutoReviewMetrics{Queued: 2, OldestQueuedAge: 7 * time.Minute, SyncDebounce: 10 * time.Minute}
	for _, c := range Evaluate(m).Checks {
		if c.Name == "auto-review backlog" {
			if c.Status != StatusOK || c.Detail != "2 heads owed a review, oldest waiting 7m00s (a pushed head waits 10m00s before admission)" {
				t.Fatalf("backlog = %+v", c)
			}
			return
		}
	}
	t.Fatal("no backlog check")
}

func TestCommentQualityCheckFlagsAHighBadRate(t *testing.T) {
	q := QualityMetrics{Graded: 20, ByGrade: map[string]int{"GOOD": 5, "OK": 7, "BAD": 8}, ByKind: map[string]map[string]int{"inline": {"BAD": 8, "OK": 7, "GOOD": 5}},
		BadCategories: map[string]int{"wrong-claim": 8}, Trend: []QualityDay{{Date: "2026-10-21"}, {Date: "2026-10-22", Graded: 20, Bad: 8}}, CostUSD: 12.5}
	if got := qualityStatus(q); got != StatusWarn {
		t.Fatalf("status = %s, want warn", got)
	}
	detail := qualityDetail(q)
	for _, want := range []string{"20 graded: 5 good, 7 ok, 8 bad (40% bad)", "7-day bad rate - 40%", "grading cost $12.50"} {
		if !strings.Contains(detail, want) {
			t.Errorf("detail %q missing %q", detail, want)
		}
	}
	q.ByGrade["BAD"], q.ByGrade["OK"] = 11, 4
	if got := qualityStatus(q); got != StatusCritical {
		t.Errorf("status = %s, want critical", got)
	}
	if got := qualityStatus(QualityMetrics{Graded: 3, ByGrade: map[string]int{"BAD": 3}}); got != StatusOK {
		t.Errorf("too few graded: status = %s, want ok", got)
	}
	if got := qualityDetail(QualityMetrics{}); got != "no comments graded in the window" {
		t.Errorf("empty detail = %q", got)
	}
	if got := qualityStatus(QualityMetrics{Enabled: true}); got != StatusWarn {
		t.Errorf("enabled grader that graded nothing: status = %s, want warn", got)
	}
}
