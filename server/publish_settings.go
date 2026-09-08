package server

import (
	"strconv"
	"strings"
)

// GitHub publication settings, shared with the poller by key name. Defaults
// mean "post nothing": the pilot widens author by author via PATCH.
const (
	settingPublishEnabledAuthors    = "publish_enabled_authors"
	settingPublishInlineCap         = "publish_inline_cap"
	settingPublishInlineMinSeverity = "publish_inline_min_severity"
	settingPublishReplyMode         = "publish_reply_mode"

	defaultPublishInlineCap         = 5
	defaultPublishInlineMinSeverity = "medium"
	defaultPublishReplyMode         = "off"
)

var publishSeverities = map[string]bool{"critical": true, "medium": true, "low": true}

// publishReplyModes: off does nothing, observe records author replies to our
// inline comments, react also acknowledges each with a 👍.
var publishReplyModes = map[string]bool{"off": true, "observe": true, "react": true}

func (s *Server) addPublishSettings(response map[string]interface{}) {
	authors, _ := s.db.GetSetting(settingPublishEnabledAuthors)
	response[settingPublishEnabledAuthors] = authors

	cap := defaultPublishInlineCap
	if v, err := s.db.GetSetting(settingPublishInlineCap); err == nil {
		if n, convErr := strconv.Atoi(strings.TrimSpace(v)); convErr == nil && n >= 0 {
			cap = n
		}
	}
	response[settingPublishInlineCap] = cap

	sev := defaultPublishInlineMinSeverity
	if v, err := s.db.GetSetting(settingPublishInlineMinSeverity); err == nil && publishSeverities[strings.TrimSpace(v)] {
		sev = strings.TrimSpace(v)
	}
	response[settingPublishInlineMinSeverity] = sev

	mode := defaultPublishReplyMode
	if v, err := s.db.GetSetting(settingPublishReplyMode); err == nil && publishReplyModes[strings.TrimSpace(v)] {
		mode = strings.TrimSpace(v)
	}
	response[settingPublishReplyMode] = mode
}
