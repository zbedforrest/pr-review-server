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

func fixtureRun(t *testing.T, legacy bool) Result {
	t.Helper()
	dumps, err := loadDumps("testdata/dumps")
	if err != nil {
		t.Fatal(err)
	}
	st, err := newStore("testdata/sidecars", nil)
	if err != nil {
		t.Fatal(err)
	}
	pol := publisher.DefaultPolicy()
	pol.LegacyLedger = legacy
	res, err := Run(context.Background(), Options{Dumps: dumps, Store: st, Policy: pol})
	if err != nil {
		t.Fatal(err)
	}
	return res
}

func comparable(m Metrics) Metrics {
	m.Observed = ObservedMetrics{}
	m.PrismResolvedNote = ""
	return m
}

// The fixtures hold a same-marker repost after an unrelated push (#1), a
// rewording on a new line (#2) and a finding that returns after a real fix
// (#3). The ledger policy posts each once and says the rest in the thread.
func TestRun_FixtureMetrics(t *testing.T) {
	m := fixtureRun(t, false).Metrics
	want := Metrics{
		PRs: 3, PRsWithRounds: 3, Rounds: 9, RoundsReplayed: 9, SameCommitRounds: 1,
		RootsPosted: 3, Fixed: 1, InThreadReplies: 1,
		CommentsPerPushP50: 0, RoundsPerPRP50: 3,
	}
	if got := comparable(m); got != want {
		t.Fatalf("metrics\n got %+v\nwant %+v", got, want)
	}
	if m.Observed.Roots != 6 || m.Observed.SameMarkerReposts != 2 || m.Observed.SameDefectReposts != 0 || m.Observed.ThreadsResolved != 2 {
		t.Fatalf("observed = %+v", m.Observed)
	}
}

// The legacy numbers pin the defects the ledger policy removes.
func TestRun_FixtureMetricsLegacy(t *testing.T) {
	m := fixtureRun(t, true).Metrics
	want := Metrics{
		PRs: 3, PRsWithRounds: 3, Rounds: 9, RoundsReplayed: 9, SameCommitRounds: 1,
		RootsPosted: 6, SameMarkerReposts: 2, SameDefectReposts: 1, SameDefectRepostsPerPR: 0.333, PRsWithReposts: 3,
		Fixed: 3, FixedWithoutFileChange: 1,
		CommentsPerPushP50: 1, RoundsPerPRP50: 3,
	}
	if got := comparable(m); got != want {
		t.Fatalf("metrics\n got %+v\nwant %+v", got, want)
	}
}

func TestRun_PerPRRowsAndCSV(t *testing.T) {
	res := fixtureRun(t, false)
	if len(res.PerPR) != 3 {
		t.Fatalf("rows = %d", len(res.PerPR))
	}
	repost, reword, reopen := res.PerPR[0], res.PerPR[1], res.PerPR[2]
	if repost.PR != "example#1" || repost.RootsPosted != 1 || repost.SameMarkerReposts != 0 || repost.Fixed != 0 || repost.FixedWithoutFileChange != 0 {
		t.Errorf("example#1 = %+v", repost)
	}
	if reword.PR != "example#2" || reword.RootsPosted != 1 || reword.SameDefectReposts != 0 || reword.Fixed != 0 || reword.SameCommitRounds != 1 {
		t.Errorf("example#2 = %+v", reword)
	}
	if reopen.PR != "example#3" || reopen.RootsPosted != 1 || reopen.Fixed != 1 || reopen.FixedWithoutFileChange != 0 || reopen.InThreadReplies != 1 {
		t.Errorf("example#3 = %+v", reopen)
	}
	var buf bytes.Buffer
	if err := writeCSV(&buf, res.PerPR); err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
	if len(lines) != 4 || !strings.HasPrefix(lines[0], "pr,rounds,") || !strings.HasPrefix(lines[3], "example#3,3,0,0,1,0,0,0,1,0,0,0,1,") {
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
	got := replayPolicy(publisher.DefaultInlineCap, publisher.DefaultInlineMinSeverity, publisher.DefaultPolicy().ShowUnverified, false)
	if got != publisher.DefaultPolicy() {
		t.Fatalf("replayPolicy defaults = %+v, want %+v", got, publisher.DefaultPolicy())
	}
	if p := replayPolicy(1, "high", false, false); p.InlineCap != 1 || p.InlineMinSeverity != "high" || p.ShowUnverified {
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

type flaggedCompareFetcher struct{ res compareResult }

func (flaggedCompareFetcher) Sidecar(string, string, int, string) ([]byte, error) {
	return nil, errNotFound
}

func (f flaggedCompareFetcher) Compare(string, string, string, string) (compareResult, error) {
	return f.res, nil
}

func compareJSON(files int, renames int) []byte {
	var entries []string
	for i := 0; i < files; i++ {
		e := fmt.Sprintf(`{"filename":"f%d.go"`, i)
		if i < renames {
			e += fmt.Sprintf(`,"previous_filename":"old%d.go"`, i)
		}
		entries = append(entries, e+"}")
	}
	return []byte(`{"files":[` + strings.Join(entries, ",") + `]}`)
}

func TestParseCompare_TruncationUsesRawEntryCount(t *testing.T) {
	cases := []struct {
		name           string
		files, renames int
		wantFlat       int
		wantTruncated  bool
	}{
		{"under the cap with renames", compareFilesCap - 1, 50, compareFilesCap + 49, false},
		{"at the cap", compareFilesCap, 0, compareFilesCap, true},
		{"over the cap", compareFilesCap + 5, 0, compareFilesCap + 5, true},
		{"empty", 0, 0, 0, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			res, err := parseCompare(compareJSON(c.files, c.renames))
			if err != nil {
				t.Fatal(err)
			}
			if len(res.Files) != c.wantFlat || res.Truncated != c.wantTruncated || res.complete() == c.wantTruncated {
				t.Fatalf("files=%d truncated=%v complete=%v, want %d/%v", len(res.Files), res.Truncated, res.complete(), c.wantFlat, c.wantTruncated)
			}
		})
	}
}

func TestStoreCompare_TruncatedIsUnknownFreshAndCached(t *testing.T) {
	cases := []struct {
		name  string
		res   compareResult
		known bool
	}{
		{"complete", compareResult{Files: []string{"a.go"}}, true},
		{"truncated", compareResult{Files: []string{"a.go"}, Truncated: true}, false},
		{"errored", compareResult{Error: "404"}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			dir := t.TempDir()
			st, err := newStore(dir, flaggedCompareFetcher{res: c.res})
			if err != nil {
				t.Fatal(err)
			}
			if _, known, err := st.compare("acme", "example", "aaaaaaa", "bbbbbbb"); err != nil || known != c.known {
				t.Errorf("fresh known = %v, %v, want %v", known, err, c.known)
			}
			cached, err := newStore(dir, nil)
			if err != nil {
				t.Fatal(err)
			}
			if _, known, err := cached.compare("acme", "example", "aaaaaaa", "bbbbbbb"); err != nil || known != c.known {
				t.Errorf("cached known = %v, %v, want %v", known, err, c.known)
			}
		})
	}
}
