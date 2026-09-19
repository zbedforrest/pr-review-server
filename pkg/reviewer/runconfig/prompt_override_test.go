package runconfig

import (
	"errors"
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
		if snapshot.Effective.Agent.WallClockSeconds != 300 || snapshot.Effective.Agent.MaxTurns != 60 || snapshot.Effective.BugMemory {
			t.Fatalf("%s: a prompt override must leave the rest of the lite profile alone: %+v", prompt, snapshot.Effective)
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
