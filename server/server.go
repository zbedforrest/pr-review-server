package server

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"path/filepath"

	"pr-review-server/config"
	"pr-review-server/db"
)

type Server struct {
	cfg *config.Config
	db  *db.DB
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
}

func New(cfg *config.Config, database *db.DB) *Server {
	return &Server{
		cfg: cfg,
		db:  database,
	}
}

func (s *Server) Start() error {
	http.HandleFunc("/", s.handleIndex)
	http.HandleFunc("/api/prs", s.handleGetPRs)
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
                <th>PR #</th>
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
                            : 'Pending...';

                        return '<tr>' +
                            '<td>' + pr.owner + '/' + pr.repo + '</td>' +
                            '<td><a href="' + pr.github_url + '" target="_blank">#' + pr.number + '</a></td>' +
                            '<td class="commit-sha">' + pr.commit_sha.substring(0, 7) + '</td>' +
                            '<td>' + formatDate(pr.last_reviewed_at) + '</td>' +
                            '<td>' +
                                '<a href="' + pr.github_url + '" target="_blank">GitHub</a> | ' +
                                reviewLink +
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
	prs, err := s.db.GetAllPRs()
	if err != nil {
		http.Error(w, fmt.Sprintf("Failed to fetch PRs: %v", err), http.StatusInternalServerError)
		return
	}

	response := make([]PRResponse, len(prs))
	for i, pr := range prs {
		var reviewedAt *string
		if pr.LastReviewedAt != nil {
			formatted := pr.LastReviewedAt.Format("2006-01-02T15:04:05Z")
			reviewedAt = &formatted
		}

		response[i] = PRResponse{
			Owner:          pr.RepoOwner,
			Repo:           pr.RepoName,
			Number:         pr.PRNumber,
			CommitSHA:      pr.LastCommitSHA,
			LastReviewedAt: reviewedAt,
			ReviewHTMLPath: pr.ReviewHTMLPath,
			GitHubURL:      fmt.Sprintf("https://github.com/%s/%s/pull/%d", pr.RepoOwner, pr.RepoName, pr.PRNumber),
			ReviewURL:      filepath.Join("/reviews", pr.ReviewHTMLPath),
		}
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(response)
}
