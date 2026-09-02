package runconfig

import (
	"errors"
	"testing"
)

func testDefaults() Effective {
	return Effective{
		SchemaVersion: SchemaVersion,
		Agent: Agent{
			Enabled:           true,
			Backend:           "claude",
			Model:             "claude-fable-5",
			Effort:            "medium",
			WallClockSeconds:  900,
			MaxTurns:          120,
			TurnBudgetUnit:    TurnBudgetUnitAssistantEvent,
			TurnBudgetVersion: TurnBudgetVersion,
		},
		FirstPass:      FirstPass{Samples: 3, Provider: "gemini", Model: "gemini-3.1-pro-preview"},
		RequiredChecks: true,
	}
}

func testPolicy() Policy {
	return Policy{
		Backends: map[string]BackendPolicy{
			"claude": {
				Available: true, Ready: true, PolicyEnabled: true, CredentialConfigured: true, ExecutableAvailable: true,
				TurnBudgetUnit: TurnBudgetUnitAssistantEvent, TurnBudgetVersion: TurnBudgetVersion,
				DefaultMaxTurns: 120, MaxTurns: 240,
				Models:  []string{"claude-fable-5", "claude-opus-4-8"},
				Efforts: []string{"low", "medium", "high"},
			},
			"openrouter": {
				Available: true, Ready: true, PolicyEnabled: true, CredentialConfigured: true, CredentialRequired: true, ExecutableAvailable: true,
				TurnBudgetUnit: TurnBudgetUnitCompletedNonReasoningItem, TurnBudgetVersion: TurnBudgetVersion,
				DefaultMaxTurns: 200, MaxTurns: 240,
				Models:  []string{"openai/gpt-5.6-sol"},
				Efforts: []string{"medium", "high", "xhigh", "max"},
			},
		},
		FirstPassProviders: map[string]FirstPassProviderPolicy{
			"gemini": {
				CredentialConfigured: true,
				DefaultModel:         "gemini-3.1-pro-preview",
				Models:               []string{"gemini-3.1-pro-preview", "gemini-2.5-pro"},
			},
			"claude": {
				CredentialConfigured: true,
				DefaultModel:         "claude-sonnet-5",
				Models:               []string{"claude-sonnet-5"},
			},
			"openrouter": {
				CredentialConfigured: false,
				DefaultModel:         "openai/gpt-5.6-sol",
				Models:               []string{"openai/gpt-5.6-sol"},
			},
		},
		MaxWallClockSeconds: 1800,
		MaxTurns:            240,
		MaxFirstPassSamples: 5,
	}
}

func TestResolveDerivesMaxTurnsWhenBackendChangesUnit(t *testing.T) {
	requested := Overrides{Agent: &AgentOverrides{
		Backend: ptr("openrouter"),
		Model:   ptr("openai/gpt-5.6-sol"),
	}}
	snapshot, err := Resolve(requested, testDefaults(), testPolicy())
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.Effective.Agent.MaxTurns != 200 {
		t.Fatalf("max turns=%d want OpenRouter default 200", snapshot.Effective.Agent.MaxTurns)
	}
	if snapshot.Sources["agent.max_turns"] != SourceDerived {
		t.Fatalf("max turns source=%q", snapshot.Sources["agent.max_turns"])
	}
}

func TestResolveClampsDerivedBackendDefaultToBackendCeiling(t *testing.T) {
	policy := testPolicy()
	openRouter := policy.Backends["openrouter"]
	openRouter.MaxTurns = 150
	policy.Backends["openrouter"] = openRouter
	requested := Overrides{Agent: &AgentOverrides{
		Backend: ptr("openrouter"),
		Model:   ptr("openai/gpt-5.6-sol"),
	}}
	snapshot, err := Resolve(requested, testDefaults(), policy)
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.Effective.Agent.MaxTurns != 150 {
		t.Fatalf("max turns=%d want backend ceiling 150", snapshot.Effective.Agent.MaxTurns)
	}
}

func TestResolveRejectsRequestedTurnsAboveBackendCeiling(t *testing.T) {
	policy := testPolicy()
	openRouter := policy.Backends["openrouter"]
	openRouter.MaxTurns = 150
	policy.Backends["openrouter"] = openRouter
	requested := Overrides{Agent: &AgentOverrides{
		Backend:  ptr("openrouter"),
		Model:    ptr("openai/gpt-5.6-sol"),
		MaxTurns: ptr(151),
	}}
	_, err := Resolve(requested, testDefaults(), policy)
	var validationErr *ValidationError
	if !errors.As(err, &validationErr) || validationErr.Field != "agent.max_turns" {
		t.Fatalf("err=%v", err)
	}
}

func TestResolveRejectsMissingPerBackendCeilingInMultiBackendPolicy(t *testing.T) {
	policy := testPolicy()
	openRouter := policy.Backends["openrouter"]
	openRouter.MaxTurns = 0
	policy.Backends["openrouter"] = openRouter
	requested := Overrides{Agent: &AgentOverrides{
		Backend:  ptr("openrouter"),
		Model:    ptr("openai/gpt-5.6-sol"),
		MaxTurns: ptr(80),
	}}

	_, err := Resolve(requested, testDefaults(), policy)
	var validationErr *ValidationError
	if !errors.As(err, &validationErr) || validationErr.Field != "agent.max_turns" {
		t.Fatalf("err=%v", err)
	}
}

func TestResolveRejectsBackendSwitchWithoutTurnDefault(t *testing.T) {
	policy := testPolicy()
	openRouter := policy.Backends["openrouter"]
	openRouter.DefaultMaxTurns = 0
	policy.Backends["openrouter"] = openRouter
	_, err := Resolve(Overrides{Agent: &AgentOverrides{
		Backend: ptr("openrouter"), Model: ptr("openai/gpt-5.6-sol"),
	}}, testDefaults(), policy)
	var validationErr *ValidationError
	if !errors.As(err, &validationErr) || validationErr.Field != "agent.max_turns" {
		t.Fatalf("err=%v", err)
	}
}

func ptr[T any](value T) *T { return &value }

func TestResolveUsesDefaultsWithoutOverrides(t *testing.T) {
	snapshot, err := Resolve(Overrides{}, testDefaults(), testPolicy())
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.Effective != testDefaults() {
		t.Fatalf("effective=%+v want %+v", snapshot.Effective, testDefaults())
	}
	if snapshot.Sources["agent.model"] != SourceDeploymentDefault {
		t.Fatalf("agent.model source=%q", snapshot.Sources["agent.model"])
	}
	if len(snapshot.Hash) != 64 {
		t.Fatalf("hash=%q", snapshot.Hash)
	}
}

func TestResolveCanonicalizesDefaultsBeforeHashing(t *testing.T) {
	canonical, err := Resolve(Overrides{}, testDefaults(), testPolicy())
	if err != nil {
		t.Fatal(err)
	}
	variantDefaults := testDefaults()
	variantDefaults.Agent.Backend = " Claude "
	variantDefaults.Agent.Model = " claude-fable-5 "
	variantDefaults.Agent.Effort = " Medium "
	variant, err := Resolve(Overrides{}, variantDefaults, testPolicy())
	if err != nil {
		t.Fatal(err)
	}
	if variant.Effective != canonical.Effective {
		t.Fatalf("effective=%+v want %+v", variant.Effective, canonical.Effective)
	}
	if variant.Hash != canonical.Hash {
		t.Fatalf("hash=%q want %q", variant.Hash, canonical.Hash)
	}
}

func TestResolveAppliesAndAttributesOverrides(t *testing.T) {
	requested := Overrides{Agent: &AgentOverrides{
		Backend:          ptr(" openrouter "),
		Model:            ptr("openai/gpt-5.6-sol"),
		Effort:           ptr("XHIGH"),
		WallClockSeconds: ptr(600),
		MaxTurns:         ptr(80),
	}}
	snapshot, err := Resolve(requested, testDefaults(), testPolicy())
	if err != nil {
		t.Fatal(err)
	}
	if got := snapshot.Effective.Agent; got.Backend != "openrouter" || got.Model != "openai/gpt-5.6-sol" || got.Effort != "xhigh" || got.WallClockSeconds != 600 || got.MaxTurns != 80 {
		t.Fatalf("agent=%+v", got)
	}
	if snapshot.Effective.Agent.TurnBudgetUnit != TurnBudgetUnitCompletedNonReasoningItem || snapshot.Effective.Agent.TurnBudgetVersion != TurnBudgetVersion {
		t.Fatalf("turn semantics=%q/v%d", snapshot.Effective.Agent.TurnBudgetUnit, snapshot.Effective.Agent.TurnBudgetVersion)
	}
	if snapshot.Sources["agent.turn_budget_unit"] != SourceDerived || snapshot.Sources["agent.turn_budget_version"] != SourceDerived {
		t.Fatalf("turn semantic sources=%v", snapshot.Sources)
	}
	for _, field := range []string{"agent.backend", "agent.model", "agent.effort", "agent.wall_clock_seconds", "agent.max_turns"} {
		if snapshot.Sources[field] != SourceRequest {
			t.Errorf("%s source=%q", field, snapshot.Sources[field])
		}
	}
}

func TestResolveRejectsUnavailableBackend(t *testing.T) {
	policy := testPolicy()
	openRouter := policy.Backends["openrouter"]
	openRouter.Available = false
	policy.Backends["openrouter"] = openRouter
	_, err := Resolve(Overrides{Agent: &AgentOverrides{
		Backend: ptr("openrouter"),
		Model:   ptr("openai/gpt-5.6-sol"),
	}}, testDefaults(), policy)
	var validationErr *ValidationError
	if !errors.As(err, &validationErr) || validationErr.Field != "agent.backend" {
		t.Fatalf("err=%v", err)
	}
}

func TestResolveRejectsPolicyAvailableButUnreadyBackend(t *testing.T) {
	policy := testPolicy()
	openRouter := policy.Backends["openrouter"]
	openRouter.Ready = false
	openRouter.CredentialConfigured = false
	openRouter.UnavailableReasons = []string{BackendUnavailableCredentialMissing}
	policy.Backends["openrouter"] = openRouter
	_, err := Resolve(Overrides{Agent: &AgentOverrides{
		Backend: ptr("openrouter"), Model: ptr("openai/gpt-5.6-sol"),
	}}, testDefaults(), policy)
	var validationErr *ValidationError
	if !errors.As(err, &validationErr) || validationErr.Field != "agent.backend" {
		t.Fatalf("err=%v", err)
	}
}

func TestPolicyBackendNamesRoundTripCanonically(t *testing.T) {
	policy := testPolicy()
	claude := policy.Backends["claude"]
	delete(policy.Backends, "claude")
	policy.Backends[" Claude "] = claude

	snapshot, err := Resolve(Overrides{}, testDefaults(), policy)
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.Effective.Agent.Backend != "claude" {
		t.Fatalf("backend=%q", snapshot.Effective.Agent.Backend)
	}
	available := policy.AvailableBackends()
	if len(available) != 2 || available[0] != "claude" || available[1] != "openrouter" {
		t.Fatalf("available=%v", available)
	}
}

func TestResolveRejectsModelBackendMismatch(t *testing.T) {
	_, err := Resolve(Overrides{Agent: &AgentOverrides{
		Model: ptr("openai/gpt-5.6-sol"),
	}}, testDefaults(), testPolicy())
	var validationErr *ValidationError
	if !errors.As(err, &validationErr) || validationErr.Field != "agent.model" {
		t.Fatalf("err=%v", err)
	}
}

func TestResolveRejectsLimitsInsteadOfClamping(t *testing.T) {
	_, err := Resolve(Overrides{Agent: &AgentOverrides{
		WallClockSeconds: ptr(1801),
	}}, testDefaults(), testPolicy())
	var validationErr *ValidationError
	if !errors.As(err, &validationErr) || validationErr.Field != "agent.wall_clock_seconds" {
		t.Fatalf("err=%v", err)
	}
}

func TestResolveAppliesFirstPassProviderAndModelOverrides(t *testing.T) {
	snapshot, err := Resolve(Overrides{FirstPass: &FirstPassOverrides{
		Provider: ptr(" Claude "),
		Model:    ptr(" claude-sonnet-5 "),
	}}, testDefaults(), testPolicy())
	if err != nil {
		t.Fatal(err)
	}
	if got := snapshot.Effective.FirstPass; got.Provider != "claude" || got.Model != "claude-sonnet-5" || got.Samples != 3 {
		t.Fatalf("first pass=%+v", got)
	}
	if snapshot.Sources["first_pass.provider"] != SourceRequest || snapshot.Sources["first_pass.model"] != SourceRequest {
		t.Fatalf("sources=%v", snapshot.Sources)
	}
	if snapshot.Sources["first_pass.samples"] != SourceDeploymentDefault {
		t.Fatalf("samples source=%q", snapshot.Sources["first_pass.samples"])
	}
}

func TestResolveDerivesProviderDefaultModelOnProviderSwitch(t *testing.T) {
	snapshot, err := Resolve(Overrides{FirstPass: &FirstPassOverrides{
		Provider: ptr("claude"),
	}}, testDefaults(), testPolicy())
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.Effective.FirstPass.Model != "claude-sonnet-5" {
		t.Fatalf("model=%q want claude default", snapshot.Effective.FirstPass.Model)
	}
	if snapshot.Sources["first_pass.model"] != SourceDerived {
		t.Fatalf("model source=%q", snapshot.Sources["first_pass.model"])
	}
}

func TestResolveAcceptsAllowlistedModelForDefaultProvider(t *testing.T) {
	snapshot, err := Resolve(Overrides{FirstPass: &FirstPassOverrides{
		Model: ptr("gemini-2.5-pro"),
	}}, testDefaults(), testPolicy())
	if err != nil {
		t.Fatal(err)
	}
	if got := snapshot.Effective.FirstPass; got.Provider != "gemini" || got.Model != "gemini-2.5-pro" {
		t.Fatalf("first pass=%+v", got)
	}
}

func TestResolveRejectsFirstPassModelOutsideProviderAllowlist(t *testing.T) {
	cases := []Overrides{
		{FirstPass: &FirstPassOverrides{Model: ptr("claude-sonnet-5")}},
		{FirstPass: &FirstPassOverrides{Provider: ptr("claude"), Model: ptr("claude-opus-9")}},
	}
	for _, requested := range cases {
		_, err := Resolve(requested, testDefaults(), testPolicy())
		var validationErr *ValidationError
		if !errors.As(err, &validationErr) || validationErr.Field != "first_pass.model" {
			t.Fatalf("requested=%+v err=%v", requested.FirstPass, err)
		}
	}
}

func TestResolveRejectsUnknownFirstPassProvider(t *testing.T) {
	_, err := Resolve(Overrides{FirstPass: &FirstPassOverrides{
		Provider: ptr("gpt"),
	}}, testDefaults(), testPolicy())
	var validationErr *ValidationError
	if !errors.As(err, &validationErr) || validationErr.Field != "first_pass.provider" {
		t.Fatalf("err=%v", err)
	}
}

func TestResolveRejectsFirstPassProviderWithoutCredential(t *testing.T) {
	_, err := Resolve(Overrides{FirstPass: &FirstPassOverrides{
		Provider: ptr("openrouter"),
		Model:    ptr("openai/gpt-5.6-sol"),
	}}, testDefaults(), testPolicy())
	var validationErr *ValidationError
	if !errors.As(err, &validationErr) || validationErr.Field != "first_pass.provider" {
		t.Fatalf("err=%v", err)
	}
}

func TestResolveAcceptsLegacyPolicyWithoutFirstPassProviders(t *testing.T) {
	defaults := testDefaults()
	defaults.FirstPass = FirstPass{Samples: 3}
	policy := testPolicy()
	policy.FirstPassProviders = nil
	snapshot, err := Resolve(Overrides{}, defaults, policy)
	if err != nil {
		t.Fatal(err)
	}
	if got := snapshot.Effective.FirstPass; got.Provider != "" || got.Model != "" {
		t.Fatalf("first pass=%+v want empty legacy identity", got)
	}
}

func TestHashChangesWithEffectiveConfig(t *testing.T) {
	base, err := Hash(testDefaults())
	if err != nil {
		t.Fatal(err)
	}
	changed := testDefaults()
	changed.Agent.MaxTurns++
	other, err := Hash(changed)
	if err != nil {
		t.Fatal(err)
	}
	if base == other {
		t.Fatal("config hash did not change")
	}
}
