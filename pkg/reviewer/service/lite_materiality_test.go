package service

import (
	"strings"
	"testing"

	"pr-review-server/pkg/reviewer/runconfig"
)

func TestLiteCurrentImpactFailures_OffByDefaultAndBeforeTheDiffWhenOn(t *testing.T) {
	const rule = "Materiality of failure paths"
	t.Setenv("LITE_CURRENT_IMPACT_FAILURES", "")
	if p := buildLitePrompt(runconfig.PromptLiteArmAV2, "main", liteDiff{Text: "diff"}, "", nil, nil); strings.Contains(p, rule) {
		t.Fatal("the rule is in the prompt with the flag off")
	}
	t.Setenv("LITE_CURRENT_IMPACT_FAILURES", "true")
	p := buildLitePrompt(runconfig.PromptLiteArmAV2, "main", liteDiff{Text: "diff"}, "", nil, nil)
	if i := strings.Index(p, rule); i < 0 || i > strings.Index(p, "<diff>") {
		t.Fatalf("the rule is missing or after the diff:\n%s", p)
	}
}
