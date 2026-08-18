package config

import "testing"

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
	t.Setenv("AGENT_MODEL", "openai/gpt-5.6-sol")
	t.Setenv("AGENT_EFFORT", "xhigh")
	t.Setenv("OPENROUTER_BASE_URL", "https://router.example/v1")
	cfg := Load()
	if cfg.AgentBackend != "openrouter" || cfg.AgentModel != "openai/gpt-5.6-sol" || cfg.AgentEffort != "xhigh" {
		t.Errorf("agent config: backend=%q model=%q effort=%q", cfg.AgentBackend, cfg.AgentModel, cfg.AgentEffort)
	}
	if cfg.OpenRouterBaseURL != "https://router.example/v1" {
		t.Errorf("OpenRouterBaseURL=%q", cfg.OpenRouterBaseURL)
	}
}
