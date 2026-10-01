package server

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	gh "github.com/google/go-github/v57/github"

	"pr-review-server/github"
)

func TestApprovalRateGatePausesUntilTheReportedReset(t *testing.T) {
	var g approvalRateGate
	now := time.Now()
	if g.note(errors.New("boom"), now) || !g.pausedUntil().IsZero() {
		t.Fatal("a non-rate-limit error must not pause")
	}
	reset := now.Add(5 * time.Minute)
	if !g.note(&gh.RateLimitError{Rate: gh.Rate{Reset: gh.Timestamp{Time: reset}}}, now) || !g.pausedUntil().Equal(reset) {
		t.Fatalf("paused until %v, want %v", g.pausedUntil(), reset)
	}
	g.note(&github.GraphQLRateLimitError{ResetAt: now.Add(time.Minute)}, now)
	if !g.pausedUntil().Equal(reset) {
		t.Fatal("an earlier reset must not shorten the pause")
	}
	var fresh approvalRateGate
	fresh.note(&gh.AbuseRateLimitError{}, now)
	if got := fresh.pausedUntil().Sub(now); got != approvalRateLimitFallback {
		t.Fatalf("unknown reset paused %v, want %v", got, approvalRateLimitFallback)
	}
	fresh.note(&gh.RateLimitError{Rate: gh.Rate{Reset: gh.Timestamp{Time: now.Add(24 * time.Hour)}}}, now)
	if got := fresh.pausedUntil().Sub(now); got != approvalRateLimitMaxPause {
		t.Fatalf("pause of %v not capped at %v", got, approvalRateLimitMaxPause)
	}
}

func TestApprovalPRCacheSharesReadsWithinTheFreshnessWindow(t *testing.T) {
	var c approvalPRCache
	var fetches atomic.Int32
	release := make(chan struct{})
	fetch := func(context.Context) (*gh.PullRequest, error) {
		fetches.Add(1)
		<-release
		return &gh.PullRequest{Number: gh.Int(7)}, nil
	}
	now := time.Now()
	var wg sync.WaitGroup
	for i := 0; i < 5; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := c.get(context.Background(), "o/r#7", now, approvalPRFreshness, fetch); err != nil {
				t.Error(err)
			}
		}()
	}
	time.Sleep(20 * time.Millisecond)
	close(release)
	wg.Wait()
	if _, err := c.get(context.Background(), "o/r#7", now.Add(approvalPRFreshness/2), approvalPRFreshness, fetch); err != nil {
		t.Fatal(err)
	}
	if n := fetches.Load(); n != 1 {
		t.Fatalf("%d fetches for concurrent and fresh reads, want 1", n)
	}
	if _, err := c.get(context.Background(), "o/r#7", now.Add(approvalPRFreshness), approvalPRFreshness, fetch); err != nil {
		t.Fatal(err)
	}
	if n := fetches.Load(); n != 2 {
		t.Fatalf("%d fetches after the freshness window, want 2", n)
	}
}

func TestApprovalPRCacheDoesNotCacheFailures(t *testing.T) {
	var c approvalPRCache
	calls := 0
	fetch := func(context.Context) (*gh.PullRequest, error) {
		calls++
		if calls == 1 {
			return nil, errors.New("transient")
		}
		return &gh.PullRequest{}, nil
	}
	now := time.Now()
	if _, err := c.get(context.Background(), "k", now, approvalPRFreshness, fetch); err == nil {
		t.Fatal("want the first error")
	}
	if _, err := c.get(context.Background(), "k", now, approvalPRFreshness, fetch); err != nil || calls != 2 {
		t.Fatalf("err=%v calls=%d, want a fresh fetch after a failure", err, calls)
	}
}

func TestApprovalRetryAfterRateLimitSkipsARetryThatCannotFinish(t *testing.T) {
	server, database := newTestServer(t, "tester")
	defer database.Close()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	calls := 0
	limited := &gh.RateLimitError{Rate: gh.Rate{Reset: gh.Timestamp{Time: time.Now().Add(time.Minute)}}}
	err := server.approvalRetryAfterRateLimit(ctx, func() error { calls++; return limited })
	if calls != 1 || !errors.Is(err, limited) {
		t.Fatalf("calls=%d err=%v, want no retry when the reset is past the deadline", calls, err)
	}
	if approvalFailure(err, "access_unavailable") != "github_rate_limited" {
		t.Fatal("rate limit reported as an access failure")
	}
	if approvalFailure(errors.New("denied"), "access_unavailable") != "access_unavailable" {
		t.Fatal("other failures keep their step name")
	}
	if !time.Now().Before(server.approvalRate.pausedUntil()) {
		t.Fatal("the rate limit must pause further claims")
	}
}

func TestApprovalRetryAfterRateLimitRetriesOnceAfterAShortReset(t *testing.T) {
	server, database := newTestServer(t, "tester")
	defer database.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	calls := 0
	err := server.approvalRetryAfterRateLimit(ctx, func() error {
		calls++
		if calls == 1 {
			return &github.GraphQLRateLimitError{ResetAt: time.Now().Add(50 * time.Millisecond)}
		}
		return nil
	})
	if err != nil || calls != 2 {
		t.Fatalf("err=%v calls=%d, want one retry after the reset", err, calls)
	}
}
