package server

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"sync"

	"pr-review-server/config"
	"pr-review-server/db"
	"pr-review-server/github"
)

type Server struct {
	cfg            *config.Config
	db             *db.DB
	ghClient       *github.Client
	prCache        []github.PullRequest
	prCacheMux     sync.RWMutex
	pollTriggerFunc func()
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
}

func New(cfg *config.Config, database *db.DB, ghClient *github.Client) *Server {
	return &Server{
		cfg:      cfg,
		db:       database,
		ghClient: ghClient,
	}
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
	http.Handle("/reviews/", http.StripPrefix("/reviews/", http.FileServer(http.Dir(s.cfg.ReviewsDir))))

	addr := ":" + s.cfg.ServerPort
	log.Printf("Starting server on http://localhost%s", addr)
	return http.ListenAndServe(addr, nil)
}

func (s *Server) handleIndex(w http.ResponseWriter, r *http.Request) {
	html := `<!DOCTYPE html>
<html>
<head>
    <title>PR Review Dashboard</title>
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
            font-size: 11px;
            color: #7d8590;
            margin-bottom: 8px;
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
    <table id="pr-table" style="display:none;">
        <thead>
            <tr>
                <th>Repository</th>
                <th>PR # / Title</th>
                <th>Author</th>
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

        function fetchPRs() {
            fetch('/api/prs')
                .then(response => {
                    if (!response.ok) throw new Error('Failed to fetch PRs');
                    return response.json();
                })
                .then(data => {
                    const prList = document.getElementById('pr-list');
                    const status = document.getElementById('status');
                    const table = document.getElementById('pr-table');
                    const errorDiv = document.getElementById('error');

                    errorDiv.style.display = 'none';

                    if (data.length === 0) {
                        status.textContent = 'No PRs requesting your review';
                        table.style.display = 'none';
                        return;
                    }

                    status.textContent = 'Last updated: ' + new Date().toLocaleTimeString();
                    table.style.display = 'table';

                    prList.innerHTML = data.map(pr => {
                        const reviewLink = pr.review_html_path
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

                        const deleteBtn = '<button class="delete-btn" onclick="deletePR(\'' +
                            pr.owner + '\', \'' + pr.repo + '\', ' + pr.number + ')">Delete</button>';

                        const prKey = pr.owner + '/' + pr.repo + '#' + pr.number;
                        return '<tr id="pr-' + pr.owner + '-' + pr.repo + '-' + pr.number + '">' +
                            '<td>' + pr.owner + '/' + pr.repo + '</td>' +
                            '<td>' +
                                '<a href="' + pr.github_url + '" target="_blank">#' + pr.number + '</a>' +
                                '<div class="pr-title" title="' + pr.title + '">' + pr.title + '</div>' +
                            '</td>' +
                            '<td>' + pr.author + '</td>' +
                            '<td>' + statusBadge + '</td>' +
                            '<td class="commit-sha">' + pr.commit_sha.substring(0, 7) + '</td>' +
                            '<td>' + formatDate(pr.last_reviewed_at) + '</td>' +
                            '<td>' +
                                '<a href="' + pr.github_url + '" target="_blank">GitHub</a> | ' +
                                reviewLink + ' | ' +
                                deleteBtn +
                            '</td>' +
                        '</tr>';
                    }).join('');
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

        // Fetch immediately and then every 30 seconds
        fetchPRs();
        setInterval(fetchPRs, 30000);

        // Update elapsed times every second
        setInterval(updateElapsedTimes, 1000);
    </script>
</body>
</html>`
	w.Header().Set("Content-Type", "text/html")
	w.Write([]byte(html))
}

func (s *Server) handleGetPRs(w http.ResponseWriter, r *http.Request) {
	// Use cached PRs instead of fetching from GitHub on every request
	githubPRs := s.GetCachedPRs()

	response := make([]PRResponse, len(githubPRs))
	for i, ghPR := range githubPRs {
		// Get status from database
		dbPR, err := s.db.GetPR(ghPR.Owner, ghPR.Repo, ghPR.Number)

		status := "pending"
		var reviewedAt *string
		var generatingSince *string
		reviewHTMLPath := ""

		if err == nil && dbPR != nil {
			status = dbPR.Status
			if dbPR.LastReviewedAt != nil {
				formatted := dbPR.LastReviewedAt.Format("2006-01-02T15:04:05Z")
				reviewedAt = &formatted
			}
			if dbPR.GeneratingSince != nil {
				formatted := dbPR.GeneratingSince.Format("2006-01-02T15:04:05Z")
				generatingSince = &formatted
			}
			reviewHTMLPath = dbPR.ReviewHTMLPath
		}

		response[i] = PRResponse{
			Owner:           ghPR.Owner,
			Repo:            ghPR.Repo,
			Number:          ghPR.Number,
			CommitSHA:       ghPR.CommitSHA,
			Title:           ghPR.Title,
			Author:          ghPR.Author,
			LastReviewedAt:  reviewedAt,
			ReviewHTMLPath:  reviewHTMLPath,
			GitHubURL:       ghPR.URL,
			ReviewURL:       filepath.Join("/reviews", reviewHTMLPath),
			Status:          status,
			GeneratingSince: generatingSince,
		}
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
