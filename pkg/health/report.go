// Package health turns a day of PRism's own records into a short report: what
// ran, what failed, what was posted, and whether anything needs a human.
package health

import (
	"fmt"
	"sort"
	"strings"
	"time"
)

type Status string

const (
	StatusOK       Status = "ok"
	StatusWarn     Status = "warn"
	StatusCritical Status = "critical"
)

// Metrics is everything Evaluate looks at, gathered from the database by the
// caller so the evaluation itself stays pure and testable.
type Metrics struct {
	WindowStart time.Time
	WindowEnd   time.Time
	Now         time.Time

	Runs      RunMetrics
	Attempts  map[string]int // "<stage>/<error_code>" -> failed attempts
	Queue     QueueMetrics
	Publish   PublishMetrics
	Replies   ReplyMetrics
	Telemetry map[string]int // action -> events in the window
	Lease     LeaseMetrics
	PRErrors  int           // PRs currently carrying an error message
	WallClock time.Duration // the configured agent wall clock
	// PollingDisabled marks an on-demand deployment, which holds no poller
	// lease by design.
	PollingDisabled bool
}

type RunMetrics struct {
	Total          int
	ByStatus       map[string]int
	ByTrigger      map[string]int
	ByTerminalCode map[string]int
	DurationsMS    []int64 // completed runs
	ModelFallbacks int
	Verdicts       map[string]int
	Criticals      int
}

type QueueMetrics struct {
	Queued           int
	Running          int
	OldestQueuedAge  time.Duration
	OldestRunningAge time.Duration
}

type PublishMetrics struct {
	Summaries   int
	Inline      int
	Annotations int
	Dismissed   int // current total, not windowed: the ledger has no dismissal time
}

type ReplyMetrics struct {
	Handled       int
	ByClass       map[string]int
	ByAction      map[string]int
	ByOutcome     map[string]int
	ByDecision    map[string]int
	TextPosted    int
	StuckPending  int // pending action, no outcome, older than an hour
	Failed        int
	UnlinkedRoots int
}

type LeaseMetrics struct {
	Present   bool
	Holder    string
	ExpiresAt time.Time
}

// Check is one line of the report.
type Check struct {
	Name   string `json:"name"`
	Status Status `json:"status"`
	Detail string `json:"detail"`
}

type Report struct {
	WindowStart time.Time `json:"window_start"`
	WindowEnd   time.Time `json:"window_end"`
	Overall     Status    `json:"overall"`
	Headline    string    `json:"headline"`
	Checks      []Check   `json:"checks"`
	Metrics     Metrics   `json:"metrics"`
}

// Evaluate applies fixed thresholds. They are deliberately simple: the point
// is a reliable morning signal, not a scoring system.
func Evaluate(m Metrics) Report {
	r := Report{WindowStart: m.WindowStart, WindowEnd: m.WindowEnd, Metrics: m, Overall: StatusOK}
	add := func(name string, status Status, detail string) {
		r.Checks = append(r.Checks, Check{Name: name, Status: status, Detail: detail})
		if status == StatusCritical || (status == StatusWarn && r.Overall == StatusOK) {
			r.Overall = status
		}
	}

	total := 0
	for _, n := range m.Runs.ByStatus {
		total += n
	}
	completed := m.Runs.ByStatus["completed"]
	timedOut := m.Runs.ByStatus["timed_out"]
	switch {
	case total == 0:
		add("review volume", StatusWarn, "No reviews in the window")
	default:
		rate := float64(completed) / float64(total)
		status := StatusOK
		if rate < 0.8 {
			status = StatusCritical
		} else if rate < 0.95 {
			status = StatusWarn
		}
		add("review success rate", status, fmt.Sprintf("%d of %d reviews completed (%.0f%%); %s", completed, total, rate*100, countList(m.Runs.ByStatus)))
		if timedOut > 0 {
			status := StatusWarn
			if timedOut > 3 && float64(timedOut)/float64(total) > 0.2 {
				status = StatusCritical
			}
			add("wall-clock timeouts", status, fmt.Sprintf("%d reviews hit the wall clock", timedOut))
		}
		p50, p90, max := percentiles(m.Runs.DurationsMS)
		add("review latency", StatusOK, fmt.Sprintf("p50 %s, p90 %s, max %s over %d completed", dur(p50), dur(p90), dur(max), len(m.Runs.DurationsMS)))
		if len(m.Runs.ByTerminalCode) > 0 {
			add("failure codes", StatusOK, countList(m.Runs.ByTerminalCode))
		}
	}
	if m.Runs.ModelFallbacks > 0 {
		add("model fallbacks", StatusWarn, fmt.Sprintf("%d reviews were served by a different model than requested", m.Runs.ModelFallbacks))
	}
	if failed := sumValues(m.Attempts); failed > 0 {
		add("stage attempt failures", StatusOK, countList(m.Attempts))
	}

	switch {
	case m.Queue.OldestQueuedAge > 30*time.Minute:
		add("queue age", StatusCritical, fmt.Sprintf("%d queued, oldest waiting %s", m.Queue.Queued, dur(m.Queue.OldestQueuedAge.Milliseconds())))
	case m.Queue.OldestQueuedAge > 10*time.Minute:
		add("queue age", StatusWarn, fmt.Sprintf("%d queued, oldest waiting %s", m.Queue.Queued, dur(m.Queue.OldestQueuedAge.Milliseconds())))
	default:
		add("queue age", StatusOK, fmt.Sprintf("%d queued", m.Queue.Queued))
	}
	if m.WallClock > 0 && m.Queue.OldestRunningAge > m.WallClock*2 {
		add("running reviews", StatusCritical, fmt.Sprintf("%d running, oldest for %s, past twice the wall clock", m.Queue.Running, dur(m.Queue.OldestRunningAge.Milliseconds())))
	} else if m.WallClock > 0 && m.Queue.OldestRunningAge > m.WallClock {
		add("running reviews", StatusWarn, fmt.Sprintf("%d running, oldest for %s, past the wall clock", m.Queue.Running, dur(m.Queue.OldestRunningAge.Milliseconds())))
	} else {
		add("running reviews", StatusOK, fmt.Sprintf("%d running", m.Queue.Running))
	}

	switch {
	case m.PollingDisabled:
		add("poller lease", StatusOK, "polling disabled on this deployment (on-demand reviews only)")
	case !m.Lease.Present:
		add("poller lease", StatusCritical, "no poller lease row; nothing is polling")
	case m.Lease.ExpiresAt.Before(m.Now):
		add("poller lease", StatusCritical, fmt.Sprintf("lease held by %s expired %s ago", m.Lease.Holder, dur(m.Now.Sub(m.Lease.ExpiresAt).Milliseconds())))
	default:
		add("poller lease", StatusOK, fmt.Sprintf("held by %s", m.Lease.Holder))
	}

	add("publications", StatusOK, fmt.Sprintf("%d summaries, %d inline comments, %d annotations; %d findings currently dismissed by concession (all time)", m.Publish.Summaries, m.Publish.Inline, m.Publish.Annotations, m.Publish.Dismissed))

	replyDetail := fmt.Sprintf("%d author replies handled", m.Replies.Handled)
	if m.Replies.Handled > 0 {
		replyDetail += fmt.Sprintf(" (%s); actions %s", countList(m.Replies.ByClass), countList(m.Replies.ByAction))
	}
	if len(m.Replies.ByDecision) > 0 {
		replyDetail += fmt.Sprintf("; decisions %s; outcomes %s; %d text replies posted", countList(m.Replies.ByDecision), countList(m.Replies.ByOutcome), m.Replies.TextPosted)
	}
	add("author replies", StatusOK, replyDetail)
	if n := m.Telemetry["reply_text_error"]; n > 0 {
		add("reply text errors", StatusWarn, fmt.Sprintf("%d reply steps errored (they resume next cycle)", n))
	}
	if m.Replies.StuckPending > 0 || m.Replies.Failed > 0 {
		add("stuck reply steps", StatusWarn, fmt.Sprintf("%d pending for over an hour, %d given up after repeated failures", m.Replies.StuckPending, m.Replies.Failed))
	}
	if n := m.Telemetry["reply_scan_error"] + m.Telemetry["reply_link_error"]; n > 0 {
		add("reply scan errors", StatusWarn, fmt.Sprintf("%d scan or link errors", n))
	}
	if m.Replies.UnlinkedRoots > 0 {
		add("unlinked reply roots", StatusWarn, fmt.Sprintf("%d inline findings on open PRs have no comment id", m.Replies.UnlinkedRoots))
	}

	if m.PRErrors > 0 {
		add("PR error messages", StatusWarn, fmt.Sprintf("%d PRs currently show an error on the dashboard", m.PRErrors))
	}

	r.Headline = headline(r, total, completed)
	return r
}

func headline(r Report, total, completed int) string {
	var bad []string
	for _, c := range r.Checks {
		if c.Status != StatusOK {
			bad = append(bad, c.Name)
		}
	}
	switch {
	case r.Overall == StatusOK:
		return fmt.Sprintf("OK: %d reviews, all systems normal", total)
	case total == 0 && len(bad) == 1:
		return "Quiet day: no reviews ran"
	default:
		return fmt.Sprintf("%s: %d of %d reviews completed; attention: %s", strings.ToUpper(string(r.Overall)), completed, total, strings.Join(bad, ", "))
	}
}

// Markdown renders the report for people.
func (r Report) Markdown() string {
	var b strings.Builder
	fmt.Fprintf(&b, "# PRism daily health: %s\n\n", strings.ToUpper(string(r.Overall)))
	fmt.Fprintf(&b, "%s\n\nWindow %s to %s UTC.\n\n", r.Headline, r.WindowStart.UTC().Format("2006-01-02 15:04"), r.WindowEnd.UTC().Format("2006-01-02 15:04"))
	for _, c := range r.Checks {
		fmt.Fprintf(&b, "- %s **%s**: %s\n", marker(c.Status), c.Name, c.Detail)
	}
	m := r.Metrics
	if len(m.Runs.ByTrigger) > 0 {
		fmt.Fprintf(&b, "\nTriggers: %s. Verdicts: %s. Critical findings: %d.\n", countList(m.Runs.ByTrigger), countList(m.Runs.Verdicts), m.Runs.Criticals)
	}
	if len(m.Telemetry) > 0 {
		fmt.Fprintf(&b, "Telemetry: %s.\n", countList(m.Telemetry))
	}
	return b.String()
}

func marker(s Status) string {
	switch s {
	case StatusCritical:
		return "🔴"
	case StatusWarn:
		return "🟡"
	}
	return "🟢"
}

func percentiles(v []int64) (p50, p90, max int64) {
	if len(v) == 0 {
		return 0, 0, 0
	}
	s := append([]int64(nil), v...)
	sort.Slice(s, func(i, j int) bool { return s[i] < s[j] })
	at := func(p float64) int64 {
		i := int(p*float64(len(s))+0.5) - 1
		if i < 0 {
			i = 0
		}
		if i >= len(s) {
			i = len(s) - 1
		}
		return s[i]
	}
	return at(0.5), at(0.9), s[len(s)-1]
}

func dur(ms int64) string {
	d := time.Duration(ms) * time.Millisecond
	if d < time.Minute {
		return fmt.Sprintf("%ds", int(d.Seconds()))
	}
	return fmt.Sprintf("%dm%02ds", int(d.Minutes()), int(d.Seconds())%60)
}

func countList(m map[string]int) string {
	keys := make([]string, 0, len(m))
	for k, n := range m {
		if n != 0 {
			keys = append(keys, k)
		}
	}
	sort.Slice(keys, func(i, j int) bool {
		if m[keys[i]] != m[keys[j]] {
			return m[keys[i]] > m[keys[j]]
		}
		return keys[i] < keys[j]
	})
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, fmt.Sprintf("%s %d", k, m[k]))
	}
	if len(parts) == 0 {
		return "none"
	}
	return strings.Join(parts, ", ")
}

func sumValues(m map[string]int) int {
	n := 0
	for _, v := range m {
		n += v
	}
	return n
}
