package config

import "testing"

func TestApprovalReasoningTokens(t *testing.T) {
	for _, c := range []struct {
		env    string
		want   int
		reason string
	}{
		{"", 6000, ""},
		{"0", 0, ""},
		{"4000", 4000, ""},
		{"-1", -1, "Invalid approval reasoning token budget"},
		{"lots", 0, "Invalid approval reasoning token budget"},
	} {
		t.Run(c.env, func(t *testing.T) {
			t.Setenv("APPROVAL_CANDIDATES_PROVIDER", "openrouter")
			t.Setenv("APPROVAL_CANDIDATES_MODEL", "fixture-model")
			t.Setenv("APPROVAL_CANDIDATES_DAILY_INPUT_TOKENS", "")
			t.Setenv("APPROVAL_CANDIDATES_DAILY_OUTPUT_TOKENS", "")
			t.Setenv("APPROVAL_CANDIDATES_PROVIDER_IDENTITIES", "")
			t.Setenv("APPROVAL_CANDIDATES_REASONING_TOKENS", c.env)
			got := (&Config{OpenRouterAPIKey: "fixture"}).ApprovalCandidates()
			if got.ReasoningTokens != c.want || got.UnavailableReason != c.reason {
				t.Fatalf("tokens=%d reason=%q", got.ReasoningTokens, got.UnavailableReason)
			}
		})
	}
}
