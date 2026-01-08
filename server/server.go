package server

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"

	"pr-review-server/config"
	"pr-review-server/db"
	"pr-review-server/github"
)

type Server struct {
	cfg      *config.Config
	db       *db.DB
	ghClient *github.Client
}

type PRResponse struct {
	Owner          string  `json:"owner"`
	Repo           string  `json:"repo"`
	Number         int     `json:"number"`
	CommitSHA      string  `json:"commit_sha"`
	LastReviewedAt *string `json:"last_reviewed_at"`
	ReviewHTMLPath string  `json:"review_html_path"`
	GitHubURL      string  `json:"github_url"`
	ReviewURL      string  `json:"review_url"`
	Status         string  `json:"status"` // "pending", "generating", "completed", "error"
	Title          string  `json:"title"`
	Author         string  `json:"author"`
}

func New(cfg *config.Config, database *db.DB, ghClient *github.Client) *Server {
	return &Server{
		cfg:      cfg,
		db:       database,
		ghClient: ghClient,
	}
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
        body {
            font-family: -apple-system, BlinkMacSystemFont, "Segoe UI", Roboto, "Helvetica Neue", Arial, sans-serif;
            max-width: 1200px;
            margin: 0 auto;
            padding: 20px;
            background: #f5f5f5;
        }
        h1 {
            color: #333;
        }
        table {
            width: 100%;
            background: white;
            border-collapse: collapse;
            box-shadow: 0 2px 4px rgba(0,0,0,0.1);
            margin-top: 20px;
        }
        th, td {
            padding: 12px;
            text-align: left;
            border-bottom: 1px solid #ddd;
        }
        th {
            background: #4CAF50;
            color: white;
            font-weight: 600;
        }
        tr:hover {
            background: #f5f5f5;
        }
        a {
            color: #4CAF50;
            text-decoration: none;
        }
        a:hover {
            text-decoration: underline;
        }
        .status {
            font-size: 0.9em;
            color: #666;
        }
        .loading {
            text-align: center;
            padding: 20px;
            color: #666;
        }
        .error {
            background: #f44336;
            color: white;
            padding: 10px;
            border-radius: 4px;
            margin-top: 20px;
        }
        .commit-sha {
            font-family: monospace;
            font-size: 0.9em;
            color: #666;
        }
        .status-badge {
            display: inline-block;
            padding: 4px 8px;
            border-radius: 4px;
            font-size: 0.85em;
            font-weight: 600;
        }
        .status-pending { background: #ffa726; color: white; }
        .status-generating {
            background: #42a5f5;
            color: white;
            animation: pulse 1.5s ease-in-out infinite;
        }
        .status-completed { background: #66bb6a; color: white; }
        .status-error { background: #ef5350; color: white; }
        @keyframes pulse {
            0%, 100% { opacity: 1; }
            50% { opacity: 0.6; }
        }
        .pr-title {
            font-size: 0.9em;
            color: #666;
            max-width: 300px;
            overflow: hidden;
            text-overflow: ellipsis;
            white-space: nowrap;
        }
        .delete-btn {
            background: #ef5350;
            color: white;
            border: none;
            padding: 4px 8px;
            border-radius: 4px;
            cursor: pointer;
            font-size: 0.85em;
        }
        .delete-btn:hover {
            background: #e53935;
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

                        const statusBadge = '<span class="status-badge status-' + pr.status + '">' +
                            pr.status.charAt(0).toUpperCase() + pr.status.slice(1) +
                        '</span>';

                        const deleteBtn = '<button class="delete-btn" onclick="deletePR(\'' +
                            pr.owner + '\', \'' + pr.repo + '\', ' + pr.number + ')">Delete</button>';

                        return '<tr>' +
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
            if (!confirm('Delete review for ' + owner + '/' + repo + ' #' + number + '?')) {
                return;
            }

            fetch('/api/prs/delete', {
                method: 'POST',
                headers: { 'Content-Type': 'application/json' },
                body: JSON.stringify({ owner, repo, number })
            })
            .then(response => {
                if (!response.ok) throw new Error('Failed to delete PR');
                return response.json();
            })
            .then(() => {
                fetchPRs(); // Refresh the list
            })
            .catch(error => {
                alert('Error deleting PR: ' + error.message);
            });
        }

        // Fetch immediately and then every 30 seconds
        fetchPRs();
        setInterval(fetchPRs, 30000);
    </script>
</body>
</html>`
	w.Header().Set("Content-Type", "text/html")
	w.Write([]byte(html))
}

func (s *Server) handleGetPRs(w http.ResponseWriter, r *http.Request) {
	ctx := context.Background()

	// Fetch all PRs from GitHub
	githubPRs, err := s.ghClient.GetPRsRequestingReview(ctx)
	if err != nil {
		http.Error(w, fmt.Sprintf("Failed to fetch PRs from GitHub: %v", err), http.StatusInternalServerError)
		return
	}

	response := make([]PRResponse, len(githubPRs))
	for i, ghPR := range githubPRs {
		// Get status from database
		dbPR, err := s.db.GetPR(ghPR.Owner, ghPR.Repo, ghPR.Number)

		status := "pending"
		var reviewedAt *string
		reviewHTMLPath := ""

		if err == nil && dbPR != nil {
			status = dbPR.Status
			if dbPR.LastReviewedAt != nil {
				formatted := dbPR.LastReviewedAt.Format("2006-01-02T15:04:05Z")
				reviewedAt = &formatted
			}
			reviewHTMLPath = dbPR.ReviewHTMLPath
		}

		response[i] = PRResponse{
			Owner:          ghPR.Owner,
			Repo:           ghPR.Repo,
			Number:         ghPR.Number,
			CommitSHA:      ghPR.CommitSHA,
			Title:          ghPR.Title,
			Author:         ghPR.Author,
			LastReviewedAt: reviewedAt,
			ReviewHTMLPath: reviewHTMLPath,
			GitHubURL:      ghPR.URL,
			ReviewURL:      filepath.Join("/reviews", reviewHTMLPath),
			Status:         status,
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

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{"status": "success"})
}
