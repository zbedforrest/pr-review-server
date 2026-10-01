package main

import (
	"bytes"
	"context"
	"strings"
	"testing"

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
	if len(lines) != 3 || !strings.HasPrefix(lines[0], "pr,rounds,") || !strings.HasPrefix(lines[1], "example#1,3,0,0,2,1,0,1,1,0,0,") {
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
	if !sameDefect("api/users.go", 140, a, nil, "src/api/users.go", 145, a, nil) {
		t.Fatal("same text, path suffix and nearby line must match")
	}
	if sameDefect("api/users.go", 140, a, []string{"fetchuser"}, "api/users.go", 200, a, []string{"fetchuser"}) {
		t.Fatal("lines more than ten apart must not match")
	}
	if sameDefect("api/users.go", 140, a, nil, "api/other.go", 140, a, nil) {
		t.Fatal("different files must not match")
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
	if got := filterDumps(dumps, "acme/example#2", 0); len(got) != 1 || got[0].Number != 2 {
		t.Fatalf("owner-qualified filter = %d dumps", len(got))
	}
	if got := filterDumps(dumps, "example#1", 0); len(got) != 1 || got[0].Number != 1 {
		t.Fatalf("bare filter = %d dumps", len(got))
	}
	if got := filterDumps(dumps, "", 1); len(got) != 1 {
		t.Fatalf("limit = %d dumps", len(got))
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
