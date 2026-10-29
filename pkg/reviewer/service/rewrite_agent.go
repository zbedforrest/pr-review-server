package service

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"pr-review-server/pkg/rewrite"
)

// RewriteInput is one PR's findings to rewrite for posting.
type RewriteInput struct {
	Owner, Repo string
	PRNumber    int
	Items       []rewrite.Item
}

// RewriteResult is one rewrite run's output and cost.
type RewriteResult struct {
	Rewrites   []rewrite.Result
	Model      string
	CostUSD    float64
	DurationMS int64
}

// rewriteTools is the narrowest allowlist the CLI takes; the run starts in an
// empty directory, so there is nothing to read.
const rewriteTools = "Read"

// RunRewrite asks the model for a short title and a direct body for each
// item. It reads no repository: the rewrite may use only what the finding
// already says.
func RunRewrite(ctx context.Context, cfg AgentConfig, spawner Spawner, in RewriteInput) (*RewriteResult, error) {
	if cfg.MaxTurns <= 0 || cfg.WallClock <= 0 {
		return nil, errors.New("rewrite: MaxTurns and WallClock must be > 0")
	}
	if len(in.Items) == 0 {
		return nil, errors.New("rewrite: no items")
	}
	runtime, err := resolveAgentRuntime(cfg)
	if err != nil {
		return nil, err
	}
	if runtime.backend != AgentBackendClaude {
		return nil, fmt.Errorf("rewrite: backend %q unsupported (claude only)", runtime.backend)
	}
	if err := os.MkdirAll(cfg.CloneRootDir, 0o755); err != nil {
		return nil, fmt.Errorf("rewrite: %w", err)
	}
	dir, err := os.MkdirTemp(cfg.CloneRootDir, fmt.Sprintf("%s__%s__pr%d__rewrite__", in.Owner, in.Repo, in.PRNumber))
	if err != nil {
		return nil, fmt.Errorf("rewrite: %w", err)
	}
	defer os.RemoveAll(dir)
	prompt, err := rewrite.Prompt(in.Items)
	if err != nil {
		return nil, fmt.Errorf("rewrite: prompt: %w", err)
	}

	runCtx, cancel := context.WithTimeout(ctx, cfg.WallClock)
	defer cancel()
	started := time.Now()
	proc, err := spawner.SpawnWithEnv(runCtx, runtime.command, runtime.argsWithToolsAndSchema(prompt, rewriteTools, rewrite.JSONSchema), dir,
		agentChildEnvironment(os.Environ(), "ANTHROPIC_API_KEY", cfg.AnthropicAPIKey))
	if err != nil {
		return nil, fmt.Errorf("rewrite: spawn %s: %w", runtime.command, err)
	}
	stderrOutput := collectStderr(proc)
	logFile, err := os.Create(filepath.Join(dir, "stream.jsonl"))
	if err != nil {
		_ = proc.Kill()
		_ = proc.Wait()
		return nil, fmt.Errorf("rewrite: create log: %w", err)
	}
	defer logFile.Close()

	parsed, parseErr := runtime.parseStream(proc, logFile, cfg.MaxTurns)
	waitErr := proc.Wait()
	redact := func(s string) string { return truncate(redactToken(s, cfg.AnthropicAPIKey), 600) }
	spent := &RewriteResult{Model: runtime.model, CostUSD: parsed.costUSD, DurationMS: time.Since(started).Milliseconds()}
	switch {
	case runCtx.Err() == context.DeadlineExceeded:
		return spent, fmt.Errorf("rewrite: wall-clock timeout (%s)", cfg.WallClock)
	case parseErr != nil:
		return spent, fmt.Errorf("rewrite: %w (stderr: %s)", parseErr, redact(stderrOutput()))
	case parsed.streamErr != "":
		return spent, fmt.Errorf("rewrite: CLI reported error: %s", redact(parsed.streamErr))
	case waitErr != nil:
		return spent, fmt.Errorf("rewrite: %s exited with error: %w (stderr: %s)", runtime.command, waitErr, redact(stderrOutput()))
	case strings.TrimSpace(parsed.finalOutput) == "":
		return spent, errors.New("rewrite: no final result emitted")
	}
	spent.Rewrites, err = rewrite.ParseReply(parsed.finalOutput, in.Items)
	return spent, err
}
