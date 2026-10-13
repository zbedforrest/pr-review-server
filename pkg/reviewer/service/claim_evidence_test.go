package service

import (
	"strings"
	"testing"

	"pr-review-server/pkg/reviewer/runconfig"
)

func TestClaimEvidenceRules_OffByDefaultAndInEveryPromptWhenOn(t *testing.T) {
	t.Setenv("AGENT_CLAIM_EVIDENCE", "")
	full, err := buildAgentPromptContent("main", nil, "", nil, nil, nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	lite := buildLitePrompt(runconfig.PromptLiteArmA, "main", liteDiff{Text: "diff"}, "", nil, nil)
	liteV2 := buildLitePrompt(runconfig.PromptLiteArmAV2, "main", liteDiff{Text: "diff"}, "", nil, nil)
	for name, prompt := range map[string]string{"full": full, "lite": lite, "lite v2": liteV2} {
		if strings.Contains(prompt, "Evidence rules for two kinds of claim") {
			t.Errorf("%s prompt carries the rules with the flag off", name)
		}
	}

	t.Setenv("AGENT_CLAIM_EVIDENCE", "true")
	full, _ = buildAgentPromptContent("main", nil, "", nil, nil, nil, nil, nil)
	lite = buildLitePrompt(runconfig.PromptLiteArmA, "main", liteDiff{Text: "diff"}, "", nil, nil)
	liteV2 = buildLitePrompt(runconfig.PromptLiteArmAV2, "main", liteDiff{Text: "diff"}, "", nil, nil)
	for name, prompt := range map[string]string{"full": full, "lite": lite, "lite v2": liteV2} {
		if !strings.Contains(prompt, "unverified against the pinned version") || !strings.Contains(prompt, "must quote the removed line from the diff") {
			t.Errorf("%s prompt lacks the rules with the flag on", name)
		}
		if strings.Index(prompt, "Evidence rules") > strings.Index(prompt, "<diff>") && strings.Contains(prompt, "<diff>") {
			t.Errorf("%s prompt puts the rules after the diff", name)
		}
	}
}
