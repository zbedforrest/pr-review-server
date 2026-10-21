package verify

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"pr-review-server/pkg/reviewer/types"
)

type repo struct {
	t   *testing.T
	dir string
}

func newRepo(t *testing.T) *repo {
	t.Helper()
	r := &repo{t: t, dir: t.TempDir()}
	r.git("init", "-q", "-b", "main")
	r.git("config", "user.email", "t@example.com")
	r.git("config", "user.name", "t")
	return r
}

func (r *repo) git(args ...string) string {
	r.t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = r.dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		r.t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

func (r *repo) write(path, content string) {
	r.t.Helper()
	full := filepath.Join(r.dir, path)
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		r.t.Fatal(err)
	}
	if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
		r.t.Fatal(err)
	}
}

func (r *repo) commit() string {
	r.git("add", "-A")
	r.git("commit", "-q", "-m", "c")
	return r.git("rev-parse", "HEAD")
}

func lines(prefix string, n int) string {
	var b strings.Builder
	for i := 1; i <= n; i++ {
		b.WriteString(prefix + " " + strconv.Itoa(i) + "\n")
	}
	return b.String()
}

func items(p *Packet, key string) map[string][]Item {
	out := map[string][]Item{}
	for _, id := range p.ByFinding[key] {
		it, _ := p.Item(id)
		out[it.Kind] = append(out[it.Kind], it)
	}
	return out
}

func finding(path string, line int, body string) Finding {
	return Finding{Key: "F1", Comment: types.LineComment{FilePath: path, LineNumber: line, CommentBody: body}}
}

func TestBuild_ChangedLineGetsHeadHunkAndTheBaseTextItReplaced(t *testing.T) {
	r := newRepo(t)
	r.write("svc/handler.go", lines("keep", 50)+"func Handle() { return checkLimit(req) }\n"+lines("tail", 50))
	base := r.commit()
	r.write("svc/handler.go", lines("keep", 50)+"func Handle() { return nil }\n"+lines("tail", 50))
	head := r.commit()

	p, err := Build(context.Background(), RepoGit{Dir: r.dir}, Input{Head: head, Base: base, Findings: []Finding{
		finding("svc/handler.go", 51, "`Handle` no longer checks the limit"),
	}}, DefaultLimits)
	if err != nil {
		t.Fatal(err)
	}
	got := items(p, "F1")
	if h := got[KindHead]; len(h) != 1 || h[0].Start != 21 || h[0].End != 81 || !strings.Contains(h[0].Text, "   51| func Handle() { return nil }") {
		t.Fatalf("head = %+v", h)
	}
	if h := got[KindHunk]; len(h) != 1 || !strings.Contains(h[0].Text, "-func Handle() { return checkLimit(req) }") {
		t.Fatalf("hunk = %+v", h)
	}
	if b := got[KindBase]; len(b) != 1 || b[0].Rev != base || !strings.Contains(b[0].Text, "   51| func Handle() { return checkLimit(req) }") {
		t.Fatalf("base = %+v", b)
	}
}

func TestBuild_UnchangedLineMapsThroughEarlierHunksToItsBaseLocation(t *testing.T) {
	r := newRepo(t)
	r.write("a.py", lines("old", 5)+lines("ctx", 80)+"target = compute()\n")
	base := r.commit()
	r.write("a.py", lines("new", 15)+lines("ctx", 80)+"target = compute()\n")
	head := r.commit()

	p, err := Build(context.Background(), RepoGit{Dir: r.dir}, Input{Head: head, Base: base, Findings: []Finding{
		finding("a.py", 96, "target is wrong"),
	}}, DefaultLimits)
	if err != nil {
		t.Fatal(err)
	}
	got := items(p, "F1")
	if len(got[KindHunk]) != 0 {
		t.Fatalf("an unchanged line got a hunk: %+v", got[KindHunk])
	}
	if b := got[KindBase]; len(b) != 1 || !strings.Contains(b[0].Text, "   86| target = compute()") {
		t.Fatalf("base = %+v", b)
	}
}

func TestBuild_NewFileSaysSoInsteadOfBaseUnavailable(t *testing.T) {
	r := newRepo(t)
	r.write("README", "x\n")
	base := r.commit()
	r.write("new.go", "package x\n")
	head := r.commit()

	p, err := Build(context.Background(), RepoGit{Dir: r.dir}, Input{Head: head, Base: base, Findings: []Finding{finding("new.go", 1, "claim")}}, DefaultLimits)
	if err != nil {
		t.Fatal(err)
	}
	notes := items(p, "F1")[KindNote]
	if len(notes) != 1 || !strings.Contains(notes[0].Text, "file is new in this PR") {
		t.Fatalf("notes = %+v", notes)
	}
}

func TestBuild_MissingHeadPathAndUnknownBaseBecomeNotes(t *testing.T) {
	r := newRepo(t)
	r.write("a.go", "package a\n")
	head := r.commit()

	p, err := Build(context.Background(), RepoGit{Dir: r.dir}, Input{Head: head, Base: "0000000000000000000000000000000000000000", Findings: []Finding{finding("gone.go", 3, "claim")}}, DefaultLimits)
	if err != nil {
		t.Fatal(err)
	}
	var texts []string
	for _, it := range items(p, "F1")[KindNote] {
		texts = append(texts, it.Text)
	}
	joined := strings.Join(texts, "|")
	if !strings.Contains(joined, "head source unavailable") || !strings.Contains(joined, "diff unavailable") {
		t.Fatalf("notes = %v", texts)
	}
}

func TestBuild_ReferencesComeFromSubjectsAndSkipTheCitedRegion(t *testing.T) {
	r := newRepo(t)
	r.write("lib/limits.go", "package lib\n\nfunc checkLimit(r int) bool { return r < 10 }\n")
	r.write("api/handler.go", "package api\n\nfunc Handle(r int) bool { return checkLimit(r) }\n")
	r.write("api/other.go", "package api\n\nvar x = checkLimit(3)\n")
	head := r.commit()

	c := types.LineComment{FilePath: "api/handler.go", LineNumber: 3, CommentBody: "claim",
		FindingContract: &types.FindingContract{Subjects: []types.FindingSubject{{Kind: "symbol", Path: "lib/limits.go", Name: "checkLimit"}}}}
	p, err := Build(context.Background(), RepoGit{Dir: r.dir}, Input{Head: head, Base: head, Findings: []Finding{{Key: "F1", Comment: c}}}, DefaultLimits)
	if err != nil {
		t.Fatal(err)
	}
	var paths []string
	for _, it := range items(p, "F1")[KindRef] {
		paths = append(paths, it.Path)
	}
	if strings.Join(paths, ",") != "api/other.go,lib/limits.go" {
		t.Fatalf("ref paths = %v", paths)
	}
}

func TestBuild_SharesPRTextAndReusesIdenticalItems(t *testing.T) {
	r := newRepo(t)
	r.write("a.go", lines("l", 10))
	head := r.commit()

	p, err := Build(context.Background(), RepoGit{Dir: r.dir}, Input{Head: head, Base: head, PRText: strings.Repeat("é", 5000), Findings: []Finding{
		{Key: "F1", Comment: types.LineComment{FilePath: "a.go", LineNumber: 4, CommentBody: "one"}},
		{Key: "F2", Comment: types.LineComment{FilePath: "a.go", LineNumber: 4, CommentBody: "two"}},
	}}, DefaultLimits)
	if err != nil {
		t.Fatal(err)
	}
	pr, _ := p.Item(p.Shared[0])
	if pr.Kind != KindPR || !strings.HasSuffix(pr.Text, "[truncated]") || len([]rune(pr.Text)) != 4000+len("\n[truncated]") {
		t.Fatalf("pr item = %d runes", len([]rune(pr.Text)))
	}
	if p.ByFinding["F1"][0] != p.ByFinding["F2"][0] {
		t.Fatalf("identical head regions got separate ids: %v %v", p.ByFinding["F1"], p.ByFinding["F2"])
	}
}

func TestPacketItem_RejectsUnknownIDs(t *testing.T) {
	p := &Packet{Items: []Item{{ID: "P1"}}}
	for _, id := range []string{"P0", "P2", "1", "Px", ""} {
		if _, ok := p.Item(id); ok {
			t.Errorf("%q resolved", id)
		}
	}
	if it, ok := p.Item("P1"); !ok || it.ID != "P1" {
		t.Fatal("P1 did not resolve")
	}
}

func TestSymbols_TakesSubjectsThenCodeSpansThenCompoundNames(t *testing.T) {
	c := types.LineComment{
		CommentBody:     "The `cache.get_or_set()` call in fetchRooms ignores the TTL and return value\nbody text `ignored`",
		FindingContract: &types.FindingContract{Subjects: []types.FindingSubject{{Name: "RoomCache.Refresh"}}},
	}
	got := strings.Join(symbols(c, 10), ",")
	if got != "RoomCache,Refresh,cache,get_or_set,fetchRooms" {
		t.Fatalf("symbols = %s", got)
	}
}

func TestBuild_SubjectsInOtherFilesAreIncludedOrNotedMissing(t *testing.T) {
	r := newRepo(t)
	r.write("api/handler.go", "package api\n\nfunc Handle() {}\n")
	r.write("lib/limits.go", lines("pad", 40)+"func checkLimit() {}\n"+lines("tail", 40))
	head := r.commit()

	c := types.LineComment{FilePath: "api/handler.go", LineNumber: 3, CommentBody: "claim",
		FindingContract: &types.FindingContract{Subjects: []types.FindingSubject{
			{Kind: "symbol", Path: "./lib/limits.go", Name: "checkLimit"},
			{Kind: "file", Path: "node_modules/react/index.js"},
			{Kind: "file", Path: "api/handler.go"},
		}}}
	p, err := Build(context.Background(), RepoGit{Dir: r.dir}, Input{Head: head, Base: head, Findings: []Finding{{Key: "F1", Comment: c}}}, DefaultLimits)
	if err != nil {
		t.Fatal(err)
	}
	got := items(p, "F1")
	var subject *Item
	for i, it := range got[KindHead] {
		if it.Path == "lib/limits.go" {
			subject = &got[KindHead][i]
		}
	}
	if subject == nil || subject.Start != 31 || !strings.Contains(subject.Text, "   41| func checkLimit() {}") {
		t.Fatalf("subject item = %+v", subject)
	}
	var notes []string
	for _, it := range got[KindNote] {
		notes = append(notes, it.Path+": "+it.Text)
	}
	if len(notes) != 1 || notes[0] != "node_modules/react/index.js: subject source not in the repository at head: path not found at that revision" {
		t.Fatalf("notes = %v", notes)
	}
}

func TestBuild_ClipsMinifiedLines(t *testing.T) {
	r := newRepo(t)
	r.write("app.js", "callMe()\n")
	r.write("vendor/bundle.min.js", "var a="+strings.Repeat("x", 5000)+";callMe();\n")
	head := r.commit()

	c := types.LineComment{FilePath: "app.js", LineNumber: 1, CommentBody: "`callMe` is wrong"}
	p, err := Build(context.Background(), RepoGit{Dir: r.dir}, Input{Head: head, Base: head, Findings: []Finding{{Key: "F1", Comment: c}}}, DefaultLimits)
	if err != nil {
		t.Fatal(err)
	}
	refs := items(p, "F1")[KindRef]
	if len(refs) != 1 || len(refs[0].Text) > 400 || !strings.Contains(refs[0].Text, "more chars]") {
		t.Fatalf("refs = %+v", refs)
	}
}
