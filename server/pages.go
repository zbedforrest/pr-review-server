package server

import (
	"context"
	"encoding/json"
	"html/template"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"pr-review-server/db"
)

// Published pages live in storage, not the repo, under pages/<slug>/:
// index.html (the page), meta.json ({"title","description"}), and the
// preview.png, favicon.svg, favicon.png and apple-touch-icon.png assets.
const pagesPath = "/pages/"

var pageSlugRe = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{2,63}$`)

// pageAssets are served to anyone: link unfurlers fetch them without a
// session, and they carry only what the preview card already shows.
var pageAssets = map[string]string{
	"preview.png":          "image/png",
	"favicon.svg":          "image/svg+xml",
	"favicon.png":          "image/png",
	"apple-touch-icon.png": "image/png",
}

type pageMeta struct {
	Title       string `json:"title"`
	Description string `json:"description"`
}

// handlePage serves /pages/<slug>/ and its assets. Signed-in org members get
// the page; everyone else, including Slack and iMessage unfurlers, gets a
// card page with the same preview tags and a sign-in link that returns here.
func (s *Server) handlePage(w http.ResponseWriter, r *http.Request, user func(*http.Request) *db.User) {
	w.Header().Set("X-Robots-Tag", "noindex, nofollow")
	rest := strings.TrimPrefix(r.URL.Path, pagesPath)
	slug, file, hasSlash := strings.Cut(rest, "/")
	if !pageSlugRe.MatchString(slug) {
		http.NotFound(w, r)
		return
	}
	if !hasSlash {
		http.Redirect(w, r, pagesPath+slug+"/", http.StatusMovedPermanently)
		return
	}
	if contentType, ok := pageAssets[file]; ok {
		s.servePageObject(w, r, slug, file, contentType, "public, max-age=300")
		return
	}
	if file != "" {
		http.NotFound(w, r)
		return
	}
	meta := s.pageMeta(r.Context(), slug)
	if meta == nil {
		http.NotFound(w, r)
		return
	}
	if user(r) != nil {
		s.servePageObject(w, r, slug, "index.html", "text/html; charset=utf-8", "private, no-cache")
		return
	}
	s.servePageCard(w, r, slug, *meta)
}

func (s *Server) servePageObject(w http.ResponseWriter, r *http.Request, slug, file, contentType, cacheControl string) {
	content, err := s.readPageObject(r.Context(), slug, file)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", contentType)
	w.Header().Set("Cache-Control", cacheControl)
	_, _ = w.Write(content) // nolint:errcheck
}

func (s *Server) pageMeta(ctx context.Context, slug string) *pageMeta {
	raw, err := s.readPageObject(ctx, slug, "meta.json")
	if err != nil {
		return nil
	}
	var m pageMeta
	if err := json.Unmarshal(raw, &m); err != nil || m.Title == "" {
		log.Printf("[PAGES] invalid meta.json for %s: %v", slug, err)
		return nil
	}
	return &m
}

// readPageObject reads from the review bucket when one is configured, and
// from <ReviewsDir>/pages otherwise (local development).
func (s *Server) readPageObject(ctx context.Context, slug, file string) ([]byte, error) {
	name := "pages/" + slug + "/" + file
	if s.gcsClient != nil && s.gcsClient.BucketName() != "" {
		return s.gcsClient.GetReviewContent(ctx, name)
	}
	return os.ReadFile(filepath.Join(s.cfg.ReviewsDir, filepath.FromSlash(name)))
}

var pageCardTmpl = template.Must(template.New("page-card").Parse(`<!doctype html>
<html lang="en"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1">
<title>{{.Title}}</title>
<meta name="description" content="{{.Description}}">
<meta property="og:type" content="website">
<meta property="og:title" content="{{.Title}}">
<meta property="og:description" content="{{.Description}}">
<meta property="og:url" content="{{.URL}}">
<meta property="og:image" content="{{.Image}}">
<meta property="og:image:width" content="1200">
<meta property="og:image:height" content="630">
<meta name="twitter:card" content="summary_large_image">
<meta name="twitter:title" content="{{.Title}}">
<meta name="twitter:description" content="{{.Description}}">
<meta name="twitter:image" content="{{.Image}}">
<link rel="icon" type="image/svg+xml" href="{{.Base}}favicon.svg">
<link rel="icon" type="image/png" href="{{.Base}}favicon.png">
<link rel="apple-touch-icon" href="{{.Base}}apple-touch-icon.png">
<style>
:root{color-scheme:dark}
body{margin:0;min-height:100vh;display:grid;place-items:center;background:#050b10;color:#f4f8fa;font:16px ui-sans-serif,-apple-system,"Segoe UI",Helvetica,Arial,sans-serif;padding:24px 16px;box-sizing:border-box}
main{max-width:720px;display:grid;gap:20px}
h1{margin:0;font-size:clamp(26px,4vw,40px);line-height:1.1;letter-spacing:-.03em;text-wrap:balance}
p{margin:0;color:#9eb0bc;line-height:1.45}
img{width:100%;height:auto;border:1px solid #294252;border-radius:14px}
a{justify-self:start;color:#050b10;background:#42d7e8;border-radius:999px;padding:10px 18px;font-weight:700;text-decoration:none}
a:focus-visible{outline:2px solid #f4f8fa;outline-offset:3px}
</style></head>
<body><main>
<h1>{{.Title}}</h1>
<p>{{.Description}}</p>
<img src="{{.Base}}preview.png" alt="Preview of the page" width="1200" height="630">
<a href="{{.Login}}">Sign in with GitHub to view</a>
</main></body></html>`))

func (s *Server) servePageCard(w http.ResponseWriter, r *http.Request, slug string, m pageMeta) {
	base := pagesPath + slug + "/"
	origin := strings.TrimRight(s.cfg.BaseURL, "/")
	if origin == "" {
		// Unfurlers need absolute URLs; without a configured base, trust the
		// host this request reached (Cloud Run and most proxies terminate TLS).
		origin = "https://" + r.Host
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	err := pageCardTmpl.Execute(w, map[string]string{
		"Title":       m.Title,
		"Description": m.Description,
		"URL":         origin + base,
		"Image":       origin + base + "preview.png",
		"Base":        base,
		"Login":       "/login?next=" + base,
	})
	if err != nil {
		log.Printf("[PAGES] render card for %s: %v", slug, err)
	}
}
