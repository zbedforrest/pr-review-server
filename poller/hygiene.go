package poller

import (
	"log"

	"pr-review-server/db"
	"pr-review-server/github"
	"pr-review-server/pkg/health"
	"pr-review-server/pkg/publisher"
)

// hygieneTelemetryEvents turns a round's hygiene notes into one telemetry
// event per occurrence, named by the health package's action vocabulary so
// the daily report and the replay harness count the same things.
func hygieneTelemetryEvents(pr github.PullRequest, h publisher.Hygiene, userID int) []db.TelemetryEvent {
	var events []db.TelemetryEvent
	add := func(action string, notes []publisher.HygieneNote) {
		for _, n := range notes {
			events = append(events, hygieneEvent(pr, action, "fp="+n.Fingerprint+" "+n.Detail, userID))
		}
	}
	add(health.ActionRepeatedPost, h.RepeatedPosts)
	add(health.ActionRepeatAfterDismiss, h.RepeatAfterDismiss)
	add(health.ActionFixedWithoutFileChange, h.FixedWithoutFileChange)
	add(health.ActionSameCommitResolve, h.SameCommitResolves)
	add(health.ActionSeverityEscalation, h.SeverityEscalations)
	return events
}

func hygieneEvent(pr github.PullRequest, action, label string, userID int) db.TelemetryEvent {
	return db.TelemetryEvent{UserID: userID, Action: action, Label: truncateLabel(label, 255), PROwner: pr.Owner, PRRepo: pr.Repo, PRNumber: pr.Number}
}

// recordHygiene writes a published round's hygiene notes. Best-effort, like
// every server-emitted telemetry event.
func (p *Poller) recordHygiene(pr github.PullRequest, h publisher.Hygiene) {
	userID := p.systemTelemetryUserID()
	if userID == 0 {
		return
	}
	events := hygieneTelemetryEvents(pr, h, userID)
	if len(events) == 0 {
		return
	}
	if err := p.db.CreateTelemetryEvents(events); err != nil {
		log.Printf("[PUBLISH] %s/%s#%d: could not record hygiene telemetry: %v", pr.Owner, pr.Repo, pr.Number, err)
	}
}

// recordHygieneEvent writes one hygiene event outside a publish round. The
// author verdict fast path records health.ActionVerdictSettled and the
// opt-out gate health.ActionOptedOut through it.
func (p *Poller) recordHygieneEvent(pr github.PullRequest, action, label string) {
	userID := p.systemTelemetryUserID()
	if userID == 0 {
		return
	}
	if err := p.db.CreateTelemetryEvents([]db.TelemetryEvent{hygieneEvent(pr, action, label, userID)}); err != nil {
		log.Printf("[PUBLISH] %s/%s#%d: could not record %s telemetry: %v", pr.Owner, pr.Repo, pr.Number, action, err)
	}
}
