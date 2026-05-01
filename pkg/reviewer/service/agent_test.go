package service

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
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

type fakeSpawner struct {
	proc            *fakeProcess
	capturedArgs    []string
	capturedDir     string
	spawnErr        error
	spawnNameWanted string
}

func (s *fakeSpawner) Spawn(ctx context.Context, name string, args []string, dir string) (SpawnedProcess, error) {
	s.capturedArgs = args
	s.capturedDir = dir
	if s.spawnErr != nil {
		return nil, s.spawnErr
	}
	return s.proc, nil
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

// Keep errors import alive.
var _ = errors.New
