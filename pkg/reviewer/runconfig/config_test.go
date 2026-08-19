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
		FirstPass:      FirstPass{Samples: 3},
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
