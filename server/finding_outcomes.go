package server

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"pr-review-server/auth"
	"pr-review-server/db"
	"pr-review-server/pkg/reviewer/payload"
)

// Outcome-triage endpoints: record and list per-finding human triage
// decisions (dismiss-with-reason / acknowledge / unresolved). This is
// forensics and label harvesting, not enforcement — nothing here blocks a
// merge or mutates the review itself. Dismissing a CRITICAL becomes an
// explicit, recorded act instead of a silent judgment call, and the recorded
// labels feed the offline FP-economics harvest (benchmark/harvest_outcomes.py).
//
// Feature-gated by FINDING_OUTCOMES_ENABLED=true; when unset the endpoint
// returns 404 for every method. The 404 doubles as the frontend's feature
// probe: a failed list GET hides the triage UI entirely, so default behavior
// is byte-identical to a build without the feature.

// findingOutcomesPath is the route for both the list GET and the record POST.
// Registered in server.go alongside the other /api/prs/* handlers.
const findingOutcomesPath = "/api/prs/finding-outcomes"

// findingOutcomesEnabled is read per-request (not cached at init) so tests
// can flip it with t.Setenv.
func findingOutcomesEnabled() bool {
	return os.Getenv("FINDING_OUTCOMES_ENABLED") == "true"
}

// findingOutcomeStore is the narrow, optional persistence capability for
// outcome triage. Implemented by *db.GormDB; the handler type-asserts at
// request time so the shared db.Database interface (and every mock of it)
// stays untouched.
type findingOutcomeStore interface {
	UpsertFindingOutcome(o *db.FindingOutcome) error
	GetFindingOutcomesForPR(owner, repo string, prNumber int) ([]db.FindingOutcome, error)
}

// Outcome vocabulary. "dismissed" additionally requires a non-empty reason —
// the one-line reason is the entire point of the paper trail.
var validFindingOutcomes = map[string]bool{
	"dismissed":    true,
	"acknowledged": true,
	"unresolved":   true,
}

// maxOutcomeReasonLen bounds the free-text reason so a paste accident can't
// bloat the table. Generous: reasons are meant to be one line.
const maxOutcomeReasonLen = 2000

// findingOutcomeJSON is the wire shape for a recorded outcome (both the GET
// list items and the POST echo).
type findingOutcomeJSON struct {
	Owner       string `json:"owner"`
	Repo        string `json:"repo"`
	Number      int    `json:"number"`
	ReviewedSHA string `json:"reviewed_sha"`
	Fingerprint string `json:"fingerprint"`
	Provenance  string `json:"provenance,omitempty"`
	Severity    string `json:"severity,omitempty"`
	Outcome     string `json:"outcome"`
	Reason      string `json:"reason,omitempty"`
	DecidedBy   string `json:"decided_by"`
	DecidedAt   string `json:"decided_at"` // RFC3339 UTC
}

func findingOutcomeToJSON(o db.FindingOutcome) findingOutcomeJSON {
	return findingOutcomeJSON{
		Owner:       o.RepoOwner,
		Repo:        o.RepoName,
		Number:      o.PRNumber,
		ReviewedSHA: o.ReviewedSHA,
		Fingerprint: o.Fingerprint,
		Provenance:  o.Provenance,
		Severity:    o.Severity,
		Outcome:     o.Outcome,
		Reason:      o.Reason,
		DecidedBy:   o.DecidedBy,
		DecidedAt:   o.DecidedAt.UTC().Format(time.RFC3339),
	}
}

// handleFindingOutcomes serves GET (list a PR's recorded outcomes) and POST
// (record one decision) on /api/prs/finding-outcomes.
//
// GET query: ?owner=&repo=&number=
// POST body: {owner, repo, number, sha, file, line, comment, severity,
//
//	provenance, outcome, reason}
//
// Auth: the same middleware that protects the other mutating /api/prs/*
// routes; decided_by is taken from the authenticated user, never the body.
func (s *Server) handleFindingOutcomes(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-cache, no-store, must-revalidate")

	if !findingOutcomesEnabled() {
		http.Error(w, "Not found", http.StatusNotFound)
		return
	}

	user := auth.GetCurrentUser(r)
	if user == nil {
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}

	store, ok := s.db.(findingOutcomeStore)
	if !ok {
		http.Error(w, "Finding outcomes unsupported by this storage backend", http.StatusNotImplemented)
		return
	}

	switch r.Method {
	case http.MethodGet:
		s.handleListFindingOutcomes(w, r, store)
	case http.MethodPost:
		s.handleRecordFindingOutcome(w, r, store, user)
	default:
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
	}
}

func (s *Server) handleListFindingOutcomes(w http.ResponseWriter, r *http.Request, store findingOutcomeStore) {
	q := r.URL.Query()
	owner := q.Get("owner")
	repo := q.Get("repo")
	number, err := strconv.Atoi(q.Get("number"))
	if owner == "" || repo == "" || err != nil || number <= 0 {
		http.Error(w, "owner, repo, and a positive number are required", http.StatusBadRequest)
		return
	}

	outcomes, err := store.GetFindingOutcomesForPR(owner, repo, number)
	if err != nil {
		log.Printf("[API] finding-outcomes: list failed for %s/%s#%d: %v", owner, repo, number, err)
		http.Error(w, "Failed to list finding outcomes", http.StatusInternalServerError)
		return
	}

	items := make([]findingOutcomeJSON, len(outcomes))
	for i, o := range outcomes {
		items[i] = findingOutcomeToJSON(o)
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]interface{}{"outcomes": items}) // nolint:errcheck
}

func (s *Server) handleRecordFindingOutcome(w http.ResponseWriter, r *http.Request, store findingOutcomeStore, user *db.User) {
	var req struct {
		Owner      string `json:"owner"`
		Repo       string `json:"repo"`
		Number     int    `json:"number"`
		SHA        string `json:"sha"`
		File       string `json:"file"`
		Line       int    `json:"line"`
		Comment    string `json:"comment"`
		Severity   string `json:"severity"`
		Provenance string `json:"provenance"`
		Outcome    string `json:"outcome"`
		Reason     string `json:"reason"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, fmt.Sprintf("Invalid request: %v", err), http.StatusBadRequest)
		return
	}

	if req.Owner == "" || req.Repo == "" || req.Number <= 0 {
		http.Error(w, "owner, repo, and a positive number are required", http.StatusBadRequest)
		return
	}
	// Same hex-only validation as the review API's ?sha= — the SHA also keys
	// the sidecar join in the harvest, so garbage here poisons the labels.
	if !isSafeSHA(req.SHA) {
		http.Error(w, "sha must be a 4-40 char hex commit SHA", http.StatusBadRequest)
		return
	}
	// Column-size validation: reject oversized inputs as client errors
	// instead of surfacing Postgres value-too-long failures as 500s. The
	// owner/repo columns are varchar(255); the fingerprint column is
	// varchar(512) and the file path is its only unbounded component.
	if len(req.Owner) > 255 || len(req.Repo) > 255 {
		http.Error(w, "owner and repo must be at most 255 characters", http.StatusBadRequest)
		return
	}
	if req.File == "" || strings.TrimSpace(req.Comment) == "" {
		http.Error(w, "file and comment are required (they form the finding fingerprint)", http.StatusBadRequest)
		return
	}
	if req.Line < 0 {
		http.Error(w, "line must be non-negative", http.StatusBadRequest)
		return
	}
	fingerprint := payload.Fingerprint(req.File, req.Line, req.Comment)
	if len(fingerprint) > 512 {
		http.Error(w, "file path too long for a finding fingerprint (512 bytes max)", http.StatusBadRequest)
		return
	}

	outcome := strings.ToLower(strings.TrimSpace(req.Outcome))
	if !validFindingOutcomes[outcome] {
		http.Error(w, "outcome must be one of: dismissed, acknowledged, unresolved", http.StatusBadRequest)
		return
	}
	reason := strings.TrimSpace(req.Reason)
	if outcome == "dismissed" && reason == "" {
		http.Error(w, "dismissing a finding requires a reason", http.StatusBadRequest)
		return
	}
	if len(reason) > maxOutcomeReasonLen {
		http.Error(w, fmt.Sprintf("reason must be at most %d characters", maxOutcomeReasonLen), http.StatusBadRequest)
		return
	}

	// Note: no PR-existence check on purpose. Outcomes are self-contained by
	// (owner, repo, pr, sha) so they stay recordable for merged PRs whose
	// rows the closed-PR cleanup has already pruned — exactly the population
	// the harvest cares about.
	record := &db.FindingOutcome{
		RepoOwner:   req.Owner,
		RepoName:    req.Repo,
		PRNumber:    req.Number,
		ReviewedSHA: strings.ToLower(req.SHA),
		Fingerprint: fingerprint,
		Provenance:  truncateString(strings.ToLower(strings.TrimSpace(req.Provenance)), 32),
		Severity:    truncateString(strings.ToLower(strings.TrimSpace(req.Severity)), 16),
		Outcome:     outcome,
		Reason:      reason,
		DecidedBy:   user.GitHubUsername,
		DecidedAt:   time.Now().UTC(),
	}
	if err := store.UpsertFindingOutcome(record); err != nil {
		log.Printf("[API] finding-outcomes: upsert failed for %s/%s#%d %s: %v",
			req.Owner, req.Repo, req.Number, record.Fingerprint, err)
		http.Error(w, "Failed to record finding outcome", http.StatusInternalServerError)
		return
	}

	log.Printf("[API] finding-outcomes: %s recorded %q for %s/%s#%d@%s %s",
		user.GitHubUsername, outcome, req.Owner, req.Repo, req.Number, record.ReviewedSHA, record.Fingerprint)

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]interface{}{ // nolint:errcheck
		"status":  "success",
		"outcome": findingOutcomeToJSON(*record),
	})
}

// truncateString clamps s to at most n runes — Postgres measures varchar
// limits in characters, and a byte clamp could cut a multibyte rune mid-
// encoding, turning the INSERT into an invalid-UTF-8 500. (The inputs it
// guards are short ASCII vocabularies; the clamp is only a defensive
// backstop for the column sizes.)
func truncateString(s string, n int) string {
	if len(s) <= n {
		return s // ≤ n bytes implies ≤ n runes
	}
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n])
}
