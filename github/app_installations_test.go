package github

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func newTestAppClient(t *testing.T, apiBase string) *AppClient {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 1024)
	if err != nil {
		t.Fatal(err)
	}
	return &AppClient{
		appID:          "1",
		installationID: "100",
		privateKey:     key,
		httpClient:     &http.Client{Timeout: 5 * time.Second},
		apiBase:        apiBase,
	}
}

func installationAPI(t *testing.T, lookups, mints *int32) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.Header.Get("Authorization"), "Bearer ") {
			t.Errorf("missing app JWT on %s", r.URL.Path)
		}
		switch {
		case r.URL.Path == "/repos/personal/tool/installation":
			atomic.AddInt32(lookups, 1)
			_ = json.NewEncoder(w).Encode(map[string]any{"id": 200})
		case r.URL.Path == "/repos/nobody/repo/installation":
			atomic.AddInt32(lookups, 1)
			w.WriteHeader(http.StatusNotFound)
		case strings.HasSuffix(r.URL.Path, "/access_tokens"):
			atomic.AddInt32(mints, 1)
			id := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/app/installations/"), "/access_tokens")
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(map[string]any{"token": "tok-" + id, "expires_at": time.Now().Add(time.Hour)})
		default:
			t.Errorf("unexpected request %s", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
}

func TestTokenForRepoResolvesAndCachesTheOwnerInstallation(t *testing.T) {
	var lookups, mints int32
	srv := installationAPI(t, &lookups, &mints)
	defer srv.Close()
	c := newTestAppClient(t, srv.URL)

	for i := 0; i < 3; i++ {
		tok, _, err := c.TokenForRepo(context.Background(), "personal", "tool")
		if err != nil {
			t.Fatal(err)
		}
		if tok != "tok-200" {
			t.Fatalf("token = %q, want the personal installation's token", tok)
		}
	}
	if lookups != 1 || mints != 1 {
		t.Errorf("lookups=%d mints=%d, want one of each across repeated calls", lookups, mints)
	}
}

func TestTokenForRepoReportsUninstalledOwner(t *testing.T) {
	var lookups, mints int32
	srv := installationAPI(t, &lookups, &mints)
	defer srv.Close()
	c := newTestAppClient(t, srv.URL)

	_, _, err := c.TokenForRepo(context.Background(), "nobody", "repo")
	if err == nil || !strings.Contains(err.Error(), "not installed") {
		t.Fatalf("err = %v, want an error naming the missing installation", err)
	}
}

func TestClientForRepoIsDistinctPerInstallation(t *testing.T) {
	var lookups, mints int32
	srv := installationAPI(t, &lookups, &mints)
	defer srv.Close()
	ac := newTestAppClient(t, srv.URL)
	c := &Client{}
	c.SetAppClient(ac)

	personal, err := c.clientFor(context.Background(), "personal", "tool")
	if err != nil {
		t.Fatal(err)
	}
	again, err := c.clientFor(context.Background(), "personal", "tool")
	if err != nil {
		t.Fatal(err)
	}
	if personal == c.gh {
		t.Error("personal repo resolved to the primary installation's client")
	}
	if personal != again {
		t.Error("per-installation client is not reused")
	}
}

func TestCreateIssueCommentUsesTheOwnersInstallation(t *testing.T) {
	var lookups, mints int32
	var gotAuth string
	inner := installationAPI(t, &lookups, &mints)
	defer inner.Close()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && r.URL.Path == "/repos/personal/tool/issues/1/comments" {
			gotAuth = r.Header.Get("Authorization")
			_ = json.NewEncoder(w).Encode(map[string]any{"id": 5})
			return
		}
		inner.Config.Handler.ServeHTTP(w, r)
	}))
	defer srv.Close()
	c := &Client{}
	c.SetAppClient(newTestAppClient(t, srv.URL))

	id, err := c.CreateIssueComment(context.Background(), "personal", "tool", 1, "hello")
	if err != nil {
		t.Fatal(err)
	}
	if id != 5 || gotAuth != "Bearer tok-200" {
		t.Errorf("id=%d auth=%q, want the personal installation's token", id, gotAuth)
	}
}

func TestClientForRepoFallsBackToPrimaryWhenNotInstalled(t *testing.T) {
	var lookups, mints int32
	srv := installationAPI(t, &lookups, &mints)
	defer srv.Close()
	c := &Client{}
	c.SetAppClient(newTestAppClient(t, srv.URL))

	for i := 0; i < 3; i++ {
		gh, err := c.clientFor(context.Background(), "nobody", "repo")
		if err != nil {
			t.Fatal(err)
		}
		if gh != c.gh {
			t.Fatal("an uninstalled owner must use the primary client, as before")
		}
	}
	if lookups != 1 {
		t.Errorf("negative lookups must be cached, got %d", lookups)
	}
}

func TestTokenForRepoReresolvesAfterAStaleInstallation(t *testing.T) {
	var lookups int32
	stale := true
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/repos/personal/tool/installation":
			atomic.AddInt32(&lookups, 1)
			id := 200
			if !stale {
				id = 201
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"id": id})
		case r.URL.Path == "/app/installations/200/access_tokens":
			w.WriteHeader(http.StatusNotFound)
		case r.URL.Path == "/app/installations/201/access_tokens":
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(map[string]any{"token": "tok-201", "expires_at": time.Now().Add(time.Hour)})
		default:
			t.Errorf("unexpected request %s", r.URL.Path)
		}
	}))
	defer srv.Close()
	c := newTestAppClient(t, srv.URL)

	if _, _, err := c.TokenForRepo(context.Background(), "personal", "tool"); err == nil {
		t.Fatal("minting against a stale installation must fail")
	}
	stale = false
	tok, _, err := c.TokenForRepo(context.Background(), "personal", "tool")
	if err != nil || tok != "tok-201" {
		t.Fatalf("after a 404 the owner must be re-resolved: tok=%q err=%v", tok, err)
	}
	if lookups != 2 {
		t.Errorf("lookups = %d, want 2 (initial and after invalidation)", lookups)
	}
}

func TestInstallationMissForOneRepoDoesNotPoisonSiblings(t *testing.T) {
	var lookups int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&lookups, 1)
		switch r.URL.Path {
		case "/repos/personal/excluded/installation":
			w.WriteHeader(http.StatusNotFound)
		case "/repos/personal/tool/installation":
			_ = json.NewEncoder(w).Encode(map[string]any{"id": 200})
		default:
			t.Errorf("unexpected request %s", r.URL.Path)
		}
	}))
	defer srv.Close()
	c := newTestAppClient(t, srv.URL)

	if _, err := c.installationFor(context.Background(), "personal", "excluded"); !errors.Is(err, ErrAppNotInstalled) {
		t.Fatalf("excluded repo: err = %v", err)
	}
	id, err := c.installationFor(context.Background(), "personal", "tool")
	if err != nil || id != "200" {
		t.Fatalf("a sibling repo under the same owner must resolve on its own: id=%q err=%v", id, err)
	}
	if lookups != 2 {
		t.Errorf("lookups = %d, want one per repo", lookups)
	}
}
