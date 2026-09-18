package server

import (
	"strconv"
	"strings"
	"time"

	"pr-review-server/pkg/publisher"
	"pr-review-server/pkg/reviewer/runconfig"
	"pr-review-server/poller"
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
	// settingAutoReviewReadyPRs turns on automatic, published reviews for
	// allowlisted authors' PRs when they become ready, open ready, or get a push.
	settingAutoReviewReadyPRs = "auto_review_ready_prs"

	defaultPublishInlineCap         = publisher.DefaultInlineCap
	defaultPublishInlineMinSeverity = "medium"
	defaultPublishReplyMode         = "off"
	defaultPublishShowUnverified    = true
)

var publishSeverities = map[string]bool{"critical": true, "medium": true, "low": true}

// publishReplyModes: off does nothing, observe records author replies to our
// inline comments, react also acknowledges each with a 👍, shadow also runs
// the reply model and records what it would say, respond posts it.
var publishReplyModes = map[string]bool{"off": true, "observe": true, "react": true, "shadow": true, "respond": true}

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

	autoReviewReady := false
	if v, err := s.db.GetSetting(settingAutoReviewReadyPRs); err == nil {
		if b, convErr := strconv.ParseBool(strings.TrimSpace(v)); convErr == nil {
			autoReviewReady = b
		}
	}
	response[settingAutoReviewReadyPRs] = autoReviewReady

	rawProfiles, _ := s.db.GetSetting(poller.SettingAutoReviewProfileByTrigger)
	profilePolicy, err := poller.ParseAutoReviewProfilePolicy(rawProfiles)
	if err != nil {
		profilePolicy, _ = poller.ParseAutoReviewProfilePolicy("")
	}
	response[poller.SettingAutoReviewProfileByTrigger] = profilePolicy.Map()
	liteAuthors, _ := s.db.GetSetting(poller.SettingAutoReviewLiteAuthors)
	response[poller.SettingAutoReviewLiteAuthors] = liteAuthors
	response["review_default_profile"] = s.defaultReviewProfile()
	response["review_profiles"] = runconfig.Profiles()
}

// defaultReviewProfile is what "default" resolves to for the settings page:
// the poller's admitted default (the configured profile unless policy rejects
// it), falling back to the raw setting when no poller is attached.
func (s *Server) defaultReviewProfile() string {
	if s.poller != nil {
		if _, policy, err := s.poller.ReviewConfigDefaultsAndPolicy(); err == nil {
			return runconfig.NormalizeProfile(policy.DefaultProfile)
		}
	}
	if s.cfg == nil {
		return runconfig.ProfileFull
	}
	profile := runconfig.NormalizeProfile(s.cfg.ReviewDefaultProfile)
	if !runconfig.KnownProfile(profile) {
		return runconfig.ProfileFull
	}
	return profile
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
