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
	// down) settled a finding without a model reply. Recorded per verdict by
	// the reply scan.
	ActionVerdictSettled = "verdicts_settled"
	// ActionPublishDenied: the publish gate stopped a finished review from
	// posting. Today that means the author is outside publish_enabled_authors;
	// the opt-out list will add a reason label.
	ActionPublishDenied = "publish_denied_total"
)

// ActionFeedbackFrustrated: the daily feedback scan classified a new author
// reply, comment or reaction as frustrated or very frustrated.
const ActionFeedbackFrustrated = "feedback_frustrated"

// HygieneActions lists every hygiene action, in report order.
var HygieneActions = []string{
	ActionRepeatedPost, ActionRepeatAfterDismiss, ActionFixedWithoutFileChange, ActionSameCommitResolve,
	ActionSeverityEscalation, ActionVerdictSettled, ActionPublishDenied,
}

// TelemetryActions are the server-emitted counters the daily report reads.
var TelemetryActions = append(append([]string{}, HygieneActions...), ActionFeedbackFrustrated)

// hygieneLabels names each counter in the report.
var hygieneLabels = map[string]string{
	ActionRepeatedPost: "repeated posts", ActionRepeatAfterDismiss: "repeats after dismiss",
	ActionFixedWithoutFileChange: "fixed without a file change", ActionSameCommitResolve: "same-commit resolves",
	ActionSeverityEscalation: "severity escalations", ActionVerdictSettled: "author verdicts settled", ActionPublishDenied: "posts stopped by the publish gate",
}

// UnwiredHygiene maps each counter whose producer has not shipped yet to the
// item that wires it. Until an event arrives their zero means "not measured",
// not "nothing happened", and the report line says so. The producing
// packages assert they emit none of these, so wiring one means removing its
// entry here.
var UnwiredHygiene = map[string]string{}

// hygieneDetail is the publication hygiene line: every measured counter, zero
// or not, then the counters that cannot fire yet.
func hygieneDetail(telemetry map[string]int) string {
	var measured, unmeasured []string
	for _, a := range HygieneActions {
		if _, unwired := UnwiredHygiene[a]; unwired && telemetry[a] == 0 {
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
