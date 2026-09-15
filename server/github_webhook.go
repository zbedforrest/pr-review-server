package server

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"

	"pr-review-server/db"
)

const (
	githubWebhookPath    = "/webhooks/github"
	githubWebhookMaxBody = 1 << 20
)

// WebhookDeliveryHandler records what a stored pull_request delivery means
// before the delivery is acknowledged. An error refuses the delivery so
// GitHub shows it failed and a redelivery is not treated as a duplicate.
type WebhookDeliveryHandler func(ctx context.Context, delivery db.WebhookDelivery) error

// handledPullRequestActions are the actions that change automatic-review
// state; every other pull_request action is acknowledged without a row.
var handledPullRequestActions = map[string]bool{
	"opened": true, "ready_for_review": true, "synchronize": true, "converted_to_draft": true, "closed": true,
}

// SetWebhookDeliveryHandler installs the consumer of accepted deliveries.
func (s *Server) SetWebhookDeliveryHandler(f WebhookDeliveryHandler) {
	s.webhookDeliveryFunc = f
}

type pullRequestWebhookPayload struct {
	Action       string `json:"action"`
	Installation struct {
		ID int64 `json:"id"`
	} `json:"installation"`
	Repository struct {
		Name  string `json:"name"`
		Owner struct {
			Login string `json:"login"`
		} `json:"owner"`
	} `json:"repository"`
	PullRequest struct {
		Number int    `json:"number"`
		Title  string `json:"title"`
		State  string `json:"state"`
		Draft  bool   `json:"draft"`
		User   struct {
			Login string `json:"login"`
		} `json:"user"`
		Head struct {
			SHA string `json:"sha"`
		} `json:"head"`
		Base struct {
			SHA  string `json:"sha"`
			Repo struct {
				Name  string `json:"name"`
				Owner struct {
					Login string `json:"login"`
				} `json:"owner"`
			} `json:"repo"`
		} `json:"base"`
	} `json:"pull_request"`
}

// verifyGitHubSignature checks the sha256 HMAC GitHub sends over the raw body.
func verifyGitHubSignature(secret string, body []byte, header string) bool {
	const prefix = "sha256="
	if !strings.HasPrefix(header, prefix) {
		return false
	}
	sent, err := hex.DecodeString(strings.TrimPrefix(header, prefix))
	if err != nil {
		return false
	}
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	return hmac.Equal(sent, mac.Sum(nil))
}

// handleGitHubWebhook is the GitHub App's webhook endpoint. It authenticates
// the delivery, stores the pull_request actions it acts on once by delivery
// id, records their intent through the installed handler, and answers. Every
// other event type or action is acknowledged so it never shows as a failed
// delivery.
func (s *Server) handleGitHubWebhook(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	secret := s.cfg.GitHubWebhookSecret
	if secret == "" {
		http.Error(w, "webhook secret not configured", http.StatusServiceUnavailable)
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, githubWebhookMaxBody))
	if err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			http.Error(w, "payload too large", http.StatusRequestEntityTooLarge)
			return
		}
		http.Error(w, "could not read body", http.StatusBadRequest)
		return
	}
	signature := r.Header.Get("X-Hub-Signature-256")
	if signature == "" {
		http.Error(w, "missing X-Hub-Signature-256", http.StatusBadRequest)
		return
	}
	if !verifyGitHubSignature(secret, body, signature) {
		http.Error(w, "invalid signature", http.StatusUnauthorized)
		return
	}
	deliveryID := strings.TrimSpace(r.Header.Get("X-GitHub-Delivery"))
	event := r.Header.Get("X-GitHub-Event")
	switch event {
	case "ping":
		writeWebhookStatus(w, "pong")
	case "pull_request":
		s.acceptPullRequestWebhook(w, deliveryID, body)
	default:
		log.Printf("[WEBHOOK] delivery=%s: ignoring %s event", deliveryID, event)
		writeWebhookStatus(w, "ignored")
	}
}

func (s *Server) acceptPullRequestWebhook(w http.ResponseWriter, deliveryID string, body []byte) {
	if deliveryID == "" {
		http.Error(w, "missing X-GitHub-Delivery", http.StatusBadRequest)
		return
	}
	var payload pullRequestWebhookPayload
	if err := json.Unmarshal(body, &payload); err != nil {
		http.Error(w, "invalid JSON payload", http.StatusBadRequest)
		return
	}
	owner, repo := payload.Repository.Owner.Login, payload.Repository.Name
	if owner == "" || repo == "" {
		owner, repo = payload.PullRequest.Base.Repo.Owner.Login, payload.PullRequest.Base.Repo.Name
	}
	if owner == "" || repo == "" || payload.PullRequest.Number <= 0 {
		http.Error(w, "payload does not identify a pull request", http.StatusBadRequest)
		return
	}
	if want := strings.TrimSpace(s.cfg.GitHubAppInstallationID); want != "" {
		wantID, err := strconv.ParseInt(want, 10, 64)
		if err != nil || wantID != payload.Installation.ID {
			log.Printf("[WEBHOOK] delivery=%s: installation %d does not match the configured app installation %q", deliveryID, payload.Installation.ID, want)
			http.Error(w, "unknown installation", http.StatusForbidden)
			return
		}
	}
	target := fmt.Sprintf("%s/%s#%d", owner, repo, payload.PullRequest.Number)
	if !handledPullRequestActions[payload.Action] {
		log.Printf("[WEBHOOK] delivery=%s %s: ignoring pull_request.%s", deliveryID, target, payload.Action)
		writeWebhookStatus(w, "ignored")
		return
	}
	delivery := db.WebhookDelivery{
		DeliveryID: deliveryID, Event: "pull_request", Action: payload.Action, InstallationID: payload.Installation.ID,
		RepoOwner: owner, RepoName: repo, PRNumber: payload.PullRequest.Number,
		HeadSHA: payload.PullRequest.Head.SHA, BaseSHA: payload.PullRequest.Base.SHA,
		Author: payload.PullRequest.User.Login, Title: payload.PullRequest.Title,
		Draft: payload.PullRequest.Draft, State: payload.PullRequest.State, ReceivedAt: time.Now().UTC(),
	}
	inserted, err := s.db.CreateWebhookDelivery(&delivery)
	if err != nil {
		log.Printf("[WEBHOOK] delivery=%s: store failed: %v", deliveryID, err)
		http.Error(w, "could not store delivery", http.StatusInternalServerError)
		return
	}
	if !inserted {
		log.Printf("[WEBHOOK] delivery=%s %s: duplicate delivery, already stored", deliveryID, target)
		writeWebhookStatus(w, "duplicate")
		return
	}
	log.Printf("[WEBHOOK] delivery=%s %s: pull_request.%s head=%s author=%s draft=%t state=%s",
		deliveryID, target, delivery.Action, delivery.HeadSHA[:min(7, len(delivery.HeadSHA))], delivery.Author, delivery.Draft, delivery.State)
	if s.webhookDeliveryFunc != nil {
		// The intent is durable before the 200, so a crash after the response
		// loses nothing; the request context is not used because the work
		// must not be cancelled by the client going away.
		if err := s.webhookDeliveryFunc(context.Background(), delivery); err != nil {
			log.Printf("[WEBHOOK] delivery=%s %s: processing failed, forgetting the delivery so it can be redelivered: %v", deliveryID, target, err)
			if delErr := s.db.DeleteWebhookDelivery(deliveryID); delErr != nil {
				log.Printf("[WEBHOOK] delivery=%s: could not forget delivery: %v", deliveryID, delErr)
			}
			http.Error(w, "could not process delivery", http.StatusInternalServerError)
			return
		}
	}
	writeWebhookStatus(w, "accepted")
}

func writeWebhookStatus(w http.ResponseWriter, status string) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]string{"status": status}) // nolint:errcheck
}
