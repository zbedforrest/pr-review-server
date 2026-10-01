package approval

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math/rand/v2"
	"net/http"
	"strconv"
	"sync"
	"time"
)

// ModelLimiter bounds concurrent model requests across investigations and
// holds every caller back while the provider is rate limiting.
type ModelLimiter struct {
	slots chan struct{}
	mu    sync.Mutex
	pause time.Time
}

func NewModelLimiter(concurrency int) *ModelLimiter {
	return &ModelLimiter{slots: make(chan struct{}, max(concurrency, 1))}
}

type modelLimiterKey struct{}

func WithModelLimiter(ctx context.Context, l *ModelLimiter) context.Context {
	return context.WithValue(ctx, modelLimiterKey{}, l)
}

func modelLimiterFrom(ctx context.Context) *ModelLimiter {
	l, _ := ctx.Value(modelLimiterKey{}).(*ModelLimiter)
	return l
}

func (l *ModelLimiter) pausedUntil() time.Time {
	if l == nil {
		return time.Time{}
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.pause
}

func (l *ModelLimiter) pauseUntil(until time.Time) {
	if l == nil {
		return
	}
	l.mu.Lock()
	if until.After(l.pause) {
		l.pause = until
	}
	l.mu.Unlock()
}

// acquire waits for a request slot and returns its release.
func (l *ModelLimiter) acquire(ctx context.Context) (func(), error) {
	if l == nil {
		return func() {}, nil
	}
	select {
	case l.slots <- struct{}{}:
	default:
		reportActivity(ctx, Activity{Stage: "model_wait"})
		select {
		case l.slots <- struct{}{}:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	return func() { <-l.slots }, nil
}

// ModelStatusError is a model request the provider rejected, after any
// retries the deadline allowed.
type ModelStatusError struct {
	Status     int
	RetryAfter time.Duration
}

func (e *ModelStatusError) Error() string { return fmt.Sprintf("model HTTP status %d", e.Status) }

// withoutUsage reports a rejection the provider makes before any work, so
// nothing was consumed.
func (e *ModelStatusError) withoutUsage() bool { return modelRetryable(e.Status) }

func modelRetryable(status int) bool {
	return status == http.StatusTooManyRequests || status == http.StatusBadGateway || status == http.StatusServiceUnavailable || status == 529
}

const (
	modelMaxAttempts = 6
	modelRetryBase   = time.Second
	modelRetryCap    = 20 * time.Second
	// modelRetryMargin is the time a request needs after a wait for the
	// retry to be worth sending.
	modelRetryMargin = 45 * time.Second
	maxModelResponse = 2 * 1024 * 1024
)

// modelAttemptTimeout bounds one request: a full CallOutputTokens reply at the
// route's measured speed plus prefill, so a stalled upstream fails the target
// instead of holding the scan to the target deadline.
var modelAttemptTimeout = 120 * time.Second

var modelSleep = func(ctx context.Context, d time.Duration) error {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-timer.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// post sends one model request. Rejections that carry no usage (429, 502,
// 503, 529, or a 200 whose body is such an error) are retried with backoff;
// transport errors and other statuses are not, since usage may be unknown.
func (c ModelConfig) post(ctx context.Context, endpoint string, data []byte) ([]byte, error) {
	client := c.Client
	if client == nil {
		client = http.DefaultClient
	}
	boundedClient := *client
	boundedClient.CheckRedirect = func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse }
	limiter := modelLimiterFrom(ctx)
	var waited time.Time
	for attempt := 1; ; attempt++ {
		if until := limiter.pausedUntil(); until.After(waited) && time.Until(until) > 0 {
			wait := time.Until(until)
			if !modelCanWait(ctx, wait) {
				return nil, &ModelStatusError{Status: http.StatusTooManyRequests, RetryAfter: wait}
			}
			reportActivity(ctx, Activity{Stage: "model_wait"})
			if err := modelSleep(ctx, wait); err != nil {
				return nil, err
			}
		}
		release, err := limiter.acquire(ctx)
		if err != nil {
			return nil, err
		}
		attemptCtx, cancel := context.WithTimeout(ctx, modelAttemptTimeout)
		raw, status, header, err := c.send(attemptCtx, &boundedClient, endpoint, data)
		timedOut := attemptCtx.Err() != nil && ctx.Err() == nil
		cancel()
		release()
		if err != nil && timedOut {
			return nil, investigationLimit(LimitBudget, "model request took longer than %s", modelAttemptTimeout)
		}
		if err != nil {
			return nil, err
		}
		if status == http.StatusOK {
			status = modelBodyErrorStatus(raw)
			if status == http.StatusOK {
				return raw, nil
			}
		}
		if !modelRetryable(status) {
			return nil, &ModelStatusError{Status: status}
		}
		delay, hinted := modelRetryAfter(header, time.Now())
		if !hinted {
			delay = rand.N(min(modelRetryCap, modelRetryBase<<(attempt-1))) + 1
		}
		rejected := &ModelStatusError{Status: status, RetryAfter: delay}
		if attempt >= modelMaxAttempts || !modelCanWait(ctx, delay) {
			return nil, rejected
		}
		waited = time.Now().Add(delay)
		if status == http.StatusTooManyRequests {
			limiter.pauseUntil(waited)
		}
		reportActivity(ctx, Activity{Stage: "model_wait"})
		if err := modelSleep(ctx, delay); err != nil {
			return nil, err
		}
	}
}

func (c ModelConfig) send(ctx context.Context, client *http.Client, endpoint string, data []byte) ([]byte, int, http.Header, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(data))
	if err != nil {
		return nil, 0, nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	if c.Provider == "anthropic" {
		req.Header.Set("x-api-key", c.APIKey)
		req.Header.Set("anthropic-version", "2023-06-01")
	} else {
		req.Header.Set("Authorization", "Bearer "+c.APIKey)
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, 0, nil, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxModelResponse+1))
	if err != nil {
		return nil, 0, nil, err
	}
	if len(raw) > maxModelResponse {
		return nil, 0, nil, fmt.Errorf("model response too large")
	}
	return raw, resp.StatusCode, resp.Header, nil
}

func modelCanWait(ctx context.Context, wait time.Duration) bool {
	deadline, ok := ctx.Deadline()
	return !ok || time.Until(deadline) >= wait+modelRetryMargin
}

// modelBodyErrorStatus reads the retryable error some providers return with
// HTTP 200; any other body keeps the 200.
func modelBodyErrorStatus(raw []byte) int {
	var body struct {
		Error *struct {
			Code json.RawMessage `json:"code"`
		} `json:"error"`
	}
	if json.Unmarshal(raw, &body) != nil || body.Error == nil {
		return http.StatusOK
	}
	var code int
	if json.Unmarshal(body.Error.Code, &code) != nil {
		var text string
		if json.Unmarshal(body.Error.Code, &text) != nil {
			return http.StatusOK
		}
		code, _ = strconv.Atoi(text)
	}
	if code == http.StatusTooManyRequests || code == http.StatusBadGateway || code == http.StatusServiceUnavailable {
		return code
	}
	return http.StatusOK
}

func modelRetryAfter(header http.Header, now time.Time) (time.Duration, bool) {
	value := header.Get("Retry-After")
	if value == "" {
		return 0, false
	}
	if seconds, err := strconv.Atoi(value); err == nil && seconds >= 0 {
		return time.Duration(seconds) * time.Second, true
	}
	if at, err := http.ParseTime(value); err == nil {
		return max(at.Sub(now), 0), true
	}
	return 0, false
}
