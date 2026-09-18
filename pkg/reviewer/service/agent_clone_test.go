package service

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// setupOldForkRepo builds a bare repo whose main has 300 commits and whose
// pull/1/head forked at commit 50 and adds feature.txt, so a --depth 200 cache
// of main has no merge-base with the PR. Returns a file:// URL (the wire
// protocol honors --depth, a plain path clone does not) and the PR head SHA.
func setupOldForkRepo(t *testing.T) (bareURL, prSHA string) {
	t.Helper()
	skipIfNoGit(t)
	bare := t.TempDir()
	gitIn(t, bare, "init", "-q", "--bare", "-b", "main", ".")

	var stream bytes.Buffer
	stream.WriteString("blob\nmark :1\ndata 3\nhi\n\n")
	for i := 1; i <= 300; i++ {
		fmt.Fprintf(&stream, "commit refs/heads/main\nmark :%d\ncommitter test <test@example.com> %d +0000\ndata %d\ncommit %d\n",
			1000+i, 1700000000+i, len(fmt.Sprintf("commit %d", i)), i)
		if i > 1 {
			fmt.Fprintf(&stream, "from :%d\n", 1000+i-1)
		}
		fmt.Fprintf(&stream, "M 100644 :1 file%d.txt\n\n", i)
	}
	stream.WriteString("blob\nmark :2\ndata 8\nfeature\n\n")
	stream.WriteString("commit refs/pull/1/head\nmark :2000\ncommitter test <test@example.com> 1700001000 +0000\ndata 9\npr commit\nfrom :1050\nM 100644 :2 feature.txt\n\n")
	cmd := exec.Command("git", "fast-import", "--quiet")
	cmd.Dir = bare
	cmd.Stdin = &stream
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("fast-import: %v (%s)", err, out)
	}
	return "file://" + bare, strings.TrimSpace(gitIn(t, bare, "rev-parse", "refs/pull/1/head"))
}

func gitIn(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v in %s: %v (%s)", args, dir, err, out)
	}
	return string(out)
}

// seedShallowCache reproduces the cache state older builds left behind: a
// --depth 200 clone of main plus a --depth 200 fetch of the PR ref.
func seedShallowCache(t *testing.T, cloneRoot, bareURL string) string {
	t.Helper()
	cacheDir := filepath.Join(cloneRoot, ".cache", "acme__example")
	if err := os.MkdirAll(filepath.Dir(cacheDir), 0o755); err != nil {
		t.Fatal(err)
	}
	gitIn(t, ".", "clone", "--quiet", "--depth", "200", "--branch", "main", bareURL, cacheDir)
	gitIn(t, cacheDir, "fetch", "--quiet", "--depth", "200", "origin", "+pull/1/head:refs/agent-pr/1")
	if _, err := os.Stat(filepath.Join(cacheDir, ".git", "shallow")); err != nil {
		t.Fatalf("seed cache must be shallow: %v", err)
	}
	if got := strings.TrimSpace(gitIn(t, cacheDir, "rev-list", "--count", "origin/main")); got != "200" {
		t.Fatalf("seed cache depth: got %s commits, want 200", got)
	}
	return cacheDir
}

// traceGit points GIT_TRACE at a file so the test can assert on the exact git
// invocations a code path ran; readTrace returns and clears them.
func traceGit(t *testing.T) (readTrace func() string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "git-trace.log")
	t.Setenv("GIT_TRACE", path)
	return func() string {
		b, _ := os.ReadFile(path)
		_ = os.WriteFile(path, nil, 0o644)
		return string(b)
	}
}

func isShallow(t *testing.T, cacheDir string) bool {
	t.Helper()
	_, err := os.Stat(filepath.Join(cacheDir, ".git", "shallow"))
	return err == nil
}

func TestCloneForAgent_CompletesShallowCacheOnceThenKeepsItComplete(t *testing.T) {
	bareURL, prSHA := setupOldForkRepo(t)
	cloneRoot := t.TempDir()
	cacheDir := seedShallowCache(t, cloneRoot, bareURL)
	readTrace := traceGit(t)

	review := func(n int) (prepDuration time.Duration, trace string) {
		t.Helper()
		start := time.Now()
		dir := filepath.Join(cloneRoot, fmt.Sprintf("wt%d", n))
		cleanup, err := cloneForAgent(context.Background(), cloneRoot, dir, "acme", "example", "main", 1, prSHA, "")
		if err != nil {
			t.Fatalf("review %d cloneForAgent: %v", n, err)
		}
		defer func() { _ = cleanup() }()
		files := diffFilesForWorktree(context.Background(), dir, "main", "", "acme/example", 1, "")
		prepDuration = time.Since(start)
		if len(files) != 1 || files[0].Path != "feature.txt" || files[0].Status != "added" {
			t.Fatalf("review %d: git diff must resolve the old fork's merge-base, got %+v", n, files)
		}
		return prepDuration, readTrace()
	}

	first, trace1 := review(1)
	if !strings.Contains(trace1, "--unshallow") {
		t.Fatalf("first use of a shallow cache must unshallow it once; trace:\n%s", trace1)
	}
	if isShallow(t, cacheDir) {
		t.Fatal("cache still shallow after the first review")
	}

	second, trace2 := review(2)
	if strings.Contains(trace2, "--unshallow") || strings.Contains(trace2, "--depth") || strings.Contains(trace2, "--deepen") {
		t.Fatalf("second review must neither re-shallow nor unshallow the cache; trace:\n%s", trace2)
	}
	if isShallow(t, cacheDir) {
		t.Fatal("second review re-shallowed the cache")
	}
	if got := strings.TrimSpace(gitIn(t, cacheDir, "rev-list", "--count", "origin/main")); got != "300" {
		t.Fatalf("cache history after two reviews: got %s commits, want 300", got)
	}
	t.Logf("prep: first review (unshallow) %s, second review %s", first, second)
}

func TestDiffFilesForWorktree_ShallowCacheFallsBackToAPIDiffWithoutUnshallow(t *testing.T) {
	bareURL, _ := setupOldForkRepo(t)
	cacheDir := seedShallowCache(t, t.TempDir(), bareURL)
	gitIn(t, cacheDir, "checkout", "-q", "--detach", "refs/agent-pr/1")
	readTrace := traceGit(t)

	const apiDiff = "diff --git a/feature.txt b/feature.txt\nnew file mode 100644\nindex 0000000..1234567\n--- /dev/null\n+++ b/feature.txt\n@@ -0,0 +1 @@\n+feature\n"
	files := diffFilesForWorktree(context.Background(), cacheDir, "main", "", "acme/example", 1, apiDiff)
	trace := readTrace()
	if strings.Contains(trace, "--unshallow") || strings.Contains(trace, "--deepen") {
		t.Fatalf("fallback must not deepen the cache; trace:\n%s", trace)
	}
	if !isShallow(t, cacheDir) {
		t.Fatal("cache must stay shallow: the review path never completes it")
	}
	if len(files) != 1 || files[0].Path != "feature.txt" || files[0].Status != "added" || len(files[0].Added) != 1 || files[0].Added[0] != "feature" {
		t.Fatalf("expected the API diff's file list, got %+v", files)
	}

	if got := diffFilesForWorktree(context.Background(), cacheDir, "main", "", "acme/example", 1, ""); got != nil {
		t.Fatalf("without an API diff the fallback is no signal, got %+v", got)
	}
}

func TestDiffFilesFromAPIDiff(t *testing.T) {
	diff := strings.Join([]string{
		"diff --git a/app/mod.py b/app/mod.py",
		"index 1111111..2222222 100644",
		"--- a/app/mod.py",
		"+++ b/app/mod.py",
		"@@ -1,4 +1,5 @@",
		" import os",
		"-old = 1",
		"+new = 1",
		"+extra = 2",
		" keep = 3",
		"+tail = 4",
		"diff --git a/app/new.py b/app/new.py",
		"new file mode 100644",
		"--- /dev/null",
		"+++ b/app/new.py",
		"@@ -0,0 +1 @@",
		"+fresh = True",
		"diff --git a/app/gone.py b/app/gone.py",
		"deleted file mode 100644",
		"--- a/app/gone.py",
		"+++ /dev/null",
		"@@ -1 +0,0 @@",
		"-doomed = True",
		"diff --git a/app/old name.py b/app/new name.py",
		"similarity index 100%",
		"rename from app/old name.py",
		"rename to app/new name.py",
		"",
	}, "\n")
	files := diffFilesFromAPIDiff(diff)
	if len(files) != 4 {
		t.Fatalf("want 4 files, got %+v", files)
	}
	mod := files[0]
	if mod.Path != "app/mod.py" || mod.Status != "modified" ||
		strings.Join(mod.Added, ",") != "new = 1,extra = 2,tail = 4" || strings.Join(mod.Removed, ",") != "old = 1" {
		t.Fatalf("modified file: %+v", mod)
	}
	if len(mod.AddedHunks) != 2 || len(mod.AddedHunks[0]) != 2 || len(mod.AddedHunks[1]) != 1 {
		t.Fatalf("context lines must split hunks: %+v", mod.AddedHunks)
	}
	if files[1].Path != "app/new.py" || files[1].Status != "added" || files[1].Added[0] != "fresh = True" {
		t.Fatalf("added file: %+v", files[1])
	}
	if files[2].Path != "app/gone.py" || files[2].Status != "removed" || files[2].Removed[0] != "doomed = True" {
		t.Fatalf("deleted file: %+v", files[2])
	}
	if files[3].Path != "app/new name.py" || files[3].Status != "modified" || len(files[3].Added) != 0 {
		t.Fatalf("renamed file must list its new path: %+v", files[3])
	}
}

// deadlineSpawner records how much of the wall clock is left when the agent
// CLI is spawned.
type deadlineSpawner struct {
	*fakeSpawner
	remaining time.Duration
}

func (s *deadlineSpawner) SpawnWithEnv(ctx context.Context, name string, args []string, dir string, env []string) (SpawnedProcess, error) {
	if deadline, ok := ctx.Deadline(); ok {
		s.remaining = time.Until(deadline)
	}
	return s.fakeSpawner.SpawnWithEnv(ctx, name, args, dir, env)
}

// slowPrep makes every worktree checkout in the cache take at least the given
// time via a post-checkout hook, standing in for a slow clone or fetch.
func slowPrep(t *testing.T, cloneRoot string, d time.Duration) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("hook needs sh")
	}
	hooks := t.TempDir()
	script := fmt.Sprintf("#!/bin/sh\nsleep %.1f\n", d.Seconds())
	if err := os.WriteFile(filepath.Join(hooks, "post-checkout"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	gitIn(t, filepath.Join(cloneRoot, ".cache", "acme__example"), "config", "core.hooksPath", hooks)
}

func TestRunAgentReview_WallClockStartsAtSpawn(t *testing.T) {
	bare, sha := setupLocalBareRepo(t)
	cloneRoot := t.TempDir()
	seedAgentCache(t, cloneRoot, "acme", "example", bare)
	const prepDelay, wallClock, agentRun = 1500 * time.Millisecond, time.Second, 300 * time.Millisecond
	slowPrep(t, cloneRoot, prepDelay)

	spawner := &deadlineSpawner{fakeSpawner: &fakeSpawner{proc: &fakeProcess{
		stdout: bytes.NewBufferString(fakeStream), stderr: &bytes.Buffer{}, killCh: make(chan struct{}), exitAfter: agentRun,
	}}}
	started := time.Now()
	out, err := RunAgentReview(context.Background(), AgentConfig{
		CloneRootDir: cloneRoot, LogsDir: t.TempDir(), WallClock: wallClock, MaxTurns: 10,
	}, spawner, "acme", "example", "main", 1, sha, nil)
	total := time.Since(started)
	if err != nil {
		t.Fatalf("prep longer than the wall clock must not fail the review: %v (total %s)", err, total)
	}
	if spawner.remaining < wallClock-200*time.Millisecond {
		t.Fatalf("wall clock at spawn: %s left of %s; prep consumed it", spawner.remaining, wallClock)
	}
	if out.PrepDurationMS < prepDelay.Milliseconds() {
		t.Fatalf("prep_ms=%d, want >= %d", out.PrepDurationMS, prepDelay.Milliseconds())
	}
	if out.DurationMS < agentRun.Milliseconds() || out.DurationMS >= wallClock.Milliseconds() {
		t.Fatalf("agent_ms=%d, want within [%d, %d)", out.DurationMS, agentRun.Milliseconds(), wallClock.Milliseconds())
	}
	t.Logf("total %s = prep %dms + agent %dms; %s of the %s wall clock left at spawn",
		total, out.PrepDurationMS, out.DurationMS, spawner.remaining, wallClock)
}

func TestRunAgentReview_PrepBudgetExhaustedIsCloneTimeout(t *testing.T) {
	bare, sha := setupLocalBareRepo(t)
	cloneRoot := t.TempDir()
	seedAgentCache(t, cloneRoot, "acme", "example", bare)
	slowPrep(t, cloneRoot, 1500*time.Millisecond)

	spawner := &fakeSpawner{proc: &fakeProcess{stdout: bytes.NewBufferString(fakeStream), stderr: &bytes.Buffer{}, killCh: make(chan struct{})}}
	_, err := RunAgentReview(context.Background(), AgentConfig{
		CloneRootDir: cloneRoot, LogsDir: t.TempDir(), WallClock: time.Minute, PrepBudget: 300 * time.Millisecond, MaxTurns: 10,
	}, spawner, "acme", "example", "main", 1, sha, nil)
	if !errors.Is(err, ErrCloneTimeout) || !strings.Contains(err.Error(), "clone_timeout") {
		t.Fatalf("want clone_timeout, got %v", err)
	}
	if spawner.name != "" {
		t.Fatal("agent must not spawn once the prep budget is gone")
	}
}
