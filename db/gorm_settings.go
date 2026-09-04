package db

import (
	"fmt"

	"gorm.io/gorm"
)

// GetSetting retrieves a setting value by key
func (g *GormDB) GetSetting(key string) (string, error) {
	var setting SettingModel
	result := g.db.Where("key = ?", key).First(&setting)

	if result.Error == gorm.ErrRecordNotFound {
		return "", nil
	}
	if result.Error != nil {
		return "", result.Error
	}

	return setting.Value, nil
}

// SetSetting sets a setting value
func (g *GormDB) SetSetting(key, value string) error {
	setting := SettingModel{Key: key, Value: value}

	// Use FirstOrCreate with Assign to update if exists
	return g.db.Where("key = ?", key).Assign(SettingModel{Value: value}).FirstOrCreate(&setting).Error
}

// GetAutoReviewRequestedPRs returns whether to automatically review requested PRs
func (g *GormDB) GetAutoReviewRequestedPRs() (bool, error) {
	value, err := g.GetSetting("auto_review_requested_prs")
	if err != nil {
		return false, err
	}
	// initDefaultSettings seeds this key to "false", so an empty value only
	// happens when that seed row is missing.
	if value == "" {
		return true, nil
	}
	return value == "true", nil
}

// SetAutoReviewRequestedPRs sets whether to automatically review requested PRs
func (g *GormDB) SetAutoReviewRequestedPRs(enabled bool) error {
	value := "false"
	if enabled {
		value = "true"
	}
	return g.SetSetting("auto_review_requested_prs", value)
}

// GetReviewNRequests returns the number of review requests to make (default 3)
func (g *GormDB) GetReviewNRequests() (int, error) {
	value, err := g.GetSetting("review_n_requests")
	if err != nil || value == "" {
		return 3, nil
	}
	var n int
	_, err = fmt.Sscanf(value, "%d", &n)
	if err != nil {
		return 3, nil
	}
	return n, nil
}

// SetReviewNRequests sets the number of review requests
func (g *GormDB) SetReviewNRequests(n int) error {
	return g.SetSetting("review_n_requests", fmt.Sprintf("%d", n))
}

// GetGenerateHTML returns whether to generate HTML reports (default true)
func (g *GormDB) GetGenerateHTML() (bool, error) {
	value, err := g.GetSetting("generate_html")
	if err != nil {
		return true, err // Default true on error
	}
	if value == "" {
		return true, nil
	}
	return value == "true", nil
}

// SetGenerateHTML sets whether to generate HTML reports
func (g *GormDB) SetGenerateHTML(enabled bool) error {
	value := "false"
	if enabled {
		value = "true"
	}
	return g.SetSetting("generate_html", value)
}
