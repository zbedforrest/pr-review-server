package service

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// mergedPRRepo is a bare repository with the history a benchmark case has: a
// PR merged into main, then a fix landed on main after it.
type mergedPRRepo struct {
	url                              string
	forkSHA, baseAtMergeSHA, headSHA string
	mergeSHA, fixSHA                 string
}

// setupMergedPRRepo builds main c1 (fork), PR c2 on it under refs/pull/1/head
// adding change.txt, main c3 (base moved), the merge of the PR into main (a
// merge commit, or a squash commit with one parent), then the fix commit on
// main touching change.txt and adding fix.txt.
func setupMergedPRRepo(t *testing.T, squash bool) mergedPRRepo {
	t.Helper()
	skipIfNoGit(t)
	src := t.TempDir()
	run := func(args ...string) string {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = src
		cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=test", "GIT_AUTHOR_EMAIL=test@example.com", "GIT_COMMITTER_NAME=test", "GIT_COMMITTER_EMAIL=test@example.com")
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v (%s)", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	write := func(name, body string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(src, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	run("init", "-q", "-b", "main", ".")
	write("hello.txt", "hi")
	run("add", ".")
	run("commit", "-q", "-m", "c1")
	r := mergedPRRepo{forkSHA: run("rev-parse", "HEAD")}
	run("checkout", "-q", "-b", "feat")
	write("change.txt", "buggy")
	run("add", ".")
	run("commit", "-q", "-m", "pr commit")
	r.headSHA = run("rev-parse", "HEAD")
	run("checkout", "-q", "main")
	write("other.txt", "moved")
	run("add", ".")
	run("commit", "-q", "-m", "c3")
	r.baseAtMergeSHA = run("rev-parse", "HEAD")
	if squash {
		run("merge", "-q", "--squash", "feat")
		run("commit", "-q", "-m", "squash (#1)")
	} else {
		run("merge", "-q", "--no-ff", "-m", "Merge pull request #1", "feat")
	}
	r.mergeSHA = run("rev-parse", "HEAD")
	write("change.txt", "fixed")
	write("fix.txt", "the fix")
	run("add", ".")
	run("commit", "-q", "-m", "fix the bug")
	r.fixSHA = run("rev-parse", "HEAD")

	bare := t.TempDir()
	gitIn(t, bare, "init", "-q", "--bare", "-b", "main", ".")
	gitIn(t, bare, "config", "uploadpack.allowAnySHA1InWant", "true")
	run("push", "-q", bare, "main:main", "feat:refs/pull/1/head")
	r.url = "file://" + bare
	return r
}

func pinnedCloneURL(t *testing.T, url string) {
	t.Helper()
	prev := cloneURLFor
	cloneURLFor = func(string, string) string { return url }
	t.Cleanup(func() { cloneURLFor = prev })
}

func gitOK(dir string, args ...string) bool {
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	return cmd.Run() == nil
}

func TestCloneForAgentPinned_MergeCommitPinsTheBaseAtMergeTime(t *testing.T) {
	r := setupMergedPRRepo(t, false)
	pinnedCloneURL(t, r.url)
	cloneRoot := t.TempDir()
	dir := filepath.Join(cloneRoot, "wt")
	cleanup, err := cloneForAgent(context.Background(), cloneRoot, dir, "acme", "example", "main", 1, r.headSHA, "", &BasePin{MergeCommitSHA: r.mergeSHA, BaseSHA: r.fixSHA})
	if err != nil {
		t.Fatalf("cloneForAgent: %v", err)
	}
	defer func() { _ = cleanup() }()

	if got := strings.TrimSpace(gitIn(t, dir, "rev-parse", "origin/main")); got != r.baseAtMergeSHA {
		t.Fatalf("origin/main = %s, want the merge's first parent %s", got, r.baseAtMergeSHA)
	}
	if got := strings.TrimSpace(gitIn(t, dir, "rev-parse", "origin/HEAD")); got != r.baseAtMergeSHA {
		t.Fatalf("origin/HEAD = %s, want %s", got, r.baseAtMergeSHA)
	}
	if b, err := os.ReadFile(filepath.Join(dir, "change.txt")); err != nil || string(b) != "buggy" {
		t.Fatalf("worktree is the PR head: change.txt=%q err=%v", b, err)
	}
	if gitOK(dir, "cat-file", "-e", r.fixSHA+"^{commit}") {
		t.Fatal("the fix commit must not exist in the pinned clone")
	}
	if gitOK(dir, "show", "origin/main:fix.txt") {
		t.Fatal("origin/main must not contain the fix")
	}
	if strings.Contains(gitIn(t, dir, "log", "--all", "--format=%H"), r.fixSHA) {
		t.Fatal("no ref may reach the fix commit")
	}
	if got := strings.TrimSpace(gitIn(t, dir, "config", "remote.origin.fetch")); got != "+refs/pull/1/head:refs/agent-pr/1" {
		t.Fatalf("remote refspec must name only the PR head, got %q", got)
	}
	files := diffFilesForWorktree(context.Background(), dir, "main", "", "acme/example__pr1__pinned", "")
	if len(files) != 1 || files[0].Path != "change.txt" || files[0].Status != "added" {
		t.Fatalf("three-dot diff against the pinned base: got %+v", files)
	}
	if fetchBaseRef(context.Background(), dir, "main", "", "", "[test]") {
		t.Fatal("the gates' base-ref fetch must refuse to touch a pinned checkout")
	}
	if got := strings.TrimSpace(gitIn(t, dir, "rev-parse", "origin/main")); got != r.baseAtMergeSHA {
		t.Fatalf("origin/main moved to %s after fetchBaseRef", got)
	}
	cacheDir := filepath.Join(cloneRoot, ".cache", "acme__example__pr1__pinned")
	if _, err := os.Stat(filepath.Join(cacheDir, ".git")); err != nil {
		t.Fatalf("pinned cache dir: %v", err)
	}
	if _, err := os.Stat(filepath.Join(cloneRoot, ".cache", "acme__example")); !os.IsNotExist(err) {
		t.Fatal("a pinned review must not create or touch the shared cache")
	}
}

func TestCloneForAgentPinned_SquashMergePinsTheForkPoint(t *testing.T) {
	r := setupMergedPRRepo(t, true)
	pinnedCloneURL(t, r.url)
	cloneRoot := t.TempDir()
	dir := filepath.Join(cloneRoot, "wt")
	cleanup, err := cloneForAgent(context.Background(), cloneRoot, dir, "acme", "example", "main", 1, r.headSHA, "", &BasePin{MergeCommitSHA: r.mergeSHA})
	if err != nil {
		t.Fatalf("cloneForAgent: %v", err)
	}
	defer func() { _ = cleanup() }()
	if got := strings.TrimSpace(gitIn(t, dir, "rev-parse", "origin/main")); got != r.forkSHA {
		t.Fatalf("origin/main = %s, want the fork point %s", got, r.forkSHA)
	}
	if gitOK(dir, "cat-file", "-e", r.fixSHA+"^{commit}") || gitOK(dir, "show", "origin/main:fix.txt") {
		t.Fatal("the fix must not be reachable")
	}
	files := diffFilesForWorktree(context.Background(), dir, "main", "", "acme/example__pr1__pinned", "")
	if len(files) != 1 || files[0].Path != "change.txt" {
		t.Fatalf("diff against the fork point: got %+v", files)
	}
}

func TestCloneForAgentPinned_OpenPRPinsTheReportedBase(t *testing.T) {
	r := setupMergedPRRepo(t, false)
	pinnedCloneURL(t, r.url)
	cloneRoot := t.TempDir()
	dir := filepath.Join(cloneRoot, "wt")
	cleanup, err := cloneForAgent(context.Background(), cloneRoot, dir, "acme", "example", "", 1, r.headSHA, "", &BasePin{BaseSHA: r.baseAtMergeSHA})
	if err != nil {
		t.Fatalf("cloneForAgent: %v", err)
	}
	defer func() { _ = cleanup() }()
	if got := strings.TrimSpace(gitIn(t, dir, "rev-parse", "origin/HEAD")); got != r.baseAtMergeSHA {
		t.Fatalf("origin/HEAD = %s, want %s", got, r.baseAtMergeSHA)
	}
	files := diffFilesForWorktree(context.Background(), dir, "", "", "", "")
	if len(files) != 1 || files[0].Path != "change.txt" {
		t.Fatalf("diff against origin/HEAD: got %+v", files)
	}
	if _, err := cloneForAgent(context.Background(), cloneRoot, filepath.Join(cloneRoot, "wt2"), "acme", "example", "main", 1, r.headSHA, "", &BasePin{}); err == nil || !strings.Contains(err.Error(), "neither a merge commit nor a base SHA") {
		t.Fatalf("an empty pin must fail loudly, got %v", err)
	}
}

func TestCloneForAgentPinned_ReusesTheCacheAcrossRuns(t *testing.T) {
	r := setupMergedPRRepo(t, false)
	pinnedCloneURL(t, r.url)
	cloneRoot := t.TempDir()
	pin := &BasePin{MergeCommitSHA: r.mergeSHA}
	first, err := cloneForAgent(context.Background(), cloneRoot, filepath.Join(cloneRoot, "wt1"), "acme", "example", "main", 1, r.headSHA, "", pin)
	if err != nil {
		t.Fatalf("first: %v", err)
	}
	if err := first(); err != nil {
		t.Fatalf("first cleanup: %v", err)
	}
	readTrace := traceGit(t)
	dir := filepath.Join(cloneRoot, "wt2")
	second, err := cloneForAgent(context.Background(), cloneRoot, dir, "acme", "example", "main", 1, r.headSHA, "", pin)
	if err != nil {
		t.Fatalf("second: %v", err)
	}
	defer func() { _ = second() }()
	if trace := readTrace(); strings.Contains(trace, "fetch") {
		t.Fatalf("a second run of the same PR reuses the pinned cache without fetching; trace:\n%s", trace)
	}
	if got := strings.TrimSpace(gitIn(t, dir, "rev-parse", "origin/main")); got != r.baseAtMergeSHA {
		t.Fatalf("origin/main = %s after reuse, want %s", got, r.baseAtMergeSHA)
	}
}

// setupOldForkMergedRepo is setupOldForkRepo's history with the PR merged at
// the top of main by a merge commit and a fix after it: the fork at commit 50
// sits past a 200-deep fetch of the merge parent, so the pin must deepen.
func setupOldForkMergedRepo(t *testing.T) (r mergedPRRepo) {
	t.Helper()
	skipIfNoGit(t)
	bare := t.TempDir()
	gitIn(t, bare, "init", "-q", "--bare", "-b", "main", ".")
	gitIn(t, bare, "config", "uploadpack.allowAnySHA1InWant", "true")
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
	stream.WriteString("commit refs/heads/main\nmark :3000\ncommitter test <test@example.com> 1700002000 +0000\ndata 5\nmerge\nfrom :1300\nmerge :2000\nM 100644 :2 feature.txt\n\n")
	stream.WriteString("blob\nmark :3\ndata 3\nfix\n")
	stream.WriteString("commit refs/heads/main\nmark :4000\ncommitter test <test@example.com> 1700003000 +0000\ndata 3\nfix\nfrom :3000\nM 100644 :3 feature.txt\n\n")
	cmd := exec.Command("git", "fast-import", "--quiet")
	cmd.Dir = bare
	cmd.Stdin = &stream
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("fast-import: %v (%s)", err, out)
	}
	rev := func(ref string) string { return strings.TrimSpace(gitIn(t, bare, "rev-parse", ref)) }
	return mergedPRRepo{url: "file://" + bare, headSHA: rev("refs/pull/1/head"), fixSHA: rev("main"), mergeSHA: rev("main~1"), baseAtMergeSHA: rev("main~2")}
}

func TestCloneForAgentPinned_DeepensUntilTheBaseAndHeadConnect(t *testing.T) {
	r := setupOldForkMergedRepo(t)
	pinnedCloneURL(t, r.url)
	cloneRoot := t.TempDir()
	readTrace := traceGit(t)
	dir := filepath.Join(cloneRoot, "wt")
	cleanup, err := cloneForAgent(context.Background(), cloneRoot, dir, "acme", "example", "main", 1, r.headSHA, "", &BasePin{MergeCommitSHA: r.mergeSHA})
	if err != nil {
		t.Fatalf("cloneForAgent: %v", err)
	}
	defer func() { _ = cleanup() }()
	if trace := readTrace(); !strings.Contains(trace, "--deepen=1000") {
		t.Fatalf("a fork past the fetch depth must deepen; trace:\n%s", trace)
	}
	if got := strings.TrimSpace(gitIn(t, dir, "rev-parse", "origin/main")); got != r.baseAtMergeSHA {
		t.Fatalf("origin/main = %s, want %s", got, r.baseAtMergeSHA)
	}
	if gitOK(dir, "cat-file", "-e", r.fixSHA+"^{commit}") {
		t.Fatal("deepening must not bring the fix in")
	}
	files := diffFilesForWorktree(context.Background(), dir, "main", "", "", "")
	if len(files) != 1 || files[0].Path != "feature.txt" || files[0].Status != "added" {
		t.Fatalf("diff after deepening: got %+v", files)
	}
}

func TestRunAgentReview_PinnedReviewNeverBuildsTheSharedCache(t *testing.T) {
	r := setupMergedPRRepo(t, false)
	pinnedCloneURL(t, r.url)
	cloneRoot := t.TempDir()
	spawner := &fakeSpawner{proc: &fakeProcess{stdout: bytes.NewBufferString(fakeStream), stderr: &bytes.Buffer{}, killCh: make(chan struct{})}}
	if _, err := RunAgentReview(context.Background(), AgentConfig{
		CloneRootDir: cloneRoot, LogsDir: t.TempDir(), WallClock: time.Minute, MaxTurns: 10,
		BasePin: &BasePin{MergeCommitSHA: r.mergeSHA},
	}, spawner, "acme", "example", "main", 1, r.headSHA, nil); err != nil {
		t.Fatalf("RunAgentReview: %v", err)
	}
	if _, err := os.Stat(filepath.Join(cloneRoot, ".cache", "acme__example")); !os.IsNotExist(err) {
		t.Fatal("a pinned review built the shared cache of the live base")
	}
	dir, cleanup, err := PrepareSharedCheckout(context.Background(), AgentConfig{CloneRootDir: cloneRoot, BasePin: &BasePin{MergeCommitSHA: r.mergeSHA}}, "acme", "example", "main", 1, r.headSHA)
	if err != nil {
		t.Fatalf("PrepareSharedCheckout: %v", err)
	}
	defer func() { _ = cleanup() }()
	if got := strings.TrimSpace(gitIn(t, dir, "rev-parse", "origin/main")); got != r.baseAtMergeSHA {
		t.Fatalf("shared checkout origin/main = %s, want %s", got, r.baseAtMergeSHA)
	}
	if _, err := os.Stat(filepath.Join(cloneRoot, ".cache", "acme__example")); !os.IsNotExist(err) {
		t.Fatal("a pinned shared checkout built the shared cache of the live base")
	}
}
