package approval

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func stubModelSleep(t *testing.T) *[]time.Duration {
	t.Helper()
	var mu sync.Mutex
	slept := []time.Duration{}
	previous := modelSleep
	modelSleep = func(ctx context.Context, d time.Duration) error {
		mu.Lock()
		slept = append(slept, d)
		mu.Unlock()
		return ctx.Err()
	}
	t.Cleanup(func() { modelSleep = previous })
	return &slept
}

func modelHTTPConfig(url string) ModelConfig {
	return ModelConfig{Provider: "openrouter", Model: "fixture-model", APIKey: "fixture", BaseURL: url}
}

func writeModelAnswer(w http.ResponseWriter) {
	_ = json.NewEncoder(w).Encode(map[string]any{"model": "fixture-model", "usage": map[string]int{"prompt_tokens": 10, "completion_tokens": 5}, "choices": []any{map[string]any{"finish_reason": "stop", "message": map[string]any{"role": "assistant", "content": "{}"}}}})
}

type countingBudget struct{ reserves, settles atomic.Int32 }

func (b *countingBudget) Reserve(context.Context, Usage) error {
	b.reserves.Add(1)
	return nil
}
func (b *countingBudget) Settle(context.Context, Usage, Usage) error {
	b.settles.Add(1)
	return nil
}

func TestModelPostRetriesRateLimitWithinOneReservation(t *testing.T) {
	slept := stubModelSleep(t)
	var requests atomic.Int32
	remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if requests.Add(1) == 1 {
			w.Header().Set("Retry-After", "3")
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		writeModelAnswer(w)
	}))
	defer remote.Close()
	budget := &countingBudget{}
	reply, err := modelCallWithBudget(context.Background(), modelHTTPConfig(remote.URL), budget)
	if err != nil {
		t.Fatal(err)
	}
	if reply.Usage.InputTokens != 10 || requests.Load() != 2 {
		t.Fatalf("usage %+v after %d requests", reply.Usage, requests.Load())
	}
	if len(*slept) != 1 || (*slept)[0] != 3*time.Second {
		t.Fatalf("slept %v, want one Retry-After wait", *slept)
	}
	if budget.reserves.Load() != 1 || budget.settles.Load() != 1 {
		t.Fatalf("%d reservations and %d settlements", budget.reserves.Load(), budget.settles.Load())
	}
}

func modelCallWithBudget(ctx context.Context, c ModelConfig, budget Budget) (modelReply, error) {
	reserved := Usage{InputTokens: CallInputTokens, OutputTokens: 1000, Rounds: 1}
	if err := budget.Reserve(ctx, reserved); err != nil {
		return modelReply{}, err
	}
	reply, err := c.call(ctx, []any{map[string]any{"role": "user", "content": "fixture"}}, 1000, true)
	if err == nil {
		err = budget.Settle(ctx, reserved, reply.Usage)
	}
	return reply, err
}

func TestModelPostRetriesRetryableErrorInSuccessfulBody(t *testing.T) {
	stubModelSleep(t)
	var requests atomic.Int32
	remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if requests.Add(1) == 1 {
			_, _ = w.Write([]byte(`{"error":{"code":429,"message":"rate limited upstream"}}`))
			return
		}
		writeModelAnswer(w)
	}))
	defer remote.Close()
	if _, err := modelHTTPConfig(remote.URL).call(context.Background(), []any{}, 1000, true); err != nil {
		t.Fatal(err)
	}
	if requests.Load() != 2 {
		t.Fatalf("%d requests, want a retry", requests.Load())
	}
}

func TestModelPostDoesNotRetryRejectionsOrTransportErrors(t *testing.T) {
	for _, status := range []int{http.StatusBadRequest, http.StatusUnauthorized} {
		slept := stubModelSleep(t)
		var requests atomic.Int32
		remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			requests.Add(1)
			w.WriteHeader(status)
		}))
		_, err := modelHTTPConfig(remote.URL).call(context.Background(), []any{}, 1000, true)
		remote.Close()
		var rejected *ModelStatusError
		if !errors.As(err, &rejected) || rejected.Status != status || err.Error() != fmt.Sprintf("model HTTP status %d", status) {
			t.Fatalf("status %d: got %v", status, err)
		}
		if requests.Load() != 1 || len(*slept) != 0 {
			t.Fatalf("status %d retried: %d requests", status, requests.Load())
		}
	}
	slept := stubModelSleep(t)
	remote := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	url := remote.URL
	remote.Close()
	if _, err := modelHTTPConfig(url).call(context.Background(), []any{}, 1000, true); err == nil || len(*slept) != 0 {
		t.Fatalf("transport error: %v after %d waits", err, len(*slept))
	}
}

func TestModelPostGivesUpBeforeTheDeadline(t *testing.T) {
	slept := stubModelSleep(t)
	var requests atomic.Int32
	remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		w.Header().Set("Retry-After", "30")
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer remote.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	_, err := modelHTTPConfig(remote.URL).call(ctx, []any{}, 1000, true)
	var rejected *ModelStatusError
	if !errors.As(err, &rejected) || rejected.Status != http.StatusServiceUnavailable || rejected.RetryAfter != 30*time.Second {
		t.Fatalf("got %v", err)
	}
	if requests.Load() != 1 || len(*slept) != 0 {
		t.Fatalf("%d requests and %d waits, want no wait past the deadline", requests.Load(), len(*slept))
	}
}

func TestModelPostStopsAfterMaxAttempts(t *testing.T) {
	slept := stubModelSleep(t)
	var requests atomic.Int32
	remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		w.WriteHeader(529)
	}))
	defer remote.Close()
	_, err := modelHTTPConfig(remote.URL).call(context.Background(), []any{}, 1000, true)
	var rejected *ModelStatusError
	if !errors.As(err, &rejected) || rejected.Status != 529 {
		t.Fatalf("got %v", err)
	}
	if requests.Load() != modelMaxAttempts || len(*slept) != modelMaxAttempts-1 {
		t.Fatalf("%d requests and %d waits", requests.Load(), len(*slept))
	}
	for i, d := range *slept {
		if d <= 0 || d > modelRetryCap {
			t.Fatalf("wait %d is %s", i, d)
		}
	}
}

func TestModelLimiterPauseDelaysOtherCallers(t *testing.T) {
	slept := stubModelSleep(t)
	limiter := NewModelLimiter(4)
	var requests atomic.Int32
	remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if requests.Add(1) == 1 {
			w.Header().Set("Retry-After", "5")
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		writeModelAnswer(w)
	}))
	defer remote.Close()
	ctx := WithModelLimiter(context.Background(), limiter)
	if _, err := modelHTTPConfig(remote.URL).call(ctx, []any{}, 1000, true); err != nil {
		t.Fatal(err)
	}
	var stages []string
	observed := WithActivityObserver(ctx, func(a Activity) { stages = append(stages, a.Stage) })
	if _, err := modelHTTPConfig(remote.URL).call(observed, []any{}, 1000, true); err != nil {
		t.Fatal(err)
	}
	if len(*slept) != 2 || (*slept)[1] <= 4*time.Second {
		t.Fatalf("second caller waits %v, want the shared pause", *slept)
	}
	if len(stages) == 0 || stages[0] != "model_wait" || ActivitySummary(Activity{Stage: "model_wait"}) != "Waiting for model capacity" {
		t.Fatalf("stages %v", stages)
	}
}

func TestModelLimiterCapsConcurrentRequests(t *testing.T) {
	limiter := NewModelLimiter(3)
	var inFlight, peak atomic.Int32
	remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		now := inFlight.Add(1)
		for {
			seen := peak.Load()
			if now <= seen || peak.CompareAndSwap(seen, now) {
				break
			}
		}
		time.Sleep(20 * time.Millisecond)
		inFlight.Add(-1)
		writeModelAnswer(w)
	}))
	defer remote.Close()
	ctx := WithModelLimiter(context.Background(), limiter)
	var wg sync.WaitGroup
	errs := make(chan error, 12)
	for range 12 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := modelHTTPConfig(remote.URL).call(ctx, []any{}, 1000, true)
			errs <- err
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	if peak.Load() > 3 || peak.Load() == 0 {
		t.Fatalf("peak concurrency %d, want at most 3", peak.Load())
	}
}

func TestNativeReleasesReservationOnlyForRejectionsWithoutUsage(t *testing.T) {
	for _, c := range []struct {
		status int
		want   int
	}{{http.StatusServiceUnavailable, 0}, {http.StatusInternalServerError, CallInputTokens}} {
		stubModelSleep(t)
		remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(c.status) }))
		s, _ := validFixture()
		b := &testBudget{}
		_, err := NativeInvestigator{Config: modelHTTPConfig(remote.URL)}.Investigate(context.Background(), s, nil, b)
		remote.Close()
		var rejected *ModelStatusError
		if !errors.As(err, &rejected) || b.used.InputTokens != c.want {
			t.Fatalf("status %d: %v, %d input tokens charged", c.status, err, b.used.InputTokens)
		}
	}
}

func TestModelPostBoundsOneRequestWithoutCancellingTheTarget(t *testing.T) {
	previous := modelAttemptTimeout
	modelAttemptTimeout = 50 * time.Millisecond
	t.Cleanup(func() { modelAttemptTimeout = previous })
	stalled := make(chan struct{})
	remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-stalled:
		}
	}))
	defer remote.Close()
	defer close(stalled)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	_, err := modelHTTPConfig(remote.URL).post(ctx, remote.URL, []byte("{}"))
	if !errors.Is(err, ErrInvestigationLimit) || ctx.Err() != nil {
		t.Fatalf("err=%v target=%v", err, ctx.Err())
	}
}
