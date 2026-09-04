package server

import (
	"strconv"
	"strings"

	"pr-review-server/pkg/publisher"
)

// GitHub publication settings, shared with the poller by key name. Defaults
// mean "post nothing": the pilot widens author by author via PATCH.
const (
	settingPublishEnabledAuthors    = "publish_enabled_authors"
	settingPublishInlineCap         = "publish_inline_cap"
	settingPublishInlineMinSeverity = "publish_inline_min_severity"

)

var publishSeverities = map[string]bool{"critical": true, "medium": true, "low": true}

func (s *Server) addPublishSettings(response map[string]interface{}) {
	authors, _ := s.db.GetSetting(settingPublishEnabledAuthors)
	response[settingPublishEnabledAuthors] = authors

	cap := publisher.DefaultInlineCap
	if v, err := s.db.GetSetting(settingPublishInlineCap); err == nil {
		if n, convErr := strconv.Atoi(strings.TrimSpace(v)); convErr == nil && n >= 0 {
			cap = n
		}
	}
	response[settingPublishInlineCap] = cap

	sev := publisher.DefaultInlineMinSeverity
	if v, err := s.db.GetSetting(settingPublishInlineMinSeverity); err == nil && publishSeverities[strings.TrimSpace(v)] {
		sev = strings.TrimSpace(v)
	}
	response[settingPublishInlineMinSeverity] = sev
}
