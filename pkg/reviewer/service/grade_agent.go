package service

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"

	"pr-review-server/pkg/grader"
)

// GradeAgentInput is one grading run: one PR head and the comments posted on it.
type GradeAgentInput struct {
	Owner, Repo, DefaultBranch string
	PRNumber                   int
	HeadSHA, BaseSHA           string
	Title, Body                string
	Items                      []grader.Item
}

// GradeAgentResult is one grading run's grades and cost.
type GradeAgentResult struct {
	Grades     []grader.Grade
	Model      string
	CostUSD    float64
	DurationMS int64
}

const gradeAgentTools = "Read,Grep,Glob"

// RunAgentGrade runs a read-only agent over a head worktree, with a second
// worktree at the diff base, and returns its grades for in.Items.
func RunAgentGrade(ctx context.Context, cfg AgentConfig, spawner Spawner, in GradeAgentInput) (*GradeAgentResult, error) {
	if cfg.MaxTurns <= 0 || cfg.WallClock <= 0 {
		return nil, errors.New("grade agent: MaxTurns and WallClock must be > 0")
	}
	if len(in.Items) == 0 {
		return nil, errors.New("grade agent: no items")
	}
	if in.BaseSHA == "" {
		return nil, errors.New("grade agent: no diff base recorded for this PR")
	}
	runtime, err := resolveAgentRuntime(cfg)
	if err != nil {
		return nil, err
	}
	if runtime.backend != AgentBackendClaude {
		return nil, fmt.Errorf("grade agent: backend %q unsupported (claude only)", runtime.backend)
	}
	for _, dir := range []string{cfg.CloneRootDir, cfg.LogsDir} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, fmt.Errorf("grade agent: %w", err)
		}
	}
	slug := fmt.Sprintf("%s__%s__pr%d__grade__%d_%s", in.Owner, in.Repo, in.PRNumber, time.Now().UnixNano(), randomSuffix())
	headDir := filepath.Join(cfg.CloneRootDir, slug)
	logPath := filepath.Join(cfg.LogsDir, slug+".jsonl")
	logPrefix := fmt.Sprintf("[GRADE-AGENT %s/%s#%d]", in.Owner, in.Repo, in.PRNumber)

	runCtx, cancel := context.WithTimeout(ctx, cfg.WallClock)
	defer cancel()
	started := time.Now()

	cleanupHead, err := cloneForAgent(runCtx, cfg.CloneRootDir, headDir, in.Owner, in.Repo, in.DefaultBranch, in.PRNumber, in.HeadSHA, cfg.GitHubToken, cfg.BasePin)
	if err != nil {
		return nil, fmt.Errorf("grade agent: clone: %w", err)
	}
	defer func() {
		if cerr := cleanupHead(); cerr != nil {
			log.Printf("%s WARN: worktree cleanup failed: %v", logPrefix, cerr)
		}
	}()
	// Grading without the base the comments were judged against would mark
	// change claims BAD blind; fail so the batch is retried.
	baseDir, cleanupBase, err := baseWorktree(runCtx, cfg.CloneRootDir, in.Owner, in.Repo, headDir+"__base", in.BaseSHA)
	if err != nil {
		return nil, fmt.Errorf("grade agent: base checkout: %w", err)
	}
	defer func() {
		if cerr := cleanupBase(); cerr != nil {
			log.Printf("%s WARN: base worktree cleanup failed: %v", logPrefix, cerr)
		}
	}()
	prompt, err := grader.Prompt(in.Title, in.Body, in.HeadSHA, in.BaseSHA, baseDir, in.Items)
	if err != nil {
		return nil, fmt.Errorf("grade agent: prompt: %w", err)
	}

	log.Printf("%s spawning %s (model=%s, items=%d, prompt_chars=%d)", logPrefix, runtime.command, runtime.model, len(in.Items), len(prompt))
	proc, err := spawner.SpawnWithEnv(runCtx, runtime.command, runtime.argsWithTools(prompt, gradeAgentTools), headDir,
		agentChildEnvironment(os.Environ(), "ANTHROPIC_API_KEY", cfg.AnthropicAPIKey))
	if err != nil {
		return nil, fmt.Errorf("grade agent: spawn %s: %w", runtime.command, err)
	}
	stderrOutput := collectStderr(proc)
	logFile, err := os.Create(logPath)
	if err != nil {
		_ = proc.Kill()
		_ = proc.Wait()
		return nil, fmt.Errorf("grade agent: create log: %w", err)
	}
	defer logFile.Close()
	succeeded := false
	defer func() {
		if succeeded {
			_ = os.Remove(logPath)
			return
		}
		if cfg.FailureLogSink != nil {
			_ = logFile.Sync()
			cfg.FailureLogSink(logPath)
			_ = os.Remove(logPath)
		}
	}()

	parsed, parseErr := runtime.parseStream(proc, logFile, cfg.MaxTurns)
	waitErr := proc.Wait()
	stderrBuf := stderrOutput()
	redact := func(s string) string { return truncate(redactToken(s, cfg.AnthropicAPIKey), 600) }
	spent := &GradeAgentResult{Model: runtime.model, CostUSD: parsed.costUSD, DurationMS: time.Since(started).Milliseconds()}
	switch {
	case parseErr != nil && strings.Contains(parseErr.Error(), "max-turns"):
		return spent, fmt.Errorf("grade agent: turn budget exhausted: %v", parseErr)
	case runCtx.Err() == context.DeadlineExceeded:
		return spent, fmt.Errorf("grade agent: wall-clock timeout (%s)", cfg.WallClock)
	case parseErr != nil:
		return spent, fmt.Errorf("grade agent: %w (stderr: %s)", parseErr, redact(stderrBuf))
	case parsed.streamErr != "":
		return spent, fmt.Errorf("grade agent: CLI reported error: %s", redact(parsed.streamErr))
	case waitErr != nil:
		return spent, fmt.Errorf("grade agent: %s exited with error: %w (stderr: %s)", runtime.command, waitErr, redact(stderrBuf))
	case parsed.finalOutput == "":
		return spent, fmt.Errorf("grade agent: no final result emitted (stream: %s)", redact(parsed.diagnostic()))
	}
	grades, err := grader.ParseReply(parsed.finalOutput, in.Items)
	if err != nil {
		return spent, err
	}
	succeeded = true
	spent.Grades = grades
	return spent, nil
}
