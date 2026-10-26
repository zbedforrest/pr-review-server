package main

import (
	"strings"
	"testing"

	"pr-review-server/config"
)

func TestStartOpenRouterPinSwapsTheBaseURLOnlyWhenConfigured(t *testing.T) {
	cfg := &config.Config{OpenRouterBaseURL: "https://openrouter.ai/api/v1", OpenRouterProviderMatch: "deepseek/"}
	startOpenRouterPin(cfg)
	if cfg.OpenRouterBaseURL != "https://openrouter.ai/api/v1" {
		t.Fatalf("no preferences, base URL changed to %q", cfg.OpenRouterBaseURL)
	}
	cfg.OpenRouterProviderOrder = []string{"digitalocean"}
	startOpenRouterPin(cfg)
	if !strings.HasPrefix(cfg.OpenRouterBaseURL, "http://127.0.0.1:") {
		t.Fatalf("with preferences, base URL = %q, want a loopback proxy", cfg.OpenRouterBaseURL)
	}
}
