package server

import (
	"pr-review-server/pkg/publisher"
	"strconv"
	"strings"
	"time"
)

// GitHub publication settings, shared with the poller by key name. Defaults
// mean "post nothing": the pilot widens author by author via PATCH.
const (
	settingPublishEnabledAuthors    = "publish_enabled_authors"
	settingPublishInlineCap         = "publish_inline_cap"
	settingPublishInlineMinSeverity = "publish_inline_min_severity"
	settingPublishReplyMode         = "publish_reply_mode"
	settingPublishReplyEnabledAt    = "publish_reply_enabled_at"
	settingPublishShowUnverified    = "publish_show_unverified"

	defaultPublishInlineCap         = publisher.DefaultInlineCap
	defaultPublishInlineMinSeverity = "medium"
	defaultPublishReplyMode         = "off"
	defaultPublishShowUnverified    = true
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

	response[settingPublishReplyMode] = s.publishReplyMode()
	enabledAt, _ := s.db.GetSetting(settingPublishReplyEnabledAt)
	response[settingPublishReplyEnabledAt] = strings.TrimSpace(enabledAt)

	showUnverified := defaultPublishShowUnverified
	if v, err := s.db.GetSetting(settingPublishShowUnverified); err == nil {
		if b, convErr := strconv.ParseBool(strings.TrimSpace(v)); convErr == nil {
			showUnverified = b
		}
	}
	response[settingPublishShowUnverified] = showUnverified
}

// publishReplyMode reads the stored mode the way the poller does: trimmed and
// lowercased, falling back to off for anything unknown.
func (s *Server) publishReplyMode() string {
	v, err := s.db.GetSetting(settingPublishReplyMode)
	if err != nil {
		return defaultPublishReplyMode
	}
	if normalized := strings.ToLower(strings.TrimSpace(v)); publishReplyModes[normalized] {
		return normalized
	}
	return defaultPublishReplyMode
}

// replyActivationFor returns the activation stamp to store alongside a reply
// mode change: now when leaving off, empty when returning to off, and no
// change otherwise. Replies older than the stamp are never acknowledged, so
// re-enabling after a pause does not burst reactions for the gap.
func (s *Server) replyActivationFor(newMode string) (stamp string, change bool, err error) {
	current, err := s.db.GetSetting(settingPublishReplyMode)
	if err != nil {
		return "", false, err
	}
	current = strings.TrimSpace(current)
	wasOff := current == "" || current == defaultPublishReplyMode
	switch {
	case newMode == defaultPublishReplyMode:
		return "", true, nil
	case wasOff:
		return time.Now().UTC().Truncate(time.Second).Format(time.RFC3339), true, nil
	}
	return "", false, nil
}
