package server

import (
	"context"
	"errors"
	"sync"
	"time"

	"golang.org/x/sync/singleflight"

	"pr-review-server/db"
	"pr-review-server/pkg/approval"
)

const (
	approvalWatchEvery     = 2 * time.Second
	approvalHeartbeatEvery = 15 * time.Second
	approvalLeaseDuration  = 60 * time.Second
	// approvalScanStateFreshness lets every target of a scan share one
	// cancellation read.
	approvalScanStateFreshness = 2 * time.Second
	// approvalScanReadTolerance is how long cancellation may stay unreadable
	// before the target stops; it matches three missed five-second checks.
	approvalScanReadTolerance = 15 * time.Second
)

// approvalWake nudges the dispatcher; the zero value is ready to use.
type approvalWake struct {
	once sync.Once
	ch   chan struct{}
}

func (w *approvalWake) channel() chan struct{} {
	w.once.Do(func() { w.ch = make(chan struct{}, 1) })
	return w.ch
}

func (w *approvalWake) signal() {
	select {
	case w.channel() <- struct{}{}:
	default:
	}
}

type approvalModelLimiter struct {
	once    sync.Once
	limiter *approval.ModelLimiter
}

func (s *Server) approvalModelLimiter() *approval.ModelLimiter {
	s.approvalLimiter.once.Do(func() { s.approvalLimiter.limiter = approval.NewModelLimiter(approvalModelConcurrency()) })
	return s.approvalLimiter.limiter
}

// approvalScanStateCache shares recent scan cancellation reads across the
// targets of a scan.
type approvalScanStateCache struct {
	mu      sync.Mutex
	entries map[string]approvalScanState
	flight  singleflight.Group
}

type approvalScanState struct {
	cancelled bool
	fetched   time.Time
}

func (s *Server) approvalScanCancelled(user int, scanID string) (bool, error) {
	c := &s.approvalScanStates
	c.mu.Lock()
	if e, ok := c.entries[scanID]; ok && time.Since(e.fetched) < approvalScanStateFreshness {
		c.mu.Unlock()
		return e.cancelled, nil
	}
	c.mu.Unlock()
	v, err, _ := c.flight.Do(scanID, func() (any, error) {
		scan, err := s.approvalStore().GetApprovalScan(user, scanID)
		if err != nil {
			return false, err
		}
		now := time.Now()
		c.mu.Lock()
		defer c.mu.Unlock()
		if c.entries == nil {
			c.entries = map[string]approvalScanState{}
		}
		for id, e := range c.entries {
			if now.Sub(e.fetched) >= approvalScanStateFreshness {
				delete(c.entries, id)
			}
		}
		c.entries[scanID] = approvalScanState{cancelled: scan.CancelRequested, fetched: now}
		return scan.CancelRequested, nil
	})
	if err != nil {
		return false, err
	}
	return v.(bool), nil
}

// watchApprovalTarget renews the target's lease and cancels its context when
// the scan is cancelled, the feature becomes unavailable, or the lease is lost.
func (s *Server) watchApprovalTarget(ctx context.Context, cancel context.CancelFunc, target db.ApprovalTarget) {
	store := s.approvalStore()
	ticker := time.NewTicker(approvalWatchEvery)
	defer ticker.Stop()
	renewed := time.Now()
	heartbeatFailures := 0
	var unreadableSince time.Time
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		cancelled, err := s.approvalScanCancelled(target.UserID, target.ScanID)
		if err != nil {
			if unreadableSince.IsZero() {
				unreadableSince = time.Now()
			}
			if time.Since(unreadableSince) < approvalScanReadTolerance {
				continue
			}
			cancel()
			return
		}
		unreadableSince = time.Time{}
		if cancelled || s.approvalAvailable() != "" {
			cancel()
			return
		}
		if time.Since(renewed) < approvalHeartbeatEvery {
			continue
		}
		if err := store.HeartbeatApprovalTarget(target.ID, target.LeaseToken, time.Now(), approvalLeaseDuration); err != nil {
			heartbeatFailures++
			if heartbeatFailures < 3 && !errors.Is(err, db.ErrApprovalLeaseLost) {
				continue
			}
			cancel()
			return
		}
		renewed = time.Now()
		heartbeatFailures = 0
	}
}
