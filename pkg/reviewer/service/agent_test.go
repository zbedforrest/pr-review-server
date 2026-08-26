package service

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// fakeProcess implements SpawnedProcess by replaying a canned stdout buffer.
type fakeProcess struct {
	stdout    *bytes.Buffer
	stderr    *bytes.Buffer
	waitErr   error
	killed    bool
	killCh    chan struct{}
	exitAfter time.Duration
}

func (f *fakeProcess) Stdout() io.Reader { return f.stdout }
func (f *fakeProcess) Stderr() io.Reader { return f.stderr }
func (f *fakeProcess) Wait() error {
	if f.exitAfter > 0 {
		select {
		case <-time.After(f.exitAfter):
		case <-f.killCh:
		}
	}
	return f.waitErr
}
func (f *fakeProcess) Kill() error {
	f.killed = true
	close(f.killCh)
	return nil
}

// skipIfNoGit skips tests that need a working git binary (the clone step).
func skipIfNoGit(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
}

// setupLocalBareRepo creates a tiny local bare repo with a couple of commits
// and a fake "pull/1/head" ref so cloneForAgent can exercise its real codepath
// without hitting the network.
func setupLocalBareRepo(t *testing.T) (bareURL string, commitSHA string) {
	t.Helper()
	skipIfNoGit(t)

	srcDir := t.TempDir()
	run := func(dir string, args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=test",
			"GIT_AUTHOR_EMAIL=test@example.com",
			"GIT_COMMITTER_NAME=test",
			"GIT_COMMITTER_EMAIL=test@example.com",
		)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v (%s)", args, err, out)
		}
	}

	run(srcDir, "init", "-q", "-b", "main", ".")
	if err := os.WriteFile(filepath.Join(srcDir, "hello.txt"), []byte("hi"), 0o644); err != nil {
		t.Fatal(err)
	}
	run(srcDir, "add", ".")
	run(srcDir, "commit", "-q", "-m", "initial")
	run(srcDir, "checkout", "-q", "-b", "featbranch")
	if err := os.WriteFile(filepath.Join(srcDir, "change.txt"), []byte("new"), 0o644); err != nil {
		t.Fatal(err)
	}
	run(srcDir, "add", ".")
	run(srcDir, "commit", "-q", "-m", "pr commit")
	// Push the feat branch to a bare repo under the `refs/pull/1/head` ref so
	// the cloneForAgent `git fetch origin pull/1/head:pr-1` step succeeds.
	bare := t.TempDir()
	run(bare, "init", "-q", "--bare", ".")
	run(srcDir, "push", "-q", bare, "main:main")
	run(srcDir, "push", "-q", bare, "featbranch:refs/pull/1/head")

	// Resolve the commit SHA of the feat branch tip.
	cmd := exec.Command("git", "rev-parse", "featbranch")
	cmd.Dir = srcDir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("rev-parse: %v (%s)", err, out)
	}
	sha := strings.TrimSpace(string(out))
	return bare, sha
}

// Canned stream-json events used by the happy-path test.
const fakeStream = `{"type":"system","subtype":"init"}
{"type":"assistant","message":{"content":[{"type":"text","text":"Let me read the code."},{"type":"tool_use","name":"Read","input":{"path":"foo.go"}}]}}
{"type":"user","message":{"content":[{"type":"tool_result","content":"package foo\n"}]}}
{"type":"assistant","message":{"content":[{"type":"text","text":"Final markdown output goes here."}]}}
{"type":"result","result":"# Agent verdict\n\nLooks good. Approve.\n"}
`

// TestParseAgentStream_HappyPath exercises the stream parser directly with a
// canned stream-json payload.
func TestParseAgentStream_HappyPath(t *testing.T) {
	proc := &fakeProcess{
		stdout: bytes.NewBufferString(fakeStream),
		stderr: &bytes.Buffer{},
		killCh: make(chan struct{}),
	}
	var logBuf bytes.Buffer
	res, err := parseAgentStream(proc, &logBuf, 10)
	if err != nil {
		t.Fatalf("parseAgentStream: %v", err)
	}
	if got, want := res.assistantTurns, 2; got != want {
		t.Errorf("turns: got %d want %d", got, want)
	}
	if !strings.Contains(res.finalOutput, "Approve") {
		t.Errorf("final output missing: %q", res.finalOutput)
	}
	if !bytes.Contains(logBuf.Bytes(), []byte(`"type":"result"`)) {
		t.Error("log file missing result event")
	}
}

// TestParseAgentStream_MaxTurnsKills — exceeds cap, parser kills the proc.
func TestParseAgentStream_MaxTurnsKills(t *testing.T) {
	// Five assistant messages; max turns is 3.
	stream := strings.Repeat(`{"type":"assistant","message":{"content":[{"type":"text","text":"x"}]}}`+"\n", 5)
	proc := &fakeProcess{
		stdout: bytes.NewBufferString(stream),
		stderr: &bytes.Buffer{},
		killCh: make(chan struct{}),
	}
	var logBuf bytes.Buffer
	_, err := parseAgentStream(proc, &logBuf, 3)
	if err == nil {
		t.Fatal("expected max-turns error")
	}
	if !strings.Contains(err.Error(), "max-turns") {
		t.Errorf("error should mention max-turns: %v", err)
	}
	if !proc.killed {
		t.Error("expected proc to be killed")
	}
}

// fakeSpawner returns a canned process regardless of the command.
type fakeSpawner struct {
	proc     *fakeProcess
	name     string
	args     []string
	dir      string
	spawnErr error
}

func (s *fakeSpawner) Spawn(ctx context.Context, name string, args []string, dir string) (SpawnedProcess, error) {
	s.name = name
	s.args = append([]string(nil), args...)
	s.dir = dir
	return s.proc, s.spawnErr
}

// seedAgentCache pre-populates the clone cache for owner/repo with a clone of
// the local bare repo, so cloneForAgent's cache-hit path runs and never
// touches github.com.
func seedAgentCache(t *testing.T, cloneRoot, owner, repo, bareURL string) {
	t.Helper()
	cacheDir := filepath.Join(cloneRoot, ".cache", owner+"__"+repo)
	if err := os.MkdirAll(filepath.Dir(cacheDir), 0o755); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("git", "clone", "--quiet", bareURL, cacheDir)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("seed cache clone: %v (%s)", err, out)
	}
}

func TestParseAgentStream_CapturesStreamError(t *testing.T) {
	stream := `{"type":"assistant","message":{"content":[{"type":"text","text":"x"}]}}
{"type":"result","subtype":"error_during_execution","is_error":true,"result":"API Error: spend limit exceeded"}
`
	proc := &fakeProcess{
		stdout: bytes.NewBufferString(stream),
		stderr: &bytes.Buffer{},
		killCh: make(chan struct{}),
	}
	var logBuf bytes.Buffer
	res, err := parseAgentStream(proc, &logBuf, 10)
	if err != nil {
		t.Fatalf("parseAgentStream: %v", err)
	}
	if !strings.Contains(res.streamErr, "spend limit exceeded") {
		t.Errorf("streamErr missing CLI error: %q", res.streamErr)
	}
	if !strings.Contains(res.diagnostic(), "error_during_execution") {
		t.Errorf("diagnostic missing subtype: %q", res.diagnostic())
	}
}

func TestParseAgentStream_DiagnosticFallsBackToLastEvent(t *testing.T) {
	stream := `{"type":"system","subtype":"init"}
{"type":"assistant","message":{"content":[{"type":"text","text":"thinking"}]}}
`
	proc := &fakeProcess{
		stdout: bytes.NewBufferString(stream),
		stderr: &bytes.Buffer{},
		killCh: make(chan struct{}),
	}
	var logBuf bytes.Buffer
	res, err := parseAgentStream(proc, &logBuf, 10)
	if err != nil {
		t.Fatalf("parseAgentStream: %v", err)
	}
	if res.streamErr != "" {
		t.Errorf("unexpected streamErr: %q", res.streamErr)
	}
	if !strings.Contains(res.diagnostic(), "thinking") {
		t.Errorf("diagnostic should carry the last event: %q", res.diagnostic())
	}
}

// TestParseAgentStream_KillsOnScannerOverflow — a line over the 8MB scanner
// cap must kill the subprocess, or proc.Wait() stalls on the undrained pipe.
func TestParseAgentStream_KillsOnScannerOverflow(t *testing.T) {
	huge := `{"type":"assistant","message":{"content":[{"type":"text","text":"` +
		strings.Repeat("x", 9*1024*1024) + `"}]}}` + "\n"
	proc := &fakeProcess{
		stdout: bytes.NewBufferString(huge),
		stderr: &bytes.Buffer{},
		killCh: make(chan struct{}),
	}
	var logBuf bytes.Buffer
	_, err := parseAgentStream(proc, &logBuf, 10)
	if err == nil {
		t.Fatal("expected scanner overflow error")
	}
	if !proc.killed {
		t.Error("expected proc to be killed on scanner error")
	}
}

func TestParseAgentStream_CapturesServedModel(t *testing.T) {
	stream := `{"type":"system","subtype":"init","model":"claude-opus-4-8"}
{"type":"assistant","message":{"model":"claude-opus-4-8","content":[{"type":"text","text":"x"}]}}
{"type":"result","result":"done"}
`
	proc := &fakeProcess{
		stdout: bytes.NewBufferString(stream),
		stderr: &bytes.Buffer{},
		killCh: make(chan struct{}),
	}
	var logBuf bytes.Buffer
	res, err := parseAgentStream(proc, &logBuf, 10)
	if err != nil {
		t.Fatalf("parseAgentStream: %v", err)
	}
	if len(res.servedModels) != 1 || res.servedModels[0] != "claude-opus-4-8" {
		t.Errorf("servedModels: got %v want [claude-opus-4-8]", res.servedModels)
	}
}

func TestParseAgentStream_IgnoresSyntheticModel(t *testing.T) {
	stream := `{"type":"assistant","message":{"model":"<synthetic>","content":[{"type":"text","text":"error"}]}}
{"type":"assistant","message":{"model":"claude-fable-5","content":[{"type":"text","text":"x"}]}}
`
	proc := &fakeProcess{
		stdout: bytes.NewBufferString(stream),
		stderr: &bytes.Buffer{},
		killCh: make(chan struct{}),
	}
	var logBuf bytes.Buffer
	res, err := parseAgentStream(proc, &logBuf, 10)
	if err != nil {
		t.Fatalf("parseAgentStream: %v", err)
	}
	if len(res.servedModels) != 1 || res.servedModels[0] != "claude-fable-5" {
		t.Errorf("servedModels: got %v want [claude-fable-5] (synthetic must be skipped)", res.servedModels)
	}
}

func TestParseAgentStream_RecordsMidRunModelSwitch(t *testing.T) {
	stream := `{"type":"system","subtype":"init","model":"claude-fable-5"}
{"type":"assistant","message":{"model":"claude-fable-5","content":[{"type":"text","text":"x"}]}}
{"type":"assistant","message":{"model":"claude-opus-4-8","content":[{"type":"text","text":"y"}]}}
`
	proc := &fakeProcess{
		stdout: bytes.NewBufferString(stream),
		stderr: &bytes.Buffer{},
		killCh: make(chan struct{}),
	}
	var logBuf bytes.Buffer
	res, err := parseAgentStream(proc, &logBuf, 10)
	if err != nil {
		t.Fatalf("parseAgentStream: %v", err)
	}
	if len(res.servedModels) != 2 || res.servedModels[0] != "claude-fable-5" || res.servedModels[1] != "claude-opus-4-8" {
		t.Errorf("servedModels: got %v want [claude-fable-5 claude-opus-4-8]", res.servedModels)
	}
}

func TestModelMatches(t *testing.T) {
	cases := []struct {
		requested, served string
		want              bool
	}{
		{"claude-fable-5", "claude-fable-5", true},
		{"claude-fable-5", "claude-fable-5-20260115", true},
		{"claude-fable-5-20260115", "claude-fable-5", true},
		{"opus", "claude-opus-4-8", true},
		{"claude-fable-5", "claude-opus-4-8", false},
		{"claude-opus-4-8", "claude-fable-5", false},
		{"Claude-Fable-5", "claude-fable-5", true},
		// Documented blind spot, pinned deliberately: a same-family version
		// swap where one id prefixes the other is NOT detected. Configure
		// full model ids so this shape cannot arise.
		{"claude-opus-4", "claude-opus-4-8", true},
	}
	for _, c := range cases {
		if got := modelMatches(c.requested, c.served); got != c.want {
			t.Errorf("modelMatches(%q, %q) = %v, want %v", c.requested, c.served, got, c.want)
		}
	}
}

func TestRunAgentReview_DetectsModelFallback(t *testing.T) {
	bare, sha := setupLocalBareRepo(t)
	cloneRoot := t.TempDir()
	seedAgentCache(t, cloneRoot, "acme", "example", bare)

	stream := `{"type":"system","subtype":"init","model":"claude-opus-4-8"}
{"type":"assistant","message":{"model":"claude-opus-4-8","content":[{"type":"text","text":"x"}]}}
{"type":"result","result":"[]"}
`
	spawner := &fakeSpawner{proc: &fakeProcess{
		stdout: bytes.NewBufferString(stream),
		stderr: &bytes.Buffer{},
		killCh: make(chan struct{}),
	}}

	cfg := AgentConfig{
		CloneRootDir: cloneRoot,
		LogsDir:      t.TempDir(),
		WallClock:    time.Minute,
		MaxTurns:     10,
		Model:        "claude-fable-5",
	}

	out, err := RunAgentReview(context.Background(), cfg, spawner, "acme", "example", "main", 1, sha, nil)
	if err != nil {
		t.Fatalf("RunAgentReview: %v", err)
	}
	if !out.ModelFallback {
		t.Error("expected ModelFallback=true for opus stream under a fable request")
	}
	if out.ServedModel != "claude-opus-4-8" || out.RequestedModel != "claude-fable-5" {
		t.Errorf("model fields: served=%q requested=%q", out.ServedModel, out.RequestedModel)
	}
	if out.Backend != AgentBackendClaude || !out.ServingModelVerified {
		t.Errorf("backend metadata: backend=%q verified=%t", out.Backend, out.ServingModelVerified)
	}
}

func TestRunAgentReview_DetectsMidRunFallback(t *testing.T) {
	bare, sha := setupLocalBareRepo(t)
	cloneRoot := t.TempDir()
	seedAgentCache(t, cloneRoot, "acme", "example", bare)

	// Starts on the requested model, falls back partway through, then
	// RECOVERS — the transient fallback must still be flagged.
	stream := `{"type":"system","subtype":"init","model":"claude-fable-5"}
{"type":"assistant","message":{"model":"claude-fable-5","content":[{"type":"text","text":"x"}]}}
{"type":"assistant","message":{"model":"claude-opus-4-8","content":[{"type":"text","text":"y"}]}}
{"type":"assistant","message":{"model":"claude-fable-5","content":[{"type":"text","text":"z"}]}}
{"type":"result","result":"[]"}
`
	spawner := &fakeSpawner{proc: &fakeProcess{
		stdout: bytes.NewBufferString(stream),
		stderr: &bytes.Buffer{},
		killCh: make(chan struct{}),
	}}

	cfg := AgentConfig{
		CloneRootDir: cloneRoot,
		LogsDir:      t.TempDir(),
		WallClock:    time.Minute,
		MaxTurns:     10,
		Model:        "claude-fable-5",
	}

	out, err := RunAgentReview(context.Background(), cfg, spawner, "acme", "example", "main", 1, sha, nil)
	if err != nil {
		t.Fatalf("RunAgentReview: %v", err)
	}
	if !out.ModelFallback {
		t.Error("expected ModelFallback=true for a mid-run switch to a non-matching model")
	}
	if out.ServedModel != "claude-opus-4-8" {
		t.Errorf("ServedModel should name the fallback model, got %q", out.ServedModel)
	}
}

func TestRunAgentReview_NoFallbackOnMatchingModel(t *testing.T) {
	bare, sha := setupLocalBareRepo(t)
	cloneRoot := t.TempDir()
	seedAgentCache(t, cloneRoot, "acme", "example", bare)

	stream := `{"type":"system","subtype":"init","model":"claude-fable-5"}
{"type":"assistant","message":{"model":"claude-fable-5","content":[{"type":"text","text":"x"}]}}
{"type":"result","result":"[]"}
`
	spawner := &fakeSpawner{proc: &fakeProcess{
		stdout: bytes.NewBufferString(stream),
		stderr: &bytes.Buffer{},
		killCh: make(chan struct{}),
	}}

	cfg := AgentConfig{
		CloneRootDir: cloneRoot,
		LogsDir:      t.TempDir(),
		WallClock:    time.Minute,
		MaxTurns:     10,
		Model:        "claude-fable-5",
	}

	out, err := RunAgentReview(context.Background(), cfg, spawner, "acme", "example", "main", 1, sha, nil)
	if err != nil {
		t.Fatalf("RunAgentReview: %v", err)
	}
	if out.ModelFallback {
		t.Errorf("unexpected fallback: served=%q requested=%q", out.ServedModel, out.RequestedModel)
	}
}

func TestRunAgentReview_FailureSurfacesStreamErrorAndPersistsLog(t *testing.T) {
	bare, sha := setupLocalBareRepo(t)
	cloneRoot := t.TempDir()
	seedAgentCache(t, cloneRoot, "acme", "example", bare)

	stream := `{"type":"system","subtype":"init"}
{"type":"assistant","message":{"content":[{"type":"text","text":"x"}]}}
{"type":"result","subtype":"error_during_execution","is_error":true,"result":"API Error: spend limit exceeded"}
`
	spawner := &fakeSpawner{proc: &fakeProcess{
		stdout:  bytes.NewBufferString(stream),
		stderr:  &bytes.Buffer{},
		waitErr: errors.New("exit status 1"),
		killCh:  make(chan struct{}),
	}}

	var sinkCalls int
	var sinkContent []byte
	cfg := AgentConfig{
		CloneRootDir: cloneRoot,
		LogsDir:      t.TempDir(),
		WallClock:    time.Minute,
		MaxTurns:     10,
		FailureLogSink: func(logPath string) {
			sinkCalls++
			b, err := os.ReadFile(logPath)
			if err != nil {
				t.Errorf("sink could not read log: %v", err)
			}
			sinkContent = b
		},
	}

	_, err := RunAgentReview(context.Background(), cfg, spawner, "acme", "example", "main", 1, sha, nil)
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "spend limit exceeded") {
		t.Errorf("error should carry the CLI's stream error, got: %v", err)
	}
	if sinkCalls != 1 {
		t.Fatalf("FailureLogSink calls: got %d want 1", sinkCalls)
	}
	if !bytes.Contains(sinkContent, []byte("error_during_execution")) {
		t.Errorf("persisted log missing error event: %s", sinkContent)
	}
	assertLogsDirEmpty(t, cfg.LogsDir)
}

// TestRunAgentReview_FailureRedactsCredentialFromError — failure messages feed
// the run's error_summary, which the API exposes, so a credential echoed on
// stderr must never survive into the returned error.
func TestRunAgentReview_FailureRedactsCredentialFromError(t *testing.T) {
	bare, sha := setupLocalBareRepo(t)
	cloneRoot := t.TempDir()
	seedAgentCache(t, cloneRoot, "acme", "example", bare)

	const credential = "sk-test-secret-credential"
	spawner := &fakeSpawner{proc: &fakeProcess{
		stdout:  bytes.NewBufferString(""),
		stderr:  bytes.NewBufferString("auth failed for key " + credential + "\n"),
		waitErr: errors.New("exit status 1"),
		killCh:  make(chan struct{}),
	}}

	cfg := AgentConfig{
		CloneRootDir:    cloneRoot,
		LogsDir:         t.TempDir(),
		WallClock:       time.Minute,
		MaxTurns:        10,
		AnthropicAPIKey: credential,
	}

	_, err := RunAgentReview(context.Background(), cfg, spawner, "acme", "example", "main", 1, sha, nil)
	if err == nil {
		t.Fatal("expected error")
	}
	if strings.Contains(err.Error(), credential) {
		t.Errorf("error must not contain the credential: %v", err)
	}
	if !strings.Contains(err.Error(), "auth failed for key ***") {
		t.Errorf("error should carry the redacted stderr, got: %v", err)
	}
}

func assertLogsDirEmpty(t *testing.T, logsDir string) {
	t.Helper()
	entries, err := os.ReadDir(logsDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Errorf("expected log file to be removed, found %d file(s)", len(entries))
	}
}

// TestRunAgentReview_ErrorResultEventWithZeroExit — an error result event
// with exit 0 must fail the review, not publish as a successful SUMMARY.
func TestRunAgentReview_ErrorResultEventWithZeroExit(t *testing.T) {
	bare, sha := setupLocalBareRepo(t)
	cloneRoot := t.TempDir()
	seedAgentCache(t, cloneRoot, "acme", "example", bare)

	stream := `{"type":"assistant","message":{"content":[{"type":"text","text":"x"}]}}
{"type":"result","subtype":"error_during_execution","is_error":true,"result":"API Error: overloaded"}
`
	spawner := &fakeSpawner{proc: &fakeProcess{
		stdout: bytes.NewBufferString(stream),
		stderr: &bytes.Buffer{},
		killCh: make(chan struct{}),
	}}

	var sinkCalls int
	cfg := AgentConfig{
		CloneRootDir:   cloneRoot,
		LogsDir:        t.TempDir(),
		WallClock:      time.Minute,
		MaxTurns:       10,
		FailureLogSink: func(string) { sinkCalls++ },
	}

	_, err := RunAgentReview(context.Background(), cfg, spawner, "acme", "example", "main", 1, sha, nil)
	if err == nil {
		t.Fatal("expected error: an error result event with exit 0 must not be a successful review")
	}
	if !strings.Contains(err.Error(), "overloaded") {
		t.Errorf("error should carry the CLI's stream error, got: %v", err)
	}
	if sinkCalls != 1 {
		t.Errorf("FailureLogSink calls: got %d want 1", sinkCalls)
	}
}

// TestRunAgentReview_FailureWithoutSinkKeepsLog — with no sink (local dev)
// the on-disk jsonl is the only record of a failed run and must survive.
func TestRunAgentReview_FailureWithoutSinkKeepsLog(t *testing.T) {
	bare, sha := setupLocalBareRepo(t)
	cloneRoot := t.TempDir()
	seedAgentCache(t, cloneRoot, "acme", "example", bare)

	spawner := &fakeSpawner{proc: &fakeProcess{
		stdout:  bytes.NewBufferString(`{"type":"assistant","message":{"content":[{"type":"text","text":"x"}]}}` + "\n"),
		stderr:  &bytes.Buffer{},
		waitErr: errors.New("exit status 1"),
		killCh:  make(chan struct{}),
	}}

	cfg := AgentConfig{
		CloneRootDir: cloneRoot,
		LogsDir:      t.TempDir(),
		WallClock:    time.Minute,
		MaxTurns:     10,
	}

	if _, err := RunAgentReview(context.Background(), cfg, spawner, "acme", "example", "main", 1, sha, nil); err == nil {
		t.Fatal("expected error")
	}
	entries, err := os.ReadDir(cfg.LogsDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Errorf("expected the failure log to be kept when no sink is configured, found %d file(s)", len(entries))
	}
}

func TestRunAgentReview_SuccessDoesNotPersistLog(t *testing.T) {
	bare, sha := setupLocalBareRepo(t)
	cloneRoot := t.TempDir()
	seedAgentCache(t, cloneRoot, "acme", "example", bare)

	spawner := &fakeSpawner{proc: &fakeProcess{
		stdout: bytes.NewBufferString(fakeStream),
		stderr: &bytes.Buffer{},
		killCh: make(chan struct{}),
	}}

	var sinkCalls int
	cfg := AgentConfig{
		CloneRootDir:   cloneRoot,
		LogsDir:        t.TempDir(),
		WallClock:      time.Minute,
		MaxTurns:       10,
		FailureLogSink: func(string) { sinkCalls++ },
	}

	out, err := RunAgentReview(context.Background(), cfg, spawner, "acme", "example", "main", 1, sha, nil)
	if err != nil {
		t.Fatalf("RunAgentReview: %v", err)
	}
	if out == nil || len(out.Comments) == 0 {
		t.Error("expected comments from the canned stream")
	}
	if sinkCalls != 0 {
		t.Errorf("FailureLogSink must not fire on success, fired %d time(s)", sinkCalls)
	}
	assertLogsDirEmpty(t, cfg.LogsDir)
}

// TestCloneForAgent_LocalBare exercises the real clone path against a local
// bare repo (no network).
func TestCloneForAgent_LocalBare(t *testing.T) {
	bare, sha := setupLocalBareRepo(t)

	dir := filepath.Join(t.TempDir(), "clone")
	// Pass the bare path as "owner" and an empty "repo" with no token — then
	// buildCloneURL returns "https://github.com/bare/.git" which is wrong. We
	// need to bypass buildCloneURL. Use a small wrapper: call runGit directly.
	if out, err := runGit(context.Background(), "", "clone", "--depth", "200", bare, dir); err != nil {
		t.Fatalf("clone: %v (%s)", err, out)
	}
	if out, err := runGit(context.Background(), dir, "fetch", "--depth", "200", "origin", "pull/1/head:pr-1"); err != nil {
		t.Fatalf("fetch pr: %v (%s)", err, out)
	}
	if out, err := runGit(context.Background(), dir, "checkout", sha); err != nil {
		t.Fatalf("checkout: %v (%s)", err, out)
	}
	// change.txt was added on featbranch, should now exist in cwd.
	if _, err := os.Stat(filepath.Join(dir, "change.txt")); err != nil {
		t.Errorf("change.txt missing after checkout: %v", err)
	}
}

// TestAuthHeaderArgs locks in that token-bearing args use http.extraheader
// (so the token never lands in .git/config or in the persisted clone URL),
// and that the public path returns no auth args at all.
func TestAuthHeaderArgs(t *testing.T) {
	if got := authHeaderArgs(""); got != nil {
		t.Errorf("public token: expected nil, got %v", got)
	}
	got := authHeaderArgs("tok")
	if len(got) != 2 || got[0] != "-c" {
		t.Fatalf("expected [-c <header>], got %v", got)
	}
	expected := "http.extraheader=Authorization: Basic " +
		base64.StdEncoding.EncodeToString([]byte("x-access-token:tok"))
	if got[1] != expected {
		t.Errorf("header arg: got %q want %q", got[1], expected)
	}
	// Token should NOT appear unencoded in the args.
	for _, a := range got {
		if strings.Contains(a, "tok@github") {
			t.Errorf("plain token leaked into argv: %q", a)
		}
	}
}

// TestRedactToken locks in that any literal token occurrence in git output
// gets scrubbed before it reaches an error wrap. Regression guard against
// a future change that forgets to redact.
func TestRedactToken(t *testing.T) {
	const tok = "ghs_thisisasecrettoken"
	in := "fatal: could not read Username for 'https://x-access-token:" + tok + "@github.com'"
	out := redactToken(in, tok)
	if strings.Contains(out, tok) {
		t.Errorf("token leaked through redactToken: %q", out)
	}
	if !strings.Contains(out, "***") {
		t.Errorf("expected ***, got: %q", out)
	}
	// Empty token must be a no-op (don't replace empty string).
	if got := redactToken("hello", ""); got != "hello" {
		t.Errorf("empty token should no-op, got %q", got)
	}
}

// TestParseAgentJSON covers the agent-output-shape defenses: code-fences,
// conversational prefix/suffix, and clean JSON.
func TestParseAgentJSON(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want int // expected comment count
	}{
		{"clean array", `[{"file_path":"a.go","line_number":1,"comment_body":"x"}]`, 1},
		{"json fence", "```json\n[{\"file_path\":\"a.go\",\"line_number\":1,\"comment_body\":\"x\"}]\n```", 1},
		{"plain fence", "```\n[{\"file_path\":\"a.go\",\"line_number\":1,\"comment_body\":\"x\"}]\n```", 1},
		{"conversational prefix", "Here is the review:\n\n[{\"file_path\":\"a.go\",\"line_number\":1,\"comment_body\":\"x\"}]", 1},
		{"prefix and suffix", "Sure, here you go:\n[{\"file_path\":\"a.go\",\"line_number\":1,\"comment_body\":\"x\"}]\n\nLet me know!", 1},
		// Critical regression: array starts at position 0 (no prefix) but
		// has a trailing suffix. Prior to the start>=0 fix this slipped past
		// the slicing branch and json.Unmarshal failed on the trailing text.
		{"suffix only", "[{\"file_path\":\"a.go\",\"line_number\":1,\"comment_body\":\"x\"}]\n\nHope that helps!", 1},
		{"empty array", `[]`, 0},
		{"empty array with suffix", "[]\nNo issues found.", 0},
		// Bracket-balance regressions: suffix prose CONTAINING brackets used
		// to defeat the last-"]" slice and collapse the review to a SUMMARY
		// blob (observed at ~17-25% of runs under some configs).
		{"suffix with brackets", "[{\"file_path\":\"a.go\",\"line_number\":1,\"comment_body\":\"x\"}]\n\nNote: check arr[i] bounds too!", 1},
		{"suffix with fenced code", "[{\"file_path\":\"a.go\",\"line_number\":1,\"comment_body\":\"x\"}]\n```suggestion\nvals[0] = y\n```", 1},
		{"brackets inside string body", `[{"file_path":"a.go","line_number":1,"comment_body":"use arr[0] and \"quoted [x]\" here"}]`, 1},
		{"nested arrays in body", `[{"file_path":"a.go","line_number":1,"comment_body":"matrix"},{"file_path":"b.go","line_number":2,"comment_body":"[[1,2],[3]]"}]`, 2},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := parseAgentJSON(c.in)
			if err != nil {
				t.Fatalf("parseAgentJSON: %v (input=%q)", err, c.in)
			}
			if len(got) != c.want {
				t.Errorf("got %d comments, want %d", len(got), c.want)
			}
		})
	}
}

func TestParseAgentJSONCapsFutureOnlyNonSecuritySeverity(t *testing.T) {
	raw := `[{
        "file_path":"config.go",
        "line_number":12,
        "comment_body":"A later caller could omit the setting.",
        "importance":"CRITICAL",
        "finding_contract":{
            "schema_version":1,
            "finding_kind":"latent_hazard",
            "materiality":"future_condition_only",
            "current_impact":"No current caller omits the setting.",
            "counterfactual_trigger":"A later caller omits the setting.",
            "falsifiability":"falsifiable",
            "falsifiable_condition":"The setting is absent.",
            "expected_observable":"The request returns an error.",
            "subjects":[{"kind":"config_key","path":"config.go","name":"required_setting"}],
            "uncertainty":"Future callers are not known.",
            "severity_rationale":"The current PR creates no active failure."
        }
    }]`
	comments, err := parseAgentJSON(raw)
	if err != nil {
		t.Fatal(err)
	}
	if comments[0].Importance != "LOW" {
		t.Fatalf("importance = %q", comments[0].Importance)
	}
}

func TestParseAgentJSONNormalizesContractBeforePolicy(t *testing.T) {
	raw := `[{
        "file_path":"config.go",
        "line_number":12,
        "comment_body":"A later caller could omit the setting.",
        "importance":"CRITICAL",
        "finding_contract":{
            "schema_version":1,
            "finding_kind":"latent_hazard",
            "materiality":"future_condition_only",
            "current_impact":" No current caller omits the setting. ",
            "counterfactual_trigger":" A later caller omits the setting. ",
            "falsifiability":"falsifiable",
            "falsifiable_condition":" The setting is absent. ",
            "expected_observable":" The request returns an error. ",
            "subjects":[{"kind":"config_key","path":" config.go ","name":" required_setting "}],
            "uncertainty":" Future callers are not known. ",
            "severity_rationale":" The current PR creates no active failure. "
        }
    }]`
	comments, err := parseAgentJSON(raw)
	if err != nil {
		t.Fatal(err)
	}
	if comments[0].Importance != "LOW" {
		t.Fatalf("importance = %q", comments[0].Importance)
	}
	if comments[0].FindingContract.CurrentImpact != "No current caller omits the setting." {
		t.Fatalf("impact = %q", comments[0].FindingContract.CurrentImpact)
	}
}

func TestParseAgentJSONCapsMissingAndInvalidContractsAtMedium(t *testing.T) {
	for name, raw := range map[string]string{
		"missing": `[{
            "file_path":"config.go",
            "line_number":12,
            "comment_body":"A claim without a contract.",
            "importance":"CRITICAL"
        }]`,
		"invalid": `[{
            "file_path":"config.go",
            "line_number":12,
            "comment_body":"A claim with an invalid contract.",
            "importance":"CRITICAL",
            "finding_contract":{"schema_version":1}
        }]`,
	} {
		t.Run(name, func(t *testing.T) {
			comments, err := parseAgentJSON(raw)
			if err != nil {
				t.Fatal(err)
			}
			if comments[0].Importance != "MEDIUM" {
				t.Fatalf("importance = %q", comments[0].Importance)
			}
		})
	}
}

func TestParseAgentJSONCapsFutureOnlySecurityRiskWithoutPolicy(t *testing.T) {
	raw := `[{
        "file_path":"auth.go",
        "line_number":12,
        "comment_body":"A later configuration could expose credentials.",
        "importance":"CRITICAL",
        "finding_contract":{
            "schema_version":1,
            "finding_kind":"security_risk",
            "materiality":"future_condition_only",
            "current_impact":"The current configuration does not expose credentials.",
            "counterfactual_trigger":"A later deployment enables public diagnostics.",
            "falsifiability":"falsifiable",
            "falsifiable_condition":"Public diagnostics are enabled.",
            "expected_observable":"Credential values appear in the response.",
            "subjects":[{"kind":"symbol","path":"auth.go","name":"diagnostics"}],
            "uncertainty":"Deployment configuration can change independently.",
            "severity_rationale":"Credential exposure remains security-sensitive."
        }
    }]`
	comments, err := parseAgentJSON(raw)
	if err != nil {
		t.Fatal(err)
	}
	if comments[0].Importance != "LOW" {
		t.Fatalf("importance = %q", comments[0].Importance)
	}
}

func TestParseAgentJSONRemovesContractsFromControlEntries(t *testing.T) {
	raw := `[{"file_path":"SUMMARY","line_number":0,"comment_body":"Approve.","finding_contract":{"schema_version":1}}]`
	comments, err := parseAgentJSON(raw)
	if err != nil {
		t.Fatal(err)
	}
	if comments[0].FindingContract != nil {
		t.Fatal("summary retained a finding contract")
	}
}

// Keep errors import alive.
var _ = errors.New

func TestPRScopeSection_EmptyInputsContributeNothing(t *testing.T) {
	if got := prScopeSection("", nil); got != "" {
		t.Errorf("empty inputs must produce no section, got %q", got)
	}
	prompt, err := buildAgentPromptContent("", nil, nil, nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(prompt, "PR SCOPE") {
		t.Error("prompt without base/diff must not mention PR SCOPE")
	}
}

func TestPRScopeSection_NamesBaseAndListsFiles(t *testing.T) {
	files := []diffFile{
		{Path: "a/b.go", Status: "modified", Added: []string{"x", "y"}, Removed: []string{"z"}},
		{Path: "c.py", Status: "added", Added: []string{"q"}},
	}
	got := prScopeSection("feature/parent", files)
	for _, want := range []string{
		"--- PR SCOPE ---",
		"`git diff origin/feature/parent...HEAD`",
		"- a/b.go (modified, +2/-1)",
		"- c.py (added, +1/-0)",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("scope section missing %q:\n%s", want, got)
		}
	}
}

func TestPRScopeSection_FileListCapped(t *testing.T) {
	files := make([]diffFile, 130)
	for i := range files {
		files[i] = diffFile{Path: fmt.Sprintf("f%03d.go", i), Status: "modified"}
	}
	got := prScopeSection("master", files)
	if !strings.Contains(got, "- f099.go") {
		t.Error("file 100 should be listed")
	}
	if strings.Contains(got, "- f100.go") {
		t.Error("file 101 should be cut by the cap")
	}
	if !strings.Contains(got, "...and 30 more") {
		t.Errorf("missing truncation marker:\n%s", got)
	}
}
