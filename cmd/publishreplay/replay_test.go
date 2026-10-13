package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"pr-review-server/pkg/publisher"
	"pr-review-server/pkg/reviewer/payload"
)

func fixtureRun(t *testing.T, legacy bool) Result {
	t.Helper()
	pol := publisher.DefaultPolicy()
	pol.LegacyLedger = legacy
	return fixtureRunWith(t, pol)
}

func fixtureRunWith(t *testing.T, policy publisher.Policy) Result {
	t.Helper()
	dumps, err := loadDumps("testdata/dumps")
	if err != nil {
		t.Fatal(err)
	}
	st, err := newStore("testdata/sidecars", nil)
	if err != nil {
		t.Fatal(err)
	}
	res, err := Run(context.Background(), Options{Dumps: dumps, Store: st, Policy: policy})
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
// rewording on a new line (#2), a finding that returns after a real fix
// (#3) and a point Greptile raised two minutes earlier (#4). The ledger
// policy posts each of the first three once and says the rest in the
// thread; the fourth becomes an external row with one delta reply.
func TestRun_FixtureMetrics(t *testing.T) {
	m := fixtureRun(t, false).Metrics
	want := Metrics{
		PRs: 4, PRsWithRounds: 4, Rounds: 10, RoundsReplayed: 10, SameCommitRounds: 1, SameCommitSkipped: 1,
		RootsPosted: 3, Fixed: 1, InThreadReplies: 1, ThreadsResolved: 1, ThreadsUnresolved: 1,
		ExternalMatched: 1, ExternalReplies: 1,
		CommentsPerPushP50: 0, CommentsPerPushMean: 0.333, RoundsPerPRP50: 3,
	}
	if got := comparable(m); got != want {
		t.Fatalf("metrics\n got %+v\nwant %+v", got, want)
	}
	if o := m.Observed; o.Roots != 7 || o.SameMarkerReposts != 2 || o.SameDefectReposts != 0 || o.ThreadsResolved != 2 || o.ExternalDuplicatesNear != 1 || o.ExternalDuplicatesOpen != 1 || o.PRsWithExternalDuplicates != 1 {
		t.Fatalf("observed = %+v", m.Observed)
	}
}

// The legacy numbers pin the defects the ledger policy removes, the
// external duplicate among them: its thread's state is unknown to the
// dump, so it counts as open.
func TestRun_FixtureMetricsLegacy(t *testing.T) {
	m := fixtureRun(t, true).Metrics
	want := Metrics{
		PRs: 4, PRsWithRounds: 4, Rounds: 10, RoundsReplayed: 10, SameCommitRounds: 1, SameCommitSkipped: 1,
		RootsPosted: 7, SameMarkerReposts: 2, SameDefectReposts: 1, SameDefectRepostsPerPR: 0.25, PRsWithReposts: 3,
		Fixed: 3, FixedWithoutFileChange: 1,
		ExternalDuplicatesNear: 1, ExternalDuplicatesOpen: 1, ExternalDuplicatesResolutionUnknown: 1, PRsWithExternalDuplicates: 1,
		CommentsPerPushP50: 1, CommentsPerPushMean: 0.778, RoundsPerPRP50: 3,
	}
	if got := comparable(m); got != want {
		t.Fatalf("metrics\n got %+v\nwant %+v", got, want)
	}
}

func TestRun_ExternalReconcileOffPostsTheDuplicate(t *testing.T) {
	policy := publisher.DefaultPolicy()
	policy.ExternalReconcile = false
	m := fixtureRunWith(t, policy).Metrics
	if m.RootsPosted != 4 || m.ExternalDuplicatesNear != 1 || m.ExternalDuplicatesOpen != 1 || m.ExternalMatched != 0 || m.ExternalReplies != 0 {
		t.Fatalf("metrics = %+v", m)
	}
}

func TestRun_SameCommitGuardOffReplaysTheRound(t *testing.T) {
	policy := publisher.DefaultPolicy()
	policy.RepublishSameCommit = true
	m := fixtureRunWith(t, policy).Metrics
	if m.SameCommitRounds != 1 || m.SameCommitSkipped != 0 || m.RoundsReplayed != 10 {
		t.Fatalf("metrics = %+v", m)
	}
}

func TestRun_PerPRRowsAndCSV(t *testing.T) {
	res := fixtureRun(t, false)
	if len(res.PerPR) != 4 {
		t.Fatalf("rows = %d", len(res.PerPR))
	}
	repost, reword, reopen, external := res.PerPR[0], res.PerPR[1], res.PerPR[2], res.PerPR[3]
	if external.PR != "example#4" || external.RootsPosted != 0 || external.ExternalMatched != 1 || external.ExternalReplies != 1 || external.ExternalDuplicatesNear != 0 || external.ObservedExternalDuplicatesNear != 1 || external.ObservedExternalDuplicatesOpen != 1 {
		t.Errorf("example#4 = %+v", external)
	}
	if repost.PR != "example#1" || repost.RootsPosted != 1 || repost.SameMarkerReposts != 0 || repost.Fixed != 0 || repost.FixedWithoutFileChange != 0 {
		t.Errorf("example#1 = %+v", repost)
	}
	if reword.PR != "example#2" || reword.RootsPosted != 1 || reword.SameDefectReposts != 0 || reword.Fixed != 0 || reword.SameCommitRounds != 1 {
		t.Errorf("example#2 = %+v", reword)
	}
	if reopen.PR != "example#3" || reopen.RootsPosted != 1 || reopen.Fixed != 1 || reopen.FixedWithoutFileChange != 0 || reopen.InThreadReplies != 1 || reopen.ThreadsResolved != 1 || reopen.ThreadsUnresolved != 1 || reopen.RootsResolved != 0 {
		t.Errorf("example#3 = %+v", reopen)
	}
	var buf bytes.Buffer
	if err := writeCSV(&buf, res.PerPR); err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
	if len(lines) != 5 || !strings.HasPrefix(lines[0], "pr,rounds,rounds_after_merge_commit,rounds_skipped_unchanged_diff,") || !strings.HasSuffix(lines[0], ",external_duplicates_near,external_duplicates_open,external_duplicates_unknown,external_matched,external_replies,observed_external_duplicates_near,observed_external_duplicates_open") ||
		!strings.HasPrefix(lines[3], "example#3,3,0,0,0,0,1,0,0,0,1,0,0,0,1,1,1,0,") || !strings.HasSuffix(lines[4], ",0,0,0,1,1,1,1") {
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

func defect(file string, line int, text string, kind string, subjects map[string]string) post {
	p := post{File: file, Line: line, RawText: text, Kind: kind}
	if len(subjects) > 0 {
		p.SubjectKinds = subjects
		for name := range subjects {
			p.Subjects = append(p.Subjects, name)
		}
		sort.Strings(p.Subjects)
	}
	return p
}

func TestSameDefect(t *testing.T) {
	a := "The retry loop in fetchUser never backs off, so a 429 from upstream is retried immediately."
	b := "fetchUser hammers the endpoint after a 429 response because no delay is inserted between attempts."
	fn := map[string]string{"fetchuser": "symbol"}
	if sameDefect(defect("api/users.go", 140, a, "", nil), defect("api/users.go", 143, b, "", nil), false) {
		t.Fatal("low-overlap rewording without subjects must not match")
	}
	if !sameDefect(defect("api/users.go", 140, a, "production_behavior", fn), defect("api/users.go", 143, b, "production_behavior", fn), false) {
		t.Fatal("the same kind and subject set must match")
	}
	if sameDefect(defect("api/users.go", 140, a, "production_behavior", fn), defect("api/users.go", 143, b, "production_behavior", fn), true) {
		t.Fatal("the text clause alone must not match a rewording under the bar")
	}
	if !sameDefect(defect("api/users.go", 140, a, "", nil), defect("api/users.go", 145, a, "", nil), false) {
		t.Fatal("same text, same file and nearby line must match")
	}
	if sameDefect(defect("api/users.go", 140, a, "", nil), defect("src/api/users.go", 145, a, "", nil), false) {
		t.Fatal("a nested path that merely ends in the other must not match")
	}
	if sameDefect(defect("api/users.go", 140, a, "production_behavior", fn), defect("api/users.go", 200, a, "production_behavior", fn), false) {
		t.Fatal("lines more than ten apart must not match")
	}
	if sameDefect(defect("api/users.go", 140, a, "", nil), defect("api/other.go", 140, a, "", nil), false) {
		t.Fatal("different files must not match")
	}
	if sameDefect(defect("a/index.ts", 10, a, "", nil), defect("b/index.ts", 10, a, "", nil), false) {
		t.Fatal("a shared basename under different directories must not match")
	}
}

func TestSharedSubjectKey(t *testing.T) {
	const kind = "production_behavior"
	cases := []struct {
		name string
		p, e post
		want bool
	}{
		{"enclosing function alone, different points", defect("f.py", 10, "Re-enabling the plan wipes the stored discount.", kind, map[string]string{"save": "symbol"}),
			defect("f.py", 14, "The toggle check runs before the form is validated.", kind, map[string]string{"save": "symbol"}), true},
		{"enclosing function shared, other subjects differ", defect("f.py", 10, "x", kind, map[string]string{"save": "symbol", "discount": "symbol"}),
			defect("f.py", 14, "y", kind, map[string]string{"save": "symbol", "is_valid": "symbol"}), true},
		{"enclosing function is the only subject on one side", defect("f.py", 10, "x", kind, map[string]string{"save": "symbol"}),
			defect("f.py", 14, "y", kind, map[string]string{"save": "symbol", "is_valid": "symbol"}), false},
		{"shared bare file", defect("f.py", 10, "x", kind, map[string]string{"f.py": "file", "a": "symbol"}),
			defect("f.py", 14, "y", kind, map[string]string{"f.py": "file", "b": "symbol"}), false},
		{"different finding kinds", defect("f.py", 10, "x", kind, map[string]string{"save": "symbol"}),
			defect("f.py", 14, "y", "test_quality", map[string]string{"save": "symbol"}), false},
		{"no kind recorded", defect("f.py", 10, "x", "", map[string]string{"save": "symbol"}),
			defect("f.py", 14, "y", "", map[string]string{"save": "symbol"}), false},
		{"shared selector among several", defect("t.tsx", 10, "x", kind, map[string]string{".modal": "selector", "open": "symbol"}),
			defect("t.tsx", 14, "y", kind, map[string]string{".modal": "selector", "close": "symbol"}), true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := sharedSubjectKey(c.p, c.e); got != c.want {
				t.Fatalf("sharedSubjectKey = %v, want %v", got, c.want)
			}
		})
	}
}

func TestSameDefect_UnknownLineNeverMatches(t *testing.T) {
	text := "The retry loop in fetchUser never backs off, so a 429 from upstream is retried immediately."
	fn := map[string]string{"fetchuser": "symbol"}
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
			if sameDefect(defect("api/users.go", c.line, text, "production_behavior", fn), defect("api/users.go", c.oLine, text, "production_behavior", fn), false) {
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
	if classifyRepost(post{FindingID: "", File: "b.go", Line: 500, RawText: "y"}, earlier, false) != repostNone {
		t.Fatal("two posts without a marker must not count as a same-marker repost")
	}
	if classifyRepost(post{FindingID: "f1", File: "b.go", Line: 500}, []post{{FindingID: "f1"}}, false) != repostSameMarker {
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

// copyFixtures gives a test its own sidecar cache to edit.
func copyFixtures(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "compare"), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, sub := range []string{"", "compare"} {
		entries, err := os.ReadDir(filepath.Join("testdata/sidecars", sub))
		if err != nil {
			t.Fatal(err)
		}
		for _, e := range entries {
			if e.IsDir() {
				continue
			}
			raw, err := os.ReadFile(filepath.Join("testdata/sidecars", sub, e.Name()))
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(dir, sub, e.Name()), raw, 0o644); err != nil {
				t.Fatal(err)
			}
		}
	}
	return dir
}

// example#1's second round follows a merge-only push: the poller would not
// run it, so the replay skips it and the same-marker repost it carried never
// reaches the recorder.
func TestRun_MergeOnlyPushSkipsTheRound(t *testing.T) {
	dir := copyFixtures(t)
	mergeOnly := []byte(fmt.Sprintf(`{"files":["a.go"],"status":"ahead","commits":1,"merge_commits":1,"merge_only":true,"schema":%d}`, compareSchema))
	if err := os.WriteFile(filepath.Join(dir, "compare", "acme_example_aaaaaaa_bbbbbbb.json"), mergeOnly, 0o644); err != nil {
		t.Fatal(err)
	}
	dumps, err := loadDumps("testdata/dumps")
	if err != nil {
		t.Fatal(err)
	}
	st, err := newStore(dir, nil)
	if err != nil {
		t.Fatal(err)
	}
	res, err := Run(context.Background(), Options{Dumps: dumps, Store: st, Policy: publisher.DefaultPolicy()})
	if err != nil {
		t.Fatal(err)
	}
	m := res.Metrics
	if m.RoundsSkippedUnchangedDiff != 1 || m.RoundsAfterMergeCommit != 1 || m.RoundsReplayed != 9 || res.PerPR[0].RoundsSkippedUnchangedDiff != 1 || res.PerPR[0].Rounds != 3 {
		t.Fatalf("metrics = %+v per-pr = %+v", m, res.PerPR[0])
	}
	if m.RoundsPerPRP50 != 2.5 || m.Observed.RoundsPerPRP50 != 3 {
		t.Fatalf("p50 over four PRs with 2, 3, 3 and 1 admitted rounds is 2.5: admitted=%v observed=%v", m.RoundsPerPRP50, m.Observed.RoundsPerPRP50)
	}
	res, err = Run(context.Background(), Options{Dumps: dumps, Store: st, Policy: publisher.DefaultPolicy(), ReplayUnchangedDiff: true})
	if err != nil {
		t.Fatal(err)
	}
	if res.Metrics.RoundsSkippedUnchangedDiff != 0 || res.Metrics.RoundsReplayed != 10 {
		t.Fatalf("with the skip off every round is replayed: %+v", res.Metrics)
	}
}

// A merge of the base branch lists the base's own commits beside the merge,
// so only the first-parent chain from the tip back to the previous head
// tells a merge-only push from a real one.
func TestParseCompare_JudgesThePushByItsFirstParentChain(t *testing.T) {
	commit := func(sha string, parents ...string) string {
		ps := ""
		for i, p := range parents {
			if i > 0 {
				ps += ","
			}
			ps += `{"sha":"` + p + `"}`
		}
		return `{"sha":"` + sha + `","parents":[` + ps + `]}`
	}
	payload := func(status, base string, commits ...string) []byte {
		return []byte(fmt.Sprintf(`{"status":%q,"base_commit":{"sha":%q},"total_commits":%d,"commits":[%s],"files":[{"filename":"x.go"}]}`,
			status, base, len(commits), strings.Join(commits, ",")))
	}
	mergeOfBase := payload("ahead", "h1", commit("m1", "m0"), commit("m2", "m1"), commit("M", "h1", "m2"))
	res, err := parseCompare(mergeOfBase)
	if err != nil || !res.MergeOnly || res.MergeCommits != 1 || res.Commits != 3 || res.Status != "ahead" || res.Schema != compareSchema || !res.unchangedDiff() {
		t.Fatalf("a merge that brings in two base commits is merge-only: %+v err=%v", res, err)
	}
	twoMerges := payload("ahead", "h1", commit("m1", "m0"), commit("M1", "h1", "m1"), commit("m2", "m1"), commit("M2", "M1", "m2"))
	if res, _ := parseCompare(twoMerges); !res.MergeOnly || res.MergeCommits != 2 {
		t.Fatalf("two merges in a row are still merge-only: %+v", res)
	}
	mergeThenFix := payload("ahead", "h1", commit("m1", "m0"), commit("M", "h1", "m1"), commit("c2", "M"))
	if res, _ := parseCompare(mergeThenFix); res.MergeOnly || res.unchangedDiff() || res.MergeCommits != 1 || res.Commits != 3 {
		t.Fatalf("a merge followed by a real commit is a real push: %+v", res)
	}
	plain := payload("ahead", "h1", commit("c2", "h1"))
	if res, _ := parseCompare(plain); res.MergeOnly || res.MergeCommits != 0 {
		t.Fatalf("one plain commit: %+v", res)
	}
	rebased := payload("diverged", "h1", commit("r1", "fork"), commit("r2", "r1"))
	if res, _ := parseCompare(rebased); res.MergeOnly || res.unchangedDiff() {
		t.Fatalf("a rebase never reaches the previous head and reads as a real push: %+v", res)
	}
	mergedTheOtherWay := payload("ahead", "h1", commit("m1", "m0"), commit("M", "m1", "h1"))
	if res, _ := parseCompare(mergedTheOtherWay); res.MergeOnly {
		t.Fatalf("a chain that leaves the list before base cannot be judged: %+v", res)
	}
	identical := payload("identical", "h1")
	if res, _ := parseCompare(identical); res.MergeOnly || res.MergeCommits != 0 || !res.unchangedDiff() {
		t.Fatalf("the same tree is unchanged without being merge-only: %+v", res)
	}
	truncated := []byte(`{"status":"ahead","base_commit":{"sha":"h1"},"total_commits":300,"commits":[` + commit("M", "h1", "m1") + `],"files":[]}`)
	if res, _ := parseCompare(truncated); res.MergeOnly || res.MergeCommits != 0 || res.Commits != 300 {
		t.Fatalf("a commit list the endpoint cut short cannot be judged: %+v", res)
	}
	legacy := []byte(`{"files":[{"filename":"x.go"}]}`)
	if res, _ := parseCompare(legacy); res.Status != "unknown" || res.unchangedDiff() || res.Schema != compareSchema {
		t.Fatalf("a response without a status is unknown, never unchanged: %+v", res)
	}
}

func TestStore_PushRefetchesEntriesWithoutTheChainWhenOnline(t *testing.T) {
	dir := copyFixtures(t)
	stale := filepath.Join(dir, "compare", "acme_example_aaaaaaa_bbbbbbb.json")
	if err := os.WriteFile(stale, []byte(`{"files":["a.go"]}`), 0o644); err != nil {
		t.Fatal(err)
	}
	f := &stubFetcher{compare: compareResult{Files: []string{"a.go"}, Status: "ahead", MergeOnly: true, Schema: compareSchema}}
	st, err := newStore(dir, f)
	if err != nil {
		t.Fatal(err)
	}
	if res, known, err := st.compare("acme", "example", "aaaaaaa", "bbbbbbb"); err != nil || !known || res.MergeOnly || f.compares != 0 {
		t.Fatalf("a file lookup takes the old entry as it is: res=%+v known=%v err=%v fetches=%d", res, known, err, f.compares)
	}
	res, known, err := st.push("acme", "example", "aaaaaaa", "bbbbbbb")
	if err != nil || !known || !res.MergeOnly || f.compares != 1 {
		t.Fatalf("a push lookup fetches an entry without the chain again: res=%+v known=%v err=%v fetches=%d", res, known, err, f.compares)
	}
	if _, _, err := st.push("acme", "example", "aaaaaaa", "bbbbbbb"); err != nil || f.compares != 1 {
		t.Fatalf("the refreshed entry is served from the cache: fetches=%d", f.compares)
	}
	offline, _ := newStore(dir, nil)
	if err := os.WriteFile(stale, []byte(`{"files":["a.go"]}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if res, known, _ := offline.push("acme", "example", "aaaaaaa", "bbbbbbb"); !known || res.unchangedDiff() {
		t.Fatalf("offline the old entry still answers, as a real push: %+v known=%v", res, known)
	}
}

// stubFetcher answers every compare with one canned result and counts the
// calls; sidecars are never asked for.
type stubFetcher struct {
	compare  compareResult
	compares int
}

func (s *stubFetcher) Sidecar(string, string, int, string) ([]byte, error) { return nil, errNotFound }
func (s *stubFetcher) Compare(string, string, string, string) (compareResult, error) {
	s.compares++
	return s.compare, nil
}

func TestExternalRoots_ResolutionState(t *testing.T) {
	yes, no := true, false
	d := &prDump{Number: 9, Owner: "acme", Repo: "example"}
	mk := func(id int64, login, body string, resolved *bool) {
		var th dumpThread
		th.Path, th.OriginalLine, th.IsResolved = "a.go", 10, resolved
		th.Comments.Nodes = []dumpThreadComment{{DatabaseID: flexInt64(id), Author: &dumpActor{Login: login}, Body: body, CreatedAt: time.Date(2026, 9, 1, 0, 0, int(id), 0, time.UTC)}}
		d.ReviewThreads.Nodes = append(d.ReviewThreads.Nodes, th)
	}
	mk(1, "greptile-apps[bot]", "resolved one", &yes)
	mk(2, "octocat", "open one", &no)
	mk(3, "octocat", "unknown one", nil)
	mk(4, "acme-bot", "<!-- prism:finding:a.go:1:abc --> own", nil)
	roots := d.externalRoots(map[string]bool{"acme-bot": true})
	if len(roots) != 3 {
		t.Fatalf("roots = %+v", roots)
	}
	if !roots[0].Resolved || !roots[0].ResolvedKnown || roots[1].Resolved || !roots[1].ResolvedKnown || roots[2].Resolved || roots[2].ResolvedKnown {
		t.Fatalf("resolution state = %+v", roots)
	}
}
