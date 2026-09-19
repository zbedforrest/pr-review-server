package service

import (
	"encoding/json"
	"strings"
	"testing"

	"pr-review-server/pkg/reviewer/runconfig"
	"pr-review-server/pkg/reviewer/types"
)

const armAOpeningForMain = "Review this PR.\n\nThe working directory is a checkout of the PR head (`HEAD`); the base is `origin/main`. The complete diff (`git diff --find-renames -U12 origin/main...HEAD`) is below. Do not fetch it again; use shell commands only to read surrounding code. No dependencies are installed in this checkout and CI already passed on this PR: do not run tests, linters, type checkers, builds or package managers. Review by reading.\n"

func TestBuildLitePromptV2Content_ArmAVerbatimThenContextThenCompactFormatThenDiff(t *testing.T) {
	prompt := buildLitePromptV2Content("main", liteDiff{Text: "diff --git a/x b/x\n+1\n", Source: diffSourceGit}, prContextSection("Title", "Body", nil), nil)
	if !strings.HasPrefix(prompt, armAOpeningForMain) {
		t.Fatalf("prompt must open with the Arm A text verbatim:\n%s", prompt[:500])
	}
	ctx := strings.Index(prompt, "--- PULL REQUEST ---")
	format := strings.Index(prompt, "**Output format (STRICT):**")
	contract := strings.Index(prompt, `"finding_contract": an object`)
	summary := strings.Index(prompt, `Add exactly one summary entry`)
	diff := strings.Index(prompt, "<diff>\ndiff --git a/x b/x\n+1\n</diff>")
	for name, idx := range map[string]int{"ctx": ctx, "format": format, "contract": contract, "summary": summary, "diff": diff} {
		if idx < 0 {
			t.Fatalf("%s missing from prompt:\n%s", name, prompt)
		}
	}
	if !(len(armAOpeningForMain) <= ctx && ctx < format && format < contract && contract < summary && summary < diff) {
		t.Fatalf("sections out of order: ctx=%d format=%d contract=%d summary=%d diff=%d", ctx, format, contract, summary, diff)
	}
	if !strings.HasSuffix(prompt, "</diff>\n") {
		t.Fatalf("diff must close the prompt: %q", prompt[len(prompt)-40:])
	}
}

func TestBuildLitePromptV2Content_KeepsArmAOpeningForTruncatedDiff(t *testing.T) {
	prompt := buildLitePromptV2Content("main", liteDiff{Text: "stat\n[diff truncated after 60000 characters of 70000; fetch the remaining files with `git diff origin/main...HEAD -- <path>`]\n", Source: diffSourceGit, Truncated: true}, "", nil)
	if !strings.HasPrefix(prompt, armAOpeningForMain) {
		t.Fatalf("truncation must not change the Arm A opening:\n%s", prompt[:500])
	}
	if !strings.Contains(prompt, "[diff truncated after 60000 characters") {
		t.Fatal("the truncation note must travel inside the diff block")
	}
}

func TestBuildLitePromptV2Content_IgnoresBugMemoryUnlessGiven(t *testing.T) {
	entries := []BugMemoryEntry{{ID: "bm-1", Pattern: "Forgot to wire the new setting."}}
	v2 := buildLitePrompt(runconfig.PromptLiteArmAV2, "main", liteDiff{Text: "d"}, "", entries)
	if strings.Contains(v2, "BUG HISTORY") || strings.Contains(v2, "Forgot to wire") {
		t.Fatal("v2 must not render bug memory even when entries matched")
	}
	v3 := buildLitePrompt(runconfig.PromptLiteArmAV3, "main", liteDiff{Text: "d"}, prContextSection("Title", "", nil), entries)
	section := strings.Index(v3, "THIS REPO'S BUG HISTORY")
	ctx := strings.Index(v3, "--- PULL REQUEST ---")
	format := strings.Index(v3, "**Output format (STRICT):**")
	if section < 0 || !strings.Contains(v3, "Forgot to wire the new setting.") || !(ctx < section && section < format) {
		t.Fatalf("v3 must render bug memory between the PR context and the output format: ctx=%d section=%d format=%d\n%s", ctx, section, format, v3)
	}
	if strings.Replace(v3, bugMemorySection(entries), "", 1) != buildLitePrompt(runconfig.PromptLiteArmAV2, "main", liteDiff{Text: "d"}, prContextSection("Title", "", nil), entries) {
		t.Fatal("v3 must equal v2 plus the bug memory section")
	}
}

func TestBuildLitePrompt_LegacyNamesStillUseTheFirstBuilder(t *testing.T) {
	entries := []BugMemoryEntry{{ID: "bm-1", Pattern: "Forgot to wire the new setting."}}
	got := buildLitePrompt(runconfig.PromptLiteArmA, "main", liteDiff{Text: "d"}, "", entries)
	if got != buildLitePromptContent("main", liteDiff{Text: "d"}, "", entries, false) {
		t.Fatal("lite_arm_a must be unchanged")
	}
	plus := buildLitePrompt(runconfig.PromptLiteArmASub, "main", liteDiff{Text: "d"}, "", entries)
	if plus != buildLitePromptContent("main", liteDiff{Text: "d"}, "", entries, true) {
		t.Fatal("lite_arm_a_sub must be unchanged")
	}
}

func TestPromptLiteOutputFormatV2_IsCompactAndNamesEveryContractField(t *testing.T) {
	lines := strings.Count(strings.TrimSpace(promptLiteOutputFormatV2), "\n") + 1
	if lines > 25 {
		t.Fatalf("output block is %d lines, want at most 25", lines)
	}
	for _, required := range []string{
		`{"findings": [...]}`, `"id"`, `"file_path"`, `"line_number"`, `"comment_body"`, `"importance"`, `"finding_contract"`,
		`"schema_version": 1`, `"finding_kind"`, `"materiality"`, `"current_impact"`, `"counterfactual_trigger"`,
		`"falsifiability"`, `"falsifiable_condition"`, `"expected_observable"`, `"subjects"`, `"uncertainty"`, `"severity_rationale"`, `"headline"`,
		`"SUMMARY"`, `"verdict"`, `"upshot"`, `"priority_ids"`, `"notes"`,
		"production_behavior", "security_risk", "latent_hazard", "design_opinion", "description_drift", "test_quality", "operational_risk",
		"current_impact", "future_condition_only", "no_user_impact", "not_falsifiable",
		"return the SUMMARY entry only",
	} {
		if !strings.Contains(promptLiteOutputFormatV2, required) {
			t.Errorf("compact output block must mention %s", required)
		}
	}
	for _, banned := range []string{"first-pass", "first pass", "source_id", "disposition", "CHECK entries", "blast radius"} {
		if strings.Contains(promptLiteOutputFormatV2, banned) {
			t.Errorf("compact output block must not mention %q", banned)
		}
	}
}

func TestLiteFindingsJSONSchema_ParsesAndMatchesTheContractVocabulary(t *testing.T) {
	var schema map[string]any
	if err := json.Unmarshal([]byte(liteFindingsJSONSchema), &schema); err != nil {
		t.Fatalf("schema is not JSON: %v", err)
	}
	findings := schema["properties"].(map[string]any)["findings"].(map[string]any)
	items := findings["items"].(map[string]any)
	props := items["properties"].(map[string]any)
	contract := props["finding_contract"].(map[string]any)["properties"].(map[string]any)
	enum := func(field map[string]any) []string {
		var out []string
		for _, v := range field["enum"].([]any) {
			out = append(out, v.(string))
		}
		return out
	}
	for name, values := range map[string][]string{
		"finding_kind":   enum(contract["finding_kind"].(map[string]any)),
		"materiality":    enum(contract["materiality"].(map[string]any)),
		"falsifiability": enum(contract["falsifiability"].(map[string]any)),
	} {
		for _, v := range values {
			probe := &types.FindingContract{SchemaVersion: 1, FindingKind: "production_behavior", Materiality: "unknown", Falsifiability: "unknown",
				CurrentImpact: "x", Uncertainty: "x", SeverityRationale: "x", Subjects: []types.FindingSubject{{Kind: "file", Path: "a"}}}
			switch name {
			case "finding_kind":
				probe.FindingKind = v
				if v == "latent_hazard" {
					probe.Materiality = "future_condition_only"
					trigger := "later"
					probe.CounterfactualTrigger = &trigger
				}
				if v == "design_opinion" || v == "description_drift" {
					probe.Falsifiability = "not_falsifiable"
					probe.Materiality = "no_user_impact"
				}
			case "materiality":
				probe.Materiality = v
				if v == "future_condition_only" {
					probe.FindingKind = "security_risk"
					trigger := "later"
					probe.CounterfactualTrigger = &trigger
				}
			case "falsifiability":
				probe.Falsifiability = v
				if v == "falsifiable" {
					cond, obs := "c", "o"
					probe.FalsifiableCondition, probe.ExpectedObservable = &cond, &obs
				}
			}
			if err := types.ValidateFindingContract(probe); err != nil {
				t.Errorf("schema enum %s=%q is rejected by the contract validator: %v", name, v, err)
			}
		}
	}
	if kinds := enum(contract["subjects"].(map[string]any)["items"].(map[string]any)["properties"].(map[string]any)["kind"].(map[string]any)); len(kinds) != 7 {
		t.Fatalf("subject kinds=%v", kinds)
	}
	if verdicts := enum(props["summary"].(map[string]any)["properties"].(map[string]any)["verdict"].(map[string]any)); strings.Join(verdicts, ",") != "approve,approve_suggestions,request_changes" {
		t.Fatalf("verdicts=%v", verdicts)
	}
	required := items["required"].([]any)
	if len(required) != 2 || required[0] != "file_path" || required[1] != "line_number" {
		t.Fatalf("items must require only the anchor fields so the SUMMARY entry fits: %v", required)
	}
}

func TestParseAgentJSON_AcceptsTheFindingsObjectWrapper(t *testing.T) {
	raw := `{"findings":[{"id":"A-1","file_path":"a.py","line_number":3,"importance":"LOW","comment_body":"x is unused","finding_contract":{"schema_version":1,"finding_kind":"design_opinion","materiality":"no_user_impact","current_impact":"None","counterfactual_trigger":null,"falsifiability":"not_falsifiable","falsifiable_condition":null,"expected_observable":null,"subjects":[{"kind":"file","path":"a.py"}],"uncertainty":"Low","severity_rationale":"Nit","headline":"x is unused"}},{"file_path":"SUMMARY","line_number":0,"summary":{"verdict":"approve_suggestions","upshot":"u","priority_ids":["A-1"],"notes":"n"}}]}`
	comments, err := parseAgentJSON(raw)
	if err != nil {
		t.Fatal(err)
	}
	if len(comments) != 2 || comments[0].FilePath != "a.py" || comments[1].FilePath != "SUMMARY" || comments[1].Summary == nil || comments[1].Summary.Verdict != "approve_suggestions" {
		t.Fatalf("comments=%+v", comments)
	}
	if types.ContractStatus(comments[0].FindingContract) != "valid" {
		t.Fatalf("contract status=%s", types.ContractStatus(comments[0].FindingContract))
	}
}

func TestArgsWithToolsAndSchema_PassesTheSchemaOnlyForTheV2AndV3Prompts(t *testing.T) {
	rt := agentRuntime{backend: AgentBackendClaude, model: "m", effort: "medium"}
	for _, prompt := range []string{runconfig.PromptPipeline, runconfig.PromptLiteArmA, runconfig.PromptLiteArmASub} {
		if liteJSONSchema(prompt) != "" {
			t.Errorf("%s must not carry a JSON schema", prompt)
		}
	}
	for _, prompt := range []string{runconfig.PromptLiteArmAV2, runconfig.PromptLiteArmAV3} {
		args := rt.argsWithToolsAndSchema("p", "", liteJSONSchema(prompt))
		if args[len(args)-2] != "--json-schema" || args[len(args)-1] != liteFindingsJSONSchema {
			t.Errorf("%s args must end with --json-schema <schema>: %v", prompt, args[len(args)-3:])
		}
	}
	plain := rt.argsWithToolsAndSchema("p", "", "")
	if strings.Contains(strings.Join(plain, " "), "--json-schema") || strings.Join(plain, " ") != strings.Join(rt.argsWithTools("p", ""), " ") {
		t.Fatalf("empty schema must leave the argv unchanged: %v", plain)
	}
	codex := agentRuntime{backend: AgentBackendOpenRouter, model: "m", effort: "medium", openRouterBaseURL: "https://example.test"}
	if strings.Contains(strings.Join(codex.argsWithToolsAndSchema("p", "", liteFindingsJSONSchema), " "), "--json-schema") {
		t.Fatal("codex has no --json-schema flag")
	}
}
