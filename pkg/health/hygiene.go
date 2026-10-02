package health

import (
	"fmt"
	"strings"
)

// Publication hygiene telemetry actions. Each is one telemetry event per
// occurrence, recorded by the poller on the publisher and reply paths, so
// the daily report counts them like any other action. The replay harness
// (cmd/publishreplay, arriving with the replay PR) computes the same
// conditions offline.
const (
	// ActionRepeatedPost: a root comment posted for a fingerprint that already
	// had a root on the PR (same marker, or aliased to an earlier row).
	ActionRepeatedPost = "repeated_posts_total"
	// ActionRepeatAfterDismiss: a root posted for a fingerprint the ledger
	// records as dismissed.
	ActionRepeatAfterDismiss = "repeat_after_dismiss"
	// ActionFixedWithoutFileChange: a row resolved on a round where the cited
	// file did not change between the row's last-seen head and this head.
	ActionFixedWithoutFileChange = "fixed_without_file_change_total"
	// ActionSameCommitResolve: a row resolved on a round whose head equals the
	// row's last-seen head.
	ActionSameCommitResolve = "same_commit_resolve_total"
	// ActionSeverityEscalation: an existing row written again at a higher
	// severity than the ledger holds.
	ActionSeverityEscalation = "severity_escalations"
	// ActionVerdictSettled: an author verdict (intentional, won't fix, thumbs
	// down) settled a thread without a model reply. Recorded by the author
	// verdict fast path once it exists.
	ActionVerdictSettled = "verdicts_settled"
	// ActionOptedOut: the publish gate denied a post because the author opted
	// out. Recorded by the opt-out gate once it exists.
	ActionOptedOut = "opted_out_total"
)

// HygieneActions lists every hygiene action, in report order.
var HygieneActions = []string{
	ActionRepeatedPost, ActionRepeatAfterDismiss, ActionFixedWithoutFileChange, ActionSameCommitResolve,
	ActionSeverityEscalation, ActionVerdictSettled, ActionOptedOut,
}

// hygieneLabels names each counter in the report.
var hygieneLabels = map[string]string{
	ActionRepeatedPost: "repeated posts", ActionRepeatAfterDismiss: "repeats after dismiss",
	ActionFixedWithoutFileChange: "fixed without a file change", ActionSameCommitResolve: "same-commit resolves",
	ActionSeverityEscalation: "severity escalations", ActionVerdictSettled: "author verdicts settled", ActionOptedOut: "posts stopped by opt-out",
}

// unwiredHygiene lists the counters whose producer has not shipped yet: the
// dismissal alias, the changed-file compare, the author verdict path and the
// opt-out gate. Until an event arrives their zero means "not measured", not
// "nothing happened", and the line says so.
var unwiredHygiene = map[string]bool{
	ActionRepeatAfterDismiss: true, ActionFixedWithoutFileChange: true, ActionVerdictSettled: true, ActionOptedOut: true,
}

// hygieneDetail is the publication hygiene line: every measured counter, zero
// or not, then the counters that cannot fire yet.
func hygieneDetail(telemetry map[string]int) string {
	var measured, unmeasured []string
	for _, a := range HygieneActions {
		if unwiredHygiene[a] && telemetry[a] == 0 {
			unmeasured = append(unmeasured, hygieneLabels[a])
			continue
		}
		measured = append(measured, fmt.Sprintf("%d %s", telemetry[a], hygieneLabels[a]))
	}
	line := strings.Join(measured, ", ")
	if len(unmeasured) > 0 {
		line += "; not yet measured: " + strings.Join(unmeasured, ", ")
	}
	return line
}

// withoutHygiene drops the hygiene actions, which have their own line.
func withoutHygiene(telemetry map[string]int) map[string]int {
	hygiene := make(map[string]bool, len(HygieneActions))
	for _, a := range HygieneActions {
		hygiene[a] = true
	}
	out := make(map[string]int, len(telemetry))
	for k, n := range telemetry {
		if !hygiene[k] {
			out[k] = n
		}
	}
	return out
}
