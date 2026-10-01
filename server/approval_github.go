package server

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	gh "github.com/google/go-github/v57/github"
	"golang.org/x/sync/singleflight"

	"pr-review-server/db"
	"pr-review-server/github"
)

const (
	// approvalPRFreshness bounds how stale the PR read behind access checks
	// and non-candidate feed rows may be. Candidate rows always read live so a
	// push shows as stale immediately; without the cache every feed refresh
	// cost one GitHub request per target.
	approvalPRFreshness = 30 * time.Second
	// approvalWorkerPRFreshness lets a worker's access checks before and
	// after one investigation share a PR read; the inventory is still
	// checked each time and a candidate re-collects the PR live.
	approvalWorkerPRFreshness = approvalTargetDuration
	// approvalRateLimitFallback is the pause when GitHub rate-limits without
	// saying when the limit resets.
	approvalRateLimitFallback = time.Minute
	approvalRateLimitMaxPause = time.Hour
	// approvalRateLimitMargin is the time a target must have left after
	// waiting out a rate limit for the retry to be worth starting.
	approvalRateLimitMargin = 30 * time.Second
)

var errApprovalNoAccess = errors.New("approval target is not readable by the viewer")

// approvalRateGate pauses approval work that needs GitHub while its rate limit
// is exhausted, so one throttled response cannot turn every queued target into
// an access failure.
type approvalRateGate struct {
	mu    sync.Mutex
	until time.Time
}

// note records err when it is a GitHub rate limit and reports whether it was.
func (g *approvalRateGate) note(err error, now time.Time) bool {
	reset, limited := github.RateLimited(err, now)
	if !limited {
		return false
	}
	if !reset.After(now) {
		reset = now.Add(approvalRateLimitFallback)
	}
	if reset.After(now.Add(approvalRateLimitMaxPause)) {
		reset = now.Add(approvalRateLimitMaxPause)
	}
	g.mu.Lock()
	if reset.After(g.until) {
		g.until = reset
	}
	g.mu.Unlock()
	return true
}

func (g *approvalRateGate) pausedUntil() time.Time {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.until
}

// approvalPRCache shares recent PR reads across the feed, detail and worker
// paths, and collapses concurrent reads of one PR into a single request.
type approvalPRCache struct {
	mu      sync.Mutex
	entries map[string]approvalPREntry
	flight  singleflight.Group
}

type approvalPREntry struct {
	pr      *gh.PullRequest
	fetched time.Time
}

func (c *approvalPRCache) put(key string, pr *gh.PullRequest, now time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.entries == nil {
		c.entries = map[string]approvalPREntry{}
	}
	c.entries[key] = approvalPREntry{pr: pr, fetched: now}
}

func (c *approvalPRCache) get(ctx context.Context, key string, now time.Time, freshness time.Duration, fetch func(context.Context) (*gh.PullRequest, error)) (*gh.PullRequest, error) {
	c.mu.Lock()
	if e, ok := c.entries[key]; ok && now.Sub(e.fetched) < freshness {
		c.mu.Unlock()
		return e.pr, nil
	}
	c.mu.Unlock()
	v, err, _ := c.flight.Do(key, func() (any, error) {
		pr, err := fetch(ctx)
		if err != nil {
			return nil, err
		}
		c.mu.Lock()
		for k, e := range c.entries {
			if now.Sub(e.fetched) >= max(approvalPRFreshness, approvalWorkerPRFreshness) {
				delete(c.entries, k)
			}
		}
		c.mu.Unlock()
		c.put(key, pr, now)
		return pr, nil
	})
	if err != nil {
		return nil, err
	}
	return v.(*gh.PullRequest), nil
}

func approvalPRKey(owner, repo string, number int) string {
	return fmt.Sprintf("%s/%s#%d", owner, repo, number)
}

// approvalPR reads a PR through the shared cache and records rate limits.
func (s *Server) approvalPR(ctx context.Context, owner, repo string, number int, freshness time.Duration) (*gh.PullRequest, error) {
	pr, err := s.approvalPRs.get(ctx, approvalPRKey(owner, repo, number), time.Now(), freshness, func(ctx context.Context) (*gh.PullRequest, error) {
		// The read is shared by concurrent callers, so one caller cancelling
		// must not fail it for the rest.
		ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 20*time.Second)
		defer cancel()
		pr, _, err := s.ghClient.GetPR(ctx, owner, repo, number)
		return pr, err
	})
	s.approvalRate.note(err, time.Now())
	return pr, err
}

// approvalLivePR reads a PR from GitHub, bypassing the cache but refreshing
// it, for callers that must see a push the moment it lands.
func (s *Server) approvalLivePR(ctx context.Context, owner, repo string, number int) (*gh.PullRequest, error) {
	pr, _, err := s.ghClient.GetPR(ctx, owner, repo, number)
	s.approvalRate.note(err, time.Now())
	if err != nil {
		return nil, err
	}
	s.approvalPRs.put(approvalPRKey(owner, repo, number), pr, time.Now())
	return pr, nil
}

// approvalAccess returns nil when the viewer can still see the target and it
// still belongs to the admitted repository, errApprovalNoAccess when either
// is no longer true, and the GitHub error when access could not be checked.
// Workers use it with a briefly cached inventory.
func (s *Server) approvalAccess(ctx context.Context, user int, target db.ApprovalTarget) error {
	return s.approvalAccessWith(ctx, s.approvalInventoryCached, approvalWorkerPRFreshness, user, target)
}

func (s *Server) approvalAccessWith(ctx context.Context, load func(int) (map[string]db.PRWithUserView, error), freshness time.Duration, user int, target db.ApprovalTarget) error {
	inventory, err := load(user)
	if err != nil {
		return fmt.Errorf("load visible pull requests: %w", err)
	}
	if _, exists := inventory[approvalKey(target.Owner, target.Repo, target.Number)]; !exists || s.ghClient == nil {
		return errApprovalNoAccess
	}
	pr, err := s.approvalPR(ctx, target.Owner, target.Repo, target.Number, freshness)
	if err != nil {
		return err
	}
	if pr.GetBase().GetRepo().GetID() != target.RepositoryID {
		return errApprovalNoAccess
	}
	return nil
}

func (s *Server) approvalCanRead(ctx context.Context, user int, target db.ApprovalTarget) bool {
	return s.approvalAccessWith(ctx, s.approvalInventory, approvalPRFreshness, user, target) == nil
}

// approvalRetryAfterRateLimit runs fn, and when GitHub rate-limits it waits
// for the reset and runs fn once more, provided the context's deadline leaves
// room for the retry.
func (s *Server) approvalRetryAfterRateLimit(ctx context.Context, fn func() error) error {
	err := fn()
	if !s.approvalRate.note(err, time.Now()) {
		return err
	}
	wait := time.Until(s.approvalRate.pausedUntil())
	if deadline, ok := ctx.Deadline(); ok && time.Now().Add(wait+approvalRateLimitMargin).After(deadline) {
		return err
	}
	timer := time.NewTimer(wait)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
	}
	err = fn()
	s.approvalRate.note(err, time.Now())
	return err
}

// approvalFailure names a failure for the stored reason codes: a GitHub rate
// limit is reported as such rather than as the step that hit it.
func approvalFailure(err error, step string) string {
	if _, limited := github.RateLimited(err, time.Now()); limited {
		return "github_rate_limited"
	}
	return step
}
