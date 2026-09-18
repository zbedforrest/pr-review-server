package runconfig

import (
	"errors"
	"strings"
	"testing"
)

func litePolicy() Policy {
	policy := testPolicy()
	claude := policy.Backends["claude"]
	claude.Models = append(claude.Models, "claude-fable-5-1")
	policy.Backends["claude"] = claude
	return policy
}

func strPtr(s string) *string { return &s }

func TestResolveExpandsLiteProfile(t *testing.T) {
	snapshot, err := Resolve(Overrides{Profile: strPtr("lite")}, testDefaults(), litePolicy())
	if err != nil {
		t.Fatal(err)
	}
	e := snapshot.Effective
	if e.Profile != ProfileLite || e.SchemaVersion != 4 {
		t.Fatalf("profile=%q schema=%d", e.Profile, e.SchemaVersion)
	}
	want := Agent{
		Enabled: true, Backend: "claude", Model: "claude-fable-5-1", Effort: "medium",
		WallClockSeconds: 300, MaxTurns: 60, Tools: ToolsDefault, Prompt: PromptLiteArmA,
		TurnBudgetUnit: TurnBudgetUnitAssistantEvent, TurnBudgetVersion: TurnBudgetVersion,
	}
	if e.Agent != want {
		t.Fatalf("agent=%+v want %+v", e.Agent, want)
	}
	if e.FirstPass != (FirstPass{}) || e.RequiredChecks || e.Gates || !e.BugMemory {
		t.Fatalf("stages: first_pass=%+v checks=%t gates=%t memory=%t", e.FirstPass, e.RequiredChecks, e.Gates, e.BugMemory)
	}
	if snapshot.Sources["profile"] != SourceRequest || snapshot.Sources["agent.model"] != SourceDerived || snapshot.Sources["first_pass.samples"] != SourceDerived {
		t.Fatalf("sources=%v", snapshot.Sources)
	}
}

func TestResolveExpandsLitePlusWithAgentToolAndLongerWallClock(t *testing.T) {
	snapshot, err := Resolve(Overrides{Profile: strPtr("lite_plus")}, testDefaults(), litePolicy())
	if err != nil {
		t.Fatal(err)
	}
	e := snapshot.Effective
	if e.Profile != ProfileLitePlus || e.Agent.Tools != ToolsWithAgent || e.Agent.Prompt != PromptLiteArmASub ||
		e.Agent.WallClockSeconds != 600 || e.Agent.MaxTurns != 120 || e.Agent.Effort != "medium" {
		t.Fatalf("effective=%+v", e)
	}
}

func TestResolveRejectsUnknownProfile(t *testing.T) {
	_, err := Resolve(Overrides{Profile: strPtr("turbo")}, testDefaults(), litePolicy())
	var verr *ValidationError
	if !errors.As(err, &verr) || verr.Field != "profile" {
		t.Fatalf("err=%v", err)
	}
}

func TestResolveRejectsFirstPassOverridesOnLiteProfile(t *testing.T) {
	samples := 2
	_, err := Resolve(Overrides{Profile: strPtr("lite"), FirstPass: &FirstPassOverrides{Samples: &samples}}, testDefaults(), litePolicy())
	var verr *ValidationError
	if !errors.As(err, &verr) || verr.Field != "profile" {
		t.Fatalf("first_pass override: err=%v", err)
	}
	checks := true
	_, err = Resolve(Overrides{Profile: strPtr("lite"), RequiredChecks: &checks}, testDefaults(), litePolicy())
	if !errors.As(err, &verr) || verr.Field != "profile" {
		t.Fatalf("required_checks override: err=%v", err)
	}
}

func TestResolveAllowsAgentEffortOverrideOnLiteProfile(t *testing.T) {
	snapshot, err := Resolve(Overrides{Profile: strPtr("lite"), Agent: &AgentOverrides{Effort: strPtr("high")}}, testDefaults(), litePolicy())
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.Effective.Agent.Effort != "high" || snapshot.Sources["agent.effort"] != SourceRequest {
		t.Fatalf("effective=%+v sources=%v", snapshot.Effective.Agent, snapshot.Sources)
	}
	if snapshot.Effective.Agent.MaxTurns != 60 {
		t.Fatalf("lite turn budget must survive an effort override: %d", snapshot.Effective.Agent.MaxTurns)
	}
}

func TestResolveLiteKeepsItsTurnBudgetWhenDeploymentBackendDiffers(t *testing.T) {
	defaults := testDefaults()
	defaults.Agent.Backend = "openrouter"
	defaults.Agent.Model = "openai/gpt-5.6-sol"
	defaults.Agent.MaxTurns = 200
	defaults.Agent.TurnBudgetUnit, defaults.Agent.TurnBudgetVersion = TurnBudgetSemantics("openrouter")
	snapshot, err := Resolve(Overrides{Profile: strPtr("lite")}, defaults, litePolicy())
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.Effective.Agent.Backend != "claude" || snapshot.Effective.Agent.MaxTurns != 60 {
		t.Fatalf("agent=%+v", snapshot.Effective.Agent)
	}
}

func TestResolveUsesPolicyDefaultProfile(t *testing.T) {
	policy := litePolicy()
	policy.DefaultProfile = ProfileLite
	snapshot, err := Resolve(Overrides{}, testDefaults(), policy)
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.Effective.Profile != ProfileLite || snapshot.Sources["profile"] != SourceDeploymentDefault {
		t.Fatalf("profile=%q sources=%v", snapshot.Effective.Profile, snapshot.Sources)
	}
	full, err := Resolve(Overrides{Profile: strPtr("full")}, testDefaults(), policy)
	if err != nil {
		t.Fatal(err)
	}
	if full.Effective != testDefaults() {
		t.Fatalf("explicit full must restore the deployment pipeline: %+v", full.Effective)
	}
}

func TestValidateSkipsFirstPassChecksWhenDisabled(t *testing.T) {
	cfg := testDefaults()
	cfg.FirstPass = FirstPass{}
	if err := Validate(cfg, testPolicy()); err != nil {
		t.Fatalf("disabled first pass must not be validated: %v", err)
	}
	cfg.Agent.Enabled = false
	if err := Validate(cfg, testPolicy()); err == nil {
		t.Fatal("a config with neither stage must be rejected")
	}
}

func TestValidateRejectsUnknownToolsAndPrompt(t *testing.T) {
	cfg := testDefaults()
	cfg.Agent.Tools = "Read,Grep,Glob,Bash,Write"
	if err := Validate(cfg, testPolicy()); err == nil {
		t.Fatal("Write tool must be rejected")
	}
	cfg = testDefaults()
	cfg.Agent.Prompt = "freeform"
	if err := Validate(cfg, testPolicy()); err == nil {
		t.Fatal("unknown prompt must be rejected")
	}
}

func TestHashChangesWithProfile(t *testing.T) {
	lite, err := Resolve(Overrides{Profile: strPtr("lite")}, testDefaults(), litePolicy())
	if err != nil {
		t.Fatal(err)
	}
	litePlus, err := Resolve(Overrides{Profile: strPtr("lite_plus")}, testDefaults(), litePolicy())
	if err != nil {
		t.Fatal(err)
	}
	full, err := Resolve(Overrides{}, testDefaults(), litePolicy())
	if err != nil {
		t.Fatal(err)
	}
	if lite.Hash == litePlus.Hash || lite.Hash == full.Hash {
		t.Fatalf("hashes must differ: lite=%s lite_plus=%s full=%s", lite.Hash, litePlus.Hash, full.Hash)
	}
}

func TestFullProfileMatchesSchemaThreeDefaults(t *testing.T) {
	legacy := Effective{
		SchemaVersion: 3,
		Agent: Agent{Enabled: true, Backend: "claude", Model: "claude-fable-5", Effort: "medium", WallClockSeconds: 900, MaxTurns: 120,
			TurnBudgetUnit: TurnBudgetUnitAssistantEvent, TurnBudgetVersion: TurnBudgetVersion},
		FirstPass:      FirstPass{Samples: 3, Provider: "gemini", Model: "gemini-3.1-pro-preview"},
		RequiredChecks: true,
	}
	snapshot, err := Resolve(Overrides{}, legacy, testPolicy())
	if err != nil {
		t.Fatal(err)
	}
	e := snapshot.Effective
	if e.Agent.Backend != legacy.Agent.Backend || e.Agent.Model != legacy.Agent.Model || e.Agent.WallClockSeconds != 900 ||
		e.Agent.MaxTurns != 120 || e.FirstPass.Samples != 3 || e.FirstPass.Provider != "gemini" || !e.RequiredChecks {
		t.Fatalf("full profile changed the pipeline: %+v", e)
	}
	if e.Profile != ProfileFull || !e.FirstPass.Enabled || !e.Gates || !e.BugMemory || e.Agent.Tools != ToolsDefault || e.Agent.Prompt != PromptPipeline {
		t.Fatalf("full profile fields not made explicit: %+v", e)
	}
}

func TestDescribeProfile_ExactProfiles(t *testing.T) {
	base := testDefaults()
	for _, profile := range Profiles() {
		expanded, err := Expand(profile, base)
		if err != nil {
			t.Fatal(err)
		}
		d := DescribeProfile(expanded, base)
		if d.Profile != profile || d.Custom() || d.Legacy {
			t.Fatalf("%s: %+v", profile, d)
		}
		if d.Title() != ProfileLabel(profile) {
			t.Fatalf("%s title=%q", profile, d.Title())
		}
	}
	if DescribeProfile(base, base).Footer() != "" {
		t.Fatal("exact full run must add no footer")
	}
	lite, _ := Expand(ProfileLite, base)
	if got := DescribeProfile(lite, base).Footer(); got != "PRism Lite" {
		t.Fatalf("lite footer=%q", got)
	}
	litePlus, _ := Expand(ProfileLitePlus, base)
	if got := DescribeProfile(litePlus, base).Footer(); got != "PRism Lite+" {
		t.Fatalf("lite_plus footer=%q", got)
	}
}

func TestDescribeProfile_OneDeviation(t *testing.T) {
	base := testDefaults()
	lite, _ := Expand(ProfileLite, base)
	lite.Agent.Effort = "high"
	d := DescribeProfile(lite, base)
	if !d.Custom() || d.Title() != "Custom (based on Lite)" || d.Footer() != "PRism Lite (custom)" {
		t.Fatalf("%+v title=%q footer=%q", d, d.Title(), d.Footer())
	}
	if len(d.Deviations) != 1 || d.Deviations[0] != "effort high (default medium)" {
		t.Fatalf("deviations=%v", d.Deviations)
	}
}

func TestDescribeProfile_SeveralDeviations(t *testing.T) {
	base := testDefaults()
	litePlus, _ := Expand(ProfileLitePlus, base)
	litePlus.Agent.Effort = "high"
	litePlus.Agent.WallClockSeconds = 900
	litePlus.Agent.MaxTurns = 150
	d := DescribeProfile(litePlus, base)
	want := "effort high (default medium), wall clock 900 s (default 600 s), max turns 150 (default 120)"
	if got := strings.Join(d.Deviations, ", "); got != want {
		t.Fatalf("got %q want %q", got, want)
	}
	if d.Title() != "Custom (based on Lite+)" {
		t.Fatalf("title=%q", d.Title())
	}

	full := base
	full.FirstPass.Samples = 1
	full.RequiredChecks = false
	d = DescribeProfile(full, base)
	want = "first-pass samples 1 (default 3), required checks off (default on)"
	if got := strings.Join(d.Deviations, ", "); got != want {
		t.Fatalf("full deviations: got %q want %q", got, want)
	}
	if d.Footer() != "PRism Full (custom)" {
		t.Fatalf("footer=%q", d.Footer())
	}
}

func TestDescribeProfile_Legacy(t *testing.T) {
	legacy := Effective{SchemaVersion: 3, Agent: Agent{Enabled: true, Backend: "claude", Model: "x"}, FirstPass: FirstPass{Samples: 3}}
	d := DescribeProfile(legacy, testDefaults())
	if !d.Legacy || d.Profile != ProfileFull || d.Custom() || d.Title() != "Full (legacy config)" || d.Footer() != "" {
		t.Fatalf("%+v title=%q footer=%q", d, d.Title(), d.Footer())
	}
	unnamed := testDefaults()
	unnamed.Profile = ""
	if d := DescribeProfile(unnamed, testDefaults()); !d.Legacy {
		t.Fatalf("schema 4 without a profile is legacy: %+v", d)
	}
}
