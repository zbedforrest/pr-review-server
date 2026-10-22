package server

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"time"

	"pr-review-server/poller"
)

const (
	dailyGradePath = "/api/health/grade"
	// gradeRunTimeout bounds one background grading run.
	gradeRunTimeout = 3 * time.Hour
)

type dailyGrader interface {
	RunDailyGrade(ctx context.Context, now time.Time, onStart func()) (poller.GradeReport, error)
}

// handleDailyGradeJob is what Cloud Scheduler calls an hour before the daily
// health report. A run can outlast the scheduler's request deadline, so it
// is started in the background and the response only says whether it began.
func (s *Server) handleDailyGradeJob(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if s.cfg.HealthJobToken == "" {
		http.Error(w, "health job not configured", http.StatusServiceUnavailable)
		return
	}
	if subtle.ConstantTimeCompare([]byte(r.Header.Get(healthJobTokenHeader)), []byte(s.cfg.HealthJobToken)) != 1 {
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}
	if !s.cfg.GraderEnabled {
		http.Error(w, poller.ErrGraderDisabled.Error(), http.StatusServiceUnavailable)
		return
	}
	g, ok := s.poller.(dailyGrader)
	if !ok {
		http.Error(w, "poller unavailable", http.StatusServiceUnavailable)
		return
	}
	started := make(chan error, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), gradeRunTimeout)
		defer cancel()
		rep, err := g.RunDailyGrade(ctx, time.Now().UTC(), func() { started <- nil })
		if err != nil {
			select {
			case started <- err:
			default:
				log.Printf("[GRADE] run failed: %v", err)
			}
			return
		}
		log.Printf("[GRADE] run done: graded=%d cost=$%.2f", rep.Graded, rep.CostUSD)
	}()
	err := <-started
	switch {
	case errors.Is(err, poller.ErrGraderBusy):
		http.Error(w, err.Error(), http.StatusConflict)
		return
	case errors.Is(err, poller.ErrNotLeader):
		http.Error(w, err.Error(), http.StatusConflict)
		return
	case err != nil:
		log.Printf("[GRADE] run failed to start: %v", err)
		http.Error(w, "grading failed to start", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusAccepted)
	_ = json.NewEncoder(w).Encode(map[string]string{"status": "started"}) // nolint:errcheck
}
