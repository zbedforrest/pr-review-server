package server

import (
	"context"
	"embed"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"pr-review-server/config"
	"pr-review-server/db"
	"pr-review-server/github"
	"pr-review-server/prioritization"
)

//go:embed dist/*
var reactDist embed.FS

type PollerInterface interface {
	GetCbprStatus() (running bool, duration time.Duration)
	GetLastPollTime() time.Time
	GetPollingInterval() time.Duration
	GetSecondsUntilNextPoll() int
}

type Server struct {
	cfg            *config.Config
	db             *db.DB
	ghClient       *github.Client
	prCache        []github.PullRequest
	prCacheMux     sync.RWMutex
	pollTriggerFunc func()
	poller         PollerInterface
	startTime      time.Time
	// Cache for rate limit info to avoid calling GitHub API on every status request
	rateLimitCache    *github.RateLimitInfo
	rateLimitCacheMux sync.RWMutex
	rateLimitCacheTime time.Time
	// Prioritization cache
	priorityResult    *prioritization.Result
	priorityResultMux sync.RWMutex
	prioritizer       *prioritization.Prioritizer
}

type PRResponse struct {
	Owner           string   `json:"owner"`
	Repo            string   `json:"repo"`
	Number          int      `json:"number"`
	CommitSHA       string   `json:"commit_sha"`
	LastReviewedAt  *string  `json:"last_reviewed_at"`
	ReviewHTMLPath  string   `json:"review_html_path"`
	GitHubURL       string   `json:"github_url"`
	ReviewURL       string   `json:"review_url"`
	Status          string   `json:"status"` // "pending", "generating", "completed", "error"
	Title           string   `json:"title"`
	Author          string   `json:"author"`
	GeneratingSince *string  `json:"generating_since"`
	IsMine          bool     `json:"is_mine"`
	MyReviewStatus  string   `json:"my_review_status"` // "APPROVED", "CHANGES_REQUESTED", "COMMENTED", or ""
	ApprovalCount   int      `json:"approval_count"`   // Number of current approvals
	Draft           bool     `json:"draft"`            // true if PR is in draft mode
	Notes           string   `json:"notes"`            // User notes (max 15 chars)
	CIState         string   `json:"ci_state"`         // "success", "failure", "pending", "unknown"
	CIFailedChecks  []string `json:"ci_failed_checks"` // Names of failed checks
}

func New(cfg *config.Config, database *db.DB, ghClient *github.Client) *Server {
	prioritizer := prioritization.New(database, ghClient, cfg.GitHubUsername)

	return &Server{
		cfg:         cfg,
		db:          database,
		ghClient:    ghClient,
		startTime:   time.Now(),
		prioritizer: prioritizer,
	}
}

func (s *Server) SetPoller(p PollerInterface) {
	s.poller = p
}

func (s *Server) UpdatePRCache(prs []github.PullRequest) {
	s.prCacheMux.Lock()
	defer s.prCacheMux.Unlock()
	s.prCache = prs
}

func (s *Server) GetCachedPRs() []github.PullRequest {
	s.prCacheMux.RLock()
	defer s.prCacheMux.RUnlock()
	// Return a copy to avoid race conditions
	result := make([]github.PullRequest, len(s.prCache))
	copy(result, s.prCache)
	return result
}

func (s *Server) SetPollTrigger(f func()) {
	s.pollTriggerFunc = f
}

func (s *Server) Start() error {
	// API routes
	http.HandleFunc("/api/prs", s.handleGetPRs)
	http.HandleFunc("/api/prs/delete", s.handleDeletePR)
	http.HandleFunc("/api/prs/notes", s.handleUpdatePRNotes)
	http.HandleFunc("/api/status", s.handleStatus)
	http.HandleFunc("/api/priorities", s.handleGetPriorities)
	http.Handle("/reviews/", http.StripPrefix("/reviews/", http.FileServer(http.Dir(s.cfg.ReviewsDir))))

	// Frontend: Serve React app
	http.HandleFunc("/", s.handleReactApp)

	addr := ":" + s.cfg.ServerPort
	log.Printf("Starting server on http://localhost%s", addr)
	return http.ListenAndServe(addr, nil)
}

// StartPrioritization starts the background prioritization job
// Should be called after server is initialized
func (s *Server) StartPrioritization(ctx context.Context) {
	// Run immediately on startup
	go s.updatePriorities(ctx)

	// Then run every 30 minutes
	ticker := time.NewTicker(30 * time.Minute)
	go func() {
		for {
			select {
			case <-ticker.C:
				s.updatePriorities(ctx)
			case <-ctx.Done():
				ticker.Stop()
				log.Println("[PRIORITIZATION] Stopping prioritization service")
				return
			}
		}
	}()

	log.Println("[PRIORITIZATION] Started prioritization service (runs every 30 minutes)")
}

// updatePriorities calculates priorities and updates the cache
func (s *Server) updatePriorities(ctx context.Context) {
	result, err := s.prioritizer.Calculate(ctx)
	if err != nil {
		log.Printf("[PRIORITIZATION] Error calculating priorities: %v", err)
		return
	}

	s.priorityResultMux.Lock()
	s.priorityResult = result
	s.priorityResultMux.Unlock()

	log.Printf("[PRIORITIZATION] Updated priorities: %d PRs scored", result.TotalPRsScored)
}

func (s *Server) handleGetPRs(w http.ResponseWriter, r *http.Request) {
	// Prevent caching of API responses
	w.Header().Set("Cache-Control", "no-cache, no-store, must-revalidate")
	w.Header().Set("Pragma", "no-cache")
	w.Header().Set("Expires", "0")

	// Fetch all PRs from database (source of truth)
	dbPRs, err := s.db.GetAllPRs()
	if err != nil {
		http.Error(w, "Failed to fetch PRs from database", http.StatusInternalServerError)
		return
	}

	// Try to get cached GitHub data to fill in titles/URLs if available
	githubPRs := s.GetCachedPRs()
	githubMap := make(map[string]github.PullRequest)
	for _, ghPR := range githubPRs {
		key := fmt.Sprintf("%s/%s/%d", ghPR.Owner, ghPR.Repo, ghPR.Number)
		githubMap[key] = ghPR
	}

	response := make([]PRResponse, 0, len(dbPRs))
	for _, dbPR := range dbPRs {
		var reviewedAt *string
		var generatingSince *string

		if dbPR.LastReviewedAt != nil {
			formatted := dbPR.LastReviewedAt.UTC().Format("2006-01-02T15:04:05Z")
			reviewedAt = &formatted
		}
		if dbPR.GeneratingSince != nil {
			formatted := dbPR.GeneratingSince.UTC().Format("2006-01-02T15:04:05Z")
			generatingSince = &formatted
		}

		// Try to get GitHub URL from cache, fallback to constructed URL
		key := fmt.Sprintf("%s/%s/%d", dbPR.RepoOwner, dbPR.RepoName, dbPR.PRNumber)
		ghPR, hasCachedData := githubMap[key]

		title := dbPR.Title
		if title == "" {
			title = fmt.Sprintf("PR #%d", dbPR.PRNumber)
		}

		author := dbPR.Author
		if author == "" {
			author = "Unknown"
		}

		githubURL := fmt.Sprintf("https://github.com/%s/%s/pull/%d", dbPR.RepoOwner, dbPR.RepoName, dbPR.PRNumber)
		if hasCachedData {
			githubURL = ghPR.URL
		}

		// Parse CI failed checks JSON array
		var ciFailedChecks []string
		if dbPR.CIFailedChecks != "" && dbPR.CIFailedChecks != "[]" {
			json.Unmarshal([]byte(dbPR.CIFailedChecks), &ciFailedChecks)
		}
		if ciFailedChecks == nil {
			ciFailedChecks = []string{}
		}

		response = append(response, PRResponse{
			Owner:           dbPR.RepoOwner,
			Repo:            dbPR.RepoName,
			Number:          dbPR.PRNumber,
			CommitSHA:       dbPR.LastCommitSHA,
			Title:           title,
			Author:          author,
			LastReviewedAt:  reviewedAt,
			ReviewHTMLPath:  dbPR.ReviewHTMLPath,
			GitHubURL:       githubURL,
			ReviewURL:       filepath.Join("/reviews", dbPR.ReviewHTMLPath),
			Status:          dbPR.Status,
			GeneratingSince: generatingSince,
			IsMine:          dbPR.IsMine,
			MyReviewStatus:  dbPR.MyReviewStatus,
			ApprovalCount:   dbPR.ApprovalCount,
			Draft:           dbPR.Draft,
			Notes:           dbPR.Notes,
			CIState:         dbPR.CIState,
			CIFailedChecks:  ciFailedChecks,
		})
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(response)
}

func (s *Server) handleDeletePR(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost && r.Method != http.MethodDelete {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	// Parse request body
	var req struct {
		Owner  string `json:"owner"`
		Repo   string `json:"repo"`
		Number int    `json:"number"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, fmt.Sprintf("Invalid request: %v", err), http.StatusBadRequest)
		return
	}

	// Get PR from DB to find HTML file
	pr, err := s.db.GetPR(req.Owner, req.Repo, req.Number)
	if err != nil {
		http.Error(w, fmt.Sprintf("Failed to get PR: %v", err), http.StatusInternalServerError)
		return
	}

	// Delete HTML file if it exists
	if pr != nil && pr.ReviewHTMLPath != "" {
		htmlPath := filepath.Join(s.cfg.ReviewsDir, pr.ReviewHTMLPath)
		if err := os.Remove(htmlPath); err != nil && !os.IsNotExist(err) {
			log.Printf("Warning: failed to delete HTML file %s: %v", htmlPath, err)
		}
	}

	// Delete from database
	if err := s.db.DeletePR(req.Owner, req.Repo, req.Number); err != nil {
		http.Error(w, fmt.Sprintf("Failed to delete PR: %v", err), http.StatusInternalServerError)
		return
	}

	log.Printf("Deleted review for %s/%s#%d", req.Owner, req.Repo, req.Number)

	// Trigger immediate poll to regenerate review
	if s.pollTriggerFunc != nil {
		s.pollTriggerFunc()
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{"status": "success"})
}

func (s *Server) handleUpdatePRNotes(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost && r.Method != http.MethodPatch {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var req struct {
		Owner  string `json:"owner"`
		Repo   string `json:"repo"`
		Number int    `json:"number"`
		Notes  string `json:"notes"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, fmt.Sprintf("Invalid request: %v", err), http.StatusBadRequest)
		return
	}

	// Validate length
	if len(req.Notes) > 15 {
		http.Error(w, "Notes must be 15 characters or less", http.StatusBadRequest)
		return
	}

	// Update database
	if err := s.db.UpdatePRNotes(req.Owner, req.Repo, req.Number, req.Notes); err != nil {
		http.Error(w, fmt.Sprintf("Failed to update notes: %v", err), http.StatusInternalServerError)
		return
	}

	log.Printf("Updated notes for %s/%s#%d: %q", req.Owner, req.Repo, req.Number, req.Notes)

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{
		"status": "success",
		"notes":  req.Notes,
	})
}

func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) {
	// Prevent caching of API responses
	w.Header().Set("Cache-Control", "no-cache, no-store, must-revalidate")
	w.Header().Set("Pragma", "no-cache")
	w.Header().Set("Expires", "0")

	// Get PR counts by status
	prs, err := s.db.GetAllPRs()
	if err != nil {
		http.Error(w, fmt.Sprintf("Failed to get PRs: %v", err), http.StatusInternalServerError)
		return
	}

	counts := map[string]int{
		"completed":  0,
		"generating": 0,
		"pending":    0,
		"error":      0,
	}
	for _, pr := range prs {
		counts[pr.Status]++
	}

	// Get cbpr status from poller
	var cbprRunning bool
	var cbprDuration time.Duration
	var secondsUntilNextPoll int
	if s.poller != nil {
		cbprRunning, cbprDuration = s.poller.GetCbprStatus()
		// Get accurate countdown based on ticker timing
		secondsUntilNextPoll = s.poller.GetSecondsUntilNextPoll()
	}

	// Get recent completions (last 3)
	recentCompletions := []map[string]interface{}{}
	completedCount := 0
	for i := len(prs) - 1; i >= 0 && completedCount < 3; i-- {
		if prs[i].Status == "completed" && prs[i].LastReviewedAt != nil {
			recentCompletions = append(recentCompletions, map[string]interface{}{
				"number":     prs[i].PRNumber,
				"repo":       fmt.Sprintf("%s/%s", prs[i].RepoOwner, prs[i].RepoName),
				"reviewed_at": prs[i].LastReviewedAt.Format(time.RFC3339),
			})
			completedCount++
		}
	}

	// Count PRs with missing metadata
	missingMetadataCount := 0
	for _, pr := range prs {
		if pr.Title == "" || pr.Author == "" {
			missingMetadataCount++
		}
	}

	// Get GitHub API rate limit status (cached to avoid excessive API calls)
	// Web client polls every 1 second, so we cache for 30 seconds
	s.rateLimitCacheMux.RLock()
	cachedInfo := s.rateLimitCache
	cacheAge := time.Since(s.rateLimitCacheTime)
	s.rateLimitCacheMux.RUnlock()

	// Refresh cache if older than 30 seconds or not set
	if cachedInfo == nil || cacheAge > 30*time.Second {
		ctx := r.Context()
		freshInfo, err := s.ghClient.GetRateLimitInfo(ctx)
		if err == nil {
			s.rateLimitCacheMux.Lock()
			s.rateLimitCache = freshInfo
			s.rateLimitCacheTime = time.Now()
			cachedInfo = freshInfo
			s.rateLimitCacheMux.Unlock()
		} else {
			log.Printf("[STATUS] Warning: Failed to refresh rate limit info: %v", err)
			// Keep using old cache if we have it
		}
	}

	rateLimitData := map[string]interface{}{
		"remaining": 0,
		"limit":     5000,
		"reset_at":  "",
		"is_limited": true,
		"error":     "",
	}
	if cachedInfo != nil {
		rateLimitData["remaining"] = cachedInfo.Remaining
		rateLimitData["limit"] = cachedInfo.Limit
		rateLimitData["reset_at"] = cachedInfo.ResetTime.Format(time.RFC3339)
		rateLimitData["is_limited"] = cachedInfo.Remaining < 10
	}

	response := map[string]interface{}{
		"uptime_seconds":           int(time.Since(s.startTime).Seconds()),
		"cbpr_running":             cbprRunning,
		"cbpr_duration_seconds":    int(cbprDuration.Seconds()),
		"counts":                   counts,
		"recent_completions":       recentCompletions,
		"missing_metadata_count":   missingMetadataCount,
		"timestamp":                time.Now().Unix(),
		"seconds_until_next_poll":  secondsUntilNextPoll,
		"rate_limit":               rateLimitData,
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(response)
}

func (s *Server) handleGetPriorities(w http.ResponseWriter, r *http.Request) {
	// Prevent caching of API responses
	w.Header().Set("Cache-Control", "no-cache, no-store, must-revalidate")
	w.Header().Set("Pragma", "no-cache")
	w.Header().Set("Expires", "0")

	s.priorityResultMux.RLock()
	result := s.priorityResult
	s.priorityResultMux.RUnlock()

	if result == nil {
		// Return empty result if prioritization hasn't run yet
		result = &prioritization.Result{
			Timestamp:      time.Now(),
			TopPRs:         []prioritization.PrioritizedPR{},
			TotalPRsScored: 0,
		}
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(result)
}

// handleReactApp serves the embedded React production build with SPA routing support
func (s *Server) handleReactApp(w http.ResponseWriter, r *http.Request) {
	// Prevent caching of HTML
	w.Header().Set("Cache-Control", "no-cache, no-store, must-revalidate")
	w.Header().Set("Pragma", "no-cache")
	w.Header().Set("Expires", "0")

	path := r.URL.Path
	if path == "/" {
		path = "dist/index.html"
	} else {
		path = "dist" + path
	}

	// Try to read the requested file
	content, err := reactDist.ReadFile(path)
	if err != nil {
		// File not found - serve index.html for SPA routing
		content, err = reactDist.ReadFile("dist/index.html")
		if err != nil {
			http.Error(w, "Failed to load application", http.StatusInternalServerError)
			log.Printf("Error serving React app: %v", err)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
	} else {
		// Set content type based on file extension
		contentType := getContentType(path)
		w.Header().Set("Content-Type", contentType)

		// Cache static assets (not HTML)
		if !strings.HasSuffix(path, ".html") {
			w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
		}
	}

	w.Write(content)
}

// getContentType returns appropriate content type for file extension
func getContentType(path string) string {
	if strings.HasSuffix(path, ".html") {
		return "text/html; charset=utf-8"
	} else if strings.HasSuffix(path, ".js") {
		return "application/javascript"
	} else if strings.HasSuffix(path, ".css") {
		return "text/css"
	} else if strings.HasSuffix(path, ".json") {
		return "application/json"
	} else if strings.HasSuffix(path, ".png") {
		return "image/png"
	} else if strings.HasSuffix(path, ".svg") {
		return "image/svg+xml"
	} else if strings.HasSuffix(path, ".ico") {
		return "image/x-icon"
	}
	return "application/octet-stream"
}
