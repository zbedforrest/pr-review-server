package poller

import (
	"encoding/json"
	"fmt"
	"log"
	"sort"
	"strings"

	"pr-review-server/pkg/reviewer/runconfig"
)

// Automatic reviews pick their profile per trigger: the deployment default,
// optionally overridden per trigger and per repo by the
// auto_review_profile_by_trigger setting, then gated per author by
// auto_review_lite_authors so lite can roll out one author at a time. An
// empty author list sends everyone to full, which is also the kill switch.
const (
	SettingAutoReviewProfileByTrigger = "auto_review_profile_by_trigger"
	SettingAutoReviewLiteAuthors      = "auto_review_lite_authors"
)

// AutoReviewTriggers lists the intent triggers a profile may be mapped to.
var AutoReviewTriggers = []string{"ready_for_review", "opened", "synchronize", "poll_fallback"}

// AutoReviewProfilePolicy is the parsed auto_review_profile_by_trigger
// setting: per-trigger profiles and per-repo ("owner/repo") overrides of them.
type AutoReviewProfilePolicy struct {
	Triggers map[string]string
	Repos    map[string]map[string]string
}

// ParseAutoReviewProfilePolicy decodes the setting. Unknown trigger keys or
// profile names are errors; an empty value is an empty policy.
func ParseAutoReviewProfilePolicy(raw string) (AutoReviewProfilePolicy, error) {
	policy := AutoReviewProfilePolicy{Triggers: map[string]string{}, Repos: map[string]map[string]string{}}
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return policy, nil
	}
	var top map[string]json.RawMessage
	if err := json.Unmarshal([]byte(raw), &top); err != nil {
		return policy, fmt.Errorf("must be a JSON object: %w", err)
	}
	for key, value := range top {
		if key == "repos" {
			var repos map[string]map[string]string
			if err := json.Unmarshal(value, &repos); err != nil {
				return policy, fmt.Errorf("repos must map \"owner/repo\" to trigger profiles: %w", err)
			}
			for repo, triggers := range repos {
				repoKey := strings.ToLower(strings.TrimSpace(repo))
				if strings.Count(repoKey, "/") != 1 {
					return policy, fmt.Errorf("repos key %q must be owner/repo", repo)
				}
				normalized, err := normalizeTriggerProfiles(triggers)
				if err != nil {
					return policy, fmt.Errorf("repos[%s]: %w", repo, err)
				}
				if len(normalized) > 0 {
					policy.Repos[repoKey] = normalized
				}
			}
			continue
		}
		var profile string
		if err := json.Unmarshal(value, &profile); err != nil {
			return policy, fmt.Errorf("%s must be a profile name", key)
		}
		normalized, err := normalizeTriggerProfiles(map[string]string{key: profile})
		if err != nil {
			return policy, err
		}
		for trigger, name := range normalized {
			policy.Triggers[trigger] = name
		}
	}
	return policy, nil
}

// normalizeTriggerProfiles validates one trigger-to-profile map. An empty
// profile means "unset" so the settings API's GET shape, which lists every
// trigger, round-trips through PATCH.
func normalizeTriggerProfiles(triggers map[string]string) (map[string]string, error) {
	out := map[string]string{}
	for trigger, profile := range triggers {
		trigger = strings.ToLower(strings.TrimSpace(trigger))
		if !isAutoReviewTrigger(trigger) {
			return nil, fmt.Errorf("unknown trigger %q (want %s)", trigger, strings.Join(AutoReviewTriggers, ", "))
		}
		if strings.TrimSpace(profile) == "" {
			continue
		}
		name := runconfig.NormalizeProfile(profile)
		if !runconfig.KnownProfile(name) {
			return nil, fmt.Errorf("%s: unknown profile %q (want %s)", trigger, profile, strings.Join(runconfig.Profiles(), ", "))
		}
		out[trigger] = name
	}
	return out, nil
}

func isAutoReviewTrigger(trigger string) bool {
	for _, known := range AutoReviewTriggers {
		if known == trigger {
			return true
		}
	}
	return false
}

// ProfileFor resolves the profile for one trigger: the repo override, then
// the trigger mapping, then the deployment default.
func (p AutoReviewProfilePolicy) ProfileFor(trigger, owner, repo, defaultProfile string) string {
	trigger = strings.ToLower(strings.TrimSpace(trigger))
	if repoTriggers, ok := p.Repos[strings.ToLower(owner+"/"+repo)]; ok {
		if profile, ok := repoTriggers[trigger]; ok {
			return profile
		}
	}
	if profile, ok := p.Triggers[trigger]; ok {
		return profile
	}
	return defaultProfile
}

// JSON renders the policy canonically (sorted keys, no defaults filled in) so
// the setting round-trips exactly.
func (p AutoReviewProfilePolicy) JSON() string {
	if len(p.Triggers) == 0 && len(p.Repos) == 0 {
		return ""
	}
	out := make(map[string]any, len(p.Triggers)+1)
	for trigger, profile := range p.Triggers {
		out[trigger] = profile
	}
	if len(p.Repos) > 0 {
		out["repos"] = p.Repos
	}
	body, err := json.Marshal(out)
	if err != nil {
		return ""
	}
	return string(body)
}

// Map is the settings-API shape: one key per trigger (empty when unset) plus repos.
func (p AutoReviewProfilePolicy) Map() map[string]any {
	out := map[string]any{}
	for _, trigger := range AutoReviewTriggers {
		out[trigger] = p.Triggers[trigger]
	}
	repos := map[string]map[string]string{}
	keys := make([]string, 0, len(p.Repos))
	for repo := range p.Repos {
		keys = append(keys, repo)
	}
	sort.Strings(keys)
	for _, repo := range keys {
		repos[repo] = p.Repos[repo]
	}
	out["repos"] = repos
	return out
}

// autoReviewProfileFor picks the profile for an automatic review. Lite and
// lite_plus require the author to be on the lite allowlist; anything
// unparseable in the settings falls back to the deployment default.
func (p *Poller) autoReviewProfileFor(trigger, owner, repo, author string) string {
	defaultProfile := runconfig.ProfileFull
	if _, policy, err := p.ReviewConfigDefaultsAndPolicy(); err == nil {
		defaultProfile = runconfig.NormalizeProfile(policy.DefaultProfile)
	}
	profile := defaultProfile
	raw, err := p.db.GetSetting(SettingAutoReviewProfileByTrigger)
	if err != nil {
		log.Printf("[AUTO-REVIEW] read %s failed (%v); using %s", SettingAutoReviewProfileByTrigger, err, defaultProfile)
	} else if policy, parseErr := ParseAutoReviewProfilePolicy(raw); parseErr != nil {
		log.Printf("[AUTO-REVIEW] %s is invalid (%v); using %s", SettingAutoReviewProfileByTrigger, parseErr, defaultProfile)
	} else {
		profile = policy.ProfileFor(trigger, owner, repo, defaultProfile)
	}
	if profile == runconfig.ProfileFull {
		return profile
	}
	liteAuthors, err := p.db.GetSetting(SettingAutoReviewLiteAuthors)
	if err != nil {
		log.Printf("[AUTO-REVIEW] read %s failed (%v); %s falls back to full", SettingAutoReviewLiteAuthors, err, author)
		return runconfig.ProfileFull
	}
	if !publishEnabledFor(author, liteAuthors) {
		return runconfig.ProfileFull
	}
	return profile
}
