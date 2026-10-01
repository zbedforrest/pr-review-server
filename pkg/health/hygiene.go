package health

import "fmt"

// Publication hygiene telemetry actions. Each is one telemetry event per
// occurrence, recorded by the poller on the publisher and reply paths, so
// the daily report counts them like any other action. The replay harness
// (cmd/publishreplay) computes the same conditions offline.
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

// hygieneDetail is the publication hygiene line: every counter, zero or not,
// so a missing number is never mistaken for a quiet day.
func hygieneDetail(telemetry map[string]int) string {
	return fmt.Sprintf("%d repeated posts, %d repeats after dismiss, %d fixed without a file change, %d same-commit resolves, %d severity escalations; %d author verdicts settled, %d posts stopped by opt-out",
		telemetry[ActionRepeatedPost], telemetry[ActionRepeatAfterDismiss], telemetry[ActionFixedWithoutFileChange], telemetry[ActionSameCommitResolve],
		telemetry[ActionSeverityEscalation], telemetry[ActionVerdictSettled], telemetry[ActionOptedOut])
}
