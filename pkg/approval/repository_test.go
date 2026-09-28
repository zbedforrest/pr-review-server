package approval

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func fixtureGit(t *testing.T, dir, input string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"--git-dir=" + dir}, args...)...)
	cmd.Stdin = strings.NewReader(input)
	cmd.Env = []string{"PATH=/usr/bin:/bin", "HOME=/nonexistent", "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null", "GIT_AUTHOR_NAME=Fixture", "GIT_AUTHOR_EMAIL=fixture@example.test", "GIT_COMMITTER_NAME=Fixture", "GIT_COMMITTER_EMAIL=fixture@example.test"}
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v %s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

func fixtureRepository(t *testing.T) (*GitRepository, string, string) {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "objects.git")
	fixtureGit(t, dir, "", "init", "--bare", "--template=", dir)
	blob := fixtureGit(t, dir, "one\ntwo\nthree\n", "hash-object", "-w", "--stdin")
	link := fixtureGit(t, dir, "/etc/passwd", "hash-object", "-w", "--stdin")
	tree := fixtureGit(t, dir, "100644 blob "+blob+"\tfile.txt\n120000 blob "+link+"\tescape\n", "mktree")
	first := fixtureGit(t, dir, "first\n", "commit-tree", tree)
	blob = fixtureGit(t, dir, "one\nchanged\nthree\n", "hash-object", "-w", "--stdin")
	tree = fixtureGit(t, dir, "100644 blob "+blob+"\tfile.txt\n120000 blob "+link+"\tescape\n160000 commit "+first+"\tmodule\n", "mktree")
	second := fixtureGit(t, dir, "second\n", "commit-tree", tree, "-p", first)
	return &GitRepository{directory: dir, revisions: map[string]bool{first: true, second: true}}, first, second
}

func TestRepositoryRejectsOutOfScopeReads(t *testing.T) {
	r, _, head := fixtureRepository(t)
	ctx := context.Background()
	for _, p := range []string{"../secret", "/etc/passwd", "file.txt/../escape", "escape", "escape/passwd", "module/file.txt", "module", "file.txt\x00evil"} {
		t.Run(p, func(t *testing.T) {
			if _, err := r.Read(ctx, "read_file", ReadRequest{Revision: head, Path: p, StartLine: 1, EndLine: 1}); err == nil {
				t.Fatal("unsafe path accepted")
			}
		})
	}
	for _, rev := range []string{"HEAD", strings.Repeat("f", 40), head + ":file.txt", "--help"} {
		if _, err := r.Read(ctx, "list_files", ReadRequest{Revision: rev}); err == nil {
			t.Fatal("unregistered revision accepted")
		}
	}
	if _, err := r.Read(ctx, "shell", ReadRequest{Revision: head}); err == nil {
		t.Fatal("unsupported tool accepted")
	}
}

func TestRepositoryPinnedReadsAndDiff(t *testing.T) {
	r, base, head := fixtureRepository(t)
	ctx := context.Background()
	read, err := r.Read(ctx, "read_file", ReadRequest{Revision: head, Path: "file.txt", StartLine: 2, EndLine: 2})
	if err != nil || read.Text != "changed" {
		t.Fatalf("read: %+v %v", read, err)
	}
	diff, err := r.Read(ctx, "read_diff", ReadRequest{Revision: head, OtherRevision: base, Path: "file.txt"})
	if err != nil || !strings.Contains(diff.Text, "+changed") {
		t.Fatalf("diff: %+v %v", diff, err)
	}
	list, err := r.Read(ctx, "list_files", ReadRequest{Revision: head})
	if err != nil || list.Text != "file.txt\n" {
		t.Fatalf("list: %+v %v", list, err)
	}
	search, err := r.Read(ctx, "search_code", ReadRequest{Revision: head, Query: "changed"})
	if err != nil || search.Text != "file.txt:2:changed\n" {
		t.Fatalf("search: %+v %v", search, err)
	}
	merge, err := r.MergeBase(ctx, base, head)
	if err != nil || merge != base {
		t.Fatalf("merge base: %s %v", merge, err)
	}
}

func TestRepositoryIgnoresInheritedGitOverrides(t *testing.T) {
	r, _, head := fixtureRepository(t)
	t.Setenv("GIT_DIR", "/missing")
	t.Setenv("GIT_CONFIG_COUNT", "1")
	t.Setenv("GIT_CONFIG_KEY_0", "core.bare")
	t.Setenv("GIT_CONFIG_VALUE_0", "false")
	global := filepath.Join(t.TempDir(), "config")
	if err := os.WriteFile(global, []byte("[include]\npath=/missing\n"), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GIT_CONFIG_GLOBAL", global)
	if _, err := r.Read(context.Background(), "read_file", ReadRequest{Revision: head, Path: "file.txt", StartLine: 1, EndLine: 1}); err != nil {
		t.Fatal(err)
	}
	cmd := r.command(context.Background(), "show", head)
	for _, value := range cmd.Env {
		if strings.HasPrefix(value, "GIT_CONFIG_COUNT=") {
			t.Fatal("inherited git override")
		}
	}
}
