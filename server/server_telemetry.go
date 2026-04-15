package server

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strconv"

	"pr-review-server/auth"
	"pr-review-server/db"
)

// Allowed telemetry actions (whitelist)
var allowedActions = map[string]bool{
	"search":                      true,
	"triage_filter":               true,
	"toggle_columns":              true,
	"trigger_review":              true,
	"delete_pr":                   true,
	"edit_notes":                  true,
	"view_review":                 true,
	"open_pr_github":              true,
	"refresh_poll":                true,
	"toggle_auto_review":          true,
	"view_review_prs_page":        true,
	"toggle_filters":              true,
	"filter_team":                 true,
	"filter_repo":                 true,
	"toggle_repo_filter_dropdown": true,
	"clear_filters":               true,
}

type telemetryEventInput struct {
	Action   string `json:"action"`
	Label    string `json:"label"`
	PROwner  string `json:"pr_owner"`
	PRRepo   string `json:"pr_repo"`
	PRNumber int    `json:"pr_number"`
}

type telemetryBatchInput struct {
	Events []telemetryEventInput `json:"events"`
}

func normalizeTelemetryEvents(userID int, inputs []telemetryEventInput) []db.TelemetryEvent {
	if len(inputs) > 100 {
		inputs = inputs[:100]
	}

	events := make([]db.TelemetryEvent, 0, len(inputs))
	for _, e := range inputs {
		if !allowedActions[e.Action] {
			continue
		}

		label := e.Label
		if len(label) > 255 {
			label = label[:255]
		}

		events = append(events, db.TelemetryEvent{
			UserID:   userID,
			Action:   e.Action,
			Label:    label,
			PROwner:  e.PROwner,
			PRRepo:   e.PRRepo,
			PRNumber: e.PRNumber,
		})
	}

	return events
}

func (s *Server) ingestTelemetryEvents(userID int, inputs []telemetryEventInput) error {
	events := normalizeTelemetryEvents(userID, inputs)
	if len(events) == 0 {
		return nil
	}

	return s.db.CreateTelemetryEvents(events)
}

func (s *Server) handleTrackTelemetry(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	user := auth.GetCurrentUser(r)
	if user == nil {
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}

	var req telemetryBatchInput
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, fmt.Sprintf("Invalid request: %v", err), http.StatusBadRequest)
		return
	}

	if len(req.Events) == 0 {
		http.Error(w, "No events provided", http.StatusBadRequest)
		return
	}

	if err := s.ingestTelemetryEvents(user.ID, req.Events); err != nil {
		log.Printf("[TELEMETRY] Error saving events: %v", err)
		http.Error(w, "Failed to save events", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
}

func (s *Server) handleTelemetryStats(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	days := 7
	if d := r.URL.Query().Get("days"); d != "" {
		if parsed, err := strconv.Atoi(d); err == nil && parsed > 0 && parsed <= 90 {
			days = parsed
		}
	}

	stats, err := s.db.GetTelemetryStats(days)
	if err != nil {
		log.Printf("[TELEMETRY] Error fetching stats: %v", err)
		http.Error(w, "Failed to fetch stats", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-cache, no-store, must-revalidate")
	_ = json.NewEncoder(w).Encode(stats)
}
