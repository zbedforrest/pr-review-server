package runconfig

import (
	"errors"
	"strings"
	"testing"
)

func TestResolveAllowsLitePromptOverrideOnLiteProfile(t *testing.T) {
	for _, prompt := range []string{PromptLiteArmA, PromptLiteArmASub, PromptLiteArmAV2, PromptLiteArmAV2Sub, PromptLiteArmAV3} {
		snapshot, err := Resolve(Overrides{Profile: strPtr("lite"), Agent: &AgentOverrides{Prompt: strPtr(prompt)}}, testDefaults(), litePolicy())
		if err != nil {
			t.Fatalf("%s: %v", prompt, err)
		}
		if snapshot.Effective.Agent.Prompt != prompt || snapshot.Sources["agent.prompt"] != SourceRequest {
			t.Fatalf("%s: effective=%+v sources=%v", prompt, snapshot.Effective.Agent, snapshot.Sources)
		}
		if snapshot.Effective.Agent.WallClockSeconds != 300 || snapshot.Effective.Agent.MaxTurns != 60 {
			t.Fatalf("%s: a prompt override must leave the lite budgets alone: %+v", prompt, snapshot.Effective)
		}
		if snapshot.Effective.BugMemory != PromptUsesBugMemory(prompt) || snapshot.Sources["bug_memory"] != SourceDerived {
			t.Fatalf("%s: bug memory must follow the prompt shape: memory=%t sources=%v", prompt, snapshot.Effective.BugMemory, snapshot.Sources)
		}
		if (prompt == PromptLiteArmAV3 || prompt == PromptLiteArmA || prompt == PromptLiteArmASub) != snapshot.Effective.BugMemory {
			t.Fatalf("%s: memory=%t", prompt, snapshot.Effective.BugMemory)
		}
	}
}

func TestResolveLitePromptOverrideChangesTheHashAndDescribesAsCustom(t *testing.T) {
	plain, err := Resolve(Overrides{Profile: strPtr("lite")}, testDefaults(), litePolicy())
	if err != nil {
		t.Fatal(err)
	}
	v2, err := Resolve(Overrides{Profile: strPtr("lite"), Agent: &AgentOverrides{Prompt: strPtr(PromptLiteArmA)}}, testDefaults(), litePolicy())
	if err != nil {
		t.Fatal(err)
	}
	if plain.Hash == v2.Hash {
		t.Fatal("the prompt must be part of the config hash")
	}
	description := DescribeProfile(v2.Effective, testDefaults())
	if !description.Custom() || description.Footer() != "PRism Lite (custom)" {
		t.Fatalf("description=%+v footer=%q", description, description.Footer())
	}
	if DescribeProfile(plain.Effective, testDefaults()).Custom() {
		t.Fatal("the default lite prompt is not a deviation")
	}
	if plain.Effective.Agent.Prompt != PromptLiteArmAV2 || plain.Effective.BugMemory {
		t.Fatalf("lite default must be v2 without bug memory: %+v", plain.Effective)
	}
}

func TestResolveRejectsPromptOverridesOutsideTheProfileFamily(t *testing.T) {
	var verr *ValidationError
	_, err := Resolve(Overrides{Profile: strPtr("lite"), Agent: &AgentOverrides{Prompt: strPtr(PromptPipeline)}}, testDefaults(), litePolicy())
	if !errors.As(err, &verr) || verr.Field != "agent.prompt" {
		t.Fatalf("pipeline prompt on lite: err=%v", err)
	}
	_, err = Resolve(Overrides{Agent: &AgentOverrides{Prompt: strPtr(PromptLiteArmAV2)}}, testDefaults(), litePolicy())
	if !errors.As(err, &verr) || verr.Field != "agent.prompt" {
		t.Fatalf("lite prompt on full: err=%v", err)
	}
	_, err = Resolve(Overrides{Profile: strPtr("lite"), Agent: &AgentOverrides{Prompt: strPtr("lite_arm_b")}}, testDefaults(), litePolicy())
	if !errors.As(err, &verr) || verr.Field != "agent.prompt" {
		t.Fatalf("unknown prompt: err=%v", err)
	}
}

func TestValidateChecksThePromptEvenWhenTheAgentIsDisabled(t *testing.T) {
	off := false
	var verr *ValidationError
	_, err := Resolve(Overrides{Agent: &AgentOverrides{Enabled: &off, Prompt: strPtr(PromptLiteArmAV2)}}, testDefaults(), testPolicy())
	if !errors.As(err, &verr) || verr.Field != "agent.prompt" {
		t.Fatalf("cross-family prompt with the agent off: err=%v", err)
	}
	_, err = Resolve(Overrides{Agent: &AgentOverrides{Enabled: &off, Prompt: strPtr("lite_arm_b")}}, testDefaults(), testPolicy())
	if !errors.As(err, &verr) || verr.Field != "agent.prompt" {
		t.Fatalf("unknown prompt with the agent off: err=%v", err)
	}
	if _, err := Resolve(Overrides{Agent: &AgentOverrides{Enabled: &off}}, testDefaults(), testPolicy()); err != nil {
		t.Fatalf("a first-pass-only review must still resolve: %v", err)
	}
}

func TestResolveRejectsLiteWhenTheCeilingCannotCoverCappedDiffs(t *testing.T) {
	policy := litePolicy()
	policy.MaxWallClockSeconds = 300
	var verr *ValidationError
	_, err := Resolve(Overrides{Profile: strPtr("lite")}, testDefaults(), policy)
	if !errors.As(err, &verr) || verr.Field != "agent.wall_clock_seconds" || !strings.Contains(verr.Message, "360") {
		t.Fatalf("ceiling 300: err=%v", err)
	}
	policy.MaxWallClockSeconds = 360
	if _, err := Resolve(Overrides{Profile: strPtr("lite")}, testDefaults(), policy); err != nil {
		t.Fatalf("ceiling 360 must admit lite: %v", err)
	}
	wall := 300
	if _, err := Resolve(Overrides{Profile: strPtr("lite"), Agent: &AgentOverrides{WallClockSeconds: &wall}}, testDefaults(), policy); err != nil {
		t.Fatalf("an explicit 300 still carries the profile default budget: %v", err)
	}
}
