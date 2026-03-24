package db

import (
	"fmt"
	"time"
)

// CreateTelemetryEvents batch-inserts telemetry events
func (g *GormDB) CreateTelemetryEvents(events []TelemetryEvent) error {
	if len(events) == 0 {
		return nil
	}

	models := make([]TelemetryEventModel, len(events))
	for i, e := range events {
		models[i] = TelemetryEventModel{
			UserID:   uint(e.UserID),
			Action:   e.Action,
			Label:    e.Label,
			PROwner:  e.PROwner,
			PRRepo:   e.PRRepo,
			PRNumber: e.PRNumber,
		}
	}

	return g.db.Create(&models).Error
}

// GetTelemetryStats returns aggregated telemetry statistics for the given number of days
func (g *GormDB) GetTelemetryStats(days int) (*TelemetryStats, error) {
	since := time.Now().AddDate(0, 0, -days)
	stats := &TelemetryStats{}

	// Total events
	var totalEvents int64
	if err := g.db.Model(&TelemetryEventModel{}).Where("created_at >= ?", since).Count(&totalEvents).Error; err != nil {
		return nil, fmt.Errorf("count events: %w", err)
	}
	stats.TotalEvents = int(totalEvents)

	// Active users
	var activeUsers int64
	if err := g.db.Model(&TelemetryEventModel{}).Where("created_at >= ?", since).Distinct("user_id").Count(&activeUsers).Error; err != nil {
		return nil, fmt.Errorf("count users: %w", err)
	}
	stats.ActiveUsers = int(activeUsers)

	// Events by action
	var byAction []struct {
		Action string
		Count  int
	}
	if err := g.db.Model(&TelemetryEventModel{}).
		Select("action, count(*) as count").
		Where("created_at >= ?", since).
		Group("action").
		Order("count desc").
		Scan(&byAction).Error; err != nil {
		return nil, fmt.Errorf("by action: %w", err)
	}
	stats.ByAction = make([]ActionCount, len(byAction))
	for i, a := range byAction {
		stats.ByAction[i] = ActionCount{Action: a.Action, Count: a.Count}
	}

	// Events by day
	var byDay []struct {
		Date  string
		Count int
	}
	if err := g.db.Model(&TelemetryEventModel{}).
		Select("date(created_at) as date, count(*) as count").
		Where("created_at >= ?", since).
		Group("date(created_at)").
		Order("date asc").
		Scan(&byDay).Error; err != nil {
		return nil, fmt.Errorf("by day: %w", err)
	}
	stats.ByDay = make([]DayCount, len(byDay))
	for i, d := range byDay {
		stats.ByDay[i] = DayCount{Date: d.Date, Count: d.Count}
	}

	// Top searches (label for action="search")
	var topSearches []struct {
		Label string
		Count int
	}
	if err := g.db.Model(&TelemetryEventModel{}).
		Select("label, count(*) as count").
		Where("created_at >= ? AND action = ? AND label != ''", since, "search").
		Group("label").
		Order("count desc").
		Limit(10).
		Scan(&topSearches).Error; err != nil {
		return nil, fmt.Errorf("top searches: %w", err)
	}
	stats.TopSearches = make([]LabelCount, len(topSearches))
	for i, s := range topSearches {
		stats.TopSearches[i] = LabelCount{Label: s.Label, Count: s.Count}
	}

	// Top interacted PRs
	var topPRs []struct {
		PROwner  string `gorm:"column:pr_owner"`
		PRRepo   string `gorm:"column:pr_repo"`
		PRNumber int    `gorm:"column:pr_number"`
		Count    int
	}
	if err := g.db.Model(&TelemetryEventModel{}).
		Select("pr_owner, pr_repo, pr_number, count(*) as count").
		Where("created_at >= ? AND pr_number > 0", since).
		Group("pr_owner, pr_repo, pr_number").
		Order("count desc").
		Limit(10).
		Scan(&topPRs).Error; err != nil {
		return nil, fmt.Errorf("top prs: %w", err)
	}
	stats.TopPRs = make([]PRInteractionCount, len(topPRs))
	for i, p := range topPRs {
		stats.TopPRs[i] = PRInteractionCount{
			Owner:  p.PROwner,
			Repo:   p.PRRepo,
			Number: p.PRNumber,
			Count:  p.Count,
		}
	}

	return stats, nil
}
