package service

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"pr-review-server/pkg/reviewer/runconfig"
	"pr-review-server/pkg/reviewer/types"
)

func TestBuildLitePromptContent_ArmATextThenContextThenOutputFormatThenDiff(t *testing.T) {
	prompt := buildLitePromptContent("main", liteDiff{Text: "diff --git a/x b/x\n+1\n", Source: diffSourceGit}, prContextSection("Title", "Body", nil), nil, false)
	arm := strings.Index(prompt, "Review this PR.")
	base := strings.Index(prompt, "the base is `origin/main`")
	fetch := strings.Index(prompt, "`git diff --find-renames -U12 origin/main...HEAD`")
	ctx := strings.Index(prompt, "Title")
	format := strings.Index(prompt, "**Output format (STRICT):**")
	contract := strings.Index(prompt, `"finding_contract" (object)`)
	summary := strings.Index(prompt, `Include exactly one "SUMMARY" entry`)
	diff := strings.Index(prompt, "<diff>\ndiff --git a/x b/x\n+1\n</diff>")
	for name, idx := range map[string]int{"arm": arm, "base": base, "fetch": fetch, "ctx": ctx, "format": format, "contract": contract, "summary": summary, "diff": diff} {
		if idx < 0 {
			t.Fatalf("%s missing from prompt:\n%s", name, prompt)
		}
	}
	if !(arm < base && base < ctx && ctx < format && format < contract && contract < summary && summary < diff) {
		t.Fatalf("sections out of order: arm=%d base=%d ctx=%d format=%d contract=%d summary=%d diff=%d", arm, base, ctx, format, contract, summary, diff)
	}
	if !strings.HasSuffix(prompt, "</diff>\n") {
		t.Fatalf("diff must close the prompt: %q", prompt[len(prompt)-40:])
	}
	if !strings.Contains(prompt, "do not run tests, linters, type checkers, builds or package managers") {
		t.Fatal("no-tooling instruction missing")
	}
	if strings.Contains(prompt, "CI already passed") {
		t.Fatal("the prompt must not assert a CI outcome it cannot know")
	}
}

func TestBuildLitePromptContent_TruncatedDiffChangesTheOpening(t *testing.T) {
	complete := buildLitePromptContent("main", liteDiff{Text: "d", Source: diffSourceGit}, "", nil, false)
	if !strings.Contains(complete, "The complete diff") || strings.Contains(complete, "too large to inline") {
		t.Fatalf("complete opening wrong:\n%s", complete[:400])
	}
	partial := buildLitePromptContent("main", liteDiff{Text: "d", Source: diffSourceGit, Truncated: true}, "", nil, false)
	if strings.Contains(partial, "The complete diff") || strings.Contains(partial, "Do not fetch it again") ||
		!strings.Contains(partial, "too large to inline in full") || !strings.Contains(partial, "Read every changed file that is not shown in full") {
		t.Fatalf("truncated opening wrong:\n%s", partial[:600])
	}
}

func TestBuildLitePromptContent_NoFirstPassClaimsOrDispositionText(t *testing.T) {
	prompt := buildLitePromptContent("main", liteDiff{}, "", nil, false)
	for _, banned := range []string{"first-pass", "first pass", "FIRST-PASS", "source_id", "disposition", "Disposition", "PR SCOPE", "MECHANICAL ALERTS", "REQUIRED CHECKS (answer"} {
		if strings.Contains(prompt, banned) {
			t.Errorf("lite prompt must not mention %q", banned)
		}
	}
	for _, required := range []string{`"schema_version": 1`, `"materiality"`, `"headline"`, `"verdict" (string)`, `"priority_ids"`, `return the SUMMARY entry only`} {
		if !strings.Contains(prompt, required) {
			t.Errorf("lite prompt must keep the output contract text %q", required)
		}
	}
	if !strings.Contains(promptAgentReview, promptAgentContractFields) || !strings.Contains(promptLiteOutputFormat, promptAgentContractFields) {
		t.Fatal("both prompts must share the contract block verbatim")
	}
}

func TestBuildLitePromptContent_BugMemorySectionOnlyWhenMatched(t *testing.T) {
	without := buildLitePromptContent("main", liteDiff{Text: "d"}, "", nil, false)
	if strings.Contains(without, "BUG HISTORY") {
		t.Fatal("no bug memory section without matches")
	}
	with := buildLitePromptContent("main", liteDiff{Text: "d"}, "", []BugMemoryEntry{{ID: "bm-1", Pattern: "Forgot to wire the new setting."}}, false)
	section := strings.Index(with, "THIS REPO'S BUG HISTORY")
	format := strings.Index(with, "**Output format (STRICT):**")
	if section < 0 || !strings.Contains(with, "Forgot to wire the new setting.") || section > format {
		t.Fatalf("bug memory section must precede the output format:\n%s", with)
	}
}

func TestBuildLitePromptContent_SubAgentSentenceOnlyForLitePlus(t *testing.T) {
	const sentence = "Spawn sub-agents to investigate independent parts of the change in parallel"
	if strings.Contains(buildLitePromptContent("main", liteDiff{Text: "d"}, "", nil, false), sentence) {
		t.Fatal("lite must not ask for sub-agents")
	}
	plus := buildLitePromptContent("main", liteDiff{Text: "d"}, "", nil, true)
	at := strings.Index(plus, sentence)
	format := strings.Index(plus, "**Output format (STRICT):**")
	if at < 0 || at > format {
		t.Fatalf("lite_plus sentence must sit before the output format: at=%d format=%d", at, format)
	}
}

func liteWorktree(t *testing.T) (dir, sha string) {
	t.Helper()
	bare, sha := setupLocalBareRepo(t)
	cloneRoot := t.TempDir()
	seedAgentCache(t, cloneRoot, "acme", "example", bare)
	dir = filepath.Join(cloneRoot, "wt")
	cleanup, err := cloneForAgent(context.Background(), cloneRoot, dir, "acme", "example", "main", 1, sha, "")
	if err != nil {
		t.Fatalf("cloneForAgent: %v", err)
	}
	t.Cleanup(func() { _ = cleanup() })
	return dir, sha
}

func TestRenderedDiff_UnderCapIsVerbatim(t *testing.T) {
	dir, _ := liteWorktree(t)
	got := renderedDiff(context.Background(), dir, "main", "api diff")
	if got.Source != diffSourceGit || got.Truncated {
		t.Fatalf("source=%q truncated=%t", got.Source, got.Truncated)
	}
	if !strings.Contains(got.Text, "diff --git a/change.txt b/change.txt") || !strings.Contains(got.Text, "+new") || strings.Contains(got.Text, "truncated") {
		t.Fatalf("diff=%q", got.Text)
	}
}

func TestRenderedDiff_OverCapEmitsStatHeadAndPerPathHint(t *testing.T) {
	full := strings.Repeat("+"+strings.Repeat("x", 99)+"\n", 700)
	out := capDiff(full, " change.txt | 700 +\n", "fetch the remaining files with `git diff origin/main...HEAD -- <path>`")
	if !strings.HasPrefix(out, " change.txt | 700 +\n") {
		t.Fatalf("stat must lead: %q", out[:60])
	}
	if !strings.Contains(out, "[diff truncated after 60000 characters of 70700; fetch the remaining files with `git diff origin/main...HEAD -- <path>`]") {
		t.Fatalf("hint missing: %q", out[len(out)-200:])
	}
	body := out[len(" change.txt | 700 +\n\n"):]
	head := body[:strings.Index(body, "\n[diff truncated")]
	if len(head) > diffInlineLimit || !strings.HasSuffix(head, "\n") {
		t.Fatalf("head must be cut at the last newline within the cap: len=%d", len(head))
	}
}

func TestRenderedDiff_FallsBackToAPIDiffWhenGitFails(t *testing.T) {
	skipIfNoGit(t)
	got := renderedDiff(context.Background(), t.TempDir(), "main", "diff --git a/api b/api\n+from api\n")
	if got.Source != diffSourceAPI || got.Truncated || got.Text != "diff --git a/api b/api\n+from api\n" {
		t.Fatalf("got=%+v", got)
	}
}

func TestRenderedDiff_OverCapAPIFallbackListsPathsAndReadHint(t *testing.T) {
	skipIfNoGit(t)
	var api strings.Builder
	for i := 0; i < 8; i++ {
		fmt.Fprintf(&api, "diff --git a/pkg/f%d.go b/pkg/f%d.go\n", i, i)
		api.WriteString(strings.Repeat("+"+strings.Repeat("y", 99)+"\n", 100))
	}
	got := renderedDiff(context.Background(), t.TempDir(), "main", api.String())
	if got.Source != diffSourceAPI || !got.Truncated {
		t.Fatalf("source=%q truncated=%t", got.Source, got.Truncated)
	}
	for i := 0; i < 8; i++ {
		if !strings.Contains(got.Text, fmt.Sprintf("- pkg/f%d.go\n", i)) {
			t.Fatalf("path list missing pkg/f%d.go:\n%s", i, got.Text[:400])
		}
	}
	if !strings.HasPrefix(got.Text, "Changed paths:\n- pkg/f0.go\n") {
		t.Fatalf("path list must lead: %q", got.Text[:80])
	}
	if strings.Contains(got.Text, "git diff") || !strings.Contains(got.Text, "Read each changed path above at HEAD") {
		t.Fatalf("API fallback must not point at the failed git command:\n%s", got.Text[len(got.Text)-300:])
	}
}

func TestDiffHeaderPaths(t *testing.T) {
	got := diffHeaderPaths("diff --git a/x.go b/x.go\n+1\ndiff --git a/dir/old.go b/dir/new.go\n+2\n")
	if got != "- x.go\n- dir/new.go\n" {
		t.Fatalf("got %q", got)
	}
}

func liteResultStream(findingsJSON string) string {
	return `{"type":"system","subtype":"init","model":"claude-fable-5-1"}
{"type":"assistant","message":{"model":"claude-fable-5-1","content":[{"type":"text","text":"reading"}]}}
{"type":"result","subtype":"success","result":` + findingsJSON + `,"total_cost_usd":0.4321,"usage":{"input_tokens":1000,"cache_read_input_tokens":250,"cache_creation_input_tokens":50,"output_tokens":300}}
`
}

const liteFindingOnChange = `"[{\"id\":\"A-1\",\"file_path\":\"change.txt\",\"line_number\":1,\"comment_body\":\"x\",\"importance\":\"LOW\"},{\"file_path\":\"SUMMARY\",\"line_number\":0,\"summary\":{\"verdict\":\"approve\",\"upshot\":\"ok\",\"priority_ids\":[],\"notes\":\"fine\"}}]"`

func runLite(t *testing.T, cfg AgentConfig, stream string) (*AgentReview, *fakeSpawner) {
	t.Helper()
	bare, sha := setupLocalBareRepo(t)
	cloneRoot := t.TempDir()
	seedAgentCache(t, cloneRoot, "acme", "example", bare)
	spawner := &fakeSpawner{proc: &fakeProcess{stdout: bytes.NewBufferString(stream), stderr: &bytes.Buffer{}, killCh: make(chan struct{})}}
	cfg.CloneRootDir, cfg.LogsDir = cloneRoot, t.TempDir()
	if cfg.WallClock == 0 {
		cfg.WallClock = time.Minute
	}
	if cfg.MaxTurns == 0 {
		cfg.MaxTurns = 10
	}
	out, err := RunAgentReview(context.Background(), cfg, spawner, "acme", "example", "main", 1, sha, nil)
	if err != nil {
		t.Fatalf("RunAgentReview: %v", err)
	}
	return out, spawner
}

func TestRunAgentReview_LiteSkipsGatesAndChecksButMatchesBugMemory(t *testing.T) {
	memory := &BugMemoryLibrary{Version: "t1", Entries: []BugMemoryEntry{{ID: "bm-change", Pattern: "Changed change.txt before.", TriggerPaths: []string{"change.txt"}}}}
	out, spawner := runLite(t, AgentConfig{
		Model: "claude-fable-5-1", Prompt: runconfig.PromptLiteArmA, SkipGates: true, RequiredChecks: true, BugMemory: memory,
	}, liteResultStream(liteFindingOnChange))
	if !out.GatesStartedAt.IsZero() || len(out.Gates) != 0 || out.Checks.ChecksIssued != 0 {
		t.Fatalf("gates/checks must not run: started=%v gates=%d checks=%d", out.GatesStartedAt, len(out.Gates), out.Checks.ChecksIssued)
	}
	if len(out.BugMemory.Matched) != 1 || out.BugMemory.Matched[0] != "bm-change" {
		t.Fatalf("bug memory match=%v", out.BugMemory)
	}
	prompt := spawner.args[1]
	if !strings.Contains(prompt, "Changed change.txt before.") || !strings.Contains(prompt, "<diff>") || !strings.Contains(prompt, "+new") {
		t.Fatalf("prompt lacks memory or inlined diff:\n%s", prompt)
	}
	if strings.Contains(prompt, "FIRST-PASS CLAIMS") || strings.Contains(prompt, "REQUIRED CHECKS") {
		t.Fatal("lite prompt carried pipeline-only sections")
	}
	if out.DiffSource != diffSourceGit {
		t.Fatalf("diff source=%q", out.DiffSource)
	}
	if out.CostUSD != 0.4321 || out.InputTokens != 1300 || out.OutputTokens != 300 {
		t.Fatalf("usage: cost=%v in=%d out=%d", out.CostUSD, out.InputTokens, out.OutputTokens)
	}
}

func TestRunAgentReview_LitePassesToolsToArgs(t *testing.T) {
	_, spawner := runLite(t, AgentConfig{Model: "claude-fable-5-1", Prompt: runconfig.PromptLiteArmASub, SkipGates: true, Tools: runconfig.ToolsWithAgent}, liteResultStream(`"[]"`))
	joined := strings.Join(spawner.args, " ")
	if !strings.Contains(joined, "--tools Read,Grep,Glob,Bash,Agent") {
		t.Fatalf("args=%v", spawner.args)
	}
	if !strings.Contains(spawner.args[1], "Spawn sub-agents") {
		t.Fatal("lite_plus prompt must carry the sub-agent sentence")
	}
}

func TestRunAgentReview_LiteReadsCitedFileContentsBeforeCleanup(t *testing.T) {
	out, _ := runLite(t, AgentConfig{Model: "claude-fable-5-1", Prompt: runconfig.PromptLiteArmA, SkipGates: true, CollectCitedFiles: true}, liteResultStream(liteFindingOnChange))
	if got := out.CitedFileContents["change.txt"]; got != "new" {
		t.Fatalf("cited contents=%v", out.CitedFileContents)
	}
	if _, err := os.Stat(out.CloneDir); !os.IsNotExist(err) {
		t.Fatalf("worktree should be gone after the run: %v", err)
	}
}

func TestReadCitedFiles_SkipsControlEntriesAndEscapes(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "a.go"), []byte("package a\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	got := readCitedFiles(dir, []types.LineComment{
		{FilePath: "a.go"}, {FilePath: "SUMMARY"}, {FilePath: checkFilePath}, {FilePath: "../etc/passwd"}, {FilePath: "/etc/passwd"}, {FilePath: "missing.go"},
	})
	if len(got) != 1 || got["a.go"] != "package a\n" {
		t.Fatalf("got=%v", got)
	}
}

func TestReadCitedFiles_RefusesSymlinksOutOfTheWorktree(t *testing.T) {
	outside := t.TempDir()
	secret := filepath.Join(outside, "secret.txt")
	if err := os.WriteFile(secret, []byte("token"), 0o600); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "ok.go"), []byte("fine"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(secret, filepath.Join(dir, "leak")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(dir, "linked-dir")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("ok.go", filepath.Join(dir, "inside-link")); err != nil {
		t.Fatal(err)
	}
	got := readCitedFiles(dir, []types.LineComment{
		{FilePath: "leak"}, {FilePath: "linked-dir/secret.txt"}, {FilePath: "inside-link"}, {FilePath: "ok.go"},
	})
	if len(got) != 1 || got["ok.go"] != "fine" {
		t.Fatalf("symlinked paths must be skipped: got=%v", got)
	}
}

func TestParseAgentStream_CapturesCostAndUsageFromResultEvent(t *testing.T) {
	proc := &fakeProcess{stdout: bytes.NewBufferString(liteResultStream(`"[]"`)), stderr: &bytes.Buffer{}, killCh: make(chan struct{})}
	result, err := parseAgentStream(proc, &bytes.Buffer{}, 10)
	if err != nil {
		t.Fatal(err)
	}
	if result.costUSD != 0.4321 || result.inputTokens != 1300 || result.outputTokens != 300 {
		t.Fatalf("cost=%v in=%d out=%d", result.costUSD, result.inputTokens, result.outputTokens)
	}
}

func TestParseAgentStream_IgnoresSubAgentModelsForFallbackDetection(t *testing.T) {
	stream := `{"type":"system","subtype":"init","model":"claude-fable-5-1"}
{"type":"assistant","parent_tool_use_id":null,"message":{"model":"claude-fable-5-1","content":[{"type":"tool_use","name":"Agent","input":{}}]}}
{"type":"assistant","parent_tool_use_id":"toolu_01","message":{"model":"claude-opus-5","content":[{"type":"text","text":"sub-agent"}]}}
{"type":"assistant","parent_tool_use_id":"toolu_01","message":{"model":"claude-opus-5","content":[{"type":"text","text":"more"}]}}
{"type":"assistant","message":{"model":"claude-fable-5-1","content":[{"type":"text","text":"done"}]}}
{"type":"result","subtype":"success","result":"[]"}
`
	proc := &fakeProcess{stdout: bytes.NewBufferString(stream), stderr: &bytes.Buffer{}, killCh: make(chan struct{})}
	result, err := parseAgentStream(proc, &bytes.Buffer{}, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.servedModels) != 1 || result.servedModels[0] != "claude-fable-5-1" {
		t.Fatalf("served models=%v; sub-agent models must not count", result.servedModels)
	}
	if result.assistantTurns != 4 {
		t.Fatalf("sub-agent events still consume the turn budget: turns=%d", result.assistantTurns)
	}
}

func TestArgsWithTools_IncludesAgentForLitePlus(t *testing.T) {
	rt := agentRuntime{backend: AgentBackendClaude, model: "m", effort: "medium"}
	plus := strings.Join(rt.argsWithTools("p", runconfig.ToolsWithAgent), " ")
	if !strings.Contains(plus, "--tools Read,Grep,Glob,Bash,Agent") {
		t.Fatalf("args=%q", plus)
	}
	def := strings.Join(rt.argsWithTools("p", ""), " ")
	if !strings.Contains(def, "--tools Read,Grep,Glob,Bash ") {
		t.Fatalf("default args=%q", def)
	}
	if strings.Join(rt.args("p"), " ") != def {
		t.Fatal("args() must equal the default tool list")
	}
}
