package server

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"time"

	"pr-review-server/config"
	"pr-review-server/db"
	"pr-review-server/github"
)

//go:embed templates/index.html
var indexHTML string

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
}

type PRResponse struct {
	Owner           string  `json:"owner"`
	Repo            string  `json:"repo"`
	Number          int     `json:"number"`
	CommitSHA       string  `json:"commit_sha"`
	LastReviewedAt  *string `json:"last_reviewed_at"`
	ReviewHTMLPath  string  `json:"review_html_path"`
	GitHubURL       string  `json:"github_url"`
	ReviewURL       string  `json:"review_url"`
	Status          string  `json:"status"` // "pending", "generating", "completed", "error"
	Title           string  `json:"title"`
	Author          string  `json:"author"`
	GeneratingSince *string `json:"generating_since"`
	IsMine          bool    `json:"is_mine"`
	MyReviewStatus  string  `json:"my_review_status"` // "APPROVED", "CHANGES_REQUESTED", "COMMENTED", or ""
	ApprovalCount   int     `json:"approval_count"`   // Number of current approvals
	Draft           bool    `json:"draft"`            // true if PR is in draft mode
}

func New(cfg *config.Config, database *db.DB, ghClient *github.Client) *Server {
	return &Server{
		cfg:       cfg,
		db:        database,
		ghClient:  ghClient,
		startTime: time.Now(),
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
	http.HandleFunc("/", s.handleIndex)
	http.HandleFunc("/api/prs", s.handleGetPRs)
	http.HandleFunc("/api/prs/delete", s.handleDeletePR)
	http.HandleFunc("/api/status", s.handleStatus)
	http.Handle("/reviews/", http.StripPrefix("/reviews/", http.FileServer(http.Dir(s.cfg.ReviewsDir))))

	addr := ":" + s.cfg.ServerPort
	log.Printf("Starting server on http://localhost%s", addr)
	return http.ListenAndServe(addr, nil)
}

func (s *Server) handleIndex(w http.ResponseWriter, r *http.Request) {
	// Prevent caching of the HTML page
	w.Header().Set("Cache-Control", "no-cache, no-store, must-revalidate")
	w.Header().Set("Pragma", "no-cache")
	w.Header().Set("Expires", "0")

	w.Header().Set("Content-Type", "text/html")
	w.Write([]byte(indexHTML))
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
