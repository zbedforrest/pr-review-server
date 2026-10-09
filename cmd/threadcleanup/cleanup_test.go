package main

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	gogithub "github.com/google/go-github/v57/github"

	"pr-review-server/db"
	"pr-review-server/github"
)

const (
	shaA = "aaaaaaa111111111111111111111111111111111"
	shaB = "bbbbbbb222222222222222222222222222222222"
	shaC = "ccccccc333333333333333333333333333333333"
)

func fixtureScan(t *testing.T, offline bool) ([]*pr, []decision) {
	t.Helper()
	cache := t.TempDir()
	if !offline {
		t.Fatal("fixture scans run offline; the cache under testdata is copied")
	}
	src, _ := filepath.Glob(filepath.Join("testdata", "cache", "compare", "*.json"))
	if len(src) == 0 {
		t.Fatal("no compare fixtures")
	}
	for _, s := range src {
		copyFile(t, s, filepath.Join(cache, "compare", filepath.Base(s)))
	}
	prs, decisions, err := scanDumps(filepath.Join("testdata", "dumps"), cache, true, nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	return prs, decisions
}

func TestScanDumps_FixtureVerdicts(t *testing.T) {
	prs, decisions := fixtureScan(t, true)
	if len(prs) != 4 {
		t.Fatalf("prs = %d, want 4", len(prs))
	}
	want := map[int64]verdict{
		101: stalePRMerged, 103: skipResolved,
		201: stalePRClosed,
		301: staleFileChanged, 302: skipUnchanged, 303: skipChangeUnknown, 304: skipUnchanged,
		401: skipUnknownState,
	}
	got := map[int64]verdict{}
	for _, d := range decisions {
		got[d.Thread.RootCommentID] = d.Verdict
	}
	if len(got) != len(want) {
		t.Fatalf("decisions = %v, want %v", got, want)
	}
	for id, v := range want {
		if got[id] != v {
			t.Errorf("comment %d: verdict %q, want %q", id, got[id], v)
		}
	}
	s := summarize(prs, decisions)
	if s.PRsMerged != 1 || s.PRsClosed != 1 || s.PRsOpen != 2 {
		t.Errorf("pr states = %d/%d/%d merged/closed/open", s.PRsMerged, s.PRsClosed, s.PRsOpen)
	}
	if s.PrismRoots != 8 || s.Stale != 3 || s.StalePRMerged != 1 || s.StalePRClosed != 1 || s.StaleFileChanged != 1 {
		t.Errorf("stale counts = %+v", s)
	}
	if s.AlreadyResolved != 1 || s.ResolutionUnknown != 1 || s.OpenUnchanged != 2 || s.ChangeUnknown != 1 {
		t.Errorf("skip counts = %+v", s)
	}
	if s.StaleOutdated != 2 || s.PRsWithStale != 3 {
		t.Errorf("outdated/prs_with_stale = %d/%d, want 2/3", s.StaleOutdated, s.PRsWithStale)
	}
	stale := staleOnly(decisions)
	if len(stale) != 3 || stale[0].Thread.RootCommentID != 101 || stale[1].Thread.RootCommentID != 201 || stale[2].Thread.RootCommentID != 301 {
		t.Errorf("stale order = %v", stale)
	}
	if stale[1].Thread.NodeID != "PRRT_2" || stale[0].Thread.NodeID != "" {
		t.Errorf("node ids: graphql %q, rest %q", stale[1].Thread.NodeID, stale[0].Thread.NodeID)
	}
}

func TestScanDumps_ExplicitBotAndNoCache(t *testing.T) {
	prs, decisions, err := scanDumps(filepath.Join("testdata", "dumps"), "", true, map[string]bool{"nobody": true}, 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(prs) != 1 || len(decisions) != 0 {
		t.Fatalf("prs = %d, decisions = %d; an unknown bot owns no thread", len(prs), len(decisions))
	}
	_, decisions, err = scanDumps(filepath.Join("testdata", "dumps"), "", true, nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, d := range decisions {
		if d.Thread.RootCommentID == 301 && d.Verdict != skipChangeUnknown {
			t.Errorf("without a cache the compare is unknown, got %q", d.Verdict)
		}
	}
}

func TestClassifyThread(t *testing.T) {
	open := &pr{Owner: "acme", Repo: "example", Number: 9, State: prOpen, HeadSHA: shaC}
	changed := func(_, _, base, head string) ([]string, bool) {
		if base == shaA {
			return []string{"svc/retry.go"}, true
		}
		return nil, false
	}
	known := thread{Path: "svc/retry.go", PostedSHA: shaA, ResolvedKnown: true}
	cases := []struct {
		name string
		pr   *pr
		t    thread
		want verdict
	}{
		{"merged beats everything but resolved", &pr{State: prMerged}, known, stalePRMerged},
		{"closed", &pr{State: prClosed}, known, stalePRClosed},
		{"resolved stays", &pr{State: prMerged}, thread{Resolved: true, ResolvedKnown: true}, skipResolved},
		{"unknown resolution stays", &pr{State: prMerged}, thread{}, skipUnknownState},
		{"open, file changed", open, known, staleFileChanged},
		{"open, other file changed", open, thread{Path: "app/cache.go", PostedSHA: shaA, ResolvedKnown: true}, skipUnchanged},
		{"open, posted at head", open, thread{Path: "svc/retry.go", PostedSHA: shaC, ResolvedKnown: true}, skipUnchanged},
		{"open, compare unknown", open, thread{Path: "svc/retry.go", PostedSHA: shaB, ResolvedKnown: true}, skipChangeUnknown},
		{"open, no posted sha", open, thread{Path: "svc/retry.go", ResolvedKnown: true}, skipChangeUnknown},
	}
	for _, c := range cases {
		if got := classifyThread(c.pr, c.t, changed); got != c.want {
			t.Errorf("%s: %q, want %q", c.name, got, c.want)
		}
	}
}

type fakeSource struct {
	pr       *gogithub.PullRequest
	threads  []github.ReviewThreadInfo
	comments []github.ReviewCommentInfo
}

func (f fakeSource) GetPR(context.Context, string, string, int) (*gogithub.PullRequest, *gogithub.Response, error) {
	return f.pr, nil, nil
}
func (f fakeSource) ListReviewThreads(context.Context, string, string, int) ([]github.ReviewThreadInfo, error) {
	return f.threads, nil
}
func (f fakeSource) ListReviewComments(context.Context, string, string, int) ([]github.ReviewCommentInfo, error) {
	return f.comments, nil
}

func TestFromGitHub(t *testing.T) {
	src := fakeSource{
		pr: &gogithub.PullRequest{State: gogithub.String("closed"), Merged: gogithub.Bool(true),
			Head: &gogithub.PullRequestBranch{SHA: gogithub.String(shaC)}, User: &gogithub.User{Login: gogithub.String("dev")}},
		threads: []github.ReviewThreadInfo{
			{NodeID: "PRRT_a", RootCommentID: 11, Resolved: false, Outdated: true},
			{NodeID: "PRRT_b", RootCommentID: 12, Resolved: true},
		},
		comments: []github.ReviewCommentInfo{
			{ID: 11, Author: "acme-bot[bot]", Body: "<!-- prism:finding:svc/a.go:1:111111111111 -->\nx", Path: "svc/a.go", OriginalCommitID: shaA},
			{ID: 12, Author: "acme-bot[bot]", Body: "<!-- prism:finding:svc/b.go:1:222222222222 -->\ny", Path: "svc/b.go", OriginalCommitID: shaA},
			{ID: 13, Author: "acme-bot[bot]", Body: "<!-- prism:finding:svc/c.go:1:333333333333 -->\nz", Path: "svc/c.go", OriginalCommitID: shaB},
			{ID: 14, Author: "acme-bot[bot]", Body: "reply", Path: "svc/a.go", InReplyToID: 11},
			{ID: 15, Author: "other-bot[bot]", Body: "theirs", Path: "svc/a.go"},
		},
	}
	p, err := fromGitHub(context.Background(), src, "acme", "example", 7, nil)
	if err != nil {
		t.Fatal(err)
	}
	if p.State != prMerged || p.HeadSHA != shaC || p.Author != "dev" {
		t.Errorf("pr = %+v", p)
	}
	if len(p.Threads) != 3 {
		t.Fatalf("threads = %d, want 3", len(p.Threads))
	}
	a, b, c := p.Threads[0], p.Threads[1], p.Threads[2]
	if a.NodeID != "PRRT_a" || a.Resolved || !a.ResolvedKnown || !a.Outdated || a.PostedSHA != shaA || a.FindingID != "svc/a.go:1:111111111111" {
		t.Errorf("thread a = %+v", a)
	}
	if !b.Resolved || b.NodeID != "PRRT_b" {
		t.Errorf("thread b = %+v", b)
	}
	if c.ResolvedKnown || c.NodeID != "" {
		t.Errorf("a root the thread listing lacks is unknown, got %+v", c)
	}
	got := classify(p, func(string, string, string, string) ([]string, bool) { return nil, false })
	if got[0].Verdict != stalePRMerged || got[1].Verdict != skipResolved || got[2].Verdict != skipUnknownState {
		t.Errorf("verdicts = %v %v %v", got[0].Verdict, got[1].Verdict, got[2].Verdict)
	}
}

type fakeCloser struct {
	calls      []string
	failNote   bool
	failNodeID string
}

func (f *fakeCloser) ResolveThread(_ context.Context, _, _, nodeID string) error {
	f.calls = append(f.calls, "resolve "+nodeID)
	if nodeID == f.failNodeID {
		return errors.New("forbidden")
	}
	return nil
}

func (f *fakeCloser) CreateReviewCommentReply(_ context.Context, _, _ string, _ int, commentID int64, body string) (int64, error) {
	f.calls = append(f.calls, "note "+body)
	if f.failNote {
		return 0, errors.New("boom")
	}
	return commentID + 1000, nil
}

type fakeLedger struct {
	rows     []db.PublishedFinding
	upserted []db.PublishedFinding
}

func (l *fakeLedger) UpsertPublishedFinding(row *db.PublishedFinding) error {
	l.upserted = append(l.upserted, *row)
	return nil
}

func (l *fakeLedger) GetPublishedFindingsForPR(string, string, int) ([]db.PublishedFinding, error) {
	return l.rows, nil
}

func TestApply(t *testing.T) {
	merged := &pr{Owner: "acme", Repo: "example", Number: 1, Author: "dev", State: prMerged, HeadSHA: shaC}
	open := &pr{Owner: "acme", Repo: "example", Number: 3, Author: "dev", State: prOpen, HeadSHA: shaC}
	quiet := &pr{Owner: "acme", Repo: "example", Number: 5, Author: "optout", State: prMerged, HeadSHA: shaC}
	stale := []decision{
		{PR: merged, Thread: thread{RootCommentID: 101, NodeID: "PRRT_1", FindingID: "svc/retry.go:1:a1a1a1a1a1a1"}, Verdict: stalePRMerged},
		{PR: merged, Thread: thread{RootCommentID: 102, FindingID: "x"}, Verdict: stalePRMerged},
		{PR: open, Thread: thread{RootCommentID: 301, NodeID: "PRRT_3", Path: "svc/retry.go", FindingID: "svc/retry.go:1:e5e5e5e5e5e5"}, Verdict: staleFileChanged},
		{PR: open, Thread: thread{RootCommentID: 302, NodeID: "PRRT_bad", Path: "app/cache.go", FindingID: "app/cache.go:2:f6f6f6f6f6f6"}, Verdict: staleFileChanged},
		{PR: quiet, Thread: thread{RootCommentID: 501, NodeID: "PRRT_5"}, Verdict: stalePRMerged},
	}
	gh := &fakeCloser{failNodeID: "PRRT_bad"}
	ledger := &fakeLedger{rows: []db.PublishedFinding{
		{Kind: db.PublishedKindFinding, Fingerprint: "svc/retry.go:1:a1a1a1a1a1a1", CommentID: 101, State: db.PublishedStateOpen},
		{Kind: db.PublishedKindFinding, Fingerprint: "svc/retry.go:1:e5e5e5e5e5e5", State: db.PublishedStateOpen},
		{Kind: db.PublishedKindFinding, Fingerprint: "app/cache.go:2:f6f6f6f6f6f6", CommentID: 302, State: db.PublishedStateOpen},
		{Kind: db.PublishedKindSummary, Fingerprint: "summary", State: db.PublishedStateOpen},
	}}
	allowed := func(author string) bool { return author != "optout" }
	var logged []string
	a := apply(context.Background(), gh, ledger, allowed, stale, func(f string, args ...any) { logged = append(logged, f) })
	want := applied{Resolved: 2, Notes: 3, ResolveFailures: 2, GateDenied: 1, LedgerRows: 2}
	if a != want {
		t.Errorf("applied = %+v, want %+v", a, want)
	}
	wantCalls := []string{
		"note Resolving: the PR was merged.", "resolve PRRT_1",
		"note Resolving: svc/retry.go changed after this comment (now at ccccccc); reopen it if the point still stands.", "resolve PRRT_3",
		"note Resolving: app/cache.go changed after this comment (now at ccccccc); reopen it if the point still stands.", "resolve PRRT_bad",
	}
	if strings.Join(gh.calls, "|") != strings.Join(wantCalls, "|") {
		t.Errorf("calls:\n%s\nwant:\n%s", strings.Join(gh.calls, "\n"), strings.Join(wantCalls, "\n"))
	}
	if len(ledger.upserted) != 2 {
		t.Fatalf("ledger writes = %d, want 2", len(ledger.upserted))
	}
	for _, row := range ledger.upserted {
		if row.State != db.PublishedStateFixed || row.ThreadNodeID == "" {
			t.Errorf("row %s = state %q node %q", row.Fingerprint, row.State, row.ThreadNodeID)
		}
	}
	if ledger.upserted[1].Fingerprint != "svc/retry.go:1:e5e5e5e5e5e5" {
		t.Errorf("a row without a comment id is matched by fingerprint, got %s", ledger.upserted[1].Fingerprint)
	}
	if len(logged) != 2 {
		t.Errorf("logged %d lines, want the missing node id and the lost resolve", len(logged))
	}
}

func TestApply_LostNoteStillResolves(t *testing.T) {
	p := &pr{Owner: "acme", Repo: "example", Number: 2, Author: "dev", State: prClosed}
	gh := &fakeCloser{failNote: true}
	a := apply(context.Background(), gh, nil, func(string) bool { return true },
		[]decision{{PR: p, Thread: thread{RootCommentID: 201, NodeID: "PRRT_2"}, Verdict: stalePRClosed}}, func(string, ...any) {})
	if a.Resolved != 1 || a.NoteFailures != 1 || a.Notes != 0 {
		t.Errorf("applied = %+v", a)
	}
	if len(gh.calls) != 2 || gh.calls[0] != "note Resolving: the PR was closed without merging." {
		t.Errorf("calls = %v", gh.calls)
	}
}

func TestParsePRRef(t *testing.T) {
	owner, repo, n, err := parsePRRef("acme/example#12")
	if err != nil || owner != "acme" || repo != "example" || n != 12 {
		t.Errorf("got %s %s %d %v", owner, repo, n, err)
	}
	for _, bad := range []string{"example#12", "acme/example", "acme/example#x", "acme#1/example", "acme/example#0"} {
		if _, _, _, err := parsePRRef(bad); err == nil {
			t.Errorf("%q parsed", bad)
		}
	}
}

func TestWriteListing(t *testing.T) {
	p := &pr{Owner: "acme", Repo: "example", Number: 1, State: prMerged, HeadSHA: shaC}
	path := filepath.Join(t.TempDir(), "stale.csv")
	at := time.Date(2026, 9, 1, 10, 0, 0, 0, time.UTC)
	err := writeListing(path, []decision{{PR: p, Thread: thread{RootCommentID: 101, Path: "svc/retry.go", PostedSHA: shaA, PostedAt: at, Outdated: true}, Verdict: stalePRMerged}})
	if err != nil {
		t.Fatal(err)
	}
	got := readFile(t, path)
	want := "pr,verdict,comment_id,path,posted_sha,head_sha,outdated,posted_at,url\nacme/example#1,pr_merged,101,svc/retry.go,aaaaaaa,ccccccc,true,2026-09-01T10:00:00Z,https://github.com/acme/example/pull/1#discussion_r101\n"
	if got != want {
		t.Errorf("listing:\n%s\nwant:\n%s", got, want)
	}
}
