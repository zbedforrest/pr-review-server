package server

import (
	"crypto/subtle"
	"encoding/json"
	"log"
	"net/http"
	"strconv"
	"time"

	"pr-review-server/db"
	"pr-review-server/pkg/health"
)

const (
	dailyHealthPath        = "/api/health/daily"
	healthJobTokenHeader   = "X-Prism-Job-Token"
	defaultHealthReportLim = 7
)

// healthStore is the slice of the database the daily report needs.
type healthStore interface {
	HealthMetrics(start, end, now time.Time) (health.Metrics, error)
	SaveHealthReport(*db.HealthReport) error
	ListHealthReports(limit int) ([]db.HealthReport, error)
}

// handleDailyHealthJob is what Cloud Scheduler calls: it evaluates the
// trailing 24 hours, stores the report, and returns it. The shared job token
// stands in for a user session; an unset token disables the endpoint.
func (s *Server) handleDailyHealthJob(w http.ResponseWriter, r *http.Request) {
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
	store, ok := s.db.(healthStore)
	if !ok {
		http.Error(w, "health store unavailable", http.StatusNotImplemented)
		return
	}
	report, err := s.runDailyHealth(store, time.Now().UTC())
	if err != nil {
		log.Printf("[HEALTH] daily report failed: %v", err)
		http.Error(w, "health report failed", http.StatusInternalServerError)
		return
	}
	log.Printf("[HEALTH] daily report: %s", report.Headline)
	w.Header().Set("Cache-Control", "no-cache, no-store, must-revalidate")
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(report) // nolint:errcheck
}

func (s *Server) runDailyHealth(store healthStore, now time.Time) (health.Report, error) {
	metrics, err := store.HealthMetrics(now.Add(-24*time.Hour), now, now)
	if err != nil {
		return health.Report{}, err
	}
	metrics.WallClock = time.Duration(s.cfg.AgentWallClockSec) * time.Second
	metrics.PollingDisabled = s.cfg.DisablePolling
	report := health.Evaluate(metrics)
	body, err := json.Marshal(report)
	if err != nil {
		return health.Report{}, err
	}
	return report, store.SaveHealthReport(&db.HealthReport{
		WindowStart: report.WindowStart, WindowEnd: report.WindowEnd, Overall: string(report.Overall), Headline: report.Headline,
		ReportJSON: string(body), Markdown: report.Markdown(), CreatedAt: now,
	})
}

// handleDailyHealth returns stored reports to authenticated users, newest
// first; format=md returns the latest one as readable markdown.
func (s *Server) handleDailyHealth(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	store, ok := s.db.(healthStore)
	if !ok {
		http.Error(w, "health store unavailable", http.StatusNotImplemented)
		return
	}
	limit := defaultHealthReportLim
	if v := r.URL.Query().Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > 90 {
			http.Error(w, "limit must be between 1 and 90", http.StatusBadRequest)
			return
		}
		limit = n
	}
	rows, err := store.ListHealthReports(limit)
	if err != nil {
		log.Printf("[HEALTH] list reports: %v", err)
		http.Error(w, "health reports unavailable", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Cache-Control", "no-cache, no-store, must-revalidate")
	if r.URL.Query().Get("format") == "md" {
		w.Header().Set("Content-Type", "text/markdown; charset=utf-8")
		if len(rows) == 0 {
			_, _ = w.Write([]byte("# PRism daily health\n\nNo reports yet.\n"))
			return
		}
		_, _ = w.Write([]byte(rows[0].Markdown))
		return
	}
	items := make([]map[string]any, 0, len(rows))
	for _, row := range rows {
		items = append(items, map[string]any{
			"id": row.ID, "window_start": row.WindowStart, "window_end": row.WindowEnd, "overall": row.Overall,
			"headline": row.Headline, "report": json.RawMessage(row.ReportJSON), "markdown": row.Markdown, "created_at": row.CreatedAt,
		})
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"reports": items}) // nolint:errcheck
}
