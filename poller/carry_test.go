package poller

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"pr-review-server/gcs"
	"pr-review-server/github"
	"pr-review-server/pkg/reviewer/payload"
	"pr-review-server/pkg/reviewer/service"
	"pr-review-server/pkg/reviewer/types"
)

// capturingSidecarStorage is a minimal ReviewStorage that records sidecar
// bytes so tests can assert on the exact persisted payload.
type capturingSidecarStorage struct {
	sidecars map[string][]byte
}

func (s *capturingSidecarStorage) ReviewExists(ctx context.Context, owner, repo string, prNumber int, commitSHA string) (bool, error) {
	return false, nil
}

func (s *capturingSidecarStorage) SaveReview(ctx context.Context, owner, repo string, prNumber int, commitSHA string, content []byte) (string, error) {
	return gcs.ReviewFileName(owner, repo, prNumber, commitSHA), nil
}

func (s *capturingSidecarStorage) SaveReviewSidecar(ctx context.Context, filename, contentType string, content []byte) error {
	if s.sidecars == nil {
		s.sidecars = make(map[string][]byte)
	}
	s.sidecars[filename] = append([]byte(nil), content...)
	return nil
}

// writePriorSidecar builds a real findings sidecar for (owner, repo, pr) at
// fromSHA and writes it into dir under the production per-SHA naming.
func writePriorSidecar(t *testing.T, dir, owner, repo string, prNumber int, fromSHA string, comments []types.LineComment) string {
	t.Helper()
	pl := payload.Build(owner, repo, prNumber, fromSHA, comments, "", nil)
	body, err := json.Marshal(pl)
	if err != nil {
		t.Fatalf("marshal prior sidecar: %v", err)
	}
	name := gcs.ReviewJSONFileName(gcs.ReviewFileName(owner, repo, prNumber, fromSHA))
	if err := os.WriteFile(filepath.Join(dir, name), body, 0o644); err != nil {
		t.Fatalf("write prior sidecar: %v", err)
	}
	return name
}

func carryTestPoller(t *testing.T) *Poller {
	t.Helper()
	return &Poller{
		cfg:           testConfig(),
		reviewDir:     t.TempDir(),
		activeReviews: make(map[string]ProcessInfo),
	}
}

// With CARRY_FORWARD_FINDINGS unset/off, carryForwardSet must be a strict
// no-op: no storage reads, no compare calls, zero-valued outputs.
func TestCarryForwardSet_EnvOffIsNoop(t *testing.T) {
	t.Setenv("CARRY_FORWARD_FINDINGS", "")
	p := carryTestPoller(t)
	p.compareFilesFn = func(ctx context.Context, owner, repo, base, head, token string) ([]string, bool) {
		t.Fatal("compareFilesFn must not be called when the feature is off")
		return nil, false
	}
	// A prior sidecar exists — and must not even be read.
	writePriorSidecar(t, p.reviewDir, "acme", "widgets", 7, "aaaaaaa1111111111111", []types.LineComment{
		{FilePath: "src/kept.py", LineNumber: 10, Importance: "CRITICAL", CommentBody: "prior finding"},
	})

	set, info := p.carryForwardSet(context.Background(), github.PullRequest{
		Owner: "acme", Repo: "widgets", Number: 7, CommitSHA: "bbbbbbb2222222222222",
	}, "tok")
	if len(set.Comments) != 0 || set.Provenance != "" {
		t.Errorf("env off must return a zero FindingSet, got %+v", set)
	}
	if info != nil {
		t.Errorf("env off must return nil telemetry, got %+v", info)
	}
}

// The core carry path: a prior finding on an untouched file is re-admitted
// (with the source SHA in the set's provenance label), a prior finding on a
// file the new push modified is dropped, and the SUMMARY never carries.
func TestCarryForwardSet_UntouchedCarriesTouchedDrops(t *testing.T) {
	t.Setenv("CARRY_FORWARD_FINDINGS", "true")
	p := carryTestPoller(t)
	const fromSHA = "aaaaaaa1111111111111"
	writePriorSidecar(t, p.reviewDir, "acme", "widgets", 7, fromSHA, []types.LineComment{
		{FilePath: "SUMMARY", LineNumber: 0, CommentBody: "old verdict"},
		{FilePath: "src/kept.py", LineNumber: 10, Importance: "CRITICAL", CommentBody: "unhandled nil"},
		{FilePath: "src/changed.py", LineNumber: 5, Importance: "MEDIUM", CommentBody: "stale by now"},
	})
	var comparedBase, comparedHead string
	p.compareFilesFn = func(ctx context.Context, owner, repo, base, head, token string) ([]string, bool) {
		comparedBase, comparedHead = base, head
		return []string{"src/changed.py"}, true
	}

	pr := github.PullRequest{Owner: "acme", Repo: "widgets", Number: 7, CommitSHA: "bbbbbbb2222222222222"}
	set, info := p.carryForwardSet(context.Background(), pr, "tok")

	if comparedBase != fromSHA || comparedHead != pr.CommitSHA {
		t.Errorf("compared %s..%s, want %s..%s", comparedBase, comparedHead, fromSHA, pr.CommitSHA)
	}
	if info == nil {
		t.Fatal("want telemetry, got nil")
	}
	if info.FromSHA != fromSHA || info.CarriedIn != 1 || info.CarriedDropped != 1 {
		t.Errorf("telemetry = %+v, want from=%s carried_in=1 carried_dropped=1", info, fromSHA)
	}
	if set.Provenance != service.CarriedProvenance(fromSHA) {
		t.Errorf("set provenance = %q, want %q", set.Provenance, service.CarriedProvenance(fromSHA))
	}
	if len(set.Comments) != 1 || set.Comments[0].FilePath != "src/kept.py" {
		t.Fatalf("carried set = %+v, want exactly src/kept.py", set.Comments)
	}

	// End-to-end through the merge: a carried finding duplicating one of this
	// run's findings collapses into the agent's phrasing; the untouched-file
	// carry above would otherwise survive capped at MEDIUM.
	merged := service.MergeFindings(
		service.FindingSet{Provenance: "agent", Comments: []types.LineComment{
			{FilePath: "src/kept.py", LineNumber: 12, Importance: "LOW", CommentBody: "agent re-found it"},
		}},
		set,
	)
	if len(merged) != 1 || merged[0].CommentBody != "agent re-found it" {
		t.Fatalf("carried duplicate should collapse into agent finding: %+v", merged)
	}
}

// An unreliable inter-push comparison (force-push divergence, truncated file
// list, API error) must carry nothing — but still record the attempt.
func TestCarryForwardSet_UnreliableCompareCarriesNothing(t *testing.T) {
	t.Setenv("CARRY_FORWARD_FINDINGS", "true")
	p := carryTestPoller(t)
	const fromSHA = "aaaaaaa1111111111111"
	writePriorSidecar(t, p.reviewDir, "acme", "widgets", 7, fromSHA, []types.LineComment{
		{FilePath: "SUMMARY", LineNumber: 0, CommentBody: "old verdict"},
		{FilePath: "src/kept.py", LineNumber: 10, Importance: "CRITICAL", CommentBody: "unhandled nil"},
		{FilePath: "src/other.py", LineNumber: 3, Importance: "LOW", CommentBody: "nit"},
	})
	p.compareFilesFn = func(ctx context.Context, owner, repo, base, head, token string) ([]string, bool) {
		return nil, false
	}

	set, info := p.carryForwardSet(context.Background(), github.PullRequest{
		Owner: "acme", Repo: "widgets", Number: 7, CommitSHA: "bbbbbbb2222222222222",
	}, "tok")
	if len(set.Comments) != 0 {
		t.Errorf("unreliable compare must carry nothing, got %+v", set.Comments)
	}
	if info == nil || info.CarriedIn != 0 || info.CarriedDropped != 2 {
		t.Errorf("telemetry = %+v, want carried_in=0 carried_dropped=2 (SUMMARY never counts)", info)
	}
}

// No prior review (first push) is not an error — it is simply a carry-less run.
func TestCarryForwardSet_NoPriorReview(t *testing.T) {
	t.Setenv("CARRY_FORWARD_FINDINGS", "true")
	p := carryTestPoller(t)
	p.compareFilesFn = func(ctx context.Context, owner, repo, base, head, token string) ([]string, bool) {
		t.Fatal("compareFilesFn must not be called without a prior review")
		return nil, false
	}
	set, info := p.carryForwardSet(context.Background(), github.PullRequest{
		Owner: "acme", Repo: "widgets", Number: 7, CommitSHA: "bbbbbbb2222222222222",
	}, "tok")
	if len(set.Comments) != 0 || info != nil {
		t.Errorf("first review must be carry-less, got set=%+v info=%+v", set, info)
	}
}

// loadPriorReviewPayload must pick the most recent review at a DIFFERENT
// commit — never the current commit's own (possibly force-regenerated) review.
func TestLoadPriorReviewPayload_PicksLatestOtherSHA(t *testing.T) {
	p := carryTestPoller(t)
	currentSHA := "ccccccc3333333333333"
	older := "aaaaaaa1111111111111"
	newer := "bbbbbbb2222222222222"

	oldName := writePriorSidecar(t, p.reviewDir, "acme", "widgets", 7, older, []types.LineComment{
		{FilePath: "old.py", LineNumber: 1, Importance: "LOW", CommentBody: "from older review"},
	})
	newName := writePriorSidecar(t, p.reviewDir, "acme", "widgets", 7, newer, []types.LineComment{
		{FilePath: "new.py", LineNumber: 1, Importance: "LOW", CommentBody: "from newer review"},
	})
	// The current SHA's sidecar exists too (force re-review) and must be skipped.
	writePriorSidecar(t, p.reviewDir, "acme", "widgets", 7, currentSHA, []types.LineComment{
		{FilePath: "self.py", LineNumber: 1, Importance: "LOW", CommentBody: "current review"},
	})
	// Deterministic mtimes regardless of write order.
	base := time.Now().Add(-time.Hour)
	if err := os.Chtimes(filepath.Join(p.reviewDir, oldName), base, base); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(filepath.Join(p.reviewDir, newName), base.Add(time.Minute), base.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}

	pl, err := p.loadPriorReviewPayload(context.Background(), "acme", "widgets", 7, currentSHA)
	if err != nil {
		t.Fatalf("loadPriorReviewPayload: %v", err)
	}
	if pl == nil {
		t.Fatal("want a prior payload, got nil")
	}
	if pl.CommitSHA != newer {
		t.Errorf("picked prior review of %s, want %s (latest other-SHA)", pl.CommitSHA, newer)
	}

	// Unrelated PR: nothing to load.
	pl, err = p.loadPriorReviewPayload(context.Background(), "acme", "widgets", 8, currentSHA)
	if err != nil || pl != nil {
		t.Errorf("unrelated PR should yield (nil, nil), got (%+v, %v)", pl, err)
	}
}

// With the feature off, the persisted sidecar must be byte-identical to a
// payload built without any carry-forward plumbing.
func TestWriteSidecar_EnvOffByteIdentical(t *testing.T) {
	t.Setenv("CARRY_FORWARD_FINDINGS", "")
	storage := &capturingSidecarStorage{}
	p := carryTestPoller(t)
	p.storage = storage

	comments := []types.LineComment{
		{FilePath: "SUMMARY", LineNumber: 0, CommentBody: "verdict"},
		{FilePath: "a.go", LineNumber: 10, Importance: "CRITICAL", CommentBody: "crash"},
	}
	rr := &ReviewResult{Comments: comments, Diff: "", FileContents: nil}
	p.writeSidecarBestEffort(context.Background(), "acme", "widgets", 7, "ccccccc3333333333333", "acme_widgets_7_ccccccc.html", rr)

	got, ok := storage.sidecars["acme_widgets_7_ccccccc.json"]
	if !ok {
		t.Fatalf("sidecar not saved; saved keys: %v", storage.sidecars)
	}
	want, err := json.Marshal(payload.Build("acme", "widgets", 7, "ccccccc3333333333333", comments, "", nil))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(want) {
		t.Errorf("env-off sidecar differs from baseline payload:\n got: %s\nwant: %s", got, want)
	}
	if strings.Contains(string(got), "carried") {
		t.Errorf("env-off sidecar must not mention carry-forward: %s", got)
	}

	// With telemetry present, the sidecar gains exactly the carried_findings
	// block.
	rr.Carried = &payload.CarryForwardInfo{FromSHA: "aaaaaaa1111111111111", CarriedIn: 1, CarriedDropped: 2}
	p.writeSidecarBestEffort(context.Background(), "acme", "widgets", 7, "ccccccc3333333333333", "acme_widgets_7_ccccccc.html", rr)
	got = storage.sidecars["acme_widgets_7_ccccccc.json"]
	for _, wantFrag := range []string{`"carried_findings"`, `"from_sha":"aaaaaaa1111111111111"`, `"carried_in":1`, `"carried_dropped":2`} {
		if !strings.Contains(string(got), wantFrag) {
			t.Errorf("sidecar missing %s: %s", wantFrag, got)
		}
	}
}

func TestWriteSidecar_WritesImmutableRunAndLatestAlias(t *testing.T) {
	storage := &capturingSidecarStorage{}
	p := carryTestPoller(t)
	p.storage = storage
	const (
		sha   = "ccccccc3333333333333"
		runID = "run-0123456789abcdef0123456789abcdef"
	)
	run := &payload.ReviewRunInfo{
		RunID:    runID,
		HTMLPath: gcs.ReviewRunFileName("acme", "widgets", 7, sha, runID),
		JSONPath: gcs.ReviewRunJSONFileName("acme", "widgets", 7, sha, runID),
	}
	rr := &ReviewResult{
		ReviewRun: run,
		Comments: []types.LineComment{{
			FilePath: "a.go", LineNumber: 10, Importance: "MEDIUM", CommentBody: "check this",
		}},
	}
	htmlName := gcs.ReviewFileName("acme", "widgets", 7, sha)
	latest := gcs.ReviewJSONFileName(htmlName)

	p.writeSidecarBestEffort(context.Background(), "acme", "widgets", 7, sha, htmlName, rr)

	immutableBody, immutableOK := storage.sidecars[run.JSONPath]
	latestBody, latestOK := storage.sidecars[latest]
	if !immutableOK || !latestOK {
		t.Fatalf("sidecar keys = %v, want immutable %q and latest %q", storage.sidecars, run.JSONPath, latest)
	}
	if string(immutableBody) != string(latestBody) {
		t.Fatalf("immutable and latest sidecars differ:\nimmutable=%s\nlatest=%s", immutableBody, latestBody)
	}
}

// githubCompareChangedFiles: "ahead" comparisons yield the changed-file list
// (rename old paths included); diverged/truncated/error responses are
// unreliable and must report ok=false.
func TestGithubCompareChangedFiles(t *testing.T) {
	type file struct {
		Filename         string `json:"filename"`
		PreviousFilename string `json:"previous_filename,omitempty"`
	}
	var status string
	var files []file
	var httpStatus int
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if want := "/repos/acme/widgets/compare/aaa...bbb"; r.URL.Path != want {
			t.Errorf("path = %q, want %q", r.URL.Path, want)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer tok" {
			t.Errorf("auth header = %q", got)
		}
		if httpStatus != 0 {
			w.WriteHeader(httpStatus)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]interface{}{"status": status, "files": files})
	}))
	defer ts.Close()
	origBase := githubAPIBaseURL
	githubAPIBaseURL = ts.URL
	defer func() { githubAPIBaseURL = origBase }()

	call := func() ([]string, bool) {
		return githubCompareChangedFiles(context.Background(), "acme", "widgets", "aaa", "bbb", "tok")
	}

	status, files, httpStatus = "ahead", []file{{Filename: "a.go"}, {Filename: "new/name.go", PreviousFilename: "old/name.go"}}, 0
	got, ok := call()
	if !ok {
		t.Fatal("ahead comparison should be reliable")
	}
	want := []string{"a.go", "new/name.go", "old/name.go"}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("files = %v, want %v", got, want)
	}

	status, files = "identical", nil
	if got, ok := call(); !ok || len(got) != 0 {
		t.Errorf("identical comparison should be reliable and empty, got (%v, %t)", got, ok)
	}

	for _, s := range []string{"diverged", "behind"} {
		status = s
		if _, ok := call(); ok {
			t.Errorf("status %q must be unreliable", s)
		}
	}

	status = "ahead"
	files = make([]file, 300)
	for i := range files {
		files[i] = file{Filename: fmt.Sprintf("f%d.go", i)}
	}
	if _, ok := call(); ok {
		t.Error("300-file (truncated) list must be unreliable")
	}

	files, httpStatus = nil, http.StatusNotFound
	if _, ok := call(); ok {
		t.Error("HTTP error must be unreliable")
	}
}
