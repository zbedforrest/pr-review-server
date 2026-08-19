package service

import (
	"bytes"
	"context"
	"io"
	"os/exec"
	"runtime"
	"strings"
	"testing"
	"time"
)

type envCapturingSpawner struct {
	*fakeSpawner
	environment []string
}

// Existing test doubles opt into the production environment contract while
// retaining their canned Spawn behavior.
func (s *fakeSpawner) SpawnWithEnv(ctx context.Context, name string, args []string, dir string, _ []string) (SpawnedProcess, error) {
	return s.Spawn(ctx, name, args, dir)
}

func (s *staticProcessSpawner) SpawnWithEnv(ctx context.Context, name string, args []string, dir string, _ []string) (SpawnedProcess, error) {
	return s.Spawn(ctx, name, args, dir)
}

func (s *cancelAfterSpawnSpawner) SpawnWithEnv(ctx context.Context, name string, args []string, dir string, _ []string) (SpawnedProcess, error) {
	return s.Spawn(ctx, name, args, dir)
}

func TestAgentChildEnvironmentIsDefaultDenyWithFrozenCredential(t *testing.T) {
	environment := agentChildEnvironment([]string{
		"PATH=/usr/bin", "HOME=/home/reviewer", "DATABASE_URL=postgres://secret",
		"ANTHROPIC_API_KEY=ambient", "CUSTOM_FUTURE_SECRET=must-not-pass",
		"CLAUDE_CODE_OAUTH_TOKEN=oauth-token", "ANTHROPIC_AUTH_TOKEN=auth-token",
		"ANTHROPIC_BASE_URL=https://anthropic-gateway.example",
		"NODE_EXTRA_CA_CERTS=/certs/node.pem", "CURL_CA_BUNDLE=/certs/curl.pem",
		"GIT_SSL_CAINFO=/certs/git.pem",
		"PATH=/duplicate", "https_proxy=http://proxy.example",
	}, "ANTHROPIC_API_KEY", "frozen")
	joined := strings.Join(environment, "\n")
	for _, want := range []string{
		"PATH=/usr/bin", "HOME=/home/reviewer", "https_proxy=http://proxy.example",
		"ANTHROPIC_API_KEY=frozen", "CLAUDE_CODE_OAUTH_TOKEN=oauth-token",
		"ANTHROPIC_AUTH_TOKEN=auth-token", "ANTHROPIC_BASE_URL=https://anthropic-gateway.example",
		"NODE_EXTRA_CA_CERTS=/certs/node.pem", "CURL_CA_BUNDLE=/certs/curl.pem",
		"GIT_SSL_CAINFO=/certs/git.pem",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("child environment missing %q: %q", want, environment)
		}
	}
	for _, forbidden := range []string{"postgres://secret", "ambient", "must-not-pass", "PATH=/duplicate"} {
		if strings.Contains(joined, forbidden) {
			t.Errorf("child environment retained %q: %q", forbidden, environment)
		}
	}

	openRouterEnvironment := strings.Join(agentChildEnvironment([]string{
		"CLAUDE_CODE_OAUTH_TOKEN=oauth-token", "ANTHROPIC_AUTH_TOKEN=auth-token",
		"ANTHROPIC_BASE_URL=https://anthropic-gateway.example",
	}, "OPENROUTER_API_KEY", "frozen-openrouter"), "\n")
	for _, forbidden := range []string{"oauth-token", "auth-token", "anthropic-gateway.example"} {
		if strings.Contains(openRouterEnvironment, forbidden) {
			t.Errorf("OpenRouter child retained Claude-only value %q: %q", forbidden, openRouterEnvironment)
		}
	}
}

func TestDefaultSpawnerPreservesExplicitEmptyEnvironment(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("agent subprocesses are unsupported on Windows")
	}
	envExecutable, err := exec.LookPath("env")
	if err != nil {
		t.Fatalf("locate env executable: %v", err)
	}
	proc, err := (DefaultSpawner{}).SpawnWithEnv(context.Background(), envExecutable, nil, "", []string{})
	if err != nil {
		t.Fatalf("spawn env: %v", err)
	}
	stdout, err := io.ReadAll(proc.Stdout())
	if err != nil {
		t.Fatalf("read stdout: %v", err)
	}
	stderr, stderrErr := io.ReadAll(proc.Stderr())
	if stderrErr != nil {
		t.Fatalf("read stderr: %v", stderrErr)
	}
	if err := proc.Wait(); err != nil {
		t.Fatalf("wait: %v; stderr=%s", err, stderr)
	}
	if len(bytes.TrimSpace(stdout)) != 0 {
		t.Fatalf("empty child environment inherited parent values: %s", stdout)
	}
}

func (s *envCapturingSpawner) SpawnWithEnv(ctx context.Context, name string, args []string, dir string, environment []string) (SpawnedProcess, error) {
	s.environment = append([]string(nil), environment...)
	return s.fakeSpawner.Spawn(ctx, name, args, dir)
}

func TestResolveAgentRuntimeDefaultsToClaude(t *testing.T) {
	runtime, err := resolveAgentRuntime(AgentConfig{})
	if err != nil {
		t.Fatalf("resolveAgentRuntime: %v", err)
	}
	if runtime.backend != AgentBackendClaude || runtime.command != "claude" {
		t.Fatalf("backend=%q command=%q", runtime.backend, runtime.command)
	}
	if runtime.model != DefaultAgentModel || runtime.effort != DefaultAgentEffort {
		t.Errorf("model=%q effort=%q", runtime.model, runtime.effort)
	}

	args := runtime.args("review this")
	got := strings.Join(args, "\x00")
	want := strings.Join([]string{
		"-p", "review this",
		"--model", DefaultAgentModel,
		"--effort", DefaultAgentEffort,
		"--tools", "Read,Grep,Glob,Bash",
		"--permission-mode", "bypassPermissions",
		"--output-format", "stream-json",
		"--verbose",
	}, "\x00")
	if got != want {
		t.Errorf("Claude argv changed:\n got: %q\nwant: %q", args, strings.Split(want, "\x00"))
	}
}

func TestResolveAgentRuntimeOpenRouterRequiresFrozenKey(t *testing.T) {
	t.Setenv("OPENROUTER_API_KEY", "ambient-key-must-not-bypass-config")
	_, err := resolveAgentRuntime(AgentConfig{Backend: AgentBackendOpenRouter})
	if err == nil || !strings.Contains(err.Error(), "OPENROUTER_API_KEY") {
		t.Fatalf("expected missing-key error, got %v", err)
	}
}

func TestOpenRouterRuntimeBuildsIsolatedCodexInvocation(t *testing.T) {
	const secret = "sk-or-test-secret-that-must-not-appear-in-argv"
	runtime, err := resolveAgentRuntime(AgentConfig{Backend: " OpenRouter ", Effort: "high", OpenRouterAPIKey: secret})
	if err != nil {
		t.Fatalf("resolveAgentRuntime: %v", err)
	}
	if runtime.command != "codex" || runtime.model != DefaultOpenRouterAgentModel {
		t.Fatalf("command=%q model=%q", runtime.command, runtime.model)
	}

	args := runtime.args("review this")
	joined := strings.Join(args, "\n")
	for _, want := range []string{
		"exec",
		"--json",
		"--ephemeral",
		"--ignore-user-config",
		"--ignore-rules",
		"read-only",
		`model_provider="openrouter"`,
		`model_providers.openrouter.base_url="https://openrouter.ai/api/v1"`,
		`model_providers.openrouter.env_key="OPENROUTER_API_KEY"`,
		`model_providers.openrouter.wire_api="responses"`,
		`model_reasoning_effort="high"`,
		`shell_environment_policy.ignore_default_excludes=false`,
		`shell_environment_policy.exclude=["DATABASE_URL","GOOGLE_APPLICATION_CREDENTIALS","GITHUB_TOKEN","GEMINI_API_KEY","OPENROUTER_API_KEY","ANTHROPIC_API_KEY","GITHUB_APP_PRIVATE_KEY_PATH","GITHUB_APP_CLIENT_SECRET","SESSION_SECRET"]`,
		DefaultOpenRouterAgentModel,
		"review this",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("Codex argv missing %q: %q", want, args)
		}
	}
	if strings.Contains(joined, secret) {
		t.Fatal("OpenRouter key leaked into Codex argv")
	}
}

func TestResolveAgentRuntimeRejectsUnknownBackend(t *testing.T) {
	_, err := resolveAgentRuntime(AgentConfig{Backend: "mystery"})
	if err == nil || !strings.Contains(err.Error(), "unsupported backend") {
		t.Fatalf("expected unsupported-backend error, got %v", err)
	}
}

func TestParseCodexStreamHappyPath(t *testing.T) {
	stream := `{"type":"thread.started","thread_id":"thread_123"}
{"type":"turn.started"}
{"type":"item.completed","item":{"id":"item_0","type":"command_execution","command":"git diff","status":"completed"}}
{"type":"item.completed","item":{"id":"item_1","type":"agent_message","text":"[{\"file_path\":\"SUMMARY\",\"line_number\":0,\"comment_body\":\"Looks good.\"}]"}}
{"type":"turn.completed","usage":{"input_tokens":100,"output_tokens":20}}
`
	proc := &fakeProcess{
		stdout: bytes.NewBufferString(stream),
		stderr: &bytes.Buffer{},
		killCh: make(chan struct{}),
	}
	var logBuf bytes.Buffer
	res, err := parseCodexStream(proc, &logBuf, 10)
	if err != nil {
		t.Fatalf("parseCodexStream: %v", err)
	}
	if res.assistantTurns != 1 {
		t.Errorf("turns=%d want 1", res.assistantTurns)
	}
	if res.budgetUnits != 1 {
		t.Errorf("budget units=%d want 1", res.budgetUnits)
	}
	if !strings.Contains(res.finalOutput, "Looks good") {
		t.Errorf("final output missing: %q", res.finalOutput)
	}
	if !strings.Contains(logBuf.String(), "turn.completed") {
		t.Error("raw log missing turn.completed event")
	}
}

func TestParseCodexStreamTurnBudgetExcludesReasoningAndTerminalMessage(t *testing.T) {
	stream := `{"type":"item.completed","item":{"type":"reasoning"}}
{"type":"item.completed","item":{"type":"command_execution"}}
{"type":"item.completed","item":{"type":"file_change"}}
{"type":"item.completed","item":{"type":"agent_message","text":"[]"}}
`
	proc := &fakeProcess{
		stdout: bytes.NewBufferString(stream), stderr: &bytes.Buffer{}, killCh: make(chan struct{}),
	}
	res, err := parseCodexStream(proc, &bytes.Buffer{}, 2)
	if err != nil {
		t.Fatalf("parseCodexStream: %v", err)
	}
	if res.assistantTurns != 1 || res.budgetUnits != 2 {
		t.Fatalf("assistant turns=%d budget units=%d", res.assistantTurns, res.budgetUnits)
	}
	if res.finalOutput != "[]" {
		t.Fatalf("terminal output was discarded: %q", res.finalOutput)
	}
	if proc.killed {
		t.Fatal("terminal answer incorrectly consumed work-item budget")
	}
}

func TestParseCodexStreamCapturesFailure(t *testing.T) {
	stream := `{"type":"thread.started","thread_id":"thread_123"}
{"type":"turn.failed","error":{"message":"OpenRouter rate limit exceeded"}}
`
	proc := &fakeProcess{
		stdout: bytes.NewBufferString(stream),
		stderr: &bytes.Buffer{},
		killCh: make(chan struct{}),
	}
	res, err := parseCodexStream(proc, &bytes.Buffer{}, 10)
	if err != nil {
		t.Fatalf("parseCodexStream: %v", err)
	}
	if !strings.Contains(res.streamErr, "rate limit") {
		t.Errorf("streamErr=%q", res.streamErr)
	}
}

func TestParseCodexStreamMaxTurnsKills(t *testing.T) {
	stream := strings.Repeat(`{"type":"item.completed","item":{"type":"command_execution","status":"completed"}}`+"\n", 3)
	proc := &fakeProcess{
		stdout: bytes.NewBufferString(stream),
		stderr: &bytes.Buffer{},
		killCh: make(chan struct{}),
	}
	_, err := parseCodexStream(proc, &bytes.Buffer{}, 2)
	if err == nil || !strings.Contains(err.Error(), "max-turns") {
		t.Fatalf("expected max-turns error, got %v", err)
	}
	if !strings.Contains(err.Error(), "after 3 budget units") {
		t.Fatalf("budget usage missing from error: %v", err)
	}
	if !proc.killed {
		t.Error("expected process to be killed")
	}
}

func TestParseCodexStreamCapsBudgetExemptItems(t *testing.T) {
	const maxTurns = 2
	stream := strings.Repeat(`{"type":"item.completed","item":{"type":"agent_message","text":"working"}}`+"\n", 2*maxTurns+17)
	proc := &fakeProcess{
		stdout: bytes.NewBufferString(stream),
		stderr: &bytes.Buffer{},
		killCh: make(chan struct{}),
	}
	res, err := parseCodexStream(proc, &bytes.Buffer{}, maxTurns)
	if err == nil || !strings.Contains(err.Error(), "budget-exempt item ceiling") {
		t.Fatalf("expected budget-exempt item ceiling error, got %v", err)
	}
	if res.budgetUnits != 0 {
		t.Fatalf("message-only stream consumed work budget: %d", res.budgetUnits)
	}
	if !proc.killed {
		t.Error("expected process to be killed")
	}
}

func TestParseCodexStreamCapsMalformedUntypedItems(t *testing.T) {
	const maxTurns = 2
	stream := strings.Repeat(`{"type":"item.completed","item":{}}`+"\n", 2*maxTurns+17)
	proc := &fakeProcess{
		stdout: bytes.NewBufferString(stream), stderr: &bytes.Buffer{}, killCh: make(chan struct{}),
	}
	_, err := parseCodexStream(proc, &bytes.Buffer{}, maxTurns)
	if err == nil || !strings.Contains(err.Error(), "budget-exempt item ceiling") {
		t.Fatalf("expected malformed-item ceiling error, got %v", err)
	}
	if !proc.killed {
		t.Error("expected malformed stream to be killed")
	}
}

func TestParseCodexStreamProductiveWorkResetsExemptItemCeiling(t *testing.T) {
	const maxTurns = 2
	reasoningBlock := strings.Repeat(`{"type":"item.completed","item":{"type":"reasoning"}}`+"\n", 2*maxTurns+16)
	stream := reasoningBlock + `{"type":"item.completed","item":{"type":"command_execution"}}` + "\n" +
		reasoningBlock + `{"type":"item.completed","item":{"type":"file_change"}}` + "\n" +
		`{"type":"item.completed","item":{"type":"agent_message","text":"[]"}}` + "\n"
	proc := &fakeProcess{
		stdout: bytes.NewBufferString(stream),
		stderr: &bytes.Buffer{},
		killCh: make(chan struct{}),
	}
	res, err := parseCodexStream(proc, &bytes.Buffer{}, maxTurns)
	if err != nil {
		t.Fatalf("productive stream was capped: %v", err)
	}
	if res.budgetUnits != maxTurns || res.finalOutput != "[]" {
		t.Fatalf("result=%+v", res)
	}
	if proc.killed {
		t.Fatal("productive stream was killed")
	}
}

func TestRunAgentReviewOpenRouter(t *testing.T) {
	t.Setenv("OPENROUTER_API_KEY", "ambient-key-must-not-reach-child")
	t.Setenv("GITHUB_TOKEN", "server-secret-must-not-reach-child")
	bare, sha := setupLocalBareRepo(t)
	cloneRoot := t.TempDir()
	seedAgentCache(t, cloneRoot, "acme", "example", bare)

	stream := `{"type":"thread.started","thread_id":"thread_123"}
{"type":"turn.started"}
{"type":"item.completed","item":{"type":"command_execution","status":"completed"}}
{"type":"item.completed","item":{"type":"agent_message","text":"[]"}}
{"type":"turn.completed","usage":{"input_tokens":100,"output_tokens":20}}
`
	spawner := &envCapturingSpawner{fakeSpawner: &fakeSpawner{proc: &fakeProcess{
		stdout: bytes.NewBufferString(stream),
		stderr: &bytes.Buffer{},
		killCh: make(chan struct{}),
	}}}
	var attemptEvents []ProviderAttemptEvent
	cfg := AgentConfig{
		CloneRootDir:     cloneRoot,
		LogsDir:          t.TempDir(),
		WallClock:        time.Minute,
		MaxTurns:         10,
		Backend:          AgentBackendOpenRouter,
		OpenRouterAPIKey: "frozen-key",
		AttemptObserver: func(event ProviderAttemptEvent) error {
			attemptEvents = append(attemptEvents, event)
			return nil
		},
	}

	out, err := RunAgentReview(context.Background(), cfg, spawner,
		"acme", "example", "main", 1, sha, nil)
	if err != nil {
		t.Fatalf("RunAgentReview: %v", err)
	}
	if spawner.name != "codex" {
		t.Errorf("spawned %q want codex", spawner.name)
	}
	if spawner.dir == "" || !strings.Contains(strings.Join(spawner.args, "\n"), DefaultOpenRouterAgentModel) {
		t.Errorf("unexpected spawn: dir=%q args=%q", spawner.dir, spawner.args)
	}
	openRouterEntries := make([]string, 0, 1)
	for _, entry := range spawner.environment {
		if strings.HasPrefix(entry, "OPENROUTER_API_KEY=") {
			openRouterEntries = append(openRouterEntries, entry)
		}
	}
	if len(openRouterEntries) != 1 || openRouterEntries[0] != "OPENROUTER_API_KEY=frozen-key" {
		t.Fatalf("child OpenRouter credential=%q", openRouterEntries)
	}
	if strings.Contains(strings.Join(spawner.environment, "\n"), "server-secret-must-not-reach-child") {
		t.Fatal("unrelated server credential leaked into Codex environment")
	}
	if strings.Contains(strings.Join(spawner.args, "\n"), "frozen-key") {
		t.Fatal("frozen OpenRouter key leaked into argv")
	}
	if out.RequestedModel != DefaultOpenRouterAgentModel || out.ServedModel != DefaultOpenRouterAgentModel {
		t.Errorf("requested=%q served=%q", out.RequestedModel, out.ServedModel)
	}
	if out.ModelFallback {
		t.Error("unexpected model fallback for pinned OpenRouter model")
	}
	if out.Backend != AgentBackendOpenRouter || out.ServingModelVerified {
		t.Errorf("backend metadata: backend=%q verified=%t", out.Backend, out.ServingModelVerified)
	}
	if out.Effort != DefaultAgentEffort {
		t.Errorf("effort=%q want %q", out.Effort, DefaultAgentEffort)
	}
	if out.BudgetUnitsUsed != 1 {
		t.Errorf("budget units=%d want 1", out.BudgetUnitsUsed)
	}
	if len(attemptEvents) != 2 || attemptEvents[1].BudgetUnitsUsed != 1 || attemptEvents[1].AssistantTurns != 1 {
		t.Errorf("attempt usage telemetry=%+v", attemptEvents)
	}
}

func TestRunAgentReviewClaudeUsesFilteredFrozenEnvironment(t *testing.T) {
	t.Setenv("ANTHROPIC_API_KEY", "ambient-anthropic-key")
	t.Setenv("OPENROUTER_API_KEY", "unrelated-server-secret")
	bare, sha := setupLocalBareRepo(t)
	cloneRoot := t.TempDir()
	seedAgentCache(t, cloneRoot, "acme", "example", bare)
	stream := `{"type":"system","subtype":"init","model":"claude-opus-4-8"}
{"type":"assistant","message":{"model":"claude-opus-4-8","content":[{"type":"text","text":"done"}]}}
{"type":"result","result":"[]"}
`
	spawner := &envCapturingSpawner{fakeSpawner: &fakeSpawner{proc: &fakeProcess{
		stdout: bytes.NewBufferString(stream), stderr: &bytes.Buffer{}, killCh: make(chan struct{}),
	}}}
	cfg := AgentConfig{
		CloneRootDir: cloneRoot, LogsDir: t.TempDir(), WallClock: time.Minute, MaxTurns: 10,
		Backend: AgentBackendClaude, AnthropicAPIKey: "frozen-anthropic-key",
	}
	out, err := RunAgentReview(context.Background(), cfg, spawner, "acme", "example", "main", 1, sha, nil)
	if err != nil {
		t.Fatalf("RunAgentReview: %v", err)
	}
	if out.BudgetUnitsUsed != 1 || out.AssistantTurns != 1 {
		t.Fatalf("Claude usage: assistant turns=%d budget units=%d", out.AssistantTurns, out.BudgetUnitsUsed)
	}
	joined := strings.Join(spawner.environment, "\n")
	if !strings.Contains(joined, "ANTHROPIC_API_KEY=frozen-anthropic-key") {
		t.Fatalf("Claude child did not receive frozen credential: %q", spawner.environment)
	}
	for _, forbidden := range []string{"ambient-anthropic-key", "unrelated-server-secret"} {
		if strings.Contains(joined, forbidden) {
			t.Fatalf("Claude child inherited server secret %q", forbidden)
		}
	}
}
