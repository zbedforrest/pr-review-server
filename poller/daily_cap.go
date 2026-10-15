package poller

import (
	"log"
	"strconv"
	"strings"
	"time"

	"pr-review-server/config"
	"pr-review-server/db"
	"pr-review-server/github"
	"pr-review-server/pkg/health"
)

// Only automatic runs count: manual trigger sources never reach the cap.
const (
	SettingAutoReviewDailyRoundCap = "auto_review_daily_round_cap"
	DefaultAutoReviewDailyRoundCap = 4
	publicationSkippedDailyCap     = "skipped: daily round cap"
)

type settingReader interface {
	GetSetting(key string) (string, error)
}

// ParseDailyRoundCap reads the setting's value: a missing or unreadable
// value is the default, 0 is no bound.
func ParseDailyRoundCap(raw string) int {
	n, err := strconv.Atoi(strings.TrimSpace(raw))
	if err != nil || n < 0 {
		return DefaultAutoReviewDailyRoundCap
	}
	return n
}

// DailyRoundCap is the cap in force: 0 under the kill switch or when the
// setting removes the bound. The server's health report reads it the same way.
func DailyRoundCap(settings settingReader, cfg *config.Config) int {
	if cfg != nil && cfg.AutoReviewNoDailyRoundCap {
		return 0
	}
	raw, err := settings.GetSetting(SettingAutoReviewDailyRoundCap)
	if err != nil {
		raw = ""
	}
	return ParseDailyRoundCap(raw)
}

func (p *Poller) dailyRoundCap() int {
	return DailyRoundCap(p.db, p.cfg)
}

func utcDayStart(now time.Time) time.Time {
	y, m, d := now.UTC().Date()
	return time.Date(y, m, d, 0, 0, 0, 0, time.UTC)
}

// dailyAutoRounds counts the automatic runs since midnight UTC that posted or
// will post; a cancelled, failed or timed-out run annoyed nobody.
func (p *Poller) dailyAutoRounds(owner, repo string, number int, now time.Time) (int, error) {
	runs, err := p.db.ListReviewRuns(db.ReviewRunFilter{RepoOwner: owner, RepoName: repo, PRNumber: number, Limit: db.MaxReviewRunListLimit})
	if err != nil {
		return 0, err
	}
	since := utcDayStart(now)
	n := 0
	for _, run := range runs {
		if run.TriggerSource == autoReviewTriggerSource && db.ReviewRunCountsAsRound(run.Status) && !run.AcceptedAt.Before(since) {
			n++
		}
	}
	return n, nil
}

// skipDailyRoundCap settles a pushed head without a review when the PR has
// had its day's automatic rounds. Only pushes (synchronize and the poll
// fallback) are capped: a PR becoming ready is reviewed whatever the day
// held, as a manual review is. A count that cannot be read admits the review.
func (p *Poller) skipDailyRoundCap(intent db.AutoReviewIntent, pr github.PullRequest, now time.Time) bool {
	if intent.Trigger != autoReviewTriggerSync && intent.Trigger != db.AutoReviewTriggerPollFallback {
		return false
	}
	limit := p.dailyRoundCap()
	if limit <= 0 {
		return false
	}
	key := autoReviewKey(pr.Owner, pr.Repo, pr.Number, pr.CommitSHA)
	rounds, err := p.dailyAutoRounds(pr.Owner, pr.Repo, pr.Number, now)
	if err != nil {
		log.Printf("[AUTO-REVIEW] %s intent=%d: could not count today's rounds, reviewing anyway: %v", key, intent.ID, err)
		return false
	}
	if rounds < limit {
		return false
	}
	skipped, err := p.db.SkipQueuedAutoReviewIntent(intent.ID, publicationSkippedDailyCap)
	if err != nil {
		log.Printf("[AUTO-REVIEW] %s intent=%d: could not settle the capped head: %v", key, intent.ID, err)
		return false
	}
	if !skipped {
		return false
	}
	log.Printf("[AUTO-REVIEW] %s intent=%d: %s (%d of %d today), no review", key, intent.ID, publicationSkippedDailyCap, rounds, limit)
	p.recordHygieneEvent(pr, health.ActionReviewSkippedDailyCap, "cap="+strconv.Itoa(limit)+" trigger="+intent.Trigger)
	return true
}

// pushesCapped reports whether the PR has had its day's automatic rounds,
// counting the round being published, so the summary footer can say so.
func (p *Poller) pushesCapped(owner, repo string, number int, now time.Time) bool {
	limit := p.dailyRoundCap()
	if limit <= 0 {
		return false
	}
	rounds, err := p.dailyAutoRounds(owner, repo, number, now)
	if err != nil {
		log.Printf("[PUBLISH] %s/%s#%d: could not count today's rounds for the footer: %v", owner, repo, number, err)
		return false
	}
	return rounds >= limit
}
