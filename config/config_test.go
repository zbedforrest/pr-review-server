package config

import (
	"reflect"
	"testing"
)

// TestLoad_RequiredChecksFlag locks in the default-off contract: the
// required-checks feature only activates on the exact string "true".
func TestLoad_RequiredChecksFlag(t *testing.T) {
	cases := []struct {
		name string
		env  string
		want bool
	}{
		{"unset defaults off", "", false},
		{"true enables", "true", true},
		{"other values stay off", "1", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Setenv("REQUIRED_CHECKS", c.env)
			if got := Load().RequiredChecks; got != c.want {
				t.Errorf("REQUIRED_CHECKS=%q: got %v want %v", c.env, got, c.want)
			}
		})
	}
}

func TestLoadAgentBackendDefaultsToClaude(t *testing.T) {
	t.Setenv("AGENT_BACKEND", "")
	t.Setenv("OPENROUTER_BASE_URL", "")
	cfg := Load()
	if cfg.AgentBackend != "claude" {
		t.Errorf("AgentBackend=%q want claude", cfg.AgentBackend)
	}
	if cfg.OpenRouterBaseURL != "" {
		t.Errorf("OpenRouterBaseURL=%q want empty service default", cfg.OpenRouterBaseURL)
	}
}

func TestLoadOpenRouterAgentConfig(t *testing.T) {
	t.Setenv("AGENT_BACKEND", "openrouter")
	t.Setenv("AGENT_MAX_TURNS", "")
	t.Setenv("REVIEW_MAX_TURNS", "")
	t.Setenv("AGENT_MODEL", "openai/gpt-5.6-sol")
	t.Setenv("AGENT_EFFORT", "xhigh")
	t.Setenv("OPENROUTER_API_KEY", "sk-or-test")
	t.Setenv("OPENROUTER_BASE_URL", "https://router.example/v1")
	cfg := Load()
	if cfg.AgentBackend != "openrouter" || cfg.AgentModel != "openai/gpt-5.6-sol" || cfg.AgentEffort != "xhigh" {
		t.Errorf("agent config: backend=%q model=%q effort=%q", cfg.AgentBackend, cfg.AgentModel, cfg.AgentEffort)
	}
	if cfg.OpenRouterBaseURL != "https://router.example/v1" {
		t.Errorf("OpenRouterBaseURL=%q", cfg.OpenRouterBaseURL)
	}
	if cfg.OpenRouterAPIKey != "sk-or-test" {
		t.Error("OpenRouterAPIKey was not loaded")
	}
	if cfg.AgentMaxTurns != defaultOpenRouterAgentMaxTurns || cfg.ReviewMaxTurns != 0 {
		t.Errorf("OpenRouter turn defaults: active=%d ceiling=%d", cfg.AgentMaxTurns, cfg.ReviewMaxTurns)
	}
	if cfg.ReviewMaxTurnsConfigured {
		t.Error("unset REVIEW_MAX_TURNS was reported as operator-configured")
	}
}

func TestLoadFreezesAgentCredentials(t *testing.T) {
	t.Setenv("ANTHROPIC_API_KEY", "sk-ant-test")
	t.Setenv("OPENROUTER_API_KEY", "sk-or-test")
	cfg := Load()
	t.Setenv("ANTHROPIC_API_KEY", "changed")
	t.Setenv("OPENROUTER_API_KEY", "changed")
	if cfg.AnthropicAPIKey != "sk-ant-test" || cfg.OpenRouterAPIKey != "sk-or-test" {
		t.Fatalf("credentials were not frozen at load: anthropic=%q openrouter=%q", cfg.AnthropicAPIKey, cfg.OpenRouterAPIKey)
	}
}

func TestLoadReviewCustomizationPolicyDefaults(t *testing.T) {
	for _, key := range []string{
		"AGENT_BACKEND",
		"AGENT_MODEL",
		"AGENT_EFFORT",
		"AGENT_WALL_CLOCK_SEC",
		"AGENT_MAX_TURNS",
		"REVIEW_AGENT_MODELS_CLAUDE",
		"REVIEW_AGENT_MODELS_OPENROUTER",
		"REVIEW_AGENT_EFFORTS_CLAUDE",
		"REVIEW_AGENT_EFFORTS_OPENROUTER",
		"REVIEW_MAX_WALL_CLOCK_SEC",
		"REVIEW_MAX_TURNS",
		"REVIEW_MAX_FIRST_PASS_SAMPLES",
		"REVIEW_MAX_FIRST_PASS_CONCURRENT",
	} {
		t.Setenv(key, "")
	}

	cfg := Load()
	assertStringsEqual(t, cfg.ReviewAgentModelsClaude, []string{"claude-opus-4-8", "claude-fable-5"})
	assertStringsEqual(t, cfg.ReviewAgentModelsOpenRouter, []string{"openai/gpt-5.6-sol"})
	assertStringsEqual(t, cfg.ReviewAgentEffortsClaude, []string{"low", "medium", "high"})
	assertStringsEqual(t, cfg.ReviewAgentEffortsOpenRouter, []string{"low", "medium", "high", "xhigh", "max"})
	if cfg.ReviewMaxWallClockSec != defaultAgentWallClockSec || cfg.ReviewMaxTurns != 0 || cfg.ReviewMaxFirstPassSamples != defaultReviewFirstPassSamples {
		t.Fatalf("review limits: wall=%d turns=%d samples=%d", cfg.ReviewMaxWallClockSec, cfg.ReviewMaxTurns, cfg.ReviewMaxFirstPassSamples)
	}
	if cfg.ReviewMaxFirstPassConcurrent != defaultReviewFirstPassConcurrent {
		t.Fatalf("first-pass concurrency=%d want %d", cfg.ReviewMaxFirstPassConcurrent, defaultReviewFirstPassConcurrent)
	}
	if cfg.ReviewMaxTurnsConfigured {
		t.Error("default REVIEW_MAX_TURNS was reported as operator-configured")
	}
	// The policy is additive; it must not switch the active runtime defaults.
	if cfg.AgentBackend != "claude" || cfg.AgentModel != "" || cfg.AgentEffort != "" {
		t.Fatalf("active agent config changed: backend=%q model=%q effort=%q", cfg.AgentBackend, cfg.AgentModel, cfg.AgentEffort)
	}
}

func TestLoadReviewCustomizationPolicyNormalizesAndDeduplicatesLists(t *testing.T) {
	t.Setenv("AGENT_BACKEND", "claude")
	t.Setenv("AGENT_MODEL", "")
	t.Setenv("AGENT_EFFORT", "")
	t.Setenv("REVIEW_AGENT_MODELS_CLAUDE", " claude-fable-5,claude-opus-4-8,claude-fable-5, ,Vendor/CaseSensitive,Vendor/CaseSensitive ")
	t.Setenv("REVIEW_AGENT_MODELS_OPENROUTER", " openai/gpt-5.6-sol, anthropic/claude-opus-4.8 ,openai/gpt-5.6-sol")
	t.Setenv("REVIEW_AGENT_EFFORTS_CLAUDE", " HIGH, medium,high, low, ")
	t.Setenv("REVIEW_AGENT_EFFORTS_OPENROUTER", " XHIGH,max, Medium, xhigh ")

	cfg := Load()
	assertStringsEqual(t, cfg.ReviewAgentModelsClaude, []string{"claude-fable-5", "claude-opus-4-8", "Vendor/CaseSensitive"})
	assertStringsEqual(t, cfg.ReviewAgentModelsOpenRouter, []string{"openai/gpt-5.6-sol", "anthropic/claude-opus-4.8"})
	assertStringsEqual(t, cfg.ReviewAgentEffortsClaude, []string{"high", "medium", "low"})
	assertStringsEqual(t, cfg.ReviewAgentEffortsOpenRouter, []string{"xhigh", "max", "medium"})
}

func TestLoadReviewCustomizationPolicyIncludesActiveRuntimeValues(t *testing.T) {
	t.Setenv("AGENT_BACKEND", " OpenRouter ")
	t.Setenv("AGENT_MODEL", "vendor/frontier-experimental")
	t.Setenv("AGENT_EFFORT", "ULTRA")
	t.Setenv("REVIEW_AGENT_MODELS_OPENROUTER", "openai/gpt-5.6-sol")
	t.Setenv("REVIEW_AGENT_EFFORTS_OPENROUTER", "medium,high")

	cfg := Load()
	assertStringsEqual(t, cfg.ReviewAgentModelsOpenRouter, []string{"openai/gpt-5.6-sol", "vendor/frontier-experimental"})
	assertStringsEqual(t, cfg.ReviewAgentEffortsOpenRouter, []string{"medium", "high", "ultra"})
	if cfg.AgentBackend != " OpenRouter " || cfg.AgentModel != "vendor/frontier-experimental" || cfg.AgentEffort != "ULTRA" {
		t.Fatalf("active config was normalized or replaced: backend=%q model=%q effort=%q", cfg.AgentBackend, cfg.AgentModel, cfg.AgentEffort)
	}
}

func TestLoadReviewCustomizationPolicyLimits(t *testing.T) {
	t.Setenv("REVIEW_MAX_WALL_CLOCK_SEC", " 1800 ")
	t.Setenv("REVIEW_MAX_TURNS", "240")
	t.Setenv("REVIEW_MAX_FIRST_PASS_SAMPLES", "5")
	t.Setenv("REVIEW_MAX_FIRST_PASS_CONCURRENT", "7")

	cfg := Load()
	if cfg.ReviewMaxWallClockSec != 1800 || cfg.ReviewMaxTurns != 240 || cfg.ReviewMaxFirstPassSamples != 5 {
		t.Fatalf("review limits: wall=%d turns=%d samples=%d", cfg.ReviewMaxWallClockSec, cfg.ReviewMaxTurns, cfg.ReviewMaxFirstPassSamples)
	}
	if cfg.ReviewMaxFirstPassConcurrent != 7 {
		t.Fatalf("first-pass concurrency=%d", cfg.ReviewMaxFirstPassConcurrent)
	}
	if !cfg.ReviewMaxTurnsConfigured {
		t.Error("positive REVIEW_MAX_TURNS was not reported as operator-configured")
	}
}

func TestLoadReviewCustomizationPolicyRejectsNonPositiveOrMalformedLimits(t *testing.T) {
	invalidValues := []string{"0", "-1", "not-a-number", "1.5"}
	for _, value := range invalidValues {
		t.Run(value, func(t *testing.T) {
			t.Setenv("AGENT_WALL_CLOCK_SEC", "900")
			t.Setenv("AGENT_MAX_TURNS", "120")
			t.Setenv("REVIEW_MAX_WALL_CLOCK_SEC", value)
			t.Setenv("REVIEW_MAX_TURNS", value)
			t.Setenv("REVIEW_MAX_FIRST_PASS_SAMPLES", value)
			t.Setenv("REVIEW_MAX_FIRST_PASS_CONCURRENT", value)

			cfg := Load()
			if cfg.ReviewMaxWallClockSec != 900 || cfg.ReviewMaxTurns != 0 || cfg.ReviewMaxFirstPassSamples != defaultReviewFirstPassSamples {
				t.Fatalf("invalid %q yielded wall=%d turns=%d samples=%d", value, cfg.ReviewMaxWallClockSec, cfg.ReviewMaxTurns, cfg.ReviewMaxFirstPassSamples)
			}
			if cfg.ReviewMaxFirstPassConcurrent != defaultReviewFirstPassConcurrent {
				t.Fatalf("invalid %q yielded first-pass concurrency=%d", value, cfg.ReviewMaxFirstPassConcurrent)
			}
			if cfg.ReviewMaxTurnsConfigured {
				t.Fatalf("invalid %q was reported as operator-configured", value)
			}
		})
	}
}

func TestLoadReviewCustomizationPolicyUsesPositiveFallbackWhenActiveBudgetIsInvalid(t *testing.T) {
	t.Setenv("AGENT_WALL_CLOCK_SEC", "-10")
	t.Setenv("AGENT_MAX_TURNS", "0")
	t.Setenv("REVIEW_MAX_WALL_CLOCK_SEC", "")
	t.Setenv("REVIEW_MAX_TURNS", "")

	cfg := Load()
	if cfg.ReviewMaxWallClockSec != defaultAgentWallClockSec || cfg.ReviewMaxTurns != 0 {
		t.Fatalf("review limit fallback: wall=%d turns=%d", cfg.ReviewMaxWallClockSec, cfg.ReviewMaxTurns)
	}
	// Existing active-budget parsing remains unchanged for backwards compatibility.
	if cfg.AgentWallClockSec != -10 || cfg.AgentMaxTurns != 0 {
		t.Fatalf("active budgets changed: wall=%d turns=%d", cfg.AgentWallClockSec, cfg.AgentMaxTurns)
	}
}

func TestLoadFirstPassDefaultsToGemini(t *testing.T) {
	t.Setenv("FIRST_PASS_PROVIDER", "")
	t.Setenv("FIRST_PASS_MODEL", "")

	cfg := Load()
	if cfg.FirstPassProvider != "gemini" || cfg.FirstPassModel != "" {
		t.Fatalf("first-pass defaults: provider=%q model=%q", cfg.FirstPassProvider, cfg.FirstPassModel)
	}
}

func TestLoadFirstPassThinking(t *testing.T) {
	t.Setenv("FIRST_PASS_THINKING", "")
	if cfg := Load(); cfg.FirstPassThinking != "" {
		t.Fatalf("thinking default: %q", cfg.FirstPassThinking)
	}

	t.Setenv("FIRST_PASS_THINKING", " High ")
	if cfg := Load(); cfg.FirstPassThinking != "high" {
		t.Fatalf("thinking normalization: %q", cfg.FirstPassThinking)
	}

	t.Setenv("FIRST_PASS_THINKING", "medium")
	if cfg := Load(); cfg.FirstPassThinking != "medium" {
		t.Fatalf("thinking passthrough: %q", cfg.FirstPassThinking)
	}
}

func TestLoadFirstPassCacheStagger(t *testing.T) {
	t.Setenv("FIRST_PASS_CACHE_STAGGER_SEC", "")
	if cfg := Load(); cfg.FirstPassCacheStaggerSec != 8 {
		t.Fatalf("stagger default: %d", cfg.FirstPassCacheStaggerSec)
	}

	t.Setenv("FIRST_PASS_CACHE_STAGGER_SEC", "0")
	if cfg := Load(); cfg.FirstPassCacheStaggerSec != 0 {
		t.Fatalf("explicit zero must disable the stagger: %d", cfg.FirstPassCacheStaggerSec)
	}

	t.Setenv("FIRST_PASS_CACHE_STAGGER_SEC", " 12 ")
	if cfg := Load(); cfg.FirstPassCacheStaggerSec != 12 {
		t.Fatalf("stagger passthrough: %d", cfg.FirstPassCacheStaggerSec)
	}

	t.Setenv("FIRST_PASS_CACHE_STAGGER_SEC", "-3")
	if cfg := Load(); cfg.FirstPassCacheStaggerSec != 8 {
		t.Fatalf("negative must fall back to default: %d", cfg.FirstPassCacheStaggerSec)
	}

	t.Setenv("FIRST_PASS_CACHE_STAGGER_SEC", "soon")
	if cfg := Load(); cfg.FirstPassCacheStaggerSec != 8 {
		t.Fatalf("malformed must fall back to default: %d", cfg.FirstPassCacheStaggerSec)
	}
}

func TestLoadFirstPassNormalizesProvider(t *testing.T) {
	t.Setenv("FIRST_PASS_PROVIDER", " Claude ")
	t.Setenv("FIRST_PASS_MODEL", " claude-opus-5 ")

	cfg := Load()
	if cfg.FirstPassProvider != "claude" || cfg.FirstPassModel != "claude-opus-5" {
		t.Fatalf("first-pass parsing: provider=%q model=%q", cfg.FirstPassProvider, cfg.FirstPassModel)
	}
}

func TestFirstPassAPIKeySelection(t *testing.T) {
	cfg := &Config{
		GeminiAPIKey:     "gem",
		AnthropicAPIKey:  "ant",
		OpenRouterAPIKey: "opr",
	}

	cases := []struct {
		provider string
		want     string
	}{
		{"gemini", "gem"},
		{"", "gem"},
		{"claude", "ant"},
		{"openrouter", "opr"},
	}
	for _, c := range cases {
		cfg.FirstPassProvider = c.provider
		if got := cfg.FirstPassAPIKey(); got != c.want {
			t.Errorf("provider %q: got key %q want %q", c.provider, got, c.want)
		}
	}
}

func clearFirstPassAllowlistEnv(t *testing.T) {
	t.Helper()
	for _, key := range []string{
		"FIRST_PASS_PROVIDER",
		"FIRST_PASS_MODEL",
		"GEMINI_PRO_MODEL",
		"REVIEW_FIRST_PASS_MODELS_GEMINI",
		"REVIEW_FIRST_PASS_MODELS_CLAUDE",
		"REVIEW_FIRST_PASS_MODELS_OPENROUTER",
	} {
		t.Setenv(key, "")
	}
}

func TestLoadFirstPassModelAllowlistDefaults(t *testing.T) {
	clearFirstPassAllowlistEnv(t)

	cfg := Load()
	assertStringsEqual(t, cfg.ReviewFirstPassModelsGemini, []string{defaultFirstPassGeminiModel})
	assertStringsEqual(t, cfg.ReviewFirstPassModelsClaude, []string{defaultFirstPassClaudeModel})
	assertStringsEqual(t, cfg.ReviewFirstPassModelsOpenRouter, []string{defaultFirstPassOpenRouterModel})
}

func TestLoadFirstPassGeminiAllowlistFollowsProModelEnv(t *testing.T) {
	clearFirstPassAllowlistEnv(t)
	t.Setenv("GEMINI_PRO_MODEL", "gemini-9-pro")

	cfg := Load()
	assertStringsEqual(t, cfg.ReviewFirstPassModelsGemini, []string{"gemini-9-pro"})
}

func TestLoadFirstPassModelAllowlistsParseAndAppendActiveModel(t *testing.T) {
	clearFirstPassAllowlistEnv(t)
	t.Setenv("FIRST_PASS_PROVIDER", "claude")
	t.Setenv("FIRST_PASS_MODEL", "claude-fable-5")
	t.Setenv("REVIEW_FIRST_PASS_MODELS_CLAUDE", " claude-opus-5 , claude-opus-5, claude-sonnet-5")
	t.Setenv("REVIEW_FIRST_PASS_MODELS_GEMINI", "gemini-2.5-pro")

	cfg := Load()
	assertStringsEqual(t, cfg.ReviewFirstPassModelsClaude, []string{"claude-opus-5", "claude-sonnet-5", "claude-fable-5"})
	assertStringsEqual(t, cfg.ReviewFirstPassModelsGemini, []string{"gemini-2.5-pro"})
	assertStringsEqual(t, cfg.ReviewFirstPassModelsOpenRouter, []string{defaultFirstPassOpenRouterModel})
}

func TestLoadFirstPassAllowlistAppendsProviderDefaultWhenModelUnset(t *testing.T) {
	clearFirstPassAllowlistEnv(t)
	t.Setenv("FIRST_PASS_PROVIDER", "openrouter")
	t.Setenv("REVIEW_FIRST_PASS_MODELS_OPENROUTER", "anthropic/claude-sonnet-5")

	cfg := Load()
	assertStringsEqual(t, cfg.ReviewFirstPassModelsOpenRouter,
		[]string{"anthropic/claude-sonnet-5", defaultFirstPassOpenRouterModel})
}

func assertStringsEqual(t *testing.T, got, want []string) {
	t.Helper()
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %q want %q", got, want)
	}
}
