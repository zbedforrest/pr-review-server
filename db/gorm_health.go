package db

import (
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"pr-review-server/pkg/health"
)

// HealthReportModel stores one daily health report.
type HealthReportModel struct {
	ID          uint      `gorm:"primaryKey;autoIncrement"`
	ReportDate  string    `gorm:"size:10;not null;uniqueIndex"` // UTC date of WindowEnd; a scheduler retry replaces the day's row
	WindowStart time.Time `gorm:"not null"`
	WindowEnd   time.Time `gorm:"not null;index"`
	Overall     string    `gorm:"size:16;not null"`
	Headline    string    `gorm:"size:512;not null;default:''"`
	ReportJSON  string    `gorm:"type:text"`
	Markdown    string    `gorm:"type:text"`
	CreatedAt   time.Time `gorm:"not null;index"`
}

func (HealthReportModel) TableName() string { return "health_reports" }

type HealthReport struct {
	ID          uint
	ReportDate  string
	WindowStart time.Time
	WindowEnd   time.Time
	Overall     string
	Headline    string
	ReportJSON  string
	Markdown    string
	CreatedAt   time.Time
}

// SaveHealthReport stores one report per UTC day; a second run for the same
// day (a scheduler retry, a manual rerun) replaces the first.
func (g *GormDB) SaveHealthReport(r *HealthReport) error {
	if r.ReportDate == "" {
		// The date of the window's midpoint, so a schedule near midnight UTC
		// still yields one row per day whichever side of it the job lands.
		r.ReportDate = r.WindowStart.Add(r.WindowEnd.Sub(r.WindowStart) / 2).UTC().Format("2006-01-02")
	}
	m := HealthReportModel{ReportDate: r.ReportDate, WindowStart: r.WindowStart, WindowEnd: r.WindowEnd, Overall: r.Overall, Headline: r.Headline,
		ReportJSON: r.ReportJSON, Markdown: r.Markdown, CreatedAt: r.CreatedAt}
	err := g.db.Clauses(clause.OnConflict{
		Columns:   []clause.Column{{Name: "report_date"}},
		DoUpdates: clause.AssignmentColumns([]string{"window_start", "window_end", "overall", "headline", "report_json", "markdown", "created_at"}),
	}).Create(&m).Error
	if err != nil {
		return err
	}
	var saved HealthReportModel
	if err := g.db.Where("report_date = ?", r.ReportDate).First(&saved).Error; err != nil {
		return err
	}
	r.ID = saved.ID
	return nil
}

func (g *GormDB) ListHealthReports(limit int) ([]HealthReport, error) {
	var models []HealthReportModel
	if err := g.db.Order("created_at DESC, id DESC").Limit(limit).Find(&models).Error; err != nil {
		return nil, err
	}
	out := make([]HealthReport, 0, len(models))
	for _, m := range models {
		out = append(out, HealthReport(m))
	}
	return out, nil
}

type countRow struct {
	Key   string
	Count int
}

func toMap(rows []countRow) map[string]int {
	out := make(map[string]int, len(rows))
	for _, r := range rows {
		out[r.Key] = r.Count
	}
	return out
}

// HealthMetrics gathers the day's numbers for health.Evaluate. Every query is
// plain SQL that runs on both dialects; percentiles are left to the evaluator.
// budget returns how long a live run may take given its own configured agent
// wall clock (zero when the row has none), including pipeline overhead.
func (g *GormDB) HealthMetrics(start, end, now time.Time, budget func(agentWallClockSec int) time.Duration) (health.Metrics, error) {
	m := health.Metrics{WindowStart: start, WindowEnd: end, Now: now}
	var rows []countRow

	inWindow := g.db.Model(&ReviewRunModel{}).Where("completed_at >= ? AND completed_at < ?", start, end).Session(&gorm.Session{})
	if err := inWindow.Select("status AS key, count(*) AS count").Group("status").Scan(&rows).Error; err != nil {
		return m, err
	}
	m.Runs.ByStatus = toMap(rows)
	for _, n := range m.Runs.ByStatus {
		m.Runs.Total += n
	}
	rows = nil
	if err := inWindow.Select("trigger_source AS key, count(*) AS count").Group("trigger_source").Scan(&rows).Error; err != nil {
		return m, err
	}
	m.Runs.ByTrigger = toMap(rows)
	rows = nil
	if err := inWindow.Where("terminal_code <> ''").Select("terminal_code AS key, count(*) AS count").Group("terminal_code").Scan(&rows).Error; err != nil {
		return m, err
	}
	m.Runs.ByTerminalCode = toMap(rows)
	rows = nil
	if err := inWindow.Where("verdict <> ''").Select("verdict AS key, count(*) AS count").Group("verdict").Scan(&rows).Error; err != nil {
		return m, err
	}
	m.Runs.Verdicts = toMap(rows)
	if err := inWindow.Where("status = ?", ReviewRunStatusCompleted).Pluck("duration_ms", &m.Runs.DurationsMS).Error; err != nil {
		return m, err
	}
	var n int64
	if err := inWindow.Where("model_fallback").Count(&n).Error; err != nil {
		return m, err
	}
	m.Runs.ModelFallbacks = int(n)
	var criticals *int
	if err := inWindow.Select("sum(critical_count)").Scan(&criticals).Error; err != nil {
		return m, err
	}
	if criticals != nil {
		m.Runs.Criticals = *criticals
	}

	rows = nil
	if err := g.db.Model(&ReviewStageAttemptModel{}).
		Where("status = 'failed' AND COALESCE(completed_at, started_at) >= ? AND COALESCE(completed_at, started_at) < ?", start, end).
		Select("stage || '/' || error_code AS key, count(*) AS count").Group("stage, error_code").Scan(&rows).Error; err != nil {
		return m, err
	}
	m.Attempts = toMap(rows)

	// The live set is small; ages are computed in Go so both dialects agree.
	var live []ReviewRunModel
	if err := g.db.Where("status IN ?", []string{ReviewRunStatusQueued, ReviewRunStatusRunning}).
		Select("status, queued_at, started_at, agent_wall_clock_sec").Find(&live).Error; err != nil {
		return m, err
	}
	for _, run := range live {
		// A requeued run can keep an old started_at; a queued row is aged from
		// when it was queued, a running one from when it started.
		since := run.QueuedAt
		if run.Status == ReviewRunStatusRunning && run.StartedAt != nil {
			since = *run.StartedAt
		}
		age := now.Sub(since)
		if run.Status == ReviewRunStatusQueued {
			m.Queue.Queued++
			if age > m.Queue.OldestQueuedAge {
				m.Queue.OldestQueuedAge = age
			}
		} else {
			m.Queue.Running++
			if age > m.Queue.OldestRunningAge {
				m.Queue.OldestRunningAge = age
			}
			if budget != nil {
				if allowed := budget(run.AgentWallClockSec); allowed > 0 && age > allowed {
					m.Queue.RunningOverBudget++
					if age > 2*allowed {
						m.Queue.RunningOverTwiceBudget++
					}
				}
			}
		}
	}

	published := g.db.Model(&PublishedFindingModel{}).Where("published_at >= ? AND published_at < ?", start, end).Session(&gorm.Session{})
	if err := published.Where("kind = ?", PublishedKindSummary).Count(&n).Error; err != nil {
		return m, err
	}
	m.Publish.Summaries = int(n)
	if err := published.Where("kind = ? AND comment_id <> 0", PublishedKindFinding).Count(&n).Error; err != nil {
		return m, err
	}
	m.Publish.Inline = int(n)
	if err := published.Where("kind = ?", PublishedKindAnnotation).Count(&n).Error; err != nil {
		return m, err
	}
	m.Publish.Annotations = int(n)
	if err := g.db.Model(&PublishedFindingModel{}).Where("state = ?", PublishedStateDismissed).Count(&n).Error; err != nil {
		return m, err
	}
	m.Publish.Dismissed = int(n)

	replies := g.db.Model(&PublishedReplyModel{}).Where("processed_at >= ? AND processed_at < ?", start, end).Session(&gorm.Session{})
	if err := replies.Count(&n).Error; err != nil {
		return m, err
	}
	m.Replies.Handled = int(n)
	for col, target := range map[string]*map[string]int{"class": &m.Replies.ByClass, "action": &m.Replies.ByAction, "outcome": &m.Replies.ByOutcome, "decision": &m.Replies.ByDecision} {
		rows = nil
		if err := replies.Where(col + " <> ''").Select(col + " AS key, count(*) AS count").Group(col).Scan(&rows).Error; err != nil {
			return m, err
		}
		*target = toMap(rows)
	}
	// Posting and giving up happen on later scans than ingestion; count them
	// when they happened.
	if err := g.db.Model(&PublishedReplyModel{}).Where("reply_comment_id <> 0 AND replied_at >= ? AND replied_at < ?", start, end).Count(&n).Error; err != nil {
		return m, err
	}
	m.Replies.TextPosted = int(n)
	// A text step that was started (claimed, attempted or decided) and has
	// seen no activity for an hour is stuck, on a PR the reply scan still
	// visits: open and not draft. Rows the step never touched, as under react
	// mode, are not, and neither are rows on PRs the scan can no longer reach.
	if err := g.db.Table("published_reply_models AS r").
		Joins("JOIN prs ON prs.repo_owner = r.repo_owner AND prs.repo_name = r.repo_name AND prs.pr_number = r.pr_number").
		Where("r.outcome = '' AND (r.attempts > 0 OR r.decision <> '' OR r.claimed_at IS NOT NULL) AND COALESCE(r.updated_at, r.processed_at) < ? AND LOWER(prs.pr_state) = 'open' AND NOT prs.draft", now.Add(-time.Hour)).
		Count(&n).Error; err != nil {
		return m, err
	}
	m.Replies.StuckPending = int(n)
	if err := g.db.Model(&PublishedReplyModel{}).Where("outcome = 'failed' AND COALESCE(updated_at, processed_at) >= ? AND COALESCE(updated_at, processed_at) < ?", start, end).Count(&n).Error; err != nil {
		return m, err
	}
	m.Replies.Failed = int(n)
	unlinked, err := g.ListUnlinkedPublishedFindings()
	if err != nil {
		return m, err
	}
	m.Replies.UnlinkedRoots = len(unlinked)

	rows = nil
	if err := g.db.Model(&TelemetryEventModel{}).Where("created_at >= ? AND created_at < ? AND (action LIKE 'reply\\_%' ESCAPE '\\' OR action = 'agent_model_fallback')", start, end).
		Select("action AS key, count(*) AS count").Group("action").Scan(&rows).Error; err != nil {
		return m, err
	}
	m.Telemetry = toMap(rows)

	var lease PollerLeaseModel
	if err := g.db.Order("expires_at DESC").Limit(1).Find(&lease).Error; err != nil {
		return m, err
	}
	if lease.Holder != "" {
		m.Lease = health.LeaseMetrics{Present: true, Holder: lease.Holder, ExpiresAt: lease.ExpiresAt}
	}

	if err := g.db.Model(&PRModel{}).Where("error_message <> '' AND error_message IS NOT NULL").Count(&n).Error; err != nil {
		return m, err
	}
	m.PRErrors = int(n)
	return m, nil
}
