package github

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestBuildCIStatusQuery_MergeFieldsOnlyForPRsThatAskForThem(t *testing.T) {
	query := buildCIStatusQuery([]PRInfo{
		{Owner: "acme", Repo: "example", Number: 1, IncludeMergeState: true},
		{Owner: "acme", Repo: "example", Number: 2},
	})
	pr0 := query[strings.Index(query, "pr0:"):strings.Index(query, "pr1:")]
	pr1 := query[strings.Index(query, "pr1:"):]
	for _, field := range []string{"mergeStateStatus", "reviewDecision"} {
		if !strings.Contains(pr0, field) {
			t.Errorf("open PR alias lacks %s:\n%s", field, pr0)
		}
		if strings.Contains(pr1, field) {
			t.Errorf("checks-only PR alias requests %s:\n%s", field, pr1)
		}
	}
	if strings.Count(query, "statusCheckRollup") != 2 {
		t.Errorf("both aliases must still request the rollup:\n%s", query)
	}
}

type ciRequestRecorder struct {
	mu      sync.Mutex
	bodies  []string
	aliasRe *regexp.Regexp
}

func (r *ciRequestRecorder) record(body string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.bodies = append(r.bodies, body)
}

func (r *ciRequestRecorder) aliases(body string) int {
	return len(r.aliasRe.FindAllString(body, -1))
}

func ciPRs(n int, includeMergeState bool) []PRInfo {
	repo := "closed-example"
	if includeMergeState {
		repo = "example"
	}
	prs := make([]PRInfo, 0, n)
	for i := 0; i < n; i++ {
		prs = append(prs, PRInfo{Owner: "acme", Repo: repo, Number: i + 1, IncludeMergeState: includeMergeState})
	}
	return prs
}

func TestBatchGetCIStatus_MergeStateBatchesAreSmallerAndSeparate(t *testing.T) {
	rec := &ciRequestRecorder{aliasRe: regexp.MustCompile(`pr\d+: repository`)}
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		rec.record(string(body))
		_, _ = w.Write([]byte(`{"data":{}}`))
	}))
	defer ts.Close()
	client := NewClient("test-token", "")
	client.httpClient = &http.Client{Transport: &redirectTransport{targetURL: ts.URL}}

	var buf bytes.Buffer
	log.SetOutput(&buf)
	defer log.SetOutput(os.Stderr)

	prs := append(ciPRs(30, true), ciPRs(60, false)...)
	results, err := client.BatchGetCIStatus(context.Background(), prs)
	if err != nil {
		t.Fatalf("BatchGetCIStatus: %v", err)
	}
	if len(results) != 90 {
		t.Fatalf("results = %d, want 90 (unknown state for PRs the response omitted)", len(results))
	}

	rec.mu.Lock()
	defer rec.mu.Unlock()
	mergeBatches, checksBatches := 0, 0
	for _, body := range rec.bodies {
		n := rec.aliases(body)
		if strings.Contains(body, "mergeStateStatus") {
			mergeBatches++
			if n > ciBatchSizeMergeState {
				t.Errorf("merge-state batch has %d PRs, cap is %d", n, ciBatchSizeMergeState)
			}
		} else {
			checksBatches++
			if n > ciBatchSizeChecksOnly {
				t.Errorf("checks-only batch has %d PRs, cap is %d", n, ciBatchSizeChecksOnly)
			}
		}
	}
	if mergeBatches != 2 || checksBatches != 2 {
		t.Errorf("batches merge=%d checks=%d, want 2 and 2", mergeBatches, checksBatches)
	}
	want := "[GRAPHQL] CI status cycle: prs=90 (merge-state=30 checks-only=60) batches=4 ok=4 failed=0 skipped=0 fetched=90/90"
	if !strings.Contains(buf.String(), want) {
		t.Errorf("summary line missing %q in:\n%s", want, buf.String())
	}
}

func TestBatchGetCIStatus_First403SkipsRemainingBatches(t *testing.T) {
	var mu sync.Mutex
	requests := 0
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		requests++
		mu.Unlock()
		w.WriteHeader(http.StatusForbidden)
	}))
	defer ts.Close()
	client := NewClient("test-token", "")
	client.httpClient = &http.Client{Transport: &redirectTransport{targetURL: ts.URL}}

	var buf bytes.Buffer
	log.SetOutput(&buf)
	defer log.SetOutput(os.Stderr)

	results, err := client.BatchGetCIStatus(context.Background(), ciPRs(200, true))
	if err != nil {
		t.Fatalf("BatchGetCIStatus: %v", err)
	}
	if len(results) != 0 {
		t.Errorf("results = %d, want none after 403s", len(results))
	}
	mu.Lock()
	sent := requests
	mu.Unlock()
	const batches = 8
	if sent >= batches || sent > 5 {
		t.Fatalf("requests sent = %d, want fewer than the %d batches (at most the 5 in flight when the 403 landed)", sent, batches)
	}
	logs := buf.String()
	for _, want := range []string{
		"batches=8", "failures(403=", "rate-limited: remaining batches skipped this cycle",
	} {
		if !strings.Contains(logs, want) {
			t.Errorf("summary missing %q:\n%s", want, logs)
		}
	}
	if !strings.Contains(logs, "skipped="+strconv.Itoa(batches-sent)) {
		t.Errorf("summary should report skipped=%d:\n%s", batches-sent, logs)
	}
}

func TestBatchGetCIStatus_MergeStateRequestedFollowsTheFlag(t *testing.T) {
	body := `{"data":{"pr0":{"pullRequest":{"mergeStateStatus":"CLEAN","reviewDecision":"APPROVED",
		"commits":{"nodes":[{"commit":{"statusCheckRollup":null}}]}}}}}`
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(body))
	}))
	defer ts.Close()
	client := NewClient("test-token", "")
	client.httpClient = &http.Client{Transport: &redirectTransport{targetURL: ts.URL}}

	for _, flag := range []bool{true, false} {
		results, err := client.BatchGetCIStatus(context.Background(), []PRInfo{{Owner: "acme", Repo: "example", Number: 7, IncludeMergeState: flag}})
		if err != nil {
			t.Fatalf("BatchGetCIStatus: %v", err)
		}
		if got := results["acme/example/7"].MergeStateRequested; got != flag {
			t.Errorf("MergeStateRequested = %v for IncludeMergeState=%v", got, flag)
		}
	}
}

func TestBatchGetCIStatus_RateLimitedIn200BodySkipsRemainingBatches(t *testing.T) {
	var mu sync.Mutex
	requests := 0
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		requests++
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":null,"errors":[{"type":"RATE_LIMITED","message":"API rate limit already exceeded"}]}`))
	}))
	defer ts.Close()
	client := NewClient("test-token", "")
	client.httpClient = &http.Client{Transport: &redirectTransport{targetURL: ts.URL}}

	var buf bytes.Buffer
	log.SetOutput(&buf)
	defer log.SetOutput(os.Stderr)

	results, err := client.BatchGetCIStatus(context.Background(), ciPRs(200, true))
	if err != nil {
		t.Fatalf("BatchGetCIStatus: %v", err)
	}
	if len(results) != 0 {
		t.Errorf("results = %d, want none from throttled responses", len(results))
	}
	mu.Lock()
	sent := requests
	mu.Unlock()
	const batches = 8
	if sent >= batches || sent > 5 {
		t.Fatalf("requests sent = %d, want fewer than the %d batches (at most the 5 in flight when RATE_LIMITED landed)", sent, batches)
	}
	logs := buf.String()
	for _, want := range []string{
		"batches=8", "failures(rate_limited=" + strconv.Itoa(sent) + ")", "skipped=" + strconv.Itoa(batches-sent),
		"rate-limited: remaining batches skipped this cycle",
	} {
		if !strings.Contains(logs, want) {
			t.Errorf("summary missing %q:\n%s", want, logs)
		}
	}
}

func TestBatchGetCIStatus_PacesBatchesWithinTheCycleBudget(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"data":{}}`))
	}))
	defer ts.Close()
	client := NewClient("test-token", "")
	client.httpClient = &http.Client{Transport: &redirectTransport{targetURL: ts.URL}}
	var slept []time.Duration
	client.ciBatchSleep = func(d time.Duration) { slept = append(slept, d) }

	prs := append(ciPRs(1000, true), ciPRs(500, false)...)
	start := time.Now()
	if _, err := client.BatchGetCIStatus(context.Background(), prs); err != nil {
		t.Fatalf("BatchGetCIStatus: %v", err)
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("injected sleep must not block: took %s", elapsed)
	}

	const batches = 40 + 10
	if len(slept) != batches-1 {
		t.Fatalf("sleeps = %d, want one between each of %d batches", len(slept), batches)
	}
	var total time.Duration
	for _, d := range slept {
		if d != ciBatchPace {
			t.Errorf("sleep = %s, want %s", d, ciBatchPace)
		}
		total += d
	}
	if total >= 30*time.Second {
		t.Errorf("pacing for %d batches sleeps %s in total, must stay well inside the 60s cycle", batches, total)
	}
}

func TestBatchGetCIStatus_RateLimitStopsPacingAndLogsReset(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", "42")
		w.WriteHeader(http.StatusForbidden)
	}))
	defer ts.Close()
	client := NewClient("test-token", "")
	client.httpClient = &http.Client{Transport: &redirectTransport{targetURL: ts.URL}}
	sleeps := 0
	client.ciBatchSleep = func(time.Duration) { sleeps++ }

	var buf bytes.Buffer
	log.SetOutput(&buf)
	defer log.SetOutput(os.Stderr)

	if _, err := client.BatchGetCIStatus(context.Background(), ciPRs(500, true)); err != nil {
		t.Fatalf("BatchGetCIStatus: %v", err)
	}
	if sleeps >= 19 {
		t.Errorf("sleeps = %d, want pacing to stop once the limit trips (20 batches)", sleeps)
	}
	logs := buf.String()
	if !strings.Contains(logs, "rate-limited: remaining batches skipped this cycle (limit resets at ") || !strings.Contains(logs, ", in ") {
		t.Errorf("summary must name the reset time from Retry-After:\n%s", logs)
	}
	if !strings.Contains(logs, "status 403 (limit resets at ") {
		t.Errorf("batch warning must carry the reset time:\n%s", logs)
	}
}

func TestRateLimitResetAt_HeaderPrecedence(t *testing.T) {
	now := time.Date(2026, 9, 18, 20, 0, 0, 0, time.UTC)
	h := http.Header{}
	if got := rateLimitResetAt(h, now); !got.IsZero() {
		t.Errorf("no headers: got %s, want zero", got)
	}
	h.Set("X-RateLimit-Reset", "1789000000")
	if got := rateLimitResetAt(h, now); !got.Equal(time.Unix(1789000000, 0)) {
		t.Errorf("X-RateLimit-Reset: got %s", got)
	}
	h.Set("Retry-After", "30")
	if got := rateLimitResetAt(h, now); !got.Equal(now.Add(30 * time.Second)) {
		t.Errorf("Retry-After must win: got %s", got)
	}
}

func TestExecuteGraphQLPartial_RateLimited200CarriesReset(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-RateLimit-Reset", "1789000000")
		_, _ = w.Write([]byte(`{"data":null,"errors":[{"type":"RATE_LIMITED","message":"API rate limit already exceeded"}]}`))
	}))
	defer ts.Close()
	client := NewClient("test-token", "")
	client.httpClient = &http.Client{Transport: &redirectTransport{targetURL: ts.URL}}

	var out struct{}
	_, err := client.executeGraphQLPartial(context.Background(), "query {}", &out)
	if !errors.Is(err, ErrGraphQLRateLimited) {
		t.Fatalf("err = %v, want ErrGraphQLRateLimited", err)
	}
	if got := RateLimitResetAt(err); !got.Equal(time.Unix(1789000000, 0)) {
		t.Errorf("reset = %s, want the X-RateLimit-Reset stamp", got)
	}
}
