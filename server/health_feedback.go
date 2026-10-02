package server

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strconv"
	"time"

	"pr-review-server/db"
	"pr-review-server/github"
	"pr-review-server/pkg/feedback"
	"pr-review-server/pkg/health"
	"pr-review-server/pkg/reviewer/llm"
	"pr-review-server/poller"
)

const (
	feedbackPath        = "/api/health/feedback"
	feedbackScanTimeout = 4 * time.Minute
	feedbackModelWait   = 20 * time.Second
)

type feedbackStore interface {
	feedback.Store
	ListFeedbackItems(start, end time.Time) ([]db.FeedbackItem, error)
}

// feedbackClassifierFor builds the Gemini flash client the review pipeline
// also uses for classification, once per server, when a key is configured.
func (s *Server) feedbackClassifierFor() feedback.Classifier {
	s.feedbackOnce.Do(func() {
		if s.feedbackClassify != nil || s.cfg.GeminiAPIKey == "" {
			return
		}
		client := llm.NewGeminiClient(s.cfg.GeminiAPIKey, true, false)
		s.feedbackClassify = feedback.ModelClassifier{Name: llm.FlashModelName(), Ask: func(ctx context.Context, prompt string) (string, error) {
			callCtx, cancel := context.WithTimeout(ctx, feedbackModelWait)
			defer cancel()
			text, _, _, _, err := client.GetReviewContext(callCtx, prompt)
			return text, err
		}}
	})
	if s.feedbackClassify == nil {
		return feedback.LexiconClassifier{}
	}
	return s.feedbackClassify
}

// feedbackOutcome is how one daily scan went: whether GitHub was read in
// full, and if not, why.
type feedbackOutcome struct {
	scanned bool
	note    string
}

// scanFeedback runs the read-only GitHub scan ahead of the metrics query so
// today's items are in the table. It never fails the report.
func (s *Server) scanFeedback(now time.Time) feedbackOutcome {
	store, ok := s.db.(feedbackStore)
	switch {
	case !ok:
		return feedbackOutcome{note: "no feedback store"}
	case !s.cfg.FeedbackDigest:
		return feedbackOutcome{note: "FEEDBACK_DIGEST=false"}
	}
	gh := s.feedbackGitHub
	if gh == nil && s.ghClient != nil {
		gh = ghFeedbackAdapter{s.ghClient}
	}
	if gh == nil {
		return feedbackOutcome{note: "no GitHub client"}
	}
	ctx, cancel := context.WithTimeout(context.Background(), feedbackScanTimeout)
	defer cancel()
	scanner := feedback.Scanner{Store: store, GitHub: gh, Classifier: s.feedbackClassifierFor(), Handle: s.cfg.MentionHandle, Now: func() time.Time { return now }}
	res, err := scanner.Run(ctx)
	outcome := feedbackOutcome{scanned: true}
	switch {
	case err != nil:
		log.Printf("[FEEDBACK] scan failed after %d targets: %v", res.Targets, err)
		outcome = feedbackOutcome{note: "scan failed"}
	case res.Errors > 0:
		outcome = feedbackOutcome{note: fmt.Sprintf("%d of %d PRs could not be read", res.Errors, res.Targets)}
	}
	// The counter follows the report's day; a backfilled older item is stored
	// and listed in the week view but not counted as today's frustration.
	frustrated := 0
	var events []db.TelemetryEvent
	for _, it := range res.Added {
		if !it.Label.IsFrustrated() || it.CreatedAt.Before(now.Add(-24*time.Hour)) {
			continue
		}
		frustrated++
		events = append(events, db.TelemetryEvent{Action: health.ActionFeedbackFrustrated, Label: string(it.Label) + " by " + it.Author,
			PROwner: it.RepoOwner, PRRepo: it.RepoName, PRNumber: it.PRNumber})
	}
	if len(events) > 0 {
		if userID := poller.SystemTelemetryUserID(s.db); userID != 0 {
			for i := range events {
				events[i].UserID = userID
			}
			if err := s.db.CreateTelemetryEvents(events); err != nil {
				log.Printf("[FEEDBACK] could not record telemetry: %v", err)
			}
		}
	}
	log.Printf("[FEEDBACK] targets=%d seen=%d new=%d frustrated=%d errors=%d skipped_reaction_pages=%d", res.Targets, res.Seen, len(res.Added), frustrated, res.Errors, res.Skipped)
	return outcome
}

func (s *Server) feedbackMetrics(now time.Time, run feedbackOutcome) health.FeedbackMetrics {
	store, ok := s.db.(feedbackStore)
	if !ok {
		return health.FeedbackMetrics{Note: "no feedback store", ByLabel: map[string]int{}}
	}
	items, err := store.ListFeedbackItems(now.Add(-24*time.Hour), now)
	if err != nil {
		log.Printf("[FEEDBACK] list items: %v", err)
		return health.FeedbackMetrics{Note: "feedback query failed", ByLabel: map[string]int{}}
	}
	return feedback.NewDigest(items, now.Add(-24*time.Hour), now, 1).HealthMetrics(run.scanned, run.note)
}

// handleFeedback serves the stored feedback for the last 1 or 7 days as JSON
// or markdown. It reads the table only; the daily job does the scanning.
func (s *Server) handleFeedback(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	store, ok := s.db.(feedbackStore)
	if !ok {
		http.Error(w, "feedback store unavailable", http.StatusNotImplemented)
		return
	}
	days := 1
	if v := r.URL.Query().Get("days"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || (n != 1 && n != 7) {
			http.Error(w, "days must be 1 or 7", http.StatusBadRequest)
			return
		}
		days = n
	}
	format := r.URL.Query().Get("format")
	if format != "" && format != "md" && format != "json" {
		http.Error(w, "format must be md or json", http.StatusBadRequest)
		return
	}
	now := time.Now().UTC()
	start := now.Add(-time.Duration(days) * 24 * time.Hour)
	items, err := store.ListFeedbackItems(start, now)
	if err != nil {
		log.Printf("[FEEDBACK] list items: %v", err)
		http.Error(w, "feedback unavailable", http.StatusInternalServerError)
		return
	}
	digest := feedback.NewDigest(items, start, now, days)
	w.Header().Set("Cache-Control", "no-cache, no-store, must-revalidate")
	if format == "md" {
		w.Header().Set("Content-Type", "text/markdown; charset=utf-8")
		_, _ = w.Write([]byte(digest.Markdown()))
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(digest) // nolint:errcheck
}

type ghFeedbackAdapter struct{ c *github.Client }

func (a ghFeedbackAdapter) ListReviewComments(ctx context.Context, owner, repo string, number int) ([]feedback.Comment, error) {
	infos, err := a.c.ListReviewComments(ctx, owner, repo, number)
	if err != nil {
		return nil, err
	}
	out := make([]feedback.Comment, 0, len(infos))
	for _, c := range infos {
		out = append(out, feedback.Comment{ID: c.ID, InReplyToID: c.InReplyToID, Author: c.Author, IsBot: c.IsBot, Body: c.Body, CreatedAt: c.CreatedAt, Reactions: c.Reactions})
	}
	return out, nil
}

func (a ghFeedbackAdapter) ListIssueComments(ctx context.Context, owner, repo string, number int) ([]feedback.Comment, error) {
	infos, err := a.c.ListIssueComments(ctx, owner, repo, number)
	if err != nil {
		return nil, err
	}
	out := make([]feedback.Comment, 0, len(infos))
	for _, c := range infos {
		out = append(out, feedback.Comment{ID: c.ID, Author: c.Author, IsBot: c.IsBot, Body: c.Body, CreatedAt: c.CreatedAt, Reactions: c.Reactions})
	}
	return out, nil
}

func (a ghFeedbackAdapter) ListReviewCommentReactions(ctx context.Context, owner, repo string, commentID int64) ([]feedback.Reaction, error) {
	return feedbackReactions(a.c.ListReviewCommentReactions(ctx, owner, repo, commentID))
}

func (a ghFeedbackAdapter) ListIssueCommentReactions(ctx context.Context, owner, repo string, commentID int64) ([]feedback.Reaction, error) {
	return feedbackReactions(a.c.ListIssueCommentReactions(ctx, owner, repo, commentID))
}

func feedbackReactions(infos []github.ReactionInfo, err error) ([]feedback.Reaction, error) {
	if err != nil {
		return nil, err
	}
	out := make([]feedback.Reaction, 0, len(infos))
	for _, r := range infos {
		out = append(out, feedback.Reaction{ID: r.ID, User: r.User, IsBot: r.IsBot, Content: r.Content, CreatedAt: r.CreatedAt})
	}
	return out, nil
}
