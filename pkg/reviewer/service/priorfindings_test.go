package service

import (
	"fmt"
	"strings"
	"testing"

	"pr-review-server/pkg/reviewer/runconfig"
	"pr-review-server/pkg/reviewer/types"
)

func samplePriors() []PriorFinding {
	return []PriorFinding{
		{ID: "a.go:12:aaaaaaaaaaaa", State: "open", Severity: "medium", File: "a.go", LineFrom: 120, LineTo: 129, Headline: "Nil map write on first request"},
		{ID: "b.go:0:bbbbbbbbbbbb", State: "dismissed", Severity: "low", File: "b.go", Headline: "Config key read twice.\nSecond line", Verdicts: []string{"author: intentional"}},
		{ID: "c.go:4:cccccccccccc", State: "fixed", Severity: "critical", File: "c.go", LineFrom: 40, LineTo: 49, Headline: "```fence``` in the headline", Verdicts: []string{"author: fixed", "PRism conceded"}},
	}
}

func TestPriorFindingsSection_EmptyRendersNothing(t *testing.T) {
	if got := priorFindingsSection(nil); got != "" {
		t.Fatalf("empty priors must add nothing, got %q", got)
	}
}

func TestPriorFindingsSection_OneLinePerPriorWithLabels(t *testing.T) {
	got := priorFindingsSection(samplePriors())
	for _, want := range []string{
		"--- FINDINGS ALREADY RAISED ON THIS PR",
		"- P-1 [open, medium] a.go:120-129: Nil map write on first request\n",
		"- P-2 [dismissed, low] b.go: Config key read twice. Second line (author: intentional)\n",
		"- P-3 [fixed, critical] c.go:40-49: '''fence''' in the headline (author: fixed; PRism conceded)\n",
		`set "prior_id" in its finding_contract to that label`,
		`"relates_to" (array of labels)`,
		"Never use a label that is not listed here",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("section lacks %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "a.go:12:aaaaaaaaaaaa") {
		t.Fatal("the raw fingerprint must not be shown; the label stands for it")
	}
}

func TestPriorFindingsSection_CapsRowsAndNamesTheRest(t *testing.T) {
	var many []PriorFinding
	for i := 0; i < priorFindingsMaxRows+5; i++ {
		many = append(many, PriorFinding{ID: fmt.Sprintf("f.go:%d:%012d", i, i), State: "open", File: "f.go", LineFrom: i*10 + 1, LineTo: i*10 + 9, Headline: "Point"})
	}
	got := priorFindingsSection(many)
	if !strings.Contains(got, "- P-40 ") || strings.Contains(got, "- P-41 ") {
		t.Fatalf("exactly %d rows must be listed:\n%s", priorFindingsMaxRows, got)
	}
	if !strings.Contains(got, "...and 5 more earlier findings not listed") {
		t.Fatalf("the cut rows must be counted:\n%s", got)
	}
	if n := len(visiblePriors(many)); n != priorFindingsMaxRows {
		t.Fatalf("visiblePriors = %d", n)
	}
}

func TestPriorFindingsSection_StaysWithinBudget(t *testing.T) {
	long := strings.Repeat("word ", 60)
	var priors []PriorFinding
	for i := 0; i < priorFindingsMaxRows; i++ {
		priors = append(priors, PriorFinding{ID: fmt.Sprintf("f.go:%d:x", i), State: "open", File: "f.go", Headline: long})
	}
	got := priorFindingsSection(priors)
	visible := visiblePriors(priors)
	if len(visible) == 0 || len(visible) == len(priors) {
		t.Fatalf("budget must cut some rows, visible=%d", len(visible))
	}
	rows := strings.Count(got, "\n- P-")
	if rows != len(visible) {
		t.Fatalf("rendered %d rows, visiblePriors says %d", rows, len(visible))
	}
	if strings.Contains(got, fmt.Sprintf("...and %d more", len(priors)-len(visible))) == false {
		t.Fatalf("the rest must be counted:\n%s", got[len(got)-400:])
	}
}

func TestResolvePriorRefs_LabelsBecomeFingerprints(t *testing.T) {
	priors := samplePriors()
	comments := []types.LineComment{
		{FilePath: "a.go", LineNumber: 121, FindingContract: &types.FindingContract{PriorID: " P-1 ", RelatesTo: []string{"P-3", "P-3", "P-1"}}},
		{FilePath: "b.go", LineNumber: 5, FindingContract: &types.FindingContract{PriorID: "b.go:0:bbbbbbbbbbbb"}},
		{FilePath: "SUMMARY"},
	}
	res := ResolvePriorRefs(comments, priors)
	if res.PriorIDs != 2 || res.Related != 1 || res.Unlisted != 0 || res.Duplicate != 0 {
		t.Fatalf("resolution = %+v", res)
	}
	if c := comments[0].FindingContract; c.PriorID != "a.go:12:aaaaaaaaaaaa" || len(c.RelatesTo) != 1 || c.RelatesTo[0] != "c.go:4:cccccccccccc" {
		t.Fatalf("first contract = %+v", c)
	}
	if c := comments[1].FindingContract; c.PriorID != "b.go:0:bbbbbbbbbbbb" {
		t.Fatalf("a listed fingerprint is accepted verbatim: %+v", c)
	}
}

func TestResolvePriorRefs_DropsUnlistedAndSecondClaims(t *testing.T) {
	priors := samplePriors()
	comments := []types.LineComment{
		{FindingContract: &types.FindingContract{PriorID: "P-1"}},
		{FindingContract: &types.FindingContract{PriorID: "P-1", RelatesTo: []string{"P-9", "z.go:1:zzz"}}},
		{FindingContract: &types.FindingContract{PriorID: "P-0"}},
		{FindingContract: &types.FindingContract{PriorID: "something else"}},
	}
	res := ResolvePriorRefs(comments, priors)
	if res.PriorIDs != 1 || res.Duplicate != 1 || res.Unlisted != 4 || res.Related != 0 {
		t.Fatalf("resolution = %+v", res)
	}
	for i := 1; i < len(comments); i++ {
		if c := comments[i].FindingContract; c.PriorID != "" || len(c.RelatesTo) != 0 {
			t.Fatalf("comment %d kept a reference it must not: %+v", i, c)
		}
	}
}

func TestResolvePriorRefs_NothingShownMeansNothingKept(t *testing.T) {
	comments := []types.LineComment{{FindingContract: &types.FindingContract{PriorID: "P-1", RelatesTo: []string{"P-2"}}}}
	res := ResolvePriorRefs(comments, nil)
	if res.Unlisted != 2 || comments[0].FindingContract.PriorID != "" || comments[0].FindingContract.RelatesTo != nil {
		t.Fatalf("references without a shown list must be dropped: %+v %+v", res, comments[0].FindingContract)
	}
}

func TestResolvePriorRefs_LabelsPastTheVisibleCutAreUnlisted(t *testing.T) {
	var many []PriorFinding
	for i := 0; i < priorFindingsMaxRows+1; i++ {
		many = append(many, PriorFinding{ID: fmt.Sprintf("f.go:%d:x", i), State: "open", File: "f.go", Headline: "Point"})
	}
	comments := []types.LineComment{{FindingContract: &types.FindingContract{PriorID: "P-41"}}}
	if res := ResolvePriorRefs(comments, many); res.Unlisted != 1 || comments[0].FindingContract.PriorID != "" {
		t.Fatalf("P-41 was never shown: %+v", res)
	}
}

func TestBuildAgentPromptContent_PriorsSitAfterChecksBeforeClaims(t *testing.T) {
	without, err := buildAgentPromptContent("main", nil, prContextSection("Title", "", nil), nil, nil, nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(without, "ALREADY RAISED") {
		t.Fatal("no section without priors")
	}
	memory := []BugMemoryEntry{{ID: "bm", Pattern: "Forgot to wire the setting."}}
	withMemory, err := buildAgentPromptContent("main", nil, prContextSection("Title", "", nil), nil, nil, memory, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	with, err := buildAgentPromptContent("main", nil, prContextSection("Title", "", nil), nil, nil, memory, nil, samplePriors())
	if err != nil {
		t.Fatal(err)
	}
	memoryAt := strings.Index(with, "THIS REPO'S BUG HISTORY")
	priorsAt := strings.Index(with, "FINDINGS ALREADY RAISED ON THIS PR")
	claimsAt := strings.Index(with, "--- FIRST-PASS CLAIMS")
	if priorsAt < 0 || !(memoryAt < priorsAt && priorsAt < claimsAt) {
		t.Fatalf("section order wrong: memory=%d priors=%d claims=%d", memoryAt, priorsAt, claimsAt)
	}
	if strings.Replace(with, priorFindingsSection(samplePriors()), "", 1) != withMemory {
		t.Fatal("the section must be the only difference")
	}
}

func TestBuildLitePrompts_PriorsShownByEveryShape(t *testing.T) {
	priors := samplePriors()
	for _, prompt := range []string{runconfig.PromptLiteArmA, runconfig.PromptLiteArmASub, runconfig.PromptLiteArmAV2, runconfig.PromptLiteArmAV2Sub, runconfig.PromptLiteArmAV3, runconfig.PromptLiteArmAV2Budget} {
		without := buildLitePrompt(prompt, "main", liteDiff{Text: "d"}, prContextSection("Title", "", nil), nil, nil)
		if strings.Contains(without, "ALREADY RAISED") {
			t.Fatalf("%s: no section without priors", prompt)
		}
		with := buildLitePrompt(prompt, "main", liteDiff{Text: "d"}, prContextSection("Title", "", nil), nil, priors)
		section := strings.Index(with, "FINDINGS ALREADY RAISED ON THIS PR")
		ctx := strings.Index(with, "--- PULL REQUEST ---")
		format := strings.Index(with, "**Output format (STRICT):**")
		if section < 0 || !(ctx < section && section < format) {
			t.Fatalf("%s: section must sit between the PR context and the output format: ctx=%d section=%d format=%d", prompt, ctx, section, format)
		}
		if strings.Replace(with, priorFindingsSection(priors), "", 1) != without {
			t.Fatalf("%s: the section must be the only difference", prompt)
		}
	}
}

func TestLiteFindingsJSONSchema_AllowsPriorReferences(t *testing.T) {
	for _, want := range []string{`"prior_id":{"type":"string"}`, `"relates_to":{"type":"array","items":{"type":"string"}}`} {
		if !strings.Contains(liteFindingsJSONSchema, want) {
			t.Errorf("schema lacks %s", want)
		}
	}
}

func TestParseAgentJSON_KeepsPriorReferencesOnTheContract(t *testing.T) {
	raw := `[{"id":"A-1","file_path":"a.go","line_number":121,"comment_body":"Still writes a nil map.","importance":"MEDIUM","finding_contract":{"schema_version":1,"finding_kind":"production_behavior","materiality":"current_impact","current_impact":"First request panics.","counterfactual_trigger":null,"falsifiability":"falsifiable","falsifiable_condition":"Call it once.","expected_observable":"Panic.","subjects":[{"kind":"symbol","path":"a.go","name":"handle"}],"uncertainty":"None.","severity_rationale":"Panic.","headline":"Nil map write on first request","prior_id":" P-1 ","relates_to":["P-3"," P-3 ",""]}}]`
	comments, err := parseAgentJSON(raw)
	if err != nil {
		t.Fatal(err)
	}
	c := comments[0].FindingContract
	if c.PriorID != "P-1" || len(c.RelatesTo) != 1 || c.RelatesTo[0] != "P-3" {
		t.Fatalf("contract references = %q %v", c.PriorID, c.RelatesTo)
	}
	if err := types.ValidateFindingContract(c); err != nil {
		t.Fatalf("references must not fail the contract: %v", err)
	}
}

func TestResolvePriorRefs_AnInactiveEntryHoldsNoClaim(t *testing.T) {
	priors := []PriorFinding{{ID: "a.go:1:prior"}}
	comments := []types.LineComment{
		{FilePath: "a.go", LineNumber: 10, Inactive: true, FindingContract: &types.FindingContract{PriorID: "P-1"}},
		{FilePath: "a.go", LineNumber: 12, FindingContract: &types.FindingContract{PriorID: "P-1", RelatesTo: []string{"P-1"}}},
	}
	res := ResolvePriorRefs(comments, priors)
	if comments[0].FindingContract.PriorID != "" {
		t.Fatalf("inactive entry keeps a claim: %+v", comments[0].FindingContract)
	}
	if comments[1].FindingContract.PriorID != "a.go:1:prior" || len(comments[1].FindingContract.RelatesTo) != 0 {
		t.Fatalf("the surviving entry takes the prior: %+v", comments[1].FindingContract)
	}
	if res.PriorIDs != 1 || res.Duplicate != 0 {
		t.Fatalf("resolution = %+v", res)
	}
}

func TestMergeFindings_AFoldedDuplicateKeepsItsPriorOnTheSurvivor(t *testing.T) {
	survivor := types.LineComment{FilePath: "a.go", LineNumber: 10, Importance: "MEDIUM", CommentBody: "The handler writes to a nil map.", FindingContract: &types.FindingContract{SchemaVersion: 1, RelatesTo: []string{"b.go:0:other"}}}
	dup := types.LineComment{FilePath: "a.go", LineNumber: 12, Importance: "LOW", CommentBody: "The handler writes to a nil map on the retry path.", FindingContract: &types.FindingContract{SchemaVersion: 1, PriorID: "a.go:1:prior", RelatesTo: []string{"c.go:0:third", "b.go:0:other"}}}
	merged, records := MergeFindingsWithRecords(FindingSet{Comments: []types.LineComment{survivor, dup}})
	if len(merged) != 1 || len(records) != 1 {
		t.Fatalf("merged=%d records=%d", len(merged), len(records))
	}
	c := merged[0].FindingContract
	if c.PriorID != "a.go:1:prior" {
		t.Fatalf("survivor takes the folded prior: %+v", c)
	}
	if got := strings.Join(c.RelatesTo, ","); got != "b.go:0:other,c.go:0:third" {
		t.Fatalf("relates_to merged without repeats: %q", got)
	}
}

func TestPriorFindingLine_SettledElsewhereNamesThePRAndTheReason(t *testing.T) {
	p := PriorFinding{ID: "settled:596:4001", State: "settled", File: "store/models.py", Headline: "Buyer lookup hardcodes purchaser_id", SettledOnPR: 596, Reason: "intentional, the backfill is done"}
	got := priorFindingLine(p)
	want := "[settled on #596] store/models.py: Buyer lookup hardcodes purchaser_id (author: intentional, the backfill is done)"
	if got != want {
		t.Fatalf("line = %q, want %q", got, want)
	}
	if section := priorFindingsSection([]PriorFinding{p}); !strings.Contains(section, "- P-1 "+want) || !strings.Contains(section, `A point marked "settled on #N"`) {
		t.Fatalf("section:\n%s", section)
	}
}
