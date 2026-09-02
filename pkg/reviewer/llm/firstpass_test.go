package llm

import (
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestParseProvider(t *testing.T) {
	cases := []struct {
		input    string
		expected LLMProvider
	}{
		{"", ProviderGemini},
		{"gemini", ProviderGemini},
		{" Gemini ", ProviderGemini},
		{"claude", ProviderClaude},
		{"CLAUDE", ProviderClaude},
		{"openrouter", ProviderOpenRouter},
	}
	for _, tc := range cases {
		provider, err := ParseProvider(tc.input)
		require.NoError(t, err, "input %q", tc.input)
		assert.Equal(t, tc.expected, provider, "input %q", tc.input)
	}

	_, err := ParseProvider("gpt")
	assert.ErrorContains(t, err, "unsupported first-pass provider")
}

func TestFirstPassModelName(t *testing.T) {
	t.Setenv("GEMINI_PRO_MODEL", "")
	os.Unsetenv("GEMINI_PRO_MODEL")

	assert.Equal(t, DefaultProModel, FirstPassModelName(ProviderGemini, ""))
	assert.Equal(t, DefaultClaudeModel, FirstPassModelName(ProviderClaude, ""))
	assert.Equal(t, "claude-sonnet-5", FirstPassModelName(ProviderClaude, ""))
	assert.Equal(t, DefaultOpenRouterModel, FirstPassModelName(ProviderOpenRouter, ""))

	assert.Equal(t, "claude-opus-5", FirstPassModelName(ProviderClaude, "claude-opus-5"))
	assert.Equal(t, "openai/gpt-5.6-sol", FirstPassModelName(ProviderOpenRouter, "openai/gpt-5.6-sol"))
	assert.Equal(t, "gemini-2.5-pro", FirstPassModelName(ProviderGemini, "gemini-2.5-pro"))
}

func TestFirstPassModelName_GeminiHonorsEnvOverride(t *testing.T) {
	t.Setenv("GEMINI_PRO_MODEL", "gemini-2.5-pro")
	assert.Equal(t, "gemini-2.5-pro", FirstPassModelName(ProviderGemini, ""))
}

func TestFirstPassTelemetry(t *testing.T) {
	provider, backend := FirstPassTelemetry(ProviderGemini)
	assert.Equal(t, "google", provider)
	assert.Equal(t, "gemini_api", backend)

	provider, backend = FirstPassTelemetry(ProviderClaude)
	assert.Equal(t, "anthropic", provider)
	assert.Equal(t, "anthropic_api", backend)

	provider, backend = FirstPassTelemetry(ProviderOpenRouter)
	assert.Equal(t, "openrouter", provider)
	assert.Equal(t, "openrouter_api", backend)
}

func TestNewFirstPassClient_ProviderSelection(t *testing.T) {
	geminiClient, err := NewFirstPassClient(ProviderGemini, "dummy-key", "", "", false)
	require.NoError(t, err)
	assert.IsType(t, &Client{}, geminiClient)

	defaultClient, err := NewFirstPassClient("", "dummy-key", "", "", false)
	require.NoError(t, err)
	assert.IsType(t, &Client{}, defaultClient)

	claudeClient, err := NewFirstPassClient(ProviderClaude, "dummy-key", "", "", false)
	require.NoError(t, err)
	require.IsType(t, &ClaudeClient{}, claudeClient)
	assert.EqualValues(t, DefaultClaudeModel, claudeClient.(*ClaudeClient).model)

	openRouterClient, err := NewFirstPassClient(ProviderOpenRouter, "dummy-key", "", "", false)
	require.NoError(t, err)
	require.IsType(t, &OpenRouterClient{}, openRouterClient)
	assert.Equal(t, DefaultOpenRouterModel, openRouterClient.(*OpenRouterClient).model)
	assert.Equal(t, DefaultOpenRouterBaseURL, openRouterClient.(*OpenRouterClient).baseURL)
}

func TestNewFirstPassClient_ModelAndBaseURLOverrides(t *testing.T) {
	claudeClient, err := NewFirstPassClient(ProviderClaude, "dummy-key", "claude-opus-5", "", false)
	require.NoError(t, err)
	assert.EqualValues(t, "claude-opus-5", claudeClient.(*ClaudeClient).model)

	openRouterClient, err := NewFirstPassClient(ProviderOpenRouter, "dummy-key", "openai/gpt-5.6-sol", "https://example.test/api/v1/", false)
	require.NoError(t, err)
	assert.Equal(t, "openai/gpt-5.6-sol", openRouterClient.(*OpenRouterClient).model)
	assert.Equal(t, "https://example.test/api/v1", openRouterClient.(*OpenRouterClient).baseURL)
}

func TestNewFirstPassClient_Errors(t *testing.T) {
	_, err := NewFirstPassClient("gpt", "dummy-key", "", "", false)
	assert.ErrorContains(t, err, "unsupported first-pass provider")

	_, err = NewFirstPassClient(ProviderClaude, "", "", "", false)
	assert.ErrorContains(t, err, "ANTHROPIC_API_KEY")

	_, err = NewFirstPassClient(ProviderOpenRouter, "", "", "", false)
	assert.ErrorContains(t, err, "OPENROUTER_API_KEY")

	_, err = NewFirstPassClient(ProviderGemini, "", "", "", false)
	assert.ErrorContains(t, err, "GEMINI_API_KEY")
}
