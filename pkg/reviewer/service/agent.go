package service

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"pr-review-server/pkg/reviewer/types"
)

// AgentConfig holds runtime knobs for a single agent-review invocation.
type AgentConfig struct {
	CloneRootDir string        // parent dir for per-invocation clones
	LogsDir      string        // parent dir for raw stream-json logs
	WallClock    time.Duration // hard wall-clock timeout
	MaxTurns     int           // abort after this many assistant turns
	GitHubToken  string        // optional; HTTPS clone auth
}

// AgentReview is the result of a successful agent run.
type AgentReview struct {
	Comments []types.LineComment // parsed from the agent's final JSON response
	RawFinal string              // the agent's final result text (pre-parse, for debugging)
	CloneDir string              // path to the per-invocation clone (kept for inspection)
	LogPath  string              // path to the raw stream-json file
}

// Spawner abstracts subprocess creation so tests can stub the `claude` CLI.
type Spawner interface {
	Spawn(ctx context.Context, name string, args []string, dir string) (SpawnedProcess, error)
}

// SpawnedProcess is what a Spawner returns.
type SpawnedProcess interface {
	Stdout() io.Reader
	Stderr() io.Reader
	Wait() error
	Kill() error
}

// RunAgentReview clones the PR branch, spawns `claude -p`, parses its
// stream-json output, and returns the assembled markdown. On any failure
// (clone error, timeout, turn-cap hit, non-zero exit) it returns a descriptive
// error — caller is expected to surface it loud.
//
// defaultBranch is the name of the PR's base branch (e.g. "master") — used
// for the initial clone. The PR's head commit is then fetched via the
// `pull/<N>/head` refspec and checked out.
func RunAgentReview(
	ctx context.Context,
	agentCfg AgentConfig,
	spawner Spawner,
	owner, repo, defaultBranch string,
	prNumber int,
	commitSHA string,
	geminiComments []types.LineComment,
) (*AgentReview, error) {
	if agentCfg.MaxTurns <= 0 {
		return nil, errors.New("agent: MaxTurns must be > 0")
	}
	if agentCfg.WallClock <= 0 {
		return nil, errors.New("agent: WallClock must be > 0")
	}

	if err := os.MkdirAll(agentCfg.CloneRootDir, 0o755); err != nil {
		return nil, fmt.Errorf("agent: create clone root: %w", err)
	}
	if err := os.MkdirAll(agentCfg.LogsDir, 0o755); err != nil {
		return nil, fmt.Errorf("agent: create logs dir: %w", err)
	}

	slug := fmt.Sprintf("%s__%s__pr%d__%d", owner, repo, prNumber, time.Now().UnixNano())
	cloneDir := filepath.Join(agentCfg.CloneRootDir, slug)
	logPath := filepath.Join(agentCfg.LogsDir, slug+".jsonl")

	logPrefix := fmt.Sprintf("[AGENT %s/%s#%d]", owner, repo, prNumber)
	log.Printf("%s starting (clone=%s, log=%s, wall_clock=%s, max_turns=%d, gemini_comments=%d)",
		logPrefix, cloneDir, logPath, agentCfg.WallClock, agentCfg.MaxTurns, len(geminiComments))

	// Single wall-clock budget covers BOTH the clone and the claude subprocess.
	// That way a slow clone can't burn the budget and leave nothing for thinking
	// (or worse, run unbounded under the outer context).
	runCtx, cancel := context.WithTimeout(ctx, agentCfg.WallClock)
	defer cancel()

	cloneStart := time.Now()
	if err := cloneForAgent(runCtx, agentCfg.CloneRootDir, cloneDir, owner, repo, defaultBranch, prNumber, commitSHA, agentCfg.GitHubToken); err != nil {
		log.Printf("%s clone FAILED after %s: %v", logPrefix, time.Since(cloneStart), err)
		if runCtx.Err() == context.DeadlineExceeded {
			return nil, fmt.Errorf("agent: wall-clock timeout (%s) during clone", agentCfg.WallClock)
		}
		return nil, fmt.Errorf("agent: clone: %w", err)
	}
	log.Printf("%s clone ok (%s) at %s", logPrefix, time.Since(cloneStart), cloneDir)

	prompt, err := buildAgentPromptContent(geminiComments)
	if err != nil {
		return nil, fmt.Errorf("agent: build prompt: %w", err)
	}

	args := []string{
		"-p", prompt,
		"--model", "claude-opus-4-7",
		"--effort", "medium",
		"--tools", "Read,Grep,Glob,Bash",
		"--permission-mode", "bypassPermissions",
		"--output-format", "stream-json",
		"--verbose", // required by `claude` when combining --print + stream-json
	}

	// Log the argv without the full prompt (too big; promptAgentReview is static
	// and the comment list is in geminiComments count above).
	log.Printf("%s spawning claude (model=claude-opus-4-7, effort=medium, tools=Read,Grep,Glob,Bash, prompt_chars=%d)",
		logPrefix, len(prompt))

	spawnStart := time.Now()
	proc, err := spawner.Spawn(runCtx, "claude", args, cloneDir)
	if err != nil {
		log.Printf("%s spawn FAILED: %v", logPrefix, err)
		return nil, fmt.Errorf("agent: spawn claude: %w", err)
	}
	log.Printf("%s claude spawned, streaming output to %s", logPrefix, logPath)

	logFile, err := os.Create(logPath)
	if err != nil {
		_ = proc.Kill()
		_ = proc.Wait()
		return nil, fmt.Errorf("agent: create log: %w", err)
	}
	defer logFile.Close()

	// Drain stderr to a buffer so we can include it on failure. Safe to be
	// unbounded for now — dev use, sensible claude outputs.
	var stderrBuf strings.Builder
	var stderrWG sync.WaitGroup
	stderrWG.Add(1)
	go func() {
		defer stderrWG.Done()
		_, _ = io.Copy(&stderrBuf, proc.Stderr())
	}()

	// Stream stdout: tee to log file and parse turn-by-turn.
	parseResult, parseErr := parseAgentStream(proc, logFile, agentCfg.MaxTurns)

	waitErr := proc.Wait()
	stderrWG.Wait()

	if parseErr != nil {
		// Turn-cap hit or parse error — subprocess already killed inside parser.
		return nil, fmt.Errorf("agent: %w (stderr: %s)", parseErr, truncate(stderrBuf.String(), 1000))
	}

	if runCtx.Err() == context.DeadlineExceeded {
		return nil, fmt.Errorf("agent: wall-clock timeout (%s) after %d turns (stderr: %s)",
			agentCfg.WallClock, parseResult.assistantTurns, truncate(stderrBuf.String(), 1000))
	}

	if waitErr != nil {
		return nil, fmt.Errorf("agent: claude exited with error: %w (stderr: %s)",
			waitErr, truncate(stderrBuf.String(), 1000))
	}

	if parseResult.finalOutput == "" {
		log.Printf("%s claude finished with no final result after %d turn(s)", logPrefix, parseResult.assistantTurns)
		return nil, fmt.Errorf("agent: no final result emitted (stderr: %s)", truncate(stderrBuf.String(), 1000))
	}

	comments, parseErr := parseAgentJSON(parseResult.finalOutput)
	if parseErr != nil {
		log.Printf("%s final output is not valid JSON (%v); wrapping as SUMMARY", logPrefix, parseErr)
		comments = []types.LineComment{{
			FilePath:    "SUMMARY",
			LineNumber:  0,
			CommentBody: parseResult.finalOutput,
		}}
	}
	log.Printf("%s complete in %s (turns=%d, comments=%d)",
		logPrefix, time.Since(spawnStart), parseResult.assistantTurns, len(comments))

	return &AgentReview{
		Comments: comments,
		RawFinal: parseResult.finalOutput,
		CloneDir: cloneDir,
		LogPath:  logPath,
	}, nil
}

// parseAgentJSON extracts a []LineComment from the agent's final output,
// stripping any ```json fences the model may have added despite instructions.
func parseAgentJSON(raw string) ([]types.LineComment, error) {
	trimmed := strings.TrimSpace(raw)
	trimmed = strings.TrimPrefix(trimmed, "```json")
	trimmed = strings.TrimPrefix(trimmed, "```")
	trimmed = strings.TrimSuffix(trimmed, "```")
	trimmed = strings.TrimSpace(trimmed)

	var comments []types.LineComment
	if err := json.Unmarshal([]byte(trimmed), &comments); err != nil {
		return nil, err
	}
	return comments, nil
}

// buildAgentPromptContent prepends the static prompt template to a JSON block
// of Gemini comments.
func buildAgentPromptContent(geminiComments []types.LineComment) (string, error) {
	commentsJSON, err := json.MarshalIndent(geminiComments, "", "  ")
	if err != nil {
		return "", err
	}
	return promptAgentReview + string(commentsJSON), nil
}

// cacheMutexes serializes cache-repo work per (owner, repo). Each repo gets
// its own mutex so concurrent reviews of *different* repos don't block each
// other, but two reviews of the same repo share the cache fetch step.
var cacheMutexes sync.Map

func cacheLock(key string) *sync.Mutex {
	m, _ := cacheMutexes.LoadOrStore(key, &sync.Mutex{})
	return m.(*sync.Mutex)
}

// cloneForAgent prepares a working directory for the agent by:
//
//  1. Ensuring a per-repo cache clone exists at <cloneRoot>/.cache/<owner>__<repo>.
//     First time: full shallow clone (the slow step). Subsequent: skipped.
//  2. Fetching the PR's head ref into the cache (cheap incremental fetch).
//  3. Creating a `git worktree` at the requested commit in <dir>. Worktrees
//     share the cache's object store, so this step is near-instant even on
//     monorepos.
//
// The cache step holds a per-repo mutex so concurrent reviews of the same
// repo serialize their fetches; reviews of *different* repos run in parallel.
//
// Each step logs a START / DONE pair with a duration so it's obvious where
// any future slowness lives.
func cloneForAgent(ctx context.Context, cloneRoot, dir, owner, repo, defaultBranch string, prNumber int, commitSHA, token string) error {
	// Absolute paths everywhere — git's `worktree add <relative-path>` resolves
	// the path against the cmd's cwd, which would land worktrees inside the
	// cache dir. Make the cwd-binding moot.
	absCloneRoot, err := filepath.Abs(cloneRoot)
	if err != nil {
		return fmt.Errorf("abs cloneRoot: %w", err)
	}
	absDir, err := filepath.Abs(dir)
	if err != nil {
		return fmt.Errorf("abs worktree dir: %w", err)
	}

	cacheKey := fmt.Sprintf("%s/%s", owner, repo)
	cacheDir := filepath.Join(absCloneRoot, ".cache", fmt.Sprintf("%s__%s", owner, repo))
	sanitizedURL := fmt.Sprintf("https://github.com/%s/%s.git", owner, repo)
	logPrefix := fmt.Sprintf("[AGENT %s/%s#%d]", owner, repo, prNumber)

	mu := cacheLock(cacheKey)
	mu.Lock()
	defer mu.Unlock()

	// Step 1: ensure cache repo exists.
	cacheGitDir := filepath.Join(cacheDir, ".git")
	if _, err := os.Stat(cacheGitDir); os.IsNotExist(err) {
		if err := os.MkdirAll(filepath.Dir(cacheDir), 0o755); err != nil {
			return fmt.Errorf("create cache parent: %w", err)
		}
		log.Printf("%s cache MISS — initial clone of %s -> %s (depth=200) START", logPrefix, sanitizedURL, cacheDir)
		t0 := time.Now()
		url := buildCloneURL(owner, repo, token)
		cloneArgs := []string{"clone", "--depth", "200"}
		if defaultBranch != "" {
			cloneArgs = append(cloneArgs, "--branch", defaultBranch)
		}
		cloneArgs = append(cloneArgs, url, cacheDir)
		if out, err := runGit(ctx, "", cloneArgs...); err != nil {
			// Remove the partial directory so a future run won't see a half-clone
			// as a cache hit and try to fetch into a corrupt repo. Diagnostics
			// are in the returned error + log, not the on-disk leftovers.
			if rmErr := os.RemoveAll(cacheDir); rmErr != nil {
				log.Printf("%s WARN: failed to clean partial cache dir %s: %v", logPrefix, cacheDir, rmErr)
			}
			return fmt.Errorf("git clone (cache init): %w (%s)", err, redactToken(out, token))
		}
		log.Printf("%s cache initial clone DONE in %s", logPrefix, time.Since(t0))
	} else if err != nil {
		return fmt.Errorf("stat cache .git: %w", err)
	} else {
		log.Printf("%s cache HIT at %s", logPrefix, cacheDir)
	}

	// Step 2: fetch the PR head ref into the cache. Use a `+` refspec to force
	// update if the PR was rebased/force-pushed since last fetch. We also keep
	// it shallow at depth=200 to bound size.
	fetchSpec := fmt.Sprintf("+pull/%d/head:refs/agent-pr/%d", prNumber, prNumber)
	log.Printf("%s git fetch origin %s (in cache) START", logPrefix, fetchSpec)
	t1 := time.Now()
	if out, err := runGit(ctx, cacheDir, "fetch", "--depth", "200", "origin", fetchSpec); err != nil {
		return fmt.Errorf("git fetch pr (cache): %w (%s)", err, redactToken(out, token))
	}
	log.Printf("%s git fetch DONE in %s", logPrefix, time.Since(t1))

	// Step 3: create a worktree for this review at the requested commit.
	// Worktrees share the cache's object store, so this is near-instant.
	// Use the absolute target path so the worktree lands where the caller
	// expects regardless of git's cwd.
	log.Printf("%s git worktree add %s @ %s START", logPrefix, absDir, commitSHA)
	t2 := time.Now()
	if out, err := runGit(ctx, cacheDir, "worktree", "add", "--detach", absDir, commitSHA); err != nil {
		return fmt.Errorf("git worktree add: %w (%s)", err, out)
	}
	log.Printf("%s git worktree add DONE in %s", logPrefix, time.Since(t2))
	return nil
}

func buildCloneURL(owner, repo, token string) string {
	if token != "" {
		return fmt.Sprintf("https://x-access-token:%s@github.com/%s/%s.git", token, owner, repo)
	}
	return fmt.Sprintf("https://github.com/%s/%s.git", owner, repo)
}

// redactToken replaces any literal occurrence of token in s with "***".
// Used before including git output in error messages so an authenticated
// clone URL echoed by git can't leak the GitHub installation token.
func redactToken(s, token string) string {
	if token == "" {
		return s
	}
	return strings.ReplaceAll(s, token, "***")
}

func runGit(ctx context.Context, cwd string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", args...)
	if cwd != "" {
		cmd.Dir = cwd
	}
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// parseAgentStream reads stream-json events line-by-line from the subprocess,
// counts assistant turns, extracts the final result, and tees the raw bytes
// into a log file. Kills the subprocess when maxTurns is exceeded.
type agentParseResult struct {
	finalOutput    string
	assistantTurns int
}

func parseAgentStream(proc SpawnedProcess, logFile io.Writer, maxTurns int) (*agentParseResult, error) {
	result := &agentParseResult{}
	scanner := bufio.NewScanner(proc.Stdout())
	scanner.Buffer(make([]byte, 64*1024), 8*1024*1024)

	for scanner.Scan() {
		line := scanner.Bytes()
		_, _ = logFile.Write(line)
		_, _ = logFile.Write([]byte{'\n'})

		var ev map[string]any
		if err := json.Unmarshal(line, &ev); err != nil {
			continue
		}

		switch ev["type"] {
		case "assistant":
			result.assistantTurns++
			if result.assistantTurns%5 == 0 || result.assistantTurns == 1 {
				log.Printf("[AGENT] assistant turn %d/%d", result.assistantTurns, maxTurns)
			}
			if result.assistantTurns > maxTurns {
				_ = proc.Kill()
				return nil, fmt.Errorf("exceeded max-turns (%d)", maxTurns)
			}
		case "result":
			if s, ok := ev["result"].(string); ok {
				result.finalOutput = s
			}
		}
	}

	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("read stdout: %w", err)
	}
	return result, nil
}

func truncate(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max] + "…(truncated)"
}

// DefaultSpawner wraps exec.CommandContext for production use.
type DefaultSpawner struct{}

func (DefaultSpawner) Spawn(ctx context.Context, name string, args []string, dir string) (SpawnedProcess, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	if dir != "" {
		cmd.Dir = dir
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	return &execProcess{cmd: cmd, stdout: stdout, stderr: stderr}, nil
}

type execProcess struct {
	cmd    *exec.Cmd
	stdout io.ReadCloser
	stderr io.ReadCloser
}

func (e *execProcess) Stdout() io.Reader { return e.stdout }
func (e *execProcess) Stderr() io.Reader { return e.stderr }
func (e *execProcess) Wait() error       { return e.cmd.Wait() }
func (e *execProcess) Kill() error {
	if e.cmd.Process == nil {
		return nil
	}
	return e.cmd.Process.Kill()
}
