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

	"pr-review-server/pkg/reviewer/types"
	"pr-review-server/pkg/reviewer/verify"
)

// VerifyAgentInput is one escalation run: the findings Phase 1 could not
// judge and the packet it built for them.
type VerifyAgentInput struct {
	Owner, Repo, DefaultBranch string
	PRNumber                   int
	HeadSHA, BaseSHA           string
	Packet                     *verify.Packet
	Findings                   []verify.Finding
}

// VerifyAgentResult is the escalation run's resolved decisions and cost.
type VerifyAgentResult struct {
	Decisions      []verify.AgentDecision
	RequestedModel string
	ServedModel    string
	AssistantTurns int
	CostUSD        float64
	DurationMS     int64
}

// verifyAgentTools omits Bash: the prompt carries PR and finding text, so
// the agent only reads.
const verifyAgentTools = "Read,Grep,Glob"

// RunAgentVerify runs a read-only agent over a head worktree, with a second
// worktree at the merge-base, and returns its decisions with every cite
// checked against those worktrees before they are removed.
func RunAgentVerify(ctx context.Context, cfg AgentConfig, spawner Spawner, in VerifyAgentInput) (*VerifyAgentResult, error) {
	if cfg.MaxTurns <= 0 || cfg.WallClock <= 0 {
		return nil, errors.New("verify agent: MaxTurns and WallClock must be > 0")
	}
	runtime, err := resolveAgentRuntime(cfg)
	if err != nil {
		return nil, err
	}
	for _, dir := range []string{cfg.CloneRootDir, cfg.LogsDir} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, fmt.Errorf("verify agent: %w", err)
		}
	}
	slug := fmt.Sprintf("%s__%s__pr%d__verify__%d_%s", in.Owner, in.Repo, in.PRNumber, time.Now().UnixNano(), randomSuffix())
	headDir := filepath.Join(cfg.CloneRootDir, slug)
	logPath := filepath.Join(cfg.LogsDir, slug+".jsonl")
	logPrefix := fmt.Sprintf("[VERIFY-AGENT %s/%s#%d]", in.Owner, in.Repo, in.PRNumber)

	runCtx, cancel := context.WithTimeout(ctx, cfg.WallClock)
	defer cancel()
	started := time.Now()

	cleanupHead, err := cloneForAgent(runCtx, cfg.CloneRootDir, headDir, in.Owner, in.Repo, in.DefaultBranch, in.PRNumber, in.HeadSHA, cfg.GitHubToken)
	if err != nil {
		return nil, fmt.Errorf("verify agent: clone: %w", err)
	}
	defer func() {
		if cerr := cleanupHead(); cerr != nil {
			log.Printf("%s WARN: worktree cleanup failed: %v", logPrefix, cerr)
		}
	}()
	baseDir := ""
	if in.BaseSHA != "" {
		dir, cleanupBase, err := baseWorktree(runCtx, cfg.CloneRootDir, in.Owner, in.Repo, headDir+"__base", in.BaseSHA)
		if err != nil {
			log.Printf("%s merge-base checkout unavailable: %v", logPrefix, err)
		} else {
			baseDir = dir
			defer func() {
				if cerr := cleanupBase(); cerr != nil {
					log.Printf("%s WARN: base worktree cleanup failed: %v", logPrefix, cerr)
				}
			}()
		}
	}
	baseNote := baseDir
	if baseNote == "" {
		baseNote = "(unavailable for this run)"
	}
	prompt := verify.AgentPrompt(in.Packet, in.Findings, baseNote)

	credentialKey, credentialValue := "ANTHROPIC_API_KEY", cfg.AnthropicAPIKey
	if runtime.backend == AgentBackendOpenRouter {
		credentialKey, credentialValue = "OPENROUTER_API_KEY", cfg.OpenRouterAPIKey
	}
	log.Printf("%s spawning %s (model=%s, effort=%s, findings=%d, prompt_chars=%d)", logPrefix, runtime.command, runtime.model, runtime.effort, len(in.Findings), len(prompt))
	proc, err := spawner.SpawnWithEnv(runCtx, runtime.command, runtime.argsWithTools(prompt, verifyAgentTools), headDir,
		agentChildEnvironment(os.Environ(), credentialKey, credentialValue))
	if err != nil {
		return nil, fmt.Errorf("verify agent: spawn %s: %w", runtime.command, err)
	}
	stderrOutput := collectStderr(proc)
	logFile, err := os.Create(logPath)
	if err != nil {
		_ = proc.Kill()
		_ = proc.Wait()
		return nil, fmt.Errorf("verify agent: create log: %w", err)
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
	redact := func(s string) string { return truncate(redactToken(s, credentialValue), 600) }
	switch {
	case parseErr != nil && strings.Contains(parseErr.Error(), "max-turns"):
		return nil, fmt.Errorf("verify agent: turn budget exhausted: %v", parseErr)
	case runCtx.Err() == context.DeadlineExceeded:
		return nil, fmt.Errorf("verify agent: wall-clock timeout (%s)", cfg.WallClock)
	case parseErr != nil:
		return nil, fmt.Errorf("verify agent: %w (stderr: %s)", parseErr, redact(stderrBuf))
	case parsed.streamErr != "":
		return nil, fmt.Errorf("verify agent: CLI reported error: %s", redact(parsed.streamErr))
	case waitErr != nil:
		return nil, fmt.Errorf("verify agent: %s exited with error: %w (stderr: %s)", runtime.command, waitErr, redact(stderrBuf))
	case parsed.finalOutput == "":
		return nil, fmt.Errorf("verify agent: no final result emitted (stream: %s)", redact(parsed.diagnostic()))
	}
	decisions, err := verify.ResolveAgentReply(parsed.finalOutput, headDir, baseDir)
	if err != nil {
		return nil, err
	}
	succeeded = true
	out := &VerifyAgentResult{
		Decisions: decisions, RequestedModel: runtime.model, AssistantTurns: parsed.assistantTurns,
		CostUSD: parsed.costUSD, DurationMS: time.Since(started).Milliseconds(),
	}
	out.ServedModel, _, _, _, _ = agentServingMetadata(runtime, parsed.servedModels)
	return out, nil
}

// baseWorktree checks the merge-base out of owner/repo's clone cache, which
// cloneForAgent has just refreshed.
func baseWorktree(ctx context.Context, cloneRoot, owner, repo, dir, sha string) (string, func() error, error) {
	absRoot, err := filepath.Abs(cloneRoot)
	if err != nil {
		return "", nil, err
	}
	absDir, err := filepath.Abs(dir)
	if err != nil {
		return "", nil, err
	}
	cache := AgentCacheDir(absRoot, owner, repo)
	if out, err := runGit(ctx, cache, "worktree", "add", "--detach", absDir, sha); err != nil {
		return "", nil, fmt.Errorf("git worktree add %s: %w (%s)", sha, err, out)
	}
	return absDir, func() error {
		if out, err := runGit(context.Background(), cache, "worktree", "remove", "--force", absDir); err != nil {
			_ = os.RemoveAll(absDir)
			_, _ = runGit(context.Background(), cache, "worktree", "prune")
			return fmt.Errorf("git worktree remove: %w (%s)", err, out)
		}
		return nil
	}, nil
}

// EscalationResult is what EscalateVerify did.
type EscalationResult struct {
	Escalated  int
	Decisions  map[string]int
	CostUSD    float64
	Model      string
	DurationMS int64
}

// EscalateVerify hands the findings Phase 1 marked unavailable (up to
// verify.MaxAgentFindings) to one RunAgentVerify run and records its
// decisions on comments in place, which align with findings. On an error
// the findings stay unavailable, so they publish as merged.
func EscalateVerify(ctx context.Context, cfg AgentConfig, spawner Spawner, in VerifyAgentInput, comments []types.LineComment) (res EscalationResult, err error) {
	all := in.Findings
	index := map[string]int{}
	in.Findings = nil
	for i, c := range comments {
		if c.Verification != nil && c.Verification.Status == types.VerifyUnavailable && len(in.Findings) < verify.MaxAgentFindings {
			index[all[i].Key] = i
			in.Findings = append(in.Findings, all[i])
		}
	}
	res.Escalated = len(in.Findings)
	if res.Escalated == 0 {
		return res, nil
	}
	started := time.Now()
	defer func() { res.DurationMS = time.Since(started).Milliseconds() }()
	out, err := runVerifyAgentFn(ctx, cfg, spawner, in)
	if err != nil {
		return res, err
	}
	res.CostUSD, res.Model, res.Decisions = out.CostUSD, out.ServedModel, map[string]int{}
	for _, d := range out.Decisions {
		i, ok := index[d.Key]
		if !ok {
			continue
		}
		delete(index, d.Key)
		c, _ := verify.ApplyAgent(all[i].Comment, d)
		c.Verification.Phase, c.Verification.Model = 2, out.ServedModel
		comments[i] = c
		res.Decisions[c.Verification.Status]++
	}
	for _, i := range index {
		comments[i].Verification.Phase = 2
		res.Decisions["no_decision"]++
	}
	return res, nil
}

// runVerifyAgentFn is RunAgentVerify; tests replace it.
var runVerifyAgentFn = RunAgentVerify
