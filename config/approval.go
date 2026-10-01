package config

import (
	"encoding/json"
	"os"
	"strconv"

	"pr-review-server/pkg/approval"
)

type ApprovalProviderIdentity struct {
	Provider string `json:"provider"`
	ActorID  int64  `json:"actor_id"`
	AppID    int64  `json:"app_id,omitempty"`
}

type ApprovalConfig struct {
	Enabled           bool
	Provider          string
	Model             string
	ReasoningTokens   int
	APIKey            string
	CacheRoot         string
	DailyInput        int64
	DailyOutput       int64
	Identities        []ApprovalProviderIdentity
	UnavailableReason string
}

func (c *Config) ApprovalCandidates() ApprovalConfig {
	a := ApprovalConfig{Enabled: os.Getenv("APPROVAL_CANDIDATES_ENABLED") == "true", Provider: os.Getenv("APPROVAL_CANDIDATES_PROVIDER"), Model: os.Getenv("APPROVAL_CANDIDATES_MODEL"), CacheRoot: os.Getenv("APPROVAL_CANDIDATES_CACHE_DIR")}
	a.DailyInput, _ = strconv.ParseInt(os.Getenv("APPROVAL_CANDIDATES_DAILY_INPUT_TOKENS"), 10, 64)
	a.DailyOutput, _ = strconv.ParseInt(os.Getenv("APPROVAL_CANDIDATES_DAILY_OUTPUT_TOKENS"), 10, 64)
	a.ReasoningTokens = approval.DefaultReasoningTokens
	if raw := os.Getenv("APPROVAL_CANDIDATES_REASONING_TOKENS"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 0 || n > approval.CallOutputTokens/2 {
			a.UnavailableReason = "Invalid approval reasoning token budget"
		}
		a.ReasoningTokens = n
	}
	if a.CacheRoot == "" {
		a.CacheRoot = "data/approval-cache"
	}
	switch a.Provider {
	case "anthropic":
		a.APIKey = c.AnthropicAPIKey
	case "openrouter":
		a.APIKey = c.OpenRouterAPIKey
	default:
		a.UnavailableReason = "Configure an approval investigation provider"
	}
	if a.Model == "" {
		a.UnavailableReason = "Configure an approval investigation model"
	}
	if a.APIKey == "" {
		a.UnavailableReason = "The selected provider requires an API key"
	}
	if (a.DailyInput > 0 && a.DailyInput < approval.TargetInputTokens) || (a.DailyOutput > 0 && a.DailyOutput < approval.TargetOutputTokens) {
		a.UnavailableReason = "Daily token budgets, when set, must admit one investigation"
	}
	if raw := os.Getenv("APPROVAL_CANDIDATES_PROVIDER_IDENTITIES"); raw != "" {
		if err := json.Unmarshal([]byte(raw), &a.Identities); err != nil {
			a.UnavailableReason = "Invalid approval provider identities"
		}
		for _, identity := range a.Identities {
			if identity.AppID != 0 {
				a.UnavailableReason = "Review artifacts do not expose app identity; configure verified actor IDs"
			}
			if identity.ActorID <= 0 || (identity.Provider != "greptile" && identity.Provider != "copilot") {
				a.UnavailableReason = "Invalid approval provider identities"
			}
		}
	}
	return a
}
