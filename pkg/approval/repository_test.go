package approval

import (
	"bytes"
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
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("git %v: %v %s", args, err, stderr.String())
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

func TestRepositoryCredentialHelperReadsDedicatedPipe(t *testing.T) {
	r, _, _ := fixtureRepository(t)
	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	defer writer.Close()
	_, err = writer.WriteString("username=fixture\npassword=synthetic-secret\n\n")
	if err != nil {
		t.Fatal(err)
	}
	writer.Close()
	cmd := r.command(context.Background(), "-c", `credential.helper=!f() { if test "$1" = get; then cat <&3; fi; }; f`, "credential", "fill")
	cmd.ExtraFiles = []*os.File{reader}
	cmd.Stdin = strings.NewReader("protocol=https\nhost=github.com\n\n")
	body, err := cmd.Output()
	if err != nil || !strings.Contains(string(body), "password=synthetic-secret") {
		t.Fatal("dedicated credential pipe unavailable")
	}
	for _, arg := range append(cmd.Args, cmd.Env...) {
		if strings.Contains(arg, "synthetic-secret") {
			t.Fatal("credential appeared in process arguments or environment")
		}
	}
}

func TestRepositoryTextPagesRespectLineAndByteLimits(t *testing.T) {
	body := strings.Repeat(strings.Repeat("a", 200)+"\n", 700)
	first, err := repositoryTextPage(body, "")
	if err != nil || len(first.Text) > 64<<10 || first.NextCursor == "" {
		t.Fatalf("invalid first page: %v", err)
	}
	second, err := repositoryTextPage(body, first.NextCursor)
	if err != nil || len(second.Text) > 64<<10 {
		t.Fatalf("invalid second page: %v", err)
	}
	if _, err := repositoryTextPage(body, "-1"); err == nil {
		t.Fatal("negative cursor accepted")
	}
}

func TestRepositorySearchSkipsBinaryAndOversizedBlobs(t *testing.T) {
	r, _, _ := fixtureRepository(t)
	text := fixtureGit(t, r.directory, "needle in source\n", "hash-object", "-w", "--stdin")
	binary := fixtureGit(t, r.directory, "\x00binary\x00", "hash-object", "-w", "--stdin")
	large := fixtureGit(t, r.directory, strings.Repeat("x", (2<<20)+1), "hash-object", "-w", "--stdin")
	tree := fixtureGit(t, r.directory, "100644 blob "+binary+"\timage.bin\n100644 blob "+large+"\tlarge.txt\n100644 blob "+text+"\ttext.txt\n", "mktree")
	sha := fixtureGit(t, r.directory, "mixed files\n", "commit-tree", tree)
	r.revisions[sha] = true
	result, err := r.Read(context.Background(), "search_code", ReadRequest{Revision: sha, Query: "needle"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(result.Text, "text.txt:1:needle in source") || !strings.Contains(result.Text, "Skipped 1 oversized files") || strings.Contains(result.Text, "image.bin") {
		t.Fatalf("search failed to report skipped files: %+v", result)
	}
	if _, err = r.Read(context.Background(), "read_file", ReadRequest{Revision: sha, Path: "image.bin", StartLine: 1, EndLine: 1}); err == nil {
		t.Fatal("binary content returned to file reader")
	}
	if _, err = r.Read(context.Background(), "search_code", ReadRequest{Revision: sha, Path: "text.txt", Query: strings.Repeat("q", 256)}); err != nil {
		t.Fatalf("dispatcher-sized query rejected: %v", err)
	}
}

func TestNewGitRepositoryServesCachedRevisionsWithoutFetching(t *testing.T) {
	cache, first, second := fixtureRepository(t)
	offline := func(context.Context, string, string) (string, error) {
		t.Fatal("fetched a revision the reference already holds")
		return "", nil
	}
	r, err := NewGitRepository(context.Background(), t.TempDir(), Target{Owner: "acme", Repo: "example"}, []string{first, second}, offline, filepath.Join(cache.directory, "objects"))
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	if err := r.ValidateDiff(context.Background(), first, second); err != nil {
		t.Fatal(err)
	}
}

func TestRepositoryReadFileClampsARangePastTheEnd(t *testing.T) {
	r, _, head := fixtureRepository(t)
	got, err := r.Read(context.Background(), "read_file", ReadRequest{Revision: head, Path: "file.txt", StartLine: 2, EndLine: 300})
	if err != nil || got.Text != "changed\nthree\n" {
		t.Fatalf("range past the end should return through the last line: %+v %v", got, err)
	}
	if _, err := r.Read(context.Background(), "read_file", ReadRequest{Revision: head, Path: "file.txt", StartLine: 50, EndLine: 60}); err == nil {
		t.Fatal("a range starting past the end must still be rejected")
	}
}

func TestRepositoryCachesBlobAndDiffReads(t *testing.T) {
	r, base, head := fixtureRepository(t)
	ctx := context.Background()
	reads := []struct {
		name string
		req  ReadRequest
	}{
		{"read_file", ReadRequest{Revision: head, Path: "file.txt", StartLine: 1, EndLine: 3}},
		{"read_file", ReadRequest{Revision: head, Path: "missing.txt", StartLine: 1, EndLine: 1}},
		{"read_diff", ReadRequest{Revision: head, OtherRevision: base, Path: "file.txt"}},
		{"read_diff", ReadRequest{Revision: head, OtherRevision: base}},
	}
	type outcome struct {
		result ReadResult
		err    string
	}
	read := func() []outcome {
		var out []outcome
		for _, q := range reads {
			result, err := r.Read(ctx, q.name, q.req)
			o := outcome{result: result}
			if err != nil {
				o.err = err.Error()
			}
			out = append(out, o)
		}
		return out
	}
	first := read()
	runs := r.runs
	if runs == 0 || first[0].err != "" || first[1].err == "" || !strings.Contains(first[2].result.Text, "+changed") {
		t.Fatalf("unexpected first reads: %+v", first)
	}
	second := read()
	if r.runs != runs {
		t.Fatalf("cached reads ran git %d more times", r.runs-runs)
	}
	for i := range first {
		if first[i] != second[i] {
			t.Fatalf("read %d changed: %+v then %+v", i, first[i], second[i])
		}
	}
	if _, err := r.Read(ctx, "read_file", ReadRequest{Revision: head, Path: "file.txt", StartLine: 2, EndLine: 2}); err != nil || r.runs != runs {
		t.Fatalf("another range of a cached blob ran git: %v", err)
	}
}
