package server

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"pr-review-server/db"
)

func newPageTestServer(t *testing.T) *Server {
	t.Helper()
	server, database := newTestServer(t, "tester")
	t.Cleanup(func() { database.Close() })
	server.cfg.ReviewsDir = t.TempDir()
	server.cfg.BaseURL = "https://prism.example.com"
	dir := filepath.Join(server.cfg.ReviewsDir, "pages", "quarterly-review")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	files := map[string]string{
		"index.html":  "<title>Quarterly review</title><h1>secret numbers</h1>",
		"meta.json":   `{"title":"Quarterly review","description":"Cost & quality <summary>"}`,
		"preview.png": "PNGDATA",
	}
	for name, body := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return server
}

func signedIn(*http.Request) *db.User  { return &db.User{GitHubUsername: "member"} }
func signedOut(*http.Request) *db.User { return nil }

func getPage(s *Server, path string, user func(*http.Request) *db.User) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	s.handlePage(w, httptest.NewRequest(http.MethodGet, path, nil), user)
	return w
}

func TestPageServesTheContentToSignedInMembers(t *testing.T) {
	w := getPage(newPageTestServer(t), "/pages/quarterly-review/", signedIn)
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "secret numbers") {
		t.Fatalf("code=%d body=%q", w.Code, w.Body.String())
	}
	if cc := w.Header().Get("Cache-Control"); !strings.HasPrefix(cc, "private") {
		t.Errorf("page content must not be cached by shared caches, got %q", cc)
	}
}

func TestPageShowsSignedOutVisitorsAPreviewCardWithoutTheContent(t *testing.T) {
	w := getPage(newPageTestServer(t), "/pages/quarterly-review/", signedOut)
	body := w.Body.String()
	if w.Code != http.StatusOK || strings.Contains(body, "secret numbers") {
		t.Fatalf("code=%d, card leaked page content: %q", w.Code, body)
	}
	for _, want := range []string{
		`<meta property="og:title" content="Quarterly review">`,
		`<meta property="og:description" content="Cost &amp; quality &lt;summary&gt;">`,
		`<meta property="og:image" content="https://prism.example.com/pages/quarterly-review/preview.png">`,
		`<meta name="twitter:card" content="summary_large_image">`,
		`href="/login?next=/pages/quarterly-review/"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("card missing %s", want)
		}
	}
	if w.Header().Get("X-Robots-Tag") == "" {
		t.Error("published pages must ask search engines not to index them")
	}
}

func TestPageCardFallsBackToTheRequestHostWithoutABaseURL(t *testing.T) {
	s := newPageTestServer(t)
	s.cfg.BaseURL = ""
	w := getPage(s, "/pages/quarterly-review/", signedOut)
	want := `<meta property="og:image" content="https://example.com/pages/quarterly-review/preview.png">`
	if !strings.Contains(w.Body.String(), want) {
		t.Fatalf("card missing %s", want)
	}
}

func TestPageAssetsArePublicForLinkUnfurlers(t *testing.T) {
	w := getPage(newPageTestServer(t), "/pages/quarterly-review/preview.png", signedOut)
	if w.Code != http.StatusOK || w.Body.String() != "PNGDATA" || w.Header().Get("Content-Type") != "image/png" {
		t.Fatalf("code=%d type=%q body=%q", w.Code, w.Header().Get("Content-Type"), w.Body.String())
	}
}

func TestPageRejectsUnknownFilesSlugsAndMissingPages(t *testing.T) {
	s := newPageTestServer(t)
	for _, path := range []string{
		"/pages/quarterly-review/meta.json",
		"/pages/quarterly-review/index.html",
		"/pages/quarterly-review/../../etc/passwd",
		"/pages/Bad_Slug/",
		"/pages/missing-page/",
	} {
		if w := getPage(s, path, signedIn); w.Code != http.StatusNotFound {
			t.Errorf("%s: code=%d, want 404", path, w.Code)
		}
	}
}

func TestPageWithoutTrailingSlashRedirectsToTheCanonicalURL(t *testing.T) {
	w := getPage(newPageTestServer(t), "/pages/quarterly-review", signedOut)
	if w.Code != http.StatusMovedPermanently || w.Header().Get("Location") != "/pages/quarterly-review/" {
		t.Fatalf("code=%d location=%q", w.Code, w.Header().Get("Location"))
	}
}
