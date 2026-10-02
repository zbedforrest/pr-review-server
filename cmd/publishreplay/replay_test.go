package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"pr-review-server/pkg/publisher"
	"pr-review-server/pkg/reviewer/payload"
)

func fixtureRun(t *testing.T) Result {
	t.Helper()
	dumps, err := loadDumps("testdata/dumps")
	if err != nil {
		t.Fatal(err)
	}
	st, err := newStore("testdata/sidecars", nil)
	if err != nil {
		t.Fatal(err)
	}
	res, err := Run(context.Background(), Options{Dumps: dumps, Store: st, Policy: publisher.DefaultPolicy()})
	if err != nil {
		t.Fatal(err)
	}
	return res
}

func TestRun_FixtureMetrics(t *testing.T) {
	m := fixtureRun(t).Metrics
	// SameMarkerReposts 1 and FixedWithoutFileChange 1 encode master's publisher defects; both drop to 0 once the ledger fix ships.
	want := Metrics{
		PRs: 2, PRsWithRounds: 2, Rounds: 6, RoundsReplayed: 6, SameCommitRounds: 1,
		RootsPosted: 4, SameMarkerReposts: 1, SameDefectReposts: 1, SameDefectRepostsPerPR: 0.5, PRsWithReposts: 2,
		Fixed: 2, FixedWithoutFileChange: 1,
		CommentsPerPushP50: 1, RoundsPerPRP50: 3,
	}
	got := m
	got.Observed = ObservedMetrics{}
	got.PrismResolvedNote = ""
	if got != want {
		t.Fatalf("metrics\n got %+v\nwant %+v", got, want)
	}
	if m.Observed.Roots != 4 || m.Observed.SameMarkerReposts != 1 || m.Observed.SameDefectReposts != 0 || m.Observed.ThreadsResolved != 1 {
		t.Fatalf("observed = %+v", m.Observed)
	}
}

func TestRun_PerPRRowsAndCSV(t *testing.T) {
	res := fixtureRun(t)
	if len(res.PerPR) != 2 {
		t.Fatalf("rows = %d", len(res.PerPR))
	}
	repost, reword := res.PerPR[0], res.PerPR[1]
	if repost.PR != "example#1" || repost.SameMarkerReposts != 1 || repost.FixedWithoutFileChange != 1 || repost.SameDefectReposts != 0 {
		t.Errorf("example#1 = %+v", repost)
	}
	if reword.PR != "example#2" || reword.SameDefectReposts != 1 || reword.SameMarkerReposts != 0 || reword.Fixed != 1 || reword.FixedWithoutFileChange != 0 || reword.SameCommitRounds != 1 {
		t.Errorf("example#2 = %+v", reword)
	}
	var buf bytes.Buffer
	if err := writeCSV(&buf, res.PerPR); err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
	if len(lines) != 3 || !strings.HasPrefix(lines[0], "pr,rounds,") || !strings.HasPrefix(lines[1], "example#1,3,0,0,2,1,0,0,1,1,0,0,") {
		t.Fatalf("csv:\n%s", buf.String())
	}
}

func TestRun_MissingSidecarCountsNotFails(t *testing.T) {
	dumps, err := loadDumps("testdata/dumps")
	if err != nil {
		t.Fatal(err)
	}
	st, err := newStore(t.TempDir(), nil)
	if err != nil {
		t.Fatal(err)
	}
	res, err := Run(context.Background(), Options{Dumps: dumps[:1], Store: st, Policy: publisher.DefaultPolicy()})
	if err != nil {
		t.Fatal(err)
	}
	if res.Metrics.Rounds != 3 || res.Metrics.RoundsMissingSidecar != 3 || res.Metrics.RootsPosted != 0 {
		t.Fatalf("metrics = %+v", res.Metrics)
	}
}

func TestSameDefect(t *testing.T) {
	a := "The retry loop in fetchUser never backs off, so a 429 from upstream is retried immediately."
	b := "fetchUser hammers the endpoint after a 429 response because no delay is inserted between attempts."
	if sameDefect("api/users.go", 140, a, nil, "api/users.go", 143, b, nil) {
		t.Fatal("low-overlap rewording without subjects must not match")
	}
	if !sameDefect("api/users.go", 140, a, []string{"fetchuser"}, "api/users.go", 143, b, []string{"fetchuser"}) {
		t.Fatal("shared subject must match")
	}
	if !sameDefect("api/users.go", 140, a, nil, "api/users.go", 145, a, nil) {
		t.Fatal("same text, same file and nearby line must match")
	}
	if sameDefect("api/users.go", 140, a, nil, "src/api/users.go", 145, a, nil) {
		t.Fatal("a nested path that merely ends in the other must not match")
	}
	if sameDefect("api/users.go", 140, a, []string{"fetchuser"}, "api/users.go", 200, a, []string{"fetchuser"}) {
		t.Fatal("lines more than ten apart must not match")
	}
	if sameDefect("api/users.go", 140, a, nil, "api/other.go", 140, a, nil) {
		t.Fatal("different files must not match")
	}
	if sameDefect("a/index.ts", 10, a, nil, "b/index.ts", 10, a, nil) {
		t.Fatal("a shared basename under different directories must not match")
	}
}

func TestSameDefect_UnknownLineNeverMatches(t *testing.T) {
	text := "The retry loop in fetchUser never backs off, so a 429 from upstream is retried immediately."
	cases := []struct {
		name        string
		line, oLine int
	}{
		{"new line unknown", 0, 140},
		{"earlier line unknown", 140, 0},
		{"both unknown", 0, 0},
		{"negative line", -1, 140},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if sameDefect("api/users.go", c.line, text, []string{"fetchuser"}, "api/users.go", c.oLine, text, []string{"fetchuser"}) {
				t.Fatal("identical text and subjects must not alias when a line is unknown")
			}
		})
	}
}

func TestLoadDumps_NoFilesIsError(t *testing.T) {
	cases := map[string]string{
		"empty directory":   t.TempDir(),
		"missing directory": filepath.Join(t.TempDir(), "nope"),
	}
	for name, dir := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := loadDumps(dir); err == nil {
				t.Fatal("expected an error, got nil")
			}
		})
	}
	if err := os.WriteFile(filepath.Join(cases["empty directory"], "notes.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := loadDumps(cases["empty directory"]); err == nil {
		t.Fatal("a directory without *.json must be an error")
	}
}

type countingFetcher struct {
	mu       sync.Mutex
	compares map[string]int
}

func (c *countingFetcher) Sidecar(string, string, int, string) ([]byte, error) {
	return nil, errNotFound
}

func (c *countingFetcher) Compare(owner, repo, base, head string) (compareResult, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.compares == nil {
		c.compares = map[string]int{}
	}
	c.compares[compareKey(owner, repo, base, head)]++
	return compareResult{Files: []string{"a.go"}}, nil
}

func dumpWithRounds(owner, repo string, number int, shas ...string) *prDump {
	d := &prDump{Number: number, Owner: owner, Repo: repo}
	at := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	for i, sha := range shas {
		r := dumpReview{Author: &dumpActor{Login: "acme-bot"}, State: "COMMENTED", SubmittedAt: at.Add(time.Duration(i) * time.Hour)}
		r.Commit.Oid = sha
		d.Reviews.Nodes = append(d.Reviews.Nodes, r)
	}
	return d
}

func TestPrefetch_ComparesOncePerRepoAndPair(t *testing.T) {
	const a, b = "aaaaaaa0000000000000000000000000000000000", "bbbbbbb0000000000000000000000000000000000"
	dumps := []*prDump{
		dumpWithRounds("acme", "example", 1, a, b),
		dumpWithRounds("acme", "example", 2, a, b),
		dumpWithRounds("acme", "other", 3, a, b),
	}
	dir := t.TempDir()
	f := &countingFetcher{}
	st, err := newStore(dir, f)
	if err != nil {
		t.Fatal(err)
	}
	bots := func(*prDump) map[string]bool { return map[string]bool{"acme-bot": true} }
	if err := prefetch(st, dumps, bots, 4, func(string, ...any) {}); err != nil {
		t.Fatal(err)
	}
	if n := f.compares[compareKey("acme", "example", a, b)]; n != 1 {
		t.Errorf("acme/example compared %d times, want 1", n)
	}
	if n := f.compares[compareKey("acme", "other", a, b)]; n != 1 {
		t.Errorf("acme/other compared %d times, want 1", n)
	}
	if len(f.compares) != 2 {
		t.Errorf("compares = %v", f.compares)
	}
	entries, err := os.ReadDir(filepath.Join(dir, "compare"))
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	if len(names) != 2 || strings.Contains(strings.Join(names, " "), ".tmp") {
		t.Fatalf("compare cache = %v, want exactly the two final files", names)
	}
}

func TestWriteFileAtomic(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "x.json")
	if err := writeFileAtomic(path, []byte("one")); err != nil {
		t.Fatal(err)
	}
	if err := writeFileAtomic(path, []byte("two")); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(path)
	if err != nil || string(got) != "two" {
		t.Fatalf("read = %q, %v", got, err)
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 1 {
		t.Fatalf("temp files left behind: %d entries", len(entries))
	}
}

func TestReplayPolicy_FlagDefaultsMatchShipped(t *testing.T) {
	got := replayPolicy(publisher.DefaultInlineCap, publisher.DefaultInlineMinSeverity, publisher.DefaultPolicy().ShowUnverified)
	if got != publisher.DefaultPolicy() {
		t.Fatalf("replayPolicy defaults = %+v, want %+v", got, publisher.DefaultPolicy())
	}
	if p := replayPolicy(1, "high", false); p.InlineCap != 1 || p.InlineMinSeverity != "high" || p.ShowUnverified {
		t.Fatalf("overrides not applied: %+v", p)
	}
}

type failingFetcher struct{}

func (failingFetcher) Sidecar(string, string, int, string) ([]byte, error) {
	return nil, errors.New("503 service unavailable")
}

func (failingFetcher) Compare(string, string, string, string) (compareResult, error) {
	return compareResult{}, errors.New("rate limited")
}

func TestRun_TransientFetchErrorCountsAsMissing(t *testing.T) {
	dumps, err := loadDumps("testdata/dumps")
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	st, err := newStore(dir, failingFetcher{})
	if err != nil {
		t.Fatal(err)
	}
	res, err := Run(context.Background(), Options{Dumps: dumps[:1], Store: st, Policy: publisher.DefaultPolicy()})
	if err != nil {
		t.Fatal(err)
	}
	if res.Metrics.RoundsMissingSidecar != 3 {
		t.Fatalf("metrics = %+v", res.Metrics)
	}
	if markers, _ := filepath.Glob(filepath.Join(dir, "*.missing")); len(markers) != 0 {
		t.Fatalf("transient errors must not leave .missing markers: %v", markers)
	}
}

func TestFileOfFingerprint(t *testing.T) {
	cases := map[string]string{
		"svc/retry.go:7:f1f1f1f1f1f1": "svc/retry.go",
		"a:b/c.go:0:abcdef012345":     "a:b/c.go",
		"SUMMARY:0:abcdef012345":      "SUMMARY",
		"noseparators":                "noseparators",
	}
	for fp, want := range cases {
		if got := fileOfFingerprint(fp); got != want {
			t.Errorf("fileOfFingerprint(%q) = %q, want %q", fp, got, want)
		}
	}
}

func TestPatchesFromHunks(t *testing.T) {
	pl := payload.Payload{Findings: []payload.Finding{
		{File: "a.go", Line: 72, DiffHunk: "@@ -70,3 +70,3 @@\n x\n+y\n z"},
		{File: "a.go", Line: 90, DiffHunk: "@@ -88,3 +88,3 @@\n x\n y\n z"},
		{File: "b.go", Line: 5},
		{File: "SUMMARY", Line: 0},
	}}
	patches := patchesFromHunks(pl)
	commentable := publisher.CommentableLines(patches["a.go"])
	for _, l := range []int{70, 71, 72, 88, 89, 90} {
		if !commentable[l] {
			t.Errorf("a.go line %d should be commentable", l)
		}
	}
	if commentable[80] {
		t.Error("a.go line 80 lies between hunks")
	}
	if !publisher.CommentableLines(patches["b.go"])[5] {
		t.Error("a finding without a hunk keeps its own line")
	}
	if _, ok := patches["SUMMARY"]; ok {
		t.Error("narrative findings have no patch")
	}
}

func TestP50(t *testing.T) {
	if p50(nil) != 0 || p50([]int{3}) != 3 || p50([]int{1, 2, 3, 4}) != 2.5 || p50([]int{5, 1, 3}) != 3 {
		t.Fatal("p50 arithmetic")
	}
}

func TestFilterDumps(t *testing.T) {
	dumps, err := loadDumps("testdata/dumps")
	if err != nil {
		t.Fatal(err)
	}
	if got, err := filterDumps(dumps, "acme/example#2", 0); err != nil || len(got) != 1 || got[0].Number != 2 {
		t.Fatalf("owner-qualified filter = %d dumps, %v", len(got), err)
	}
	if got, err := filterDumps(dumps, "example#1", 0); err != nil || len(got) != 1 || got[0].Number != 1 {
		t.Fatalf("bare filter = %d dumps, %v", len(got), err)
	}
	if _, err := filterDumps(dumps, "other/example#1", 0); err == nil {
		t.Fatal("a different owner must not match")
	}
	if _, err := filterDumps(dumps, "example#9", 0); err == nil {
		t.Fatal("an unmatched filter must be an error")
	}
	if got, err := filterDumps(dumps, "", 1); err != nil || len(got) != 1 {
		t.Fatalf("limit = %d dumps, %v", len(got), err)
	}
	forks := []*prDump{{Number: 1, Owner: "acme", Repo: "example"}, {Number: 1, Owner: "other", Repo: "example"}}
	if _, err := filterDumps(forks, "example#1", 0); err == nil {
		t.Fatal("a bare filter matching two owners must be an error")
	}
	if got, err := filterDumps(forks, "other/example#1", 0); err != nil || len(got) != 1 || got[0].Owner != "other" {
		t.Fatalf("owner-qualified fork filter = %d dumps, %v", len(got), err)
	}
}

func TestClassifyRepost_EmptyMarkerNeverMatches(t *testing.T) {
	earlier := []post{{FindingID: "", File: "a.go", Line: 1, RawText: "x"}}
	if classifyRepost(post{FindingID: "", File: "b.go", Line: 500, RawText: "y"}, earlier) != repostNone {
		t.Fatal("two posts without a marker must not count as a same-marker repost")
	}
	if classifyRepost(post{FindingID: "f1", File: "b.go", Line: 500}, []post{{FindingID: "f1"}}) != repostSameMarker {
		t.Fatal("an equal marker must match")
	}
}

type fixedFilesFetcher struct{ n int }

func (fixedFilesFetcher) Sidecar(string, string, int, string) ([]byte, error) {
	return nil, errNotFound
}

func (f fixedFilesFetcher) Compare(string, string, string, string) (compareResult, error) {
	files := make([]string, f.n)
	for i := range files {
		files[i] = fmt.Sprintf("f%d.go", i)
	}
	return compareResult{Files: files}, nil
}

func TestStoreCompare_CappedFileListIsUnknown(t *testing.T) {
	cases := []struct {
		files int
		known bool
	}{
		{files: compareFilesCap - 1, known: true},
		{files: compareFilesCap, known: false},
		{files: compareFilesCap + 5, known: false},
	}
	for _, c := range cases {
		dir := t.TempDir()
		st, err := newStore(dir, fixedFilesFetcher{n: c.files})
		if err != nil {
			t.Fatal(err)
		}
		if _, known, err := st.compare("acme", "example", "aaaaaaa", "bbbbbbb"); err != nil || known != c.known {
			t.Errorf("%d files: fresh known = %v, %v, want %v", c.files, known, err, c.known)
		}
		cached, err := newStore(dir, nil)
		if err != nil {
			t.Fatal(err)
		}
		if _, known, err := cached.compare("acme", "example", "aaaaaaa", "bbbbbbb"); err != nil || known != c.known {
			t.Errorf("%d files: cached known = %v, %v, want %v", c.files, known, err, c.known)
		}
	}
}

func TestBotLoginsDetectedFromMarkers(t *testing.T) {
	dumps, err := loadDumps("testdata/dumps")
	if err != nil {
		t.Fatal(err)
	}
	bots := dumps[0].botLogins()
	if !bots["acme-bot"] || len(bots) != 1 {
		t.Fatalf("bots = %v", bots)
	}
	if rounds := dumps[0].rounds(map[string]bool{"acme-bot": true}); len(rounds) != 3 || rounds[0].SHA7 != "aaaaaaa" {
		t.Fatalf("rounds = %+v", rounds)
	}
}
