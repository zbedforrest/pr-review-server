package service

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"pr-review-server/pkg/reviewer/payload"
	"pr-review-server/pkg/reviewer/types"
)

func scopeGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.com", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.com")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

func writeFiles(t *testing.T, dir string, files map[string]string) {
	t.Helper()
	for name, body := range files {
		path := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

// syncCommitRepo builds a pull request whose head is a single-parent "sync"
// commit that copied the base branch's newer content instead of merging it:
// the base gained a change to lib.go and landed the PR's own util.go change
// through another pull request, and the PR head carries both alongside its
// real edit to page.html.
func syncCommitRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	scopeGit(t, dir, "init", "-q", "-b", "main")
	writeFiles(t, dir, map[string]string{"lib.go": "v1\n", "util.go": "u1\n", "page.html": "<p>old</p>\n", "other.go": "o1\n"})
	scopeGit(t, dir, "add", "-A")
	scopeGit(t, dir, "commit", "-qm", "base")
	scopeGit(t, dir, "checkout", "-qb", "feature")
	writeFiles(t, dir, map[string]string{"util.go": "u2\n", "page.html": "<p>new</p>\n"})
	scopeGit(t, dir, "commit", "-qam", "feature work")
	scopeGit(t, dir, "checkout", "-q", "main")
	writeFiles(t, dir, map[string]string{"lib.go": "v2\n"})
	scopeGit(t, dir, "commit", "-qam", "base moves")
	writeFiles(t, dir, map[string]string{"util.go": "u2\n", "other.go": "o2\n"})
	scopeGit(t, dir, "commit", "-qam", "util change lands through another pull request")
	scopeGit(t, dir, "checkout", "-q", "feature")
	writeFiles(t, dir, map[string]string{"lib.go": "v2\n"})
	scopeGit(t, dir, "commit", "-qam", "merge")
	scopeGit(t, dir, "update-ref", "refs/remotes/origin/main", "main")
	return dir
}

func TestReviewScopeLeavesOutContentCopiedFromTheBase(t *testing.T) {
	dir := syncCommitRepo(t)
	scope, err := computeReviewScope(context.Background(), dir, "origin/main")
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"lib.go", "util.go"}; !reflect.DeepEqual(scope.InheritedPaths, want) {
		t.Fatalf("inherited = %v, want %v", scope.InheritedPaths, want)
	}
	if scope.MergeBase == "" || scope.BaseTip != scopeGit(t, dir, "rev-parse", "main") {
		t.Fatalf("scope = %+v", scope)
	}
	diff := renderedDiff(context.Background(), dir, "main", "", scopePathspec(scope))
	if diff.Source != diffSourceGit {
		t.Fatalf("source = %q", diff.Source)
	}
	if !strings.Contains(diff.Text, "page.html") || strings.Contains(diff.Text, "lib.go") || strings.Contains(diff.Text, "util.go") {
		t.Fatalf("scoped diff must hold only the PR's own change:\n%s", diff.Text)
	}
}

func TestReviewScopeKeepsEveryChangeOnAnUpToDateBranch(t *testing.T) {
	dir := syncCommitRepo(t)
	scopeGit(t, dir, "checkout", "-q", "main")
	scopeGit(t, dir, "checkout", "-qb", "fresh")
	writeFiles(t, dir, map[string]string{"page.html": "<p>fresh</p>\n"})
	scopeGit(t, dir, "commit", "-qam", "fresh work")
	scope, err := computeReviewScope(context.Background(), dir, "main")
	if err != nil {
		t.Fatal(err)
	}
	if len(scope.InheritedPaths) != 0 || scope.MergeBase != scope.BaseTip {
		t.Fatalf("scope = %+v", scope)
	}
	if scopePathspec(scope) != nil || scopeNote(scope, "main") != "" {
		t.Fatal("an unscoped review must not change the diff or the prompt")
	}
}

func TestScopeNoteForbidsCitingInheritedPathsAsRegressions(t *testing.T) {
	note := scopeNote(&payload.ReviewScope{InheritedPaths: []string{"lib.go"}}, "main")
	for _, want := range []string{"origin/main", "`lib.go`", "do not cite them as regressions"} {
		if !strings.Contains(note, want) {
			t.Fatalf("note missing %q:\n%s", want, note)
		}
	}
}

func TestWithoutInheritedDropsOnlyInheritedFiles(t *testing.T) {
	files := []diffFile{{Path: "lib.go"}, {Path: "page.html"}}
	got := withoutInherited(files, &payload.ReviewScope{InheritedPaths: []string{"lib.go"}})
	if len(got) != 1 || got[0].Path != "page.html" {
		t.Fatalf("got %+v", got)
	}
}

func TestPayloadWithoutScopeStillParses(t *testing.T) {
	var p payload.Payload
	if err := json.Unmarshal([]byte(`{"schema_version":"1","owner":"o","repo":"r","pr_number":1,"commit_sha":"abc","counts":{},"findings":[]}`), &p); err != nil || p.Scope != nil {
		t.Fatalf("legacy sidecar: err=%v scope=%+v", err, p.Scope)
	}
	body, _ := json.Marshal(payload.Payload{Scope: &payload.ReviewScope{MergeBase: "m", InheritedPaths: []string{"lib.go"}}})
	if !strings.Contains(string(body), `"scope":{"merge_base":"m","inherited_paths":["lib.go"]}`) {
		t.Fatalf("scope not serialized: %s", body)
	}
}

func TestReviewScopeKeepsAPathTheBaseChangedAndReverted(t *testing.T) {
	dir := t.TempDir()
	scopeGit(t, dir, "init", "-q", "-b", "main")
	writeFiles(t, dir, map[string]string{"flag.go": "off\n"})
	scopeGit(t, dir, "add", "-A")
	scopeGit(t, dir, "commit", "-qm", "base")
	scopeGit(t, dir, "checkout", "-qb", "feature")
	writeFiles(t, dir, map[string]string{"flag.go": "on\n"})
	scopeGit(t, dir, "commit", "-qam", "turn the flag on")
	scopeGit(t, dir, "checkout", "-q", "main")
	writeFiles(t, dir, map[string]string{"flag.go": "on\n"})
	scopeGit(t, dir, "commit", "-qam", "try the flag")
	writeFiles(t, dir, map[string]string{"flag.go": "off\n"})
	scopeGit(t, dir, "commit", "-qam", "revert the flag")
	scopeGit(t, dir, "checkout", "-q", "feature")
	scope, err := computeReviewScope(context.Background(), dir, "main")
	if err != nil {
		t.Fatal(err)
	}
	if len(scope.InheritedPaths) != 0 {
		t.Fatalf("a PR edit the base tried and reverted is still the PR's change: %v", scope.InheritedPaths)
	}
}

func TestCommentsInScopeDropsFirstPassClaimsOnInheritedPaths(t *testing.T) {
	comments := []types.LineComment{{FilePath: "lib.go", Importance: "CRITICAL"}, {FilePath: "page.html"}, {FilePath: "SUMMARY"}}
	got := commentsInScope(comments, &payload.ReviewScope{InheritedPaths: []string{"lib.go"}})
	if len(got) != 2 || got[0].FilePath != "page.html" || got[1].FilePath != "SUMMARY" {
		t.Fatalf("got %+v", got)
	}
}
