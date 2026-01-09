package server

import (
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

	html := `<!DOCTYPE html>
<html>
<head>
    <meta charset="UTF-8">
    <title>PR Review Dashboard</title>
    <link rel="icon" type="image/svg+xml" href="data:image/svg+xml,%3Csvg xmlns='http://www.w3.org/2000/svg' viewBox='0 0 100 100'%3E%3Crect width='100' height='100' fill='%230d1117'/%3E%3Cpath d='M20 50 L40 70 L80 30' stroke='%237ee787' stroke-width='8' fill='none' stroke-linecap='round' stroke-linejoin='round'/%3E%3C/svg%3E">
    <style>
        * { box-sizing: border-box; }
        body {
            font-family: -apple-system, BlinkMacSystemFont, "Segoe UI", Roboto, "Helvetica Neue", Arial, sans-serif;
            max-width: 1600px;
            margin: 0 auto;
            padding: 12px;
            background: #0d1117;
            color: #c9d1d9;
            font-size: 13px;
        }
        h1 {
            color: #58a6ff;
            font-size: 20px;
            font-weight: 600;
            margin: 0 0 12px 0;
            padding: 0;
        }
        table {
            width: 100%;
            background: #161b22;
            border-collapse: collapse;
            border: 1px solid #30363d;
            border-radius: 6px;
            overflow: hidden;
            font-size: 12px;
        }
        th, td {
            padding: 6px 10px;
            text-align: left;
            border-bottom: 1px solid #21262d;
        }
        th {
            background: #21262d;
            color: #8b949e;
            font-weight: 600;
            font-size: 11px;
            text-transform: uppercase;
            letter-spacing: 0.5px;
        }
        tr:hover {
            background: #1c2128;
        }
        tr:last-child td {
            border-bottom: none;
        }
        a {
            color: #58a6ff;
            text-decoration: none;
        }
        a:hover {
            text-decoration: underline;
        }
        .status {
            font-size: 12px;
            color: #c9d1d9;
            margin-bottom: 16px;
            padding: 10px 12px;
            background: #161b22;
            border: 1px solid #30363d;
            border-radius: 6px;
            display: flex;
            gap: 24px;
            align-items: center;
            flex-wrap: wrap;
        }
        .status-item {
            display: flex;
            align-items: center;
            gap: 6px;
        }
        .status-label {
            color: #7d8590;
            font-size: 11px;
        }
        .status-value {
            font-weight: 600;
            color: #58a6ff;
        }
        .status-dot {
            width: 8px;
            height: 8px;
            border-radius: 50%;
            background: #7ee787;
            animation: pulse 2s ease-in-out infinite;
        }
        @keyframes pulse {
            0%, 100% { opacity: 1; }
            50% { opacity: 0.5; }
        }
        .loading {
            text-align: center;
            padding: 20px;
            color: #7d8590;
        }
        .error {
            background: #da3633;
            color: white;
            padding: 8px 12px;
            border-radius: 6px;
            margin-bottom: 12px;
            font-size: 12px;
        }
        .commit-sha {
            font-family: ui-monospace, SFMono-Regular, "SF Mono", Menlo, Consolas, monospace;
            font-size: 11px;
            color: #7d8590;
            background: #21262d;
            padding: 2px 5px;
            border-radius: 3px;
        }
        .status-badge {
            display: inline-block;
            padding: 2px 7px;
            border-radius: 12px;
            font-size: 11px;
            font-weight: 500;
            line-height: 18px;
        }
        .status-pending { background: #9e6a03; color: #f0d062; }
        .status-generating {
            background: #0969da;
            color: #79c0ff;
            animation: pulse 1.5s ease-in-out infinite;
        }
        .status-completed { background: #1a7f37; color: #7ee787; }
        .status-error { background: #da3633; color: #ffa198; }
        @keyframes pulse {
            0%, 100% { opacity: 1; }
            50% { opacity: 0.6; }
        }
        .pr-title {
            font-size: 12px;
            color: #8b949e;
            max-width: 600px;
            word-wrap: break-word;
            white-space: normal;
            line-height: 1.4;
            margin-top: 2px;
        }
        .delete-btn {
            background: transparent;
            color: #da3633;
            border: 1px solid #da3633;
            padding: 2px 8px;
            border-radius: 6px;
            cursor: pointer;
            font-size: 11px;
            transition: all 0.2s;
        }
        .delete-btn:hover {
            background: #da3633;
            color: white;
        }
        .elapsed-time {
            display: block;
            font-size: 9px;
            margin-top: 2px;
            opacity: 0.7;
        }
    </style>
</head>
<body>
    <h1>PR Review Dashboard</h1>
    <div class="status" id="status">Loading...</div>
    <div id="error" class="error" style="display:none;"></div>

    <h2 style="color: #58a6ff; font-size: 16px; font-weight: 600; margin: 24px 0 8px 0;">My PRs</h2>
    <table id="my-pr-table" style="display:none; margin-bottom: 24px;">
        <thead>
            <tr>
                <th>Repository</th>
                <th>PR # / Title</th>
                <th>Author</th>
                <th>Approvals</th>
                <th>Status</th>
                <th>Commit SHA</th>
                <th>Last Reviewed</th>
                <th>Links</th>
            </tr>
        </thead>
        <tbody id="my-pr-list">
        </tbody>
    </table>

    <h2 style="color: #58a6ff; font-size: 16px; font-weight: 600; margin: 24px 0 8px 0;">PRs to Review</h2>
    <table id="pr-table" style="display:none;">
        <thead>
            <tr>
                <th>Repository</th>
                <th>PR # / Title</th>
                <th>Author</th>
                <th>My Review</th>
                <th>Approvals</th>
                <th>Status</th>
                <th>Commit SHA</th>
                <th>Last Reviewed</th>
                <th>Links</th>
            </tr>
        </thead>
        <tbody id="pr-list">
        </tbody>
    </table>

    <script>
        function formatDate(dateStr) {
            if (!dateStr) return 'Not yet reviewed';
            const date = new Date(dateStr);
            return date.toLocaleString();
        }

        function getReviewStatusEmoji(status) {
            switch(status) {
                case 'APPROVED':
                    return '<span style="font-size: 18px;" title="Approved">✅</span>';
                case 'CHANGES_REQUESTED':
                    return '<span style="font-size: 18px;" title="Changes Requested">🚧</span>';
                case 'COMMENTED':
                    return '<span style="font-size: 18px;" title="Commented">💬</span>';
                default:
                    return '<span style="font-size: 18px; opacity: 0.5;" title="Not Reviewed">📥</span>';
            }
        }

        function renderPRRow(pr) {
            // Only show review link if PR is completed AND has a review path
            const reviewLink = (pr.status === 'completed' && pr.review_html_path)
                ? '<a href="/reviews/' + pr.review_html_path + '" target="_blank">View Review</a>'
                : '<span style="color: #ffa726; font-weight: 500;">Not yet reviewed</span>';

            let statusBadge = '<span class="status-badge status-' + pr.status + '">' +
                pr.status.charAt(0).toUpperCase() + pr.status.slice(1);

            // Add elapsed time for generating status
            if (pr.status === 'generating' && pr.generating_since) {
                const startTime = new Date(pr.generating_since).getTime();
                const elapsed = Math.floor((Date.now() - startTime) / 1000);
                statusBadge += '<br><span class="elapsed-time" data-start="' + startTime + '" style="font-size: 0.7em; font-weight: normal;">' +
                    elapsed + 's</span>';
            }

            statusBadge += '</span>';

            // Only show delete button for completed reviews
            const deleteBtn = pr.status === 'completed'
                ? '<button class="delete-btn" onclick="deletePR(\'' +
                    pr.owner + '\', \'' + pr.repo + '\', ' + pr.number + ')">Delete</button>'
                : '';

            // Build row with conditional review status column
            let row = '<tr id="pr-' + pr.owner + '-' + pr.repo + '-' + pr.number + '">' +
                '<td>' + pr.owner + '/' + pr.repo + '</td>' +
                '<td>' +
                    '<a href="' + pr.github_url + '" target="_blank">#' + pr.number + '</a>' +
                    '<div class="pr-title" title="' + pr.title + '">' + pr.title + '</div>' +
                '</td>' +
                '<td>' + pr.author + '</td>';

            // Only add review status column for PRs to review (not my PRs)
            if (!pr.is_mine) {
                row += '<td style="text-align: center;">' + getReviewStatusEmoji(pr.my_review_status) + '</td>';
            }

            // Add approval count (for all PRs)
            const approvalColor = pr.approval_count > 0 ? '#7ee787' : '#7d8590';
            row += '<td style="text-align: center; color: ' + approvalColor + '; font-weight: 600;">' +
                pr.approval_count + '</td>';

            row += '<td>' + statusBadge + '</td>' +
                '<td class="commit-sha">' + pr.commit_sha.substring(0, 7) + '</td>' +
                '<td>' + formatDate(pr.last_reviewed_at) + '</td>' +
                '<td>' +
                    '<a href="' + pr.github_url + '" target="_blank">GitHub</a> | ' +
                    reviewLink +
                    (deleteBtn ? ' | ' + deleteBtn : '') +
                '</td>' +
            '</tr>';

            return row;
        }

        function formatUptime(seconds) {
            const hours = Math.floor(seconds / 3600);
            const minutes = Math.floor((seconds % 3600) / 60);
            if (hours > 0) return hours + 'h ' + minutes + 'm';
            return minutes + 'm';
        }

        function fetchServerStatus() {
            fetch('/api/status')
                .then(response => response.ok ? response.json() : null)
                .then(data => {
                    if (!data) return;
                    const status = document.getElementById('status');

                    let html = '<div class="status-dot"></div>';
                    html += '<div class="status-item"><span class="status-label">Uptime:</span> <span class="status-value">' + formatUptime(data.uptime_seconds) + '</span></div>';
                    if (data.seconds_until_next_poll !== undefined) {
                        html += '<div class="status-item"><span class="status-label">Next poll:</span> <span class="status-value">' + data.seconds_until_next_poll + 's</span></div>';
                    }
                    html += '<div class="status-item"><span class="status-label">Completed:</span> <span class="status-value">' + data.counts.completed + '</span></div>';

                    if (data.counts.generating > 0) {
                        html += '<div class="status-item"><span class="status-label">Generating:</span> <span class="status-value">' + data.counts.generating + '</span></div>';
                    }
                    if (data.cbpr_running) {
                        html += '<div class="status-item"><span class="status-label">Current task:</span> <span class="status-value">' + formatUptime(data.cbpr_duration_seconds) + '</span></div>';
                    }
                    if (data.counts.pending > 0) {
                        html += '<div class="status-item"><span class="status-label">Pending:</span> <span class="status-value">' + data.counts.pending + '</span></div>';
                    }
                    if (data.counts.error > 0) {
                        html += '<div class="status-item"><span class="status-label">Errors:</span> <span class="status-value" style="color: #ffa198;">' + data.counts.error + '</span></div>';
                    }

                    status.innerHTML = html;
                })
                .catch(() => {});  // Silently fail - status is non-critical
        }

        function fetchPRs() {
            fetch('/api/prs')
                .then(response => {
                    if (!response.ok) throw new Error('Failed to fetch PRs');
                    return response.json();
                })
                .then(data => {
                    const myPRList = document.getElementById('my-pr-list');
                    const reviewPRList = document.getElementById('pr-list');
                    const myPRTable = document.getElementById('my-pr-table');
                    const reviewPRTable = document.getElementById('pr-table');
                    const errorDiv = document.getElementById('error');

                    errorDiv.style.display = 'none';

                    // Separate PRs into my PRs and review PRs
                    const myPRs = data.filter(pr => pr.is_mine);
                    const reviewPRs = data.filter(pr => !pr.is_mine);

                    // Render My PRs
                    if (myPRs.length > 0) {
                        myPRTable.style.display = 'table';
                        myPRList.innerHTML = myPRs.map(renderPRRow).join('');
                    } else {
                        myPRTable.style.display = 'none';
                    }

                    // Render Review PRs
                    if (reviewPRs.length > 0) {
                        reviewPRTable.style.display = 'table';
                        reviewPRList.innerHTML = reviewPRs.map(renderPRRow).join('');
                    } else {
                        reviewPRTable.style.display = 'none';
                    }
                })
                .catch(error => {
                    const errorDiv = document.getElementById('error');
                    errorDiv.textContent = 'Error: ' + error.message;
                    errorDiv.style.display = 'block';
                });
        }

        function deletePR(owner, repo, number) {
            // Immediately remove the row from UI (optimistic update)
            const rowId = 'pr-' + owner + '-' + repo + '-' + number;
            const row = document.getElementById(rowId);
            if (row) {
                row.remove();
            }

            // Call API to delete on backend
            fetch('/api/prs/delete', {
                method: 'POST',
                headers: { 'Content-Type': 'application/json' },
                body: JSON.stringify({ owner, repo, number })
            })
            .then(response => {
                if (!response.ok) throw new Error('Failed to delete PR');
                return response.json();
            })
            .catch(error => {
                alert('Error deleting PR: ' + error.message);
                // Refresh to restore correct state if delete failed
                fetchPRs();
            });
        }

        // Update elapsed time for generating PRs every second
        function updateElapsedTimes() {
            const elapsedElements = document.querySelectorAll('.elapsed-time');
            elapsedElements.forEach(el => {
                const startTime = parseInt(el.dataset.start);
                const elapsed = Math.floor((Date.now() - startTime) / 1000);
                el.textContent = elapsed + 's';
            });
        }

        // Initial load
        fetchServerStatus();
        fetchPRs();

        // Poll every 1 second for real-time updates
        setInterval(() => {
            fetchServerStatus();
            fetchPRs();
        }, 1000);

        // Update elapsed times every second
        setInterval(updateElapsedTimes, 1000);
    </script>
</body>
</html>`
	w.Header().Set("Content-Type", "text/html")
	w.Write([]byte(html))
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

	// Get GitHub API rate limit status
	ctx := r.Context()
	rateLimitInfo, err := s.ghClient.GetRateLimitInfo(ctx)
	rateLimitData := map[string]interface{}{
		"remaining": 0,
		"limit":     5000,
		"reset_at":  "",
		"is_limited": true,
		"error":     "",
	}
	if err != nil {
		rateLimitData["error"] = err.Error()
		log.Printf("[STATUS] Warning: Failed to get rate limit info: %v", err)
	} else {
		rateLimitData["remaining"] = rateLimitInfo.Remaining
		rateLimitData["limit"] = rateLimitInfo.Limit
		rateLimitData["reset_at"] = rateLimitInfo.ResetTime.Format(time.RFC3339)
		rateLimitData["is_limited"] = rateLimitInfo.Remaining < 10
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
