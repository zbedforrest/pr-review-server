package server

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strconv"
	"strings"

	"pr-review-server/db"
)

const publishRepliesPath = "/api/publish/replies"

const (
	defaultPublishRepliesLimit = 50
	maxPublishRepliesLimit     = 500
)

// replyLedgerReader is the slice of the ledger the replies status page reads.
type replyLedgerReader interface {
	ListRecentPublishedReplies(limit int) ([]db.PublishedReply, error)
	CountPublishedReplies() (db.PublishedReplyCounts, error)
	ListUnlinkedPublishedFindings() ([]db.UnlinkedPublishedFinding, error)
}

// handlePublishReplies reports what the author-reply system has seen and done:
// mode, totals by action and class, roots still waiting for a comment id, and
// the newest handled replies with links back to GitHub.
func (s *Server) handlePublishReplies(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	ledger, ok := s.db.(replyLedgerReader)
	if !ok {
		http.Error(w, "reply ledger unavailable", http.StatusNotImplemented)
		return
	}
	limit := defaultPublishRepliesLimit
	if v := r.URL.Query().Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 {
			http.Error(w, "limit must be a positive integer", http.StatusBadRequest)
			return
		}
		limit = min(n, maxPublishRepliesLimit)
	}

	counts, err := ledger.CountPublishedReplies()
	if err != nil {
		log.Printf("[API] reply counts: %v", err)
		http.Error(w, "reply ledger unavailable", http.StatusInternalServerError)
		return
	}
	recent, err := ledger.ListRecentPublishedReplies(limit)
	if err != nil {
		log.Printf("[API] recent replies: %v", err)
		http.Error(w, "reply ledger unavailable", http.StatusInternalServerError)
		return
	}
	unlinked, err := ledger.ListUnlinkedPublishedFindings()
	if err != nil {
		log.Printf("[API] unlinked roots: %v", err)
		http.Error(w, "reply ledger unavailable", http.StatusInternalServerError)
		return
	}
	mode := defaultPublishReplyMode
	if v, err := s.db.GetSetting(settingPublishReplyMode); err == nil && publishReplyModes[strings.TrimSpace(v)] {
		mode = strings.TrimSpace(v)
	}
	enabledAt, _ := s.db.GetSetting(settingPublishReplyEnabledAt)

	items := make([]map[string]any, 0, len(recent))
	for _, row := range recent {
		items = append(items, map[string]any{
			"repo":              row.RepoOwner + "/" + row.RepoName,
			"pr_number":         row.PRNumber,
			"fingerprint":       row.Fingerprint,
			"root_comment_id":   row.RootCommentID,
			"author_comment_id": row.AuthorCommentID,
			"author_id":         row.AuthorID,
			"class":             row.Class,
			"action":            row.Action,
			"body":              row.Body,
			"created_at":        row.CreatedAt,
			"processed_at":      row.ProcessedAt,
			"url":               fmt.Sprintf("https://github.com/%s/%s/pull/%d#discussion_r%d", row.RepoOwner, row.RepoName, row.PRNumber, row.AuthorCommentID),
		})
	}
	w.Header().Set("Cache-Control", "no-cache, no-store, must-revalidate")
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{ // nolint:errcheck
		"mode":           mode,
		"enabled_at":     strings.TrimSpace(enabledAt),
		"total":          counts.Total,
		"by_action":      counts.ByAction,
		"by_class":       counts.ByClass,
		"unlinked_roots": len(unlinked),
		"recent":         items,
	})
}
