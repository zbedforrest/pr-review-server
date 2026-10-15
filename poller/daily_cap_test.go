package poller

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"pr-review-server/db"
	"pr-review-server/github"
	"pr-review-server/pkg/health"
)

func seedRun(t *testing.T, f autoReviewFixture, id, trigger, status string, acceptedAt time.Time) {
	t.Helper()
	require.NoError(t, f.db.CreateReviewRun(&db.ReviewRun{
		RunID: id, RepoOwner: "acme", RepoName: "example", PRNumber: 7, CommitSHA: "sha-" + id, TriggerSource: trigger, Status: status,
		RequestedConfigJSON: "{}", EffectiveConfigJSON: "{}", ConfigSourcesJSON: "{}", ConfigHash: "h", ConfigSchemaVersion: 3,
		AcceptedAt: acceptedAt, QueuedAt: acceptedAt,
	}))
}

func earlierToday() time.Time {
	return utcDayStart(time.Now()).Add(time.Second)
}

func (f autoReviewFixture) hygieneEvents(t *testing.T, action string) []db.TelemetryEvent {
	t.Helper()
	f.db.mu.RLock()
	defer f.db.mu.RUnlock()
	var out []db.TelemetryEvent
	for _, e := range f.db.TelemetryEvents {
		if e.Action == action {
			out = append(out, e)
		}
	}
	return out
}

func TestDailyRoundCapSettlesThePushPastTheCap(t *testing.T) {
	f := newAutoReviewFixture(t, true, "*")
	for i, id := range []string{"a1", "a2", "a3", "a4"} {
		seedRun(t, f, id, autoReviewTriggerSource, db.ReviewRunStatusCompleted, earlierToday().Add(time.Duration(i)*time.Minute))
	}
	trackedReadyPR(f, autoReviewNewHead, false)

	require.NoError(t, f.p.HandleWebhookDelivery(context.Background(), readyDelivery("synchronize", autoReviewNewHead, false)))
	waitForDetachedReviews(t, f.p)
	intent := f.intentFor(t, autoReviewNewHead)
	assert.Equal(t, db.AutoReviewIntentDone, intent.Status, "the fifth push of the day is settled, not reviewed")
	assert.Equal(t, publicationSkippedDailyCap, intent.Publication)
	assert.Empty(t, f.generator.GenerateReviewCalls)
	events := f.hygieneEvents(t, health.ActionReviewSkippedDailyCap)
	require.Len(t, events, 1)
	assert.Equal(t, "cap=4 trigger=synchronize", events[0].Label)

	require.NoError(t, f.p.HandleWebhookDelivery(context.Background(), readyDelivery("ready_for_review", autoReviewThirdHead, false)))
	waitForDetachedReviews(t, f.p)
	assert.Equal(t, db.AutoReviewIntentRunning, f.intentFor(t, autoReviewThirdHead).Status, "a PR becoming ready is reviewed whatever the day held")
	require.Len(t, f.generator.GenerateReviewCalls, 1)
}

func TestDailyRoundCapCountsOnlyAdmittedAutomaticRounds(t *testing.T) {
	f := newAutoReviewFixture(t, true, "*")
	seedRun(t, f, "m1", "api_v1", db.ReviewRunStatusCompleted, earlierToday())
	seedRun(t, f, "m2", "mention", db.ReviewRunStatusCompleted, earlierToday())
	seedRun(t, f, "m3", "poller", db.ReviewRunStatusCompleted, earlierToday())
	seedRun(t, f, "m4", "api_v1", db.ReviewRunStatusCompleted, earlierToday())
	seedRun(t, f, "c1", autoReviewTriggerSource, db.ReviewRunStatusCancelled, earlierToday())
	seedRun(t, f, "a1", autoReviewTriggerSource, db.ReviewRunStatusCompleted, earlierToday())
	seedRun(t, f, "a2", autoReviewTriggerSource, db.ReviewRunStatusFailed, earlierToday())
	seedRun(t, f, "a3", autoReviewTriggerSource, db.ReviewRunStatusCompleted, earlierToday())

	rounds, err := f.p.dailyAutoRounds("acme", "example", 7, time.Now().UTC())
	require.NoError(t, err)
	assert.Equal(t, 2, rounds, "manual reviews, a rejected admission and a failed run never count")

	trackedReadyPR(f, autoReviewNewHead, false)
	require.NoError(t, f.p.HandleWebhookDelivery(context.Background(), readyDelivery("synchronize", autoReviewNewHead, false)))
	waitForDetachedReviews(t, f.p)
	assert.Equal(t, db.AutoReviewIntentRunning, f.intentFor(t, autoReviewNewHead).Status, "three automatic rounds leave room for a fourth")
	require.Len(t, f.generator.GenerateReviewCalls, 1)
	assert.Empty(t, f.hygieneEvents(t, health.ActionReviewSkippedDailyCap))
}

func TestDailyRoundCapIgnoresRunsThatPostedNothing(t *testing.T) {
	f := newAutoReviewFixture(t, true, "*")
	for i, id := range []string{"f1", "f2", "f3"} {
		seedRun(t, f, id, autoReviewTriggerSource, db.ReviewRunStatusFailed, earlierToday().Add(time.Duration(i)*time.Minute))
	}
	seedRun(t, f, "t1", autoReviewTriggerSource, db.ReviewRunStatusTimedOut, earlierToday())
	seedRun(t, f, "c1", autoReviewTriggerSource, db.ReviewRunStatusCancelled, earlierToday())
	trackedReadyPR(f, autoReviewNewHead, false)

	require.NoError(t, f.p.HandleWebhookDelivery(context.Background(), readyDelivery("synchronize", autoReviewNewHead, false)))
	waitForDetachedReviews(t, f.p)
	assert.Equal(t, db.AutoReviewIntentRunning, f.intentFor(t, autoReviewNewHead).Status, "an outage's failed runs do not use up the day")
	require.Len(t, f.generator.GenerateReviewCalls, 1)
	assert.Empty(t, f.hygieneEvents(t, health.ActionReviewSkippedDailyCap))
}

func TestCappedHeadNeverAnchorsTheUnchangedDiffSkip(t *testing.T) {
	f := newAutoReviewFixture(t, true, "*")
	f.p.cfg.AutoReviewSkipUnchangedDiff = true
	head := autoReviewNewHead
	f.p.prDiffFn = stubDiff(map[string]string{autoReviewNewHead: diffOne, autoReviewThirdHead: diffOne}, &head)
	for i, id := range []string{"a1", "a2", "a3", "a4"} {
		seedRun(t, f, id, autoReviewTriggerSource, db.ReviewRunStatusCompleted, earlierToday().Add(time.Duration(i)*time.Minute))
	}
	trackedReadyPR(f, autoReviewNewHead, false)
	require.NoError(t, f.p.HandleWebhookDelivery(context.Background(), readyDelivery("synchronize", autoReviewNewHead, false)))
	waitForDetachedReviews(t, f.p)
	capped := f.intentFor(t, autoReviewNewHead)
	require.Equal(t, publicationSkippedDailyCap, capped.Publication)
	assert.Equal(t, github.PatchID(diffOne), capped.PatchID)

	f.p.cfg.AutoReviewNoDailyRoundCap = true
	head = autoReviewThirdHead
	require.NoError(t, f.p.HandleWebhookDelivery(context.Background(), readyDelivery("synchronize", autoReviewThirdHead, false)))
	waitForDetachedReviews(t, f.p)
	assert.Equal(t, db.AutoReviewIntentRunning, f.intentFor(t, autoReviewThirdHead).Status, "a rebase of a head nobody reviewed is reviewed once the cap lifts")
	require.Len(t, f.generator.GenerateReviewCalls, 1)
}

func TestDailyRoundCapRollsOverAtMidnightUTC(t *testing.T) {
	f := newAutoReviewFixture(t, true, "*")
	lateYesterday := time.Date(2026, 10, 13, 23, 50, 0, 0, time.UTC)
	for i, id := range []string{"a1", "a2", "a3", "a4"} {
		seedRun(t, f, id, autoReviewTriggerSource, db.ReviewRunStatusCompleted, lateYesterday.Add(time.Duration(i)*time.Minute))
	}
	rounds, err := f.p.dailyAutoRounds("acme", "example", 7, time.Date(2026, 10, 13, 23, 59, 0, 0, time.UTC))
	require.NoError(t, err)
	assert.Equal(t, 4, rounds, "before midnight the day's rounds all count")

	rounds, err = f.p.dailyAutoRounds("acme", "example", 7, time.Date(2026, 10, 14, 0, 5, 0, 0, time.UTC))
	require.NoError(t, err)
	assert.Equal(t, 0, rounds, "after midnight UTC the count starts again")

	local := time.FixedZone("IST", 5*3600+1800)
	rounds, err = f.p.dailyAutoRounds("acme", "example", 7, time.Date(2026, 10, 14, 5, 15, 0, 0, local))
	require.NoError(t, err)
	assert.Equal(t, 4, rounds, "the day is the UTC day, not the local one: 05:15 IST is 23:45 UTC the day before")
	assert.Equal(t, time.Date(2026, 10, 13, 0, 0, 0, 0, time.UTC), utcDayStart(time.Date(2026, 10, 14, 5, 15, 0, 0, local)))
}

func TestDailyRoundCapKillSwitchSettingAndZero(t *testing.T) {
	for name, arrange := range map[string]func(f autoReviewFixture){
		"kill switch": func(f autoReviewFixture) { f.p.cfg.AutoReviewNoDailyRoundCap = true },
		"setting 0":   func(f autoReviewFixture) { require.NoError(t, f.db.SetSetting(SettingAutoReviewDailyRoundCap, "0")) },
		"setting 6":   func(f autoReviewFixture) { require.NoError(t, f.db.SetSetting(SettingAutoReviewDailyRoundCap, "6")) },
	} {
		t.Run(name, func(t *testing.T) {
			f := newAutoReviewFixture(t, true, "*")
			arrange(f)
			for i, id := range []string{"a1", "a2", "a3", "a4"} {
				seedRun(t, f, id, autoReviewTriggerSource, db.ReviewRunStatusCompleted, earlierToday().Add(time.Duration(i)*time.Minute))
			}
			trackedReadyPR(f, autoReviewNewHead, false)
			require.NoError(t, f.p.HandleWebhookDelivery(context.Background(), readyDelivery("synchronize", autoReviewNewHead, false)))
			waitForDetachedReviews(t, f.p)
			assert.Equal(t, db.AutoReviewIntentRunning, f.intentFor(t, autoReviewNewHead).Status)
			require.Len(t, f.generator.GenerateReviewCalls, 1)
		})
	}

	f := newAutoReviewFixture(t, true, "*")
	require.NoError(t, f.db.SetSetting(SettingAutoReviewDailyRoundCap, "2"))
	seedRun(t, f, "a1", autoReviewTriggerSource, db.ReviewRunStatusCompleted, earlierToday())
	seedRun(t, f, "a2", autoReviewTriggerSource, db.ReviewRunStatusCompleted, earlierToday())
	trackedReadyPR(f, autoReviewNewHead, false)
	require.NoError(t, f.p.HandleWebhookDelivery(context.Background(), readyDelivery("synchronize", autoReviewNewHead, false)))
	waitForDetachedReviews(t, f.p)
	assert.Equal(t, publicationSkippedDailyCap, f.intentFor(t, autoReviewNewHead).Publication, "a lower setting caps sooner")
	assert.Empty(t, f.generator.GenerateReviewCalls)
}

func TestParseDailyRoundCap(t *testing.T) {
	for raw, want := range map[string]int{"": 4, "garbage": 4, "-1": 4, "0": 0, " 3 ": 3, "10": 10} {
		assert.Equal(t, want, ParseDailyRoundCap(raw), "raw %q", raw)
	}
}

func TestPushesCappedFollowsTheDayCount(t *testing.T) {
	f := newAutoReviewFixture(t, true, "*")
	now := time.Now().UTC()
	for _, id := range []string{"a1", "a2", "a3"} {
		seedRun(t, f, id, autoReviewTriggerSource, db.ReviewRunStatusCompleted, earlierToday())
	}
	assert.False(t, f.p.pushesCapped("acme", "example", 7, now), "three rounds leave a fourth, no footer yet")
	seedRun(t, f, "a4", autoReviewTriggerSource, db.ReviewRunStatusRunning, earlierToday())
	assert.True(t, f.p.pushesCapped("acme", "example", 7, now), "the round being published is the day's last")
	assert.False(t, f.p.pushesCapped("acme", "example", 7, now.Add(24*time.Hour)), "tomorrow the footer is gone")
	f.p.cfg.AutoReviewNoDailyRoundCap = true
	assert.False(t, f.p.pushesCapped("acme", "example", 7, now), "no cap, no footer")
}
