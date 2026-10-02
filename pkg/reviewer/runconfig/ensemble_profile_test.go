package runconfig

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestLiteExpandsToTheEnsemble(t *testing.T) {
	e, err := Expand(ProfileLite, Effective{})
	if err != nil {
		t.Fatal(err)
	}
	if e.Agent.Backend != "openrouter" || e.Agent.Model != EnsembleModel || e.Agent.Prompt != PromptLiteArmAV2Budget || e.Agent.Effort != "high" {
		t.Fatalf("agent = %+v", e.Agent)
	}
	if e.Ensemble == nil || e.Ensemble.Runs != 5 || e.Ensemble.Quorum != 4 || e.Ensemble.MinValid != 2 || e.Ensemble.MinSupportCritical != 2 ||
		e.Ensemble.MergeModel != EnsembleMergeModel || e.Ensemble.FallbackProfile != ProfileLiteClassic {
		t.Fatalf("ensemble = %+v", e.Ensemble)
	}
	if e.FirstPass.Enabled || e.Gates || e.RequiredChecks || e.BugMemory {
		t.Fatalf("lite must run no pipeline stages: %+v", e)
	}
	if ProfileLabel(ProfileLite) != "Lite" || ProfileLabel(ProfileLiteClassic) != "Lite (Claude)" {
		t.Fatal("labels")
	}
}

func TestOtherProfilesPersistNoEnsembleBlock(t *testing.T) {
	for _, p := range []string{ProfileFull, ProfileLiteClassic, ProfileLitePlus} {
		e, err := Expand(p, Effective{})
		if err != nil {
			t.Fatal(err)
		}
		b, _ := json.Marshal(e)
		if e.Ensemble != nil || strings.Contains(string(b), `"ensemble"`) {
			t.Errorf("%s gained an ensemble block, which would change its persisted config hash: %s", p, b)
		}
	}
}

func TestRunsRecordedAsLiteBeforeTheEnsembleDescribeAsClassic(t *testing.T) {
	old, err := Expand(ProfileLiteClassic, Effective{})
	if err != nil {
		t.Fatal(err)
	}
	old.Profile = ProfileLite
	d := DescribeProfile(old, Effective{})
	if d.Profile != ProfileLiteClassic || len(d.Deviations) != 0 {
		t.Fatalf("a historical single-agent lite run must describe as classic with no deviations: %+v", d)
	}
}

func TestLiteHygieneEnabledDefaultsOnAndFalseDisables(t *testing.T) {
	t.Setenv("LITE_HYGIENE", "")
	if !LiteHygieneEnabled() {
		t.Fatal("lite hygiene should default on")
	}
	t.Setenv("LITE_HYGIENE", "false")
	if LiteHygieneEnabled() {
		t.Fatal("LITE_HYGIENE=false should disable it")
	}
}
