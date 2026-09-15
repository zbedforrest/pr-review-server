package server

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"pr-review-server/db"
)

const webhookTestSecret = "s3cret"

const pullRequestPayload = `{
  "action": "%s",
  "installation": {"id": 42},
  "repository": {"name": "example", "owner": {"login": "acme"}},
  "pull_request": {
    "number": 7,
    "title": "Ready PR",
    "state": "open",
    "draft": false,
    "user": {"login": "alice"},
    "head": {"sha": "1111111111111111111111111111111111111111"},
    "base": {"sha": "0000000000000000000000000000000000000000", "repo": {"name": "example", "owner": {"login": "acme"}}}
  },
  "sender": {"login": "mallory"}
}`

func signWebhook(secret string, body []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	return "sha256=" + hex.EncodeToString(mac.Sum(nil))
}

func webhookRequest(event, deliveryID, signature string, body []byte) *http.Request {
	req := httptest.NewRequest(http.MethodPost, githubWebhookPath, strings.NewReader(string(body)))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-GitHub-Event", event)
	req.Header.Set("X-GitHub-Delivery", deliveryID)
	if signature != "" {
		req.Header.Set("X-Hub-Signature-256", signature)
	}
	return req
}

func newWebhookServer(t *testing.T) (*Server, *db.GormDB, chan db.WebhookDelivery) {
	t.Helper()
	server, database := newTestServer(t, "tester")
	server.cfg.GitHubWebhookSecret = webhookTestSecret
	received := make(chan db.WebhookDelivery, 4)
	server.SetWebhookDeliveryHandler(func(_ context.Context, d db.WebhookDelivery) { received <- d })
	return server, database, received
}

func deliveriesStored(t *testing.T, database *db.GormDB) int {
	t.Helper()
	status, err := database.GetWebhookStatus(time.Now().Add(-time.Hour))
	require.NoError(t, err)
	return status.Deliveries
}

func TestGitHubWebhookAcceptsSignedPullRequestAndHandsItOff(t *testing.T) {
	server, database, received := newWebhookServer(t)
	body := []byte(strings.Replace(pullRequestPayload, "%s", "ready_for_review", 1))
	w := httptest.NewRecorder()
	server.handleGitHubWebhook(w, webhookRequest("pull_request", "delivery-1", signWebhook(webhookTestSecret, body), body))
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	assert.Contains(t, w.Body.String(), `"accepted"`)

	select {
	case d := <-received:
		assert.Equal(t, "delivery-1", d.DeliveryID)
		assert.Equal(t, "ready_for_review", d.Action)
		assert.Equal(t, "acme", d.RepoOwner)
		assert.Equal(t, "example", d.RepoName)
		assert.Equal(t, 7, d.PRNumber)
		assert.Equal(t, "1111111111111111111111111111111111111111", d.HeadSHA)
		assert.Equal(t, "alice", d.Author, "the PR author, never the sender")
		assert.Equal(t, "Ready PR", d.Title)
		assert.Equal(t, "open", d.State)
		assert.False(t, d.Draft)
		assert.Equal(t, int64(42), d.InstallationID)
	case <-time.After(5 * time.Second):
		t.Fatal("the delivery handler was not called")
	}
	assert.Equal(t, 1, deliveriesStored(t, database))
}

func TestGitHubWebhookRejectsBadOrMissingSignatures(t *testing.T) {
	server, database, received := newWebhookServer(t)
	body := []byte(strings.Replace(pullRequestPayload, "%s", "opened", 1))

	w := httptest.NewRecorder()
	server.handleGitHubWebhook(w, webhookRequest("pull_request", "delivery-2", signWebhook("wrong", body), body))
	assert.Equal(t, http.StatusUnauthorized, w.Code)

	w = httptest.NewRecorder()
	server.handleGitHubWebhook(w, webhookRequest("pull_request", "delivery-3", "", body))
	assert.Equal(t, http.StatusBadRequest, w.Code)

	w = httptest.NewRecorder()
	server.handleGitHubWebhook(w, webhookRequest("pull_request", "delivery-4", "sha256=nothex", body))
	assert.Equal(t, http.StatusUnauthorized, w.Code)

	assert.Equal(t, 0, deliveriesStored(t, database))
	assert.Empty(t, received)
}

func TestGitHubWebhookIsDisabledWithoutASecret(t *testing.T) {
	server, _ := newTestServer(t, "tester")
	body := []byte(`{}`)
	w := httptest.NewRecorder()
	server.handleGitHubWebhook(w, webhookRequest("ping", "delivery-5", signWebhook("", body), body))
	assert.Equal(t, http.StatusServiceUnavailable, w.Code)
}

func TestGitHubWebhookAcknowledgesPingAndUnhandledEventsWithoutStoringThem(t *testing.T) {
	server, database, received := newWebhookServer(t)
	for _, event := range []string{"ping", "issue_comment", "pull_request_review", "release", "commit_comment"} {
		body := []byte(`{"action":"created","zen":"Keep it logically awesome."}`)
		w := httptest.NewRecorder()
		server.handleGitHubWebhook(w, webhookRequest(event, "delivery-"+event, signWebhook(webhookTestSecret, body), body))
		assert.Equal(t, http.StatusOK, w.Code, event)
	}
	assert.Equal(t, 0, deliveriesStored(t, database))
	assert.Empty(t, received)
}

func TestGitHubWebhookDuplicateDeliveryIsANoOp(t *testing.T) {
	server, database, received := newWebhookServer(t)
	body := []byte(strings.Replace(pullRequestPayload, "%s", "synchronize", 1))
	for i := 0; i < 2; i++ {
		w := httptest.NewRecorder()
		server.handleGitHubWebhook(w, webhookRequest("pull_request", "delivery-dup", signWebhook(webhookTestSecret, body), body))
		require.Equal(t, http.StatusOK, w.Code)
		if i == 1 {
			assert.Contains(t, w.Body.String(), `"duplicate"`)
		}
	}
	select {
	case <-received:
	case <-time.After(5 * time.Second):
		t.Fatal("the first delivery was not handed off")
	}
	select {
	case d := <-received:
		t.Fatalf("the duplicate delivery %s was handed off again", d.DeliveryID)
	case <-time.After(100 * time.Millisecond):
	}
	assert.Equal(t, 1, deliveriesStored(t, database))
}

func TestGitHubWebhookRejectsOtherInstallationsAndMalformedPayloads(t *testing.T) {
	server, database, received := newWebhookServer(t)
	server.cfg.GitHubAppInstallationID = "99"
	body := []byte(strings.Replace(pullRequestPayload, "%s", "opened", 1))
	w := httptest.NewRecorder()
	server.handleGitHubWebhook(w, webhookRequest("pull_request", "delivery-inst", signWebhook(webhookTestSecret, body), body))
	assert.Equal(t, http.StatusForbidden, w.Code)

	server.cfg.GitHubAppInstallationID = "42"
	malformed := []byte(`{"action":"opened","installation":{"id":42},"pull_request":{"number":0}}`)
	w = httptest.NewRecorder()
	server.handleGitHubWebhook(w, webhookRequest("pull_request", "delivery-bad", signWebhook(webhookTestSecret, malformed), malformed))
	assert.Equal(t, http.StatusBadRequest, w.Code)

	w = httptest.NewRecorder()
	server.handleGitHubWebhook(w, webhookRequest("pull_request", "", signWebhook(webhookTestSecret, body), body))
	assert.Equal(t, http.StatusBadRequest, w.Code, "a delivery without an id cannot be deduplicated")

	oversized := []byte(`{"pad":"` + strings.Repeat("x", githubWebhookMaxBody) + `"}`)
	w = httptest.NewRecorder()
	server.handleGitHubWebhook(w, webhookRequest("pull_request", "delivery-big", signWebhook(webhookTestSecret, oversized), oversized))
	assert.Equal(t, http.StatusRequestEntityTooLarge, w.Code)

	assert.Equal(t, 0, deliveriesStored(t, database))
	assert.Empty(t, received)
}

func TestGitHubWebhookRejectsNonPost(t *testing.T) {
	server, _, _ := newWebhookServer(t)
	w := httptest.NewRecorder()
	server.handleGitHubWebhook(w, httptest.NewRequest(http.MethodGet, githubWebhookPath, nil))
	assert.Equal(t, http.StatusMethodNotAllowed, w.Code)
}

func TestStatusSnapshotReportsWebhookIngress(t *testing.T) {
	server, database := newTestServer(t, "tester")
	empty, err := server.buildStatusSnapshot(context.Background())
	require.NoError(t, err)
	assert.Equal(t, 0, empty.Webhook.Deliveries24h)
	assert.Nil(t, empty.Webhook.LastDeliveryAt)
	assert.Equal(t, 0, empty.Webhook.IntentsQueued)

	_, err = database.CreateWebhookDelivery(&db.WebhookDelivery{DeliveryID: "d-1", Event: "pull_request", Action: "opened", RepoOwner: "acme", RepoName: "example", PRNumber: 7})
	require.NoError(t, err)
	intent := db.AutoReviewIntent{RepoOwner: "acme", RepoName: "example", PRNumber: 7, HeadSHA: "abc", Trigger: "opened", DeliveryID: "d-1"}
	_, err = database.EnsureAutoReviewIntent(&intent, nil)
	require.NoError(t, err)

	snapshot, err := server.buildStatusSnapshot(context.Background())
	require.NoError(t, err)
	assert.Equal(t, 1, snapshot.Webhook.Deliveries24h)
	require.NotNil(t, snapshot.Webhook.LastDeliveryAt)
	_, parseErr := time.Parse(time.RFC3339, *snapshot.Webhook.LastDeliveryAt)
	assert.NoError(t, parseErr)
	assert.Equal(t, 1, snapshot.Webhook.IntentsQueued)
}
