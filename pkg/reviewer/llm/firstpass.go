package llm

import (
	"fmt"
	"strings"
)

// ParseProvider normalizes a first-pass provider setting. Empty means the
// historical default, Gemini.
func ParseProvider(value string) (LLMProvider, error) {
	switch LLMProvider(strings.ToLower(strings.TrimSpace(value))) {
	case ProviderGemini, "":
		return ProviderGemini, nil
	case ProviderClaude:
		return ProviderClaude, nil
	case ProviderOpenRouter:
		return ProviderOpenRouter, nil
	default:
		return "", fmt.Errorf("unsupported first-pass provider %q (expected gemini, claude, or openrouter)", value)
	}
}

// FirstPassModelName resolves the model the first pass will request:
// the configured model when set, otherwise the provider's default.
func FirstPassModelName(provider LLMProvider, model string) string {
	if model != "" {
		return model
	}
	switch provider {
	case ProviderClaude:
		return DefaultClaudeModel
	case ProviderOpenRouter:
		return DefaultOpenRouterModel
	default:
		return ProModelName()
	}
}

// FirstPassTelemetry returns the provider and backend identifiers recorded in
// review sidecar metadata for a first-pass provider.
func FirstPassTelemetry(provider LLMProvider) (providerName, backend string) {
	switch provider {
	case ProviderClaude:
		return "anthropic", "anthropic_api"
	case ProviderOpenRouter:
		return "openrouter", "openrouter_api"
	default:
		return "google", "gemini_api"
	}
}

// NewFirstPassClient builds the smart (first-pass) client for the configured
// provider. openRouterBaseURL applies only to the openrouter provider.
func NewFirstPassClient(provider LLMProvider, apiKey, model, openRouterBaseURL string, verbose bool) (IClient, error) {
	resolved, err := ParseProvider(string(provider))
	if err != nil {
		return nil, err
	}
	resolvedModel := FirstPassModelName(resolved, model)
	switch resolved {
	case ProviderClaude:
		if apiKey == "" {
			return nil, fmt.Errorf("first-pass provider claude requires ANTHROPIC_API_KEY")
		}
		return NewClaudeClient(apiKey, resolvedModel, verbose), nil
	case ProviderOpenRouter:
		if apiKey == "" {
			return nil, fmt.Errorf("first-pass provider openrouter requires OPENROUTER_API_KEY")
		}
		return NewOpenRouterClient(apiKey, openRouterBaseURL, resolvedModel, verbose), nil
	default:
		if apiKey == "" {
			return nil, fmt.Errorf("first-pass provider gemini requires GEMINI_API_KEY")
		}
		return NewGeminiClientWithModel(apiKey, resolvedModel, verbose), nil
	}
}
