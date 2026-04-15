package server

import (
	"context"
	"embed"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"pr-review-server/auth"
	"pr-review-server/config"
	"pr-review-server/db"
	"pr-review-server/gcs"
	"pr-review-server/github"

	"github.com/gorilla/websocket"
)

//go:embed dist
var reactDist embed.FS

// WebSocket event types
const (
	EventPRCreated = "pr_created"
	EventPRUpdated = "pr_updated"
	EventPRDeleted = "pr_deleted"
)

type PollerInterface interface {
	GetReviewerStatus() (running bool, duration time.Duration)
	GetLastPollTime() time.Time
	GetPollingInterval() time.Duration
	GetSecondsUntilNextPoll() int
	ProcessReviewImmediate(ctx context.Context, owner, repo string, number int, commitSHA, title, author string, createdAt *time.Time, draft bool)
	IsReviewTracked(owner, repo string, number int) bool
}

// AuthHandler interface for authentication
type AuthHandler interface {
	HandleLogin(w http.ResponseWriter, r *http.Request)
	HandleCallback(w http.ResponseWriter, r *http.Request)
	HandleLogout(w http.ResponseWriter, r *http.Request)
	Middleware(next http.Handler) http.Handler
	DevModeMiddleware(next http.Handler) http.Handler
}

type Server struct {
	cfg             *config.Config
	db              db.Database
	ghClient        *github.Client
	gcsClient       *gcs.Client
	auth            AuthHandler
	prCache         []github.PullRequest
	prCacheMux      sync.RWMutex
	pollTriggerFunc func()
	poller          PollerInterface
	startTime       time.Time
	// Cache for rate limit info to avoid calling GitHub API on every status request
	rateLimitCache     *github.RateLimitInfo
	rateLimitCacheMux  sync.RWMutex
	rateLimitCacheTime time.Time
	// Cached dev user ID for WebSocket broadcasts (resolved lazily)
	devUserID  int
	devUserMux sync.RWMutex
	// WebSocket connections
	upgrader    websocket.Upgrader
	clients     map[*websocket.Conn]*wsClient
	clientsMux  sync.RWMutex
	broadcastCh chan wsOutboundMessage
}

// reviewURL returns the review URL path if htmlPath is set, otherwise empty string
func reviewURL(htmlPath string) string {
	if htmlPath == "" {
		return ""
	}
	return filepath.Join("/reviews", htmlPath)
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
	ApprovalCount   int      `json:"approval_count"`   // Number of current approvals
	MyReviewStatus  string   `json:"my_review_status"` // "APPROVED", "CHANGES_REQUESTED", "COMMENTED", or ""
	Draft           bool     `json:"draft"`            // true if PR is in draft mode
	CIState         string   `json:"ci_state"`         // "success", "failure", "pending", "unknown"
	CIFailedChecks  []string `json:"ci_failed_checks"` // Names of failed checks
	CreatedAt       *string  `json:"created_at"`       // PR creation timestamp from GitHub
	IsMine          bool     `json:"is_mine"`          // true if current user is the PR author
	ViaTeams        []string `json:"via_teams"`        // Team names that caused this review request
	// Review importance counts
	CriticalCount int `json:"critical_count"` // Number of CRITICAL importance comments
	MediumCount   int `json:"medium_count"`   // Number of MEDIUM importance comments
	LowCount      int `json:"low_count"`      // Number of LOW importance comments
	// User notes
	Notes string `json:"notes"`
}

// WebSocket message types

type WebSocketMessage struct {
	Type string `json:"type"`

	Payload interface{} `json:"payload"`
}

type inboundWebSocketMessage struct {
	Type    string          `json:"type"`
	Payload json.RawMessage `json:"payload"`
}

type wsClient struct {
	UserID         int
	GitHubUsername string
}

type wsOutboundMessage struct {
	Type         string
	Payload      interface{}
	TargetUserID int
}

func New(cfg *config.Config, database db.Database, ghClient *github.Client, gcsClient *gcs.Client) *Server {
	return &Server{
		cfg:       cfg,
		db:        database,
		ghClient:  ghClient,
		gcsClient: gcsClient,
		startTime: time.Now(),
		upgrader: websocket.Upgrader{
			ReadBufferSize:  1024,
			WriteBufferSize: 1024,
			CheckOrigin: func(r *http.Request) bool {
				// Allow all connections
				return true
			},
		},
		clients:     make(map[*websocket.Conn]*wsClient),
		broadcastCh: make(chan wsOutboundMessage),
	}
}

func (s *Server) SetPoller(p PollerInterface) {
	s.poller = p
}

func (s *Server) SetAuth(a AuthHandler) {
	s.auth = a
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
	// Start broadcaster
	go s.broadcaster()

	// Determine which auth middleware to use
	var authMiddleware func(http.Handler) http.Handler
	if s.cfg.IsDevMode() {
		authMiddleware = s.auth.DevModeMiddleware
		log.Printf("[AUTH] Using dev mode authentication (auto-login)")
	} else {
		authMiddleware = s.auth.Middleware
		log.Printf("[AUTH] Using OAuth authentication")

		// OAuth routes (not protected)
		http.HandleFunc("/login", s.auth.HandleLogin)
		http.HandleFunc("/auth/github/callback", s.auth.HandleCallback)
		http.HandleFunc("/logout", s.auth.HandleLogout)
	}

	// Helper to wrap handler with auth middleware
	withAuth := func(handler http.HandlerFunc) http.Handler {
		return authMiddleware(http.HandlerFunc(handler))
	}

	// API routes (protected)
	http.Handle("/api/prs", withAuth(s.handleGetPRs))
	http.Handle("/api/prs/delete", withAuth(s.handleDeletePR))
	http.Handle("/api/prs/notes", withAuth(s.handleUpdatePRNotes))
	http.Handle("/api/prs/trigger-review", withAuth(s.handleTriggerReview))
	http.Handle("/api/poll/trigger", withAuth(s.handleTriggerPoll))
	http.Handle("/api/status", withAuth(s.handleStatus))
	http.Handle("/api/reviewer-health", withAuth(s.handleReviewerHealth))
	http.Handle("/api/settings", withAuth(s.handleSettings))
	http.Handle("/api/user", withAuth(s.handleGetUser))
	http.Handle("/api/telemetry/track", withAuth(s.handleTrackTelemetry))
	http.Handle("/api/telemetry/stats", withAuth(s.handleTelemetryStats))

	// Static content (protected - reviews contain sensitive code)
	http.Handle("/reviews/", withAuth(s.handleReviewFromGCS))

	// WebSocket route (not protected - uses session-based auth internally if needed)
	http.HandleFunc("/ws", s.handleWebSocket)

	// Frontend: Serve React app (not protected - handles own auth redirect)
	http.HandleFunc("/", s.handleReactApp)

	addr := ":" + s.cfg.ServerPort
	log.Printf("Starting server on http://localhost%s", addr)
	return http.ListenAndServe(addr, nil)
}

func (s *Server) handleGetPRs(w http.ResponseWriter, r *http.Request) {
	// Prevent caching of API responses
	w.Header().Set("Cache-Control", "no-cache, no-store, must-revalidate")
	w.Header().Set("Pragma", "no-cache")
	w.Header().Set("Expires", "0")

	// User is always required (auth middleware ensures this)
	user := auth.GetCurrentUser(r)
	if user == nil {
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}

	// Fetch PRs with user-specific view data
	prsWithViews, err := s.db.GetPRsForUserWithNotes(user.ID)
	if err != nil {
		log.Printf("[API] Error fetching PRs for user %d: %v", user.ID, err)
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

	response := make([]PRResponse, 0, len(prsWithViews))
	for _, prView := range prsWithViews {
		dbPR := prView.PR
		var reviewedAt *string
		var generatingSince *string
		var createdAt *string

		if dbPR.LastReviewedAt != nil {
			formatted := dbPR.LastReviewedAt.UTC().Format("2006-01-02T15:04:05Z")
			reviewedAt = &formatted
		}
		if dbPR.GeneratingSince != nil {
			formatted := dbPR.GeneratingSince.UTC().Format("2006-01-02T15:04:05Z")
			generatingSince = &formatted
		}
		if dbPR.CreatedAt != nil {
			formatted := dbPR.CreatedAt.UTC().Format("2006-01-02T15:04:05Z")
			createdAt = &formatted
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
			_ = json.Unmarshal([]byte(dbPR.CIFailedChecks), &ciFailedChecks) // nolint:errcheck
		}
		if ciFailedChecks == nil {
			ciFailedChecks = []string{}
		}

		// Use user-specific notes if available, otherwise fall back to PR notes
		notes := prView.UserNotes
		if notes == "" {
			notes = dbPR.Notes
		}

		// Ensure ViaTeams is never nil (JSON null), always at least empty array
		viaTeams := prView.ViaTeams
		if viaTeams == nil {
			viaTeams = []string{}
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
			ReviewURL:       reviewURL(dbPR.ReviewHTMLPath),
			Status:          dbPR.Status,
			GeneratingSince: generatingSince,
			ApprovalCount:   dbPR.ApprovalCount,
			MyReviewStatus:  prView.ReviewStatus, // Use user-specific review status
			Draft:           dbPR.Draft,
			CIState:         dbPR.CIState,
			CIFailedChecks:  ciFailedChecks,
			CreatedAt:       createdAt,
			IsMine:          prView.IsAuthor, // Use IsAuthor from user_pr_views
			ViaTeams:        viaTeams,
			CriticalCount:   dbPR.CriticalCount,
			MediumCount:     dbPR.MediumCount,
			LowCount:        dbPR.LowCount,
			Notes:           notes,
		})
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(response) // nolint:errcheck
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

	// User is always required (auth middleware ensures this)
	user := auth.GetCurrentUser(r)
	if user == nil {
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}

	// Get the PR to find its ID
	pr, err := s.db.GetPR(req.Owner, req.Repo, req.Number)
	if err != nil {
		http.Error(w, fmt.Sprintf("Failed to get PR: %v", err), http.StatusInternalServerError)
		return
	}
	if pr == nil {
		http.Error(w, "PR not found", http.StatusNotFound)
		return
	}

	// Hide the PR for this user (soft delete)
	if err := s.db.HidePRForUser(user.ID, pr.ID); err != nil {
		http.Error(w, fmt.Sprintf("Failed to hide PR: %v", err), http.StatusInternalServerError)
		return
	}

	log.Printf("[API] User %d hid PR %s/%s#%d", user.ID, req.Owner, req.Repo, req.Number)

	// Notify only this user's clients. Hidden PRs are user-specific.
	s.BroadcastEventToUser(user.ID, EventPRDeleted, map[string]interface{}{
		"owner":  req.Owner,
		"repo":   req.Repo,
		"number": req.Number,
	})

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]string{"status": "success"}) // nolint:errcheck
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

	// User is always required (auth middleware ensures this)
	user := auth.GetCurrentUser(r)
	if user == nil {
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}

	// Get PR to find its ID
	pr, err := s.db.GetPR(req.Owner, req.Repo, req.Number)
	if err != nil || pr == nil {
		http.Error(w, "PR not found", http.StatusNotFound)
		return
	}

	// Update user-specific notes in user_pr_views
	if err := s.db.UpdateUserPRNotes(user.ID, pr.ID, req.Notes); err != nil {
		http.Error(w, fmt.Sprintf("Failed to update notes: %v", err), http.StatusInternalServerError)
		return
	}

	log.Printf("[API] Updated notes for user %d on %s/%s#%d: %q", user.ID, req.Owner, req.Repo, req.Number, req.Notes)

	// Notify only this user's clients. Notes are user-specific.
	s.BroadcastEventToUser(user.ID, EventPRUpdated, map[string]interface{}{
		"owner":  req.Owner,
		"repo":   req.Repo,
		"number": req.Number,
	})

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]string{ // nolint:errcheck
		"status": "success",
		"notes":  req.Notes,
	})
}

func (s *Server) handleTriggerReview(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var req struct {
		Owner  string `json:"owner"`
		Repo   string `json:"repo"`
		Number int    `json:"number"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, fmt.Sprintf("Invalid request: %v", err), http.StatusBadRequest)
		return
	}

	// Get existing PR from database
	pr, err := s.db.GetPR(req.Owner, req.Repo, req.Number)
	if err != nil {
		http.Error(w, fmt.Sprintf("Failed to get PR: %v", err), http.StatusInternalServerError)
		return
	}

	// If PR doesn't exist in database, we can't trigger a review
	// (It needs to be fetched from GitHub first via the poller)
	if pr == nil {
		http.Error(w, "PR not found in database. Wait for next poll cycle to fetch it from GitHub.", http.StatusNotFound)
		return
	}

	// Fetch the latest commit SHA from GitHub so the review is always against the current HEAD
	latestSHA, err := s.ghClient.GetPRHeadSHA(r.Context(), req.Owner, req.Repo, req.Number)
	if err != nil {
		http.Error(w, fmt.Sprintf("Failed to fetch latest commit from GitHub: %v", err), http.StatusInternalServerError)
		return
	}

	// Mark PR as generating with the latest SHA so the poller reviews the right commit
	if err := s.db.SetPRGenerating(req.Owner, req.Repo, req.Number, latestSHA, pr.Title, pr.Author, pr.CreatedAt, pr.Draft); err != nil {
		http.Error(w, fmt.Sprintf("Failed to update PR status: %v", err), http.StatusInternalServerError)
		return
	}

	log.Printf("[API] Manually triggered review generation for %s/%s#%d (commit: %s)", req.Owner, req.Repo, req.Number, latestSHA[:7])

	// Notify all clients. The broadcaster resolves user-specific fields per connection.
	s.BroadcastEvent(EventPRUpdated, map[string]interface{}{
		"owner":  req.Owner,
		"repo":   req.Repo,
		"number": req.Number,
	})

	// Start review immediately, bypassing the full poll cycle.
	// ProcessReviewImmediate tracks the review synchronously and spawns
	// a goroutine for the actual generation work.
	// Use background context since the review outlives this HTTP request.
	if s.poller != nil {
		s.poller.ProcessReviewImmediate(context.Background(), req.Owner, req.Repo, req.Number, latestSHA, pr.Title, pr.Author, pr.CreatedAt, pr.Draft)
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]string{"status": "success"}) // nolint:errcheck
}

func (s *Server) handleTriggerPoll(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	if s.pollTriggerFunc == nil {
		http.Error(w, "Poll trigger not configured", http.StatusInternalServerError)
		return
	}

	log.Printf("[API] Manual poll triggered")
	s.pollTriggerFunc()

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]string{"status": "poll triggered"})
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

	// Get reviewer status from poller
	var reviewerRunning bool
	var reviewerDuration time.Duration
	var secondsUntilNextPoll int
	if s.poller != nil {
		reviewerRunning, reviewerDuration = s.poller.GetReviewerStatus()
		// Get accurate countdown based on ticker timing
		secondsUntilNextPoll = s.poller.GetSecondsUntilNextPoll()
	}

	// Get recent completions (last 3)
	recentCompletions := []map[string]interface{}{}
	completedCount := 0
	for i := len(prs) - 1; i >= 0 && completedCount < 3; i-- {
		if prs[i].Status == "completed" && prs[i].LastReviewedAt != nil {
			recentCompletions = append(recentCompletions, map[string]interface{}{
				"number":      prs[i].PRNumber,
				"repo":        fmt.Sprintf("%s/%s", prs[i].RepoOwner, prs[i].RepoName),
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
			log.Printf("[STATUS] GitHub rate limit query failed (using cached): %v", err)
			// Keep using cached value if we have it
		}
	}

	rateLimitData := map[string]interface{}{
		"remaining":  0,
		"limit":      5000,
		"reset_at":   "",
		"is_limited": true,
		"error":      "",
	}
	if cachedInfo != nil {
		rateLimitData["remaining"] = cachedInfo.Remaining
		rateLimitData["limit"] = cachedInfo.Limit
		rateLimitData["reset_at"] = cachedInfo.ResetTime.Format(time.RFC3339)
		rateLimitData["is_limited"] = cachedInfo.Remaining < 10
	}

	response := map[string]interface{}{
		"uptime_seconds":            int(time.Since(s.startTime).Seconds()),
		"reviewer_running":          reviewerRunning,
		"reviewer_duration_seconds": int(reviewerDuration.Seconds()),
		"counts":                    counts,
		"recent_completions":        recentCompletions,
		"missing_metadata_count":    missingMetadataCount,
		"timestamp":                 time.Now().Unix(),
		"seconds_until_next_poll":   secondsUntilNextPoll,
		"rate_limit":                rateLimitData,
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(response) // nolint:errcheck
}

func (s *Server) handleReviewerHealth(w http.ResponseWriter, r *http.Request) {
	// Prevent caching of API responses
	w.Header().Set("Cache-Control", "no-cache, no-store, must-revalidate")
	w.Header().Set("Pragma", "no-cache")
	w.Header().Set("Expires", "0")

	// Get all PRs to analyze reviewer health
	prs, err := s.db.GetAllPRs()
	if err != nil {
		http.Error(w, fmt.Sprintf("Failed to get PRs: %v", err), http.StatusInternalServerError)
		return
	}

	// Look at MOST RECENT attempts only (last 2-3 completed PRs)
	// If the most recent attempts failed, reviewer is unhealthy RIGHT NOW
	recentAttempts := []string{} // Track recent statuses
	totalErrors := 0
	totalSuccess := 0
	totalAttempted := 0

	// Collect last 3 completed/error PRs (most recent first)
	for i := len(prs) - 1; i >= 0 && len(recentAttempts) < 3; i-- {
		pr := prs[i]
		if pr.Status == "completed" || pr.Status == "error" {
			recentAttempts = append(recentAttempts, pr.Status)
		}
	}

	// Also count totals across all PRs for stats
	for _, pr := range prs {
		if pr.Status == "completed" || pr.Status == "error" {
			totalAttempted++
			if pr.Status == "error" {
				totalErrors++
			} else {
				totalSuccess++
			}
		}
	}

	// Determine health status based on MOST RECENT attempts
	healthy := true
	reason := "Reviewer is working normally"
	recommendation := "show_columns"

	if len(recentAttempts) == 0 {
		// No PRs attempted yet - can't determine health
		healthy = false
		reason = "No reviews attempted yet - reviewer status unknown"
		recommendation = "hide_columns"
	} else if len(recentAttempts) >= 2 {
		// Check if last 2 attempts both failed
		if recentAttempts[0] == "error" && recentAttempts[1] == "error" {
			healthy = false
			reason = "Last 2 review attempts failed - reviewer service unavailable"
			recommendation = "hide_columns"
		} else if recentAttempts[0] == "error" {
			// Most recent failed, but previous succeeded
			healthy = false
			reason = "Most recent review attempt failed"
			recommendation = "show_columns" // Keep visible but warn
		}
	} else if recentAttempts[0] == "error" {
		// Only 1 attempt, and it failed
		healthy = false
		reason = "Most recent review attempt failed"
		recommendation = "hide_columns"
	}

	// Check if API key is configured (internal reviewer)
	reviewerExists := s.cfg.GeminiAPIKey != ""

	if !reviewerExists {
		healthy = false
		reason = "GEMINI_API_KEY is not set"
		recommendation = "hide_columns"
	}

	response := map[string]interface{}{
		"healthy":         healthy,
		"reason":          reason,
		"recommendation":  recommendation,
		"error_count":     totalErrors,
		"success_count":   totalSuccess,
		"attempted_count": totalAttempted,
		"reviewer_exists": reviewerExists,
		"reviewer_path":   "internal", // Indicate internal reviewer
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(response) // nolint:errcheck
}

func (s *Server) handleSettings(w http.ResponseWriter, r *http.Request) {
	// Prevent caching of API responses
	w.Header().Set("Cache-Control", "no-cache, no-store, must-revalidate")
	w.Header().Set("Pragma", "no-cache")
	w.Header().Set("Expires", "0")

	w.Header().Set("Content-Type", "application/json")

	switch r.Method {
	case http.MethodGet:
		// Get current settings
		autoReview, err := s.db.GetAutoReviewRequestedPRs()
		if err != nil {
			http.Error(w, fmt.Sprintf("Failed to get settings: %v", err), http.StatusInternalServerError)
			return
		}
		nRequests, err := s.db.GetReviewNRequests()
		if err != nil {
			log.Printf("[SETTINGS] Failed to get review_n_requests: %v", err)
		}
		generateHTML, err := s.db.GetGenerateHTML()
		if err != nil {
			log.Printf("[SETTINGS] Failed to get generate_html: %v", err)
		}

		response := map[string]interface{}{
			"auto_review_requested_prs": autoReview,
			"review_n_requests":         nRequests,
			"generate_html":             generateHTML,
		}
		_ = json.NewEncoder(w).Encode(response) // nolint:errcheck

	case http.MethodPost, http.MethodPatch:
		// Update settings
		var req struct {
			AutoReviewRequestedPRs *bool `json:"auto_review_requested_prs"`
			ReviewNRequests        *int  `json:"review_n_requests"`
			GenerateHTML           *bool `json:"generate_html"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, fmt.Sprintf("Invalid request: %v", err), http.StatusBadRequest)
			return
		}

		// Update the setting if provided
		if req.AutoReviewRequestedPRs != nil {
			if err := s.db.SetAutoReviewRequestedPRs(*req.AutoReviewRequestedPRs); err != nil {
				http.Error(w, fmt.Sprintf("Failed to update settings: %v", err), http.StatusInternalServerError)
				return
			}
			log.Printf("[SETTINGS] Updated auto_review_requested_prs to: %v", *req.AutoReviewRequestedPRs)
		}
		if req.ReviewNRequests != nil {
			if err := s.db.SetReviewNRequests(*req.ReviewNRequests); err != nil {
				http.Error(w, fmt.Sprintf("Failed to update settings: %v", err), http.StatusInternalServerError)
				return
			}
			log.Printf("[SETTINGS] Updated review_n_requests to: %v", *req.ReviewNRequests)
		}
		if req.GenerateHTML != nil {
			if err := s.db.SetGenerateHTML(*req.GenerateHTML); err != nil {
				http.Error(w, fmt.Sprintf("Failed to update settings: %v", err), http.StatusInternalServerError)
				return
			}
			log.Printf("[SETTINGS] Updated generate_html to: %v", *req.GenerateHTML)
		}

		// Return updated settings
		autoReview, _ := s.db.GetAutoReviewRequestedPRs()
		nRequests, _ := s.db.GetReviewNRequests()
		generateHTML, _ := s.db.GetGenerateHTML()

		response := map[string]interface{}{
			"auto_review_requested_prs": autoReview,
			"review_n_requests":         nRequests,
			"generate_html":             generateHTML,
		}
		_ = json.NewEncoder(w).Encode(response) // nolint:errcheck

	default:
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
	}
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
			log.Printf("[REACT] Failed to serve index.html: %v", err)
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

	_, _ = w.Write(content) // nolint:errcheck
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

func hashWebSocketDevUsername(username string) uint32 {
	var hash uint32 = 5381
	for _, c := range username {
		hash = ((hash << 5) + hash) + uint32(c)
	}
	return hash
}

func (s *Server) getOrCreateDevWebSocketUser() (*db.User, error) {
	username := s.cfg.GitHubUsername
	if username == "" {
		return nil, fmt.Errorf("GITHUB_USERNAME is required for dev websocket auth")
	}

	user, err := s.db.GetUserByUsername(username)
	if err != nil {
		return nil, fmt.Errorf("lookup dev websocket user: %w", err)
	}
	if user != nil {
		return user, nil
	}

	user = &db.User{
		GitHubID:        int64(hashWebSocketDevUsername(username)),
		GitHubUsername:  username,
		GitHubAvatarURL: fmt.Sprintf("https://github.com/%s.png", username),
	}
	if err := s.db.CreateUser(user); err != nil {
		return nil, fmt.Errorf("create dev websocket user: %w", err)
	}

	return user, nil
}

func (s *Server) authenticateWebSocketUser(r *http.Request) (*db.User, error) {
	if s.cfg.IsDevMode() {
		return s.getOrCreateDevWebSocketUser()
	}

	cookie, err := r.Cookie(auth.SessionCookieName)
	if err != nil || cookie.Value == "" {
		return nil, fmt.Errorf("missing session cookie")
	}

	session, err := s.db.GetSession(cookie.Value)
	if err != nil {
		return nil, fmt.Errorf("fetch session: %w", err)
	}
	if session == nil || session.ExpiresAt.Before(time.Now()) {
		return nil, fmt.Errorf("session expired or invalid")
	}

	user, err := s.db.GetUserByID(session.UserID)
	if err != nil {
		return nil, fmt.Errorf("fetch websocket user: %w", err)
	}
	if user == nil {
		return nil, fmt.Errorf("websocket user not found")
	}

	return user, nil
}

func (s *Server) handleWebSocketTelemetryMessage(userID int, payload json.RawMessage) {
	var batch telemetryBatchInput
	if err := json.Unmarshal(payload, &batch); err != nil {
		log.Printf("[WS] Invalid telemetry batch payload: %v", err)
		return
	}
	if len(batch.Events) == 0 {
		return
	}
	if err := s.ingestTelemetryEvents(userID, batch.Events); err != nil {
		log.Printf("[WS] Failed to persist telemetry batch: %v", err)
	}
}

func extractPRIdentity(payload interface{}) (string, string, int, bool) {
	switch value := payload.(type) {
	case map[string]interface{}:
		owner, ownerOK := value["owner"].(string)
		repo, repoOK := value["repo"].(string)
		number, numberOK := value["number"].(int)
		return owner, repo, number, ownerOK && repoOK && numberOK
	case *PRResponse:
		if value == nil {
			return "", "", 0, false
		}
		return value.Owner, value.Repo, value.Number, true
	case PRResponse:
		return value.Owner, value.Repo, value.Number, true
	default:
		return "", "", 0, false
	}
}

func (s *Server) resolveBroadcastPayloadForClient(client *wsClient, eventType string, payload interface{}) interface{} {
	if payload == nil {
		return nil
	}

	if eventType == EventPRCreated || eventType == EventPRUpdated {
		owner, repo, number, ok := extractPRIdentity(payload)
		if ok {
			return s.getPRResponseForUser(client.UserID, owner, repo, number)
		}
	}

	return payload
}

// handleWebSocket handles WebSocket connection requests
func (s *Server) handleWebSocket(w http.ResponseWriter, r *http.Request) {
	user, err := s.authenticateWebSocketUser(r)
	if err != nil {
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		log.Printf("[WS] Rejecting websocket connection from %s: %v", r.RemoteAddr, err)
		return
	}

	conn, err := s.upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Printf("[WS] Error upgrading connection: %v", err)
		return
	}
	defer conn.Close()

	client := &wsClient{
		UserID:         user.ID,
		GitHubUsername: user.GitHubUsername,
	}

	s.clientsMux.Lock()
	s.clients[conn] = client
	clientCount := len(s.clients)
	s.clientsMux.Unlock()

	log.Printf("[WS] Client connected from %s as %s (total: %d)", r.RemoteAddr, user.GitHubUsername, clientCount)

	defer func() {
		s.clientsMux.Lock()
		delete(s.clients, conn)
		clientCount := len(s.clients)
		s.clientsMux.Unlock()
		log.Printf("[WS] Client disconnected from %s as %s (remaining: %d)", r.RemoteAddr, user.GitHubUsername, clientCount)
	}()

	for {
		messageType, reader, err := conn.NextReader()
		if err != nil {
			break
		}
		if messageType != websocket.TextMessage && messageType != websocket.BinaryMessage {
			continue
		}

		body, err := io.ReadAll(reader)
		if err != nil {
			log.Printf("[WS] Error reading message body: %v", err)
			continue
		}

		var message inboundWebSocketMessage
		if err := json.Unmarshal(body, &message); err != nil {
			log.Printf("[WS] Ignoring malformed message from %s: %v", user.GitHubUsername, err)
			continue
		}

		switch message.Type {
		case "telemetry_batch":
			s.handleWebSocketTelemetryMessage(user.ID, message.Payload)
		default:
			log.Printf("[WS] Ignoring unknown client message type %q from %s", message.Type, user.GitHubUsername)
		}
	}
}

// broadcaster listens for messages on the broadcast channel and sends them to all clients
func (s *Server) broadcaster() {
	for {
		message := <-s.broadcastCh
		s.clientsMux.RLock()
		for conn, client := range s.clients {
			if message.TargetUserID > 0 && client.UserID != message.TargetUserID {
				continue
			}

			payload := s.resolveBroadcastPayloadForClient(client, message.Type, message.Payload)
			if payload == nil {
				continue
			}

			err := conn.WriteJSON(WebSocketMessage{
				Type:    message.Type,
				Payload: payload,
			})
			if err != nil {
				log.Printf("[WS] Error writing to client: %v", err)
				_ = conn.Close() // nolint:errcheck
				s.clientsMux.RUnlock()
				s.clientsMux.Lock()
				delete(s.clients, conn)
				s.clientsMux.Unlock()
				s.clientsMux.RLock()
			}
		}
		s.clientsMux.RUnlock()
	}
}

// BroadcastEvent sends an event to all connected WebSocket clients.
func (s *Server) BroadcastEvent(eventType string, payload interface{}) {
	s.broadcastCh <- wsOutboundMessage{
		Type:    eventType,
		Payload: payload,
	}
}

// BroadcastEventToUser sends an event only to a single user's connected clients.
func (s *Server) BroadcastEventToUser(userID int, eventType string, payload interface{}) {
	s.broadcastCh <- wsOutboundMessage{
		Type:         eventType,
		Payload:      payload,
		TargetUserID: userID,
	}
}

// getPRResponse constructs a PR response using the default dev-mode user context.
func (s *Server) getPRResponse(owner, repo string, number int) *PRResponse {
	return s.getPRResponseForUser(s.getDevUserID(), owner, repo, number)
}

// getDevUserID resolves the dev user's database ID, caching the result.
// Returns 0 if not in dev mode or if the user hasn't been created yet.
func (s *Server) getDevUserID() int {
	s.devUserMux.RLock()
	if s.devUserID > 0 {
		s.devUserMux.RUnlock()
		return s.devUserID
	}
	s.devUserMux.RUnlock()

	if s.cfg.GitHubUsername == "" {
		return 0
	}

	user, err := s.db.GetUserByUsername(s.cfg.GitHubUsername)
	if err != nil || user == nil {
		return 0
	}

	s.devUserMux.Lock()
	s.devUserID = user.ID
	s.devUserMux.Unlock()

	return user.ID
}

// getPRResponseForUser constructs a full PRResponse object from the database,
// including user-specific data from user_pr_views (via_teams, review status, etc.).
func (s *Server) getPRResponseForUser(userID int, owner, repo string, number int) *PRResponse {
	pr, err := s.db.GetPR(owner, repo, number)
	if err != nil || pr == nil {
		log.Printf("[WS] Error getting PR for broadcast: %v", err)
		return nil
	}

	var reviewedAt *string
	var generatingSince *string
	var createdAt *string

	if pr.LastReviewedAt != nil {
		formatted := pr.LastReviewedAt.UTC().Format("2006-01-02T15:04:05Z")
		reviewedAt = &formatted
	}
	if pr.GeneratingSince != nil {
		formatted := pr.GeneratingSince.UTC().Format("2006-01-02T15:04:05Z")
		generatingSince = &formatted
	}
	if pr.CreatedAt != nil {
		formatted := pr.CreatedAt.UTC().Format("2006-01-02T15:04:05Z")
		createdAt = &formatted
	}

	githubURL := fmt.Sprintf("https://github.com/%s/%s/pull/%d", pr.RepoOwner, pr.RepoName, pr.PRNumber)

	var ciFailedChecks []string
	if pr.CIFailedChecks != "" && pr.CIFailedChecks != "[]" {
		_ = json.Unmarshal([]byte(pr.CIFailedChecks), &ciFailedChecks) // nolint:errcheck
	}
	if ciFailedChecks == nil {
		ciFailedChecks = []string{}
	}

	var viaTeams []string
	myReviewStatus := pr.MyReviewStatus
	notes := pr.Notes
	author := pr.Author
	if author == "" {
		author = "Unknown"
	}
	isMine := strings.EqualFold(author, s.cfg.GitHubUsername)

	if userID > 0 {
		if assignment, err := s.db.GetUserPRAssignment(userID, pr.ID); err == nil && assignment != nil {
			_ = json.Unmarshal([]byte(assignment.ReviewerGroups), &viaTeams)
			if assignment.MyReviewStatus != "" {
				myReviewStatus = assignment.MyReviewStatus
			}
			if assignment.Notes != "" {
				notes = assignment.Notes
			}
			isMine = assignment.IsAuthor
		}
	}

	if viaTeams == nil {
		viaTeams = []string{}
	}

	return &PRResponse{
		Owner:           pr.RepoOwner,
		Repo:            pr.RepoName,
		Number:          pr.PRNumber,
		CommitSHA:       pr.LastCommitSHA,
		Title:           pr.Title,
		Author:          author,
		LastReviewedAt:  reviewedAt,
		ReviewHTMLPath:  pr.ReviewHTMLPath,
		GitHubURL:       githubURL,
		ReviewURL:       reviewURL(pr.ReviewHTMLPath),
		Status:          pr.Status,
		GeneratingSince: generatingSince,
		ApprovalCount:   pr.ApprovalCount,
		MyReviewStatus:  myReviewStatus,
		Draft:           pr.Draft,
		CIState:         pr.CIState,
		CIFailedChecks:  ciFailedChecks,
		CreatedAt:       createdAt,
		IsMine:          isMine,
		ViaTeams:        viaTeams,
		CriticalCount:   pr.CriticalCount,
		MediumCount:     pr.MediumCount,
		LowCount:        pr.LowCount,
		Notes:           notes,
	}
}

// handleReviewFromGCS serves review HTML files from GCS or local storage
// URL format: /reviews/{filename}
func (s *Server) handleReviewFromGCS(w http.ResponseWriter, r *http.Request) {
	// Extract filename from path (remove /reviews/ prefix)
	filename := strings.TrimPrefix(r.URL.Path, "/reviews/")
	if filename == "" {
		http.Error(w, "Review filename required", http.StatusBadRequest)
		return
	}

	// Prevent path traversal attacks
	cleaned := filepath.Clean(filename)
	if strings.Contains(cleaned, "..") || filepath.IsAbs(cleaned) {
		http.Error(w, "Invalid filename", http.StatusBadRequest)
		return
	}
	filename = cleaned

	var content []byte
	var err error

	// Try GCS first if bucket is configured
	if s.gcsClient != nil && s.gcsClient.BucketName() != "" {
		ctx := context.Background()
		content, err = s.gcsClient.GetReviewContent(ctx, filename)
		if err == nil {
			// Successfully fetched from GCS
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			w.Header().Set("Cache-Control", "private, max-age=31536000, immutable")
			_, _ = w.Write(content) // nolint:errcheck
			return
		}
		// Log GCS error but continue to try local
		if !strings.Contains(err.Error(), "not found") {
			log.Printf("[GCS] Error fetching review %s: %v, trying local", filename, err)
		}
	}

	// Fall back to local file storage
	localPath := filepath.Join(s.cfg.ReviewsDir, filename)
	content, err = os.ReadFile(localPath)
	if err != nil {
		if os.IsNotExist(err) {
			http.Error(w, "Review not found", http.StatusNotFound)
		} else {
			log.Printf("[LOCAL] Error reading review %s: %v", localPath, err)
			http.Error(w, "Failed to fetch review", http.StatusInternalServerError)
		}
		return
	}

	// Set headers
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "private, max-age=31536000, immutable")

	_, _ = w.Write(content) // nolint:errcheck
}
