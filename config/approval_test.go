package config

import "testing"

func TestApprovalReasoningEffort(t *testing.T) {
	for _, c := range []struct{ env, want, reason string }{
		{"", "low", ""},
		{"medium", "medium", ""},
		{"low", "low", ""},
		{"high", "high", ""},
		{"off", "", ""},
		{"extreme", "", "Invalid approval reasoning effort"},
	} {
		t.Run(c.env, func(t *testing.T) {
			t.Setenv("APPROVAL_CANDIDATES_PROVIDER", "openrouter")
			t.Setenv("APPROVAL_CANDIDATES_MODEL", "fixture-model")
			t.Setenv("APPROVAL_CANDIDATES_DAILY_INPUT_TOKENS", "")
			t.Setenv("APPROVAL_CANDIDATES_DAILY_OUTPUT_TOKENS", "")
			t.Setenv("APPROVAL_CANDIDATES_PROVIDER_IDENTITIES", "")
			t.Setenv("APPROVAL_CANDIDATES_REASONING_EFFORT", c.env)
			got := (&Config{OpenRouterAPIKey: "fixture"}).ApprovalCandidates()
			if got.ReasoningEffort != c.want || got.UnavailableReason != c.reason {
				t.Fatalf("effort=%q reason=%q", got.ReasoningEffort, got.UnavailableReason)
			}
		})
	}
}
