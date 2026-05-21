package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"pr-review-server/auth"
	"pr-review-server/gcs"
	"pr-review-server/pkg/reviewer/payload"
)

// reviewAPIPathPrefix is the route prefix for the JSON review fetch endpoint.
// Registered in server.go alongside the other /api/* handlers.
const reviewAPIPathPrefix = "/api/review/"

// reviewAPIResponse is the JSON payload returned by GET /api/review/{owner}/{repo}/{pr}.
//
// `findings` is the structured form of the review: each item has the cited
// file/line, the unified-diff hunk that contains the line, and a window of
// surrounding post-image source. This is the shape an LLM/CLI consumer
// should use; the rendered HTML at `review_url` is still available for
// humans (and via ?format=html for raw passthrough).
//
// `pr_status` and `is_in_flight` let the caller distinguish the three states
// the prior shape couldn't: (a) review never generated, (b) review currently
// in flight, (c) stale review present while a fresh one is generating.
//
// `findings_available` is false when the underlying review predates the
// JSON sidecar (older reviews on disk): regenerate the review to populate it.
type reviewAPIResponse struct {
	Owner             string            `json:"owner"`
	Repo              string            `json:"repo"`
	PRNumber          int               `json:"pr_number"`
	PRStatus          string            `json:"pr_status"`
	IsInFlight        bool              `json:"is_in_flight"`
	ErrorMessage      string            `json:"error_message,omitempty"`
	CommitSHA         string            `json:"commit_sha,omitempty"`
	HeadSHA           string            `json:"head_sha"`
	IsStale           bool              `json:"is_stale"`
	GeneratedAt       *time.Time        `json:"generated_at,omitempty"`
	ReviewPath        string            `json:"review_path,omitempty"`
	ReviewURL         string            `json:"review_url,omitempty"`
	Counts            reviewAPICounts   `json:"counts"`
	FindingsAvailable bool              `json:"findings_available"`
	Findings          []payload.Finding `json:"findings,omitempty"`
	SchemaVersion     string            `json:"schema_version,omitempty"`
}

// isInFlightStatus reports whether the PR currently has a review actively
// being generated. Mirrors the poller's status vocabulary in db/gorm_prs.go.
func isInFlightStatus(status string) bool {
	return status == "generating" || status == "agent_reviewing"
}

type reviewAPICounts struct {
	Critical int `json:"critical"`
	Medium   int `json:"medium"`
	Low      int `json:"low"`
}

// handleGetReview serves GET /api/review/{owner}/{repo}/{pr}.
//
// Path:    /api/review/{owner}/{repo}/{pr}
// Query:   ?sha=<full_or_short_sha>   pin to a specific commit's review
//
//	?format=html                return raw HTML body instead of JSON
//
// Auth:    handled by the same middleware that protects /api/* routes.
//
// Errors: 400 for malformed path or path traversal in ?sha, 401 from middleware,
// 404 when the PR is unknown / has no review / the pinned sha has no file,
// 502 when storage is configured but unreachable.
func (s *Server) handleGetReview(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-cache, no-store, must-revalidate")

	user := auth.GetCurrentUser(r)
	if user == nil {
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}

	owner, repo, prNumber, err := parseReviewPath(r.URL.Path)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	pr, err := s.db.GetPR(owner, repo, prNumber)
	if err != nil {
		log.Printf("[API/review] db error %s/%s#%d: %v", owner, repo, prNumber, err)
		http.Error(w, "Failed to look up PR", http.StatusInternalServerError)
		return
	}
	if pr == nil {
		http.Error(w, "PR not found", http.StatusNotFound)
		return
	}

	inFlight := isInFlightStatus(pr.Status)

	// Resolve which review filename to serve, if any.
	pinnedSHA := strings.TrimSpace(r.URL.Query().Get("sha"))
	var filename string
	if pinnedSHA != "" {
		if !isSafeSHA(pinnedSHA) {
			http.Error(w, "Invalid sha", http.StatusBadRequest)
			return
		}
		filename = gcs.ReviewFileName(owner, repo, prNumber, pinnedSHA)
	} else {
		filename = pr.ReviewHTMLPath
	}

	// No review file for this PR yet. Three sub-cases, distinguished by
	// pr.Status so the caller can decide whether to retry, surface an
	// error, or give up.
	if filename == "" {
		base := reviewAPIResponse{
			Owner:        owner,
			Repo:         repo,
			PRNumber:     prNumber,
			PRStatus:     pr.Status,
			IsInFlight:   inFlight,
			ErrorMessage: pr.ErrorMessage,
			HeadSHA:      pr.LastCommitSHA,
		}
		switch {
		case inFlight:
			// 202 Accepted: a review is being generated. CLI consumers
			// (the prism-review skill) should retry shortly.
			writeReviewJSON(w, http.StatusAccepted, base)
		case pr.Status == "error":
			writeReviewJSON(w, http.StatusFailedDependency, base)
		default:
			writeReviewJSON(w, http.StatusNotFound, base)
		}
		return
	}

	// Defense-in-depth path-traversal guard, mirroring handleReviewFromGCS.
	cleaned := filepath.Clean(filename)
	if strings.Contains(cleaned, "..") || filepath.IsAbs(cleaned) || strings.ContainsRune(cleaned, '/') {
		http.Error(w, "Invalid review path", http.StatusBadRequest)
		return
	}
	filename = cleaned

	// ?format=html keeps the raw HTML escape hatch for humans / debugging.
	// Everything else returns the structured payload.
	if r.URL.Query().Get("format") == "html" {
		html, fetchErr := s.fetchReviewBytes(r.Context(), filename)
		if fetchErr != nil {
			if errors.Is(fetchErr, errReviewNotFound) {
				http.Error(w, "Review file not found", http.StatusNotFound)
				return
			}
			log.Printf("[API/review] fetch error for %s: %v", filename, fetchErr)
			http.Error(w, "Failed to fetch review", http.StatusBadGateway)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write(html) // nolint:errcheck
		return
	}

	// Confirm the HTML object still exists before claiming the review is
	// available — otherwise a pinned ?sha= to a non-existent file would
	// return a "success-looking" envelope.
	if _, fetchErr := s.fetchReviewBytes(r.Context(), filename); fetchErr != nil {
		if errors.Is(fetchErr, errReviewNotFound) {
			http.Error(w, "Review file not found", http.StatusNotFound)
			return
		}
		log.Printf("[API/review] fetch error for %s: %v", filename, fetchErr)
		http.Error(w, "Failed to fetch review", http.StatusBadGateway)
		return
	}

	reviewSHA := commitSHAFromFilename(filename)
	resp := reviewAPIResponse{
		Owner:        owner,
		Repo:         repo,
		PRNumber:     prNumber,
		PRStatus:     pr.Status,
		IsInFlight:   inFlight,
		ErrorMessage: pr.ErrorMessage,
		CommitSHA:    reviewSHA,
		HeadSHA:      pr.LastCommitSHA,
		IsStale:      !shaPrefixMatch(reviewSHA, pr.LastCommitSHA),
		GeneratedAt:  pr.LastReviewedAt,
		ReviewPath:   filename,
		ReviewURL:    buildReviewURL(r, filename),
		Counts: reviewAPICounts{
			Critical: pr.CriticalCount,
			Medium:   pr.MediumCount,
			Low:      pr.LowCount,
		},
	}

	// Try to load the structured findings sidecar. If absent (review predates
	// the sidecar) or malformed, return the envelope with findings_available
	// false so the CLI can tell the user to regenerate. Don't 5xx — the HTML
	// view at review_url still works.
	sidecarName := gcs.ReviewJSONFileName(filename)
	sidecarBytes, sidecarErr := s.fetchReviewBytes(r.Context(), sidecarName)
	if sidecarErr == nil {
		var pl payload.Payload
		if err := json.Unmarshal(sidecarBytes, &pl); err != nil {
			log.Printf("[API/review] malformed sidecar %s: %v", sidecarName, err)
		} else {
			resp.Findings = pl.Findings
			resp.SchemaVersion = pl.SchemaVersion
			resp.FindingsAvailable = true
		}
	} else if !errors.Is(sidecarErr, errReviewNotFound) {
		// Non-not-found sidecar error: log but don't fail. HTML view still works.
		log.Printf("[API/review] sidecar fetch error for %s: %v", sidecarName, sidecarErr)
	}

	writeReviewJSON(w, http.StatusOK, resp)
}

// writeReviewJSON encodes the response body and sets the right Content-Type +
// status code. Centralized so the no-review branches stay terse.
func writeReviewJSON(w http.ResponseWriter, status int, body reviewAPIResponse) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body) // nolint:errcheck
}

// parseReviewPath extracts owner/repo/pr from a path of the form
// /api/review/{owner}/{repo}/{pr}. Trailing slashes are tolerated.
func parseReviewPath(urlPath string) (string, string, int, error) {
	rest := strings.TrimPrefix(urlPath, reviewAPIPathPrefix)
	rest = strings.Trim(rest, "/")
	parts := strings.Split(rest, "/")
	if len(parts) != 3 {
		return "", "", 0, fmt.Errorf("path must be /api/review/{owner}/{repo}/{pr}")
	}
	owner, repo, prStr := parts[0], parts[1], parts[2]
	if owner == "" || repo == "" {
		return "", "", 0, fmt.Errorf("owner and repo are required")
	}
	prNumber, err := strconv.Atoi(prStr)
	if err != nil || prNumber <= 0 {
		return "", "", 0, fmt.Errorf("pr must be a positive integer")
	}
	return owner, repo, prNumber, nil
}

// errReviewNotFound is returned by fetchReviewBytes when the file is absent
// from both GCS and the local fallback directory. Callers should map it to 404.
var errReviewNotFound = errors.New("review not found")

// fetchReviewBytes loads the review HTML for the given filename, trying GCS
// first (if configured) and falling back to the local ReviewsDir. Mirrors the
// resolution order in handleReviewFromGCS so both endpoints serve identical
// bytes for the same filename.
func (s *Server) fetchReviewBytes(ctx context.Context, filename string) ([]byte, error) {
	if s.gcsClient != nil && s.gcsClient.BucketName() != "" {
		// Use a fresh context so a slow client disconnect doesn't poison GCS.
		fetchCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
		defer cancel()
		content, err := s.gcsClient.GetReviewContent(fetchCtx, filename)
		if err == nil {
			return content, nil
		}
		if !strings.Contains(err.Error(), "not found") {
			log.Printf("[API/review] GCS error for %s: %v (falling back to local)", filename, err)
		}
	}

	localPath := filepath.Join(s.cfg.ReviewsDir, filename)
	content, err := os.ReadFile(localPath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, errReviewNotFound
		}
		return nil, err
	}
	return content, nil
}

// commitSHAFromFilename parses the SHA segment out of a review filename of the
// form {owner}_{repo}_{pr}_{shortsha}.html. Mirrors the parsing in
// gcs.ListReviewsForPR. Returns empty string if the filename is malformed.
func commitSHAFromFilename(filename string) string {
	if !strings.HasSuffix(filename, ".html") {
		return ""
	}
	trimmed := strings.TrimSuffix(filename, ".html")
	idx := strings.LastIndex(trimmed, "_")
	if idx < 0 || idx == len(trimmed)-1 {
		return ""
	}
	return trimmed[idx+1:]
}

// shaPrefixMatch returns true if either SHA is a prefix of the other. Used to
// compare a short (7-char) SHA from a filename against the full SHA stored on
// the PR row.
func shaPrefixMatch(a, b string) bool {
	if a == "" || b == "" {
		return false
	}
	if len(a) <= len(b) {
		return strings.HasPrefix(b, a)
	}
	return strings.HasPrefix(a, b)
}

// isSafeSHA validates a user-supplied ?sha= value: hex chars only, length 4-40.
// Rejects anything that could be smuggled into a filesystem path.
func isSafeSHA(s string) bool {
	if len(s) < 4 || len(s) > 40 {
		return false
	}
	for _, r := range s {
		switch {
		case r >= '0' && r <= '9':
		case r >= 'a' && r <= 'f':
		case r >= 'A' && r <= 'F':
		default:
			return false
		}
	}
	return true
}

// buildReviewURL composes the public URL for the rendered HTML view of a
// review, so a caller can hand the link to the user. Scheme resolution order:
// X-Forwarded-Proto (set by typical reverse proxies / managed runtimes),
// then r.TLS, then http as the dev-loop fallback.
func buildReviewURL(r *http.Request, filename string) string {
	scheme := "http"
	if fwd := r.Header.Get("X-Forwarded-Proto"); fwd != "" {
		scheme = fwd
	} else if r.TLS != nil {
		scheme = "https"
	}
	host := r.Host
	if host == "" {
		host = "localhost"
	}
	return fmt.Sprintf("%s://%s/reviews/%s", scheme, host, filename)
}
