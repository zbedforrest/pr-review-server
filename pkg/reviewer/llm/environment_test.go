package llm

import (
	"strings"
	"testing"
)

func TestChildEnvironmentDefaultDenyAndClaudeOAuth(t *testing.T) {
	environment := ChildEnvironment([]string{
		"PATH=/usr/bin",
		"HOME=/home/reviewer",
		"ANTHROPIC_API_KEY=metered-key-must-not-pass",
		"CLAUDE_CODE_OAUTH_TOKEN=oauth-token",
		"ANTHROPIC_AUTH_TOKEN=auth-token",
		"ANTHROPIC_BASE_URL=https://gateway.example",
		"DATABASE_URL=postgres://secret",
		"UNRELATED_FUTURE_SECRET=drop-me",
		"PATH=/duplicate",
	}, "ANTHROPIC_API_KEY", "")
	joined := strings.Join(environment, "\n")
	for _, want := range []string{
		"PATH=/usr/bin",
		"HOME=/home/reviewer",
		"CLAUDE_CODE_OAUTH_TOKEN=oauth-token",
		"ANTHROPIC_AUTH_TOKEN=auth-token",
		"ANTHROPIC_BASE_URL=https://gateway.example",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("child environment missing %q: %q", want, environment)
		}
	}
	for _, forbidden := range []string{
		"metered-key-must-not-pass",
		"postgres://secret",
		"drop-me",
		"PATH=/duplicate",
	} {
		if strings.Contains(joined, forbidden) {
			t.Errorf("child environment retained %q: %q", forbidden, environment)
		}
	}
}

func TestChildEnvironmentInjectsOnlySelectedCredential(t *testing.T) {
	environment := strings.Join(ChildEnvironment([]string{
		"CLAUDE_CODE_OAUTH_TOKEN=oauth-token",
		"ANTHROPIC_AUTH_TOKEN=auth-token",
		"ANTHROPIC_BASE_URL=https://gateway.example",
		"OPENROUTER_API_KEY=ambient",
	}, "OPENROUTER_API_KEY", "frozen"), "\n")
	if environment != "OPENROUTER_API_KEY=frozen" {
		t.Fatalf("unexpected non-Claude child environment: %q", environment)
	}
}
