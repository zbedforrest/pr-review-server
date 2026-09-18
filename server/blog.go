package server

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"html/template"
	"log"
	"net/http"
	"regexp"
	"strings"
	"time"

	"pr-review-server/auth"
	"pr-review-server/db"
)

// The blog serves admin-uploaded HTML posts to signed-in members. A post is a
// directory of files: index.html is the page and every other relative path is
// an asset it references, so a self-contained static page publishes unchanged.

const (
	blogPath      = "/blog"
	blogIndexFile = "index.html"

	// blogPageCSP lets a post carry inline styles and same-origin images and
	// media, and nothing else: uploaded HTML must not run scripts.
	blogPageCSP = "default-src 'none'; img-src 'self' data:; media-src 'self'; style-src 'unsafe-inline'; font-src 'self'; form-action 'none'"
)

var validBlogSlug = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{1,80}$`)

// One path segment: no leading dot, so "." and ".." are impossible.
var validBlogPathSegment = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)

const maxBlogPathLen = 512

// blogStore is the narrow persistence capability implemented by *db.GormDB.
// Type-asserted per request so the shared db.Database interface and its mocks
// stay untouched.
type blogStore interface {
	SaveBlogPost(p *db.BlogPost) error
	GetBlogPost(slug string) (*db.BlogPost, error)
	ListBlogPosts(publishedOnly bool) ([]db.BlogPost, error)
	DeleteBlogPost(slug string) error
	UpsertBlogAsset(a *db.BlogAsset) error
	GetBlogAsset(slug, path string) (*db.BlogAsset, error)
	ListBlogAssets(slug string) ([]db.BlogAsset, error)
}

// cleanBlogPath validates a post-relative file path such as "img/hero.png".
func cleanBlogPath(raw string) (string, error) {
	if raw == "" || len(raw) > maxBlogPathLen {
		return "", errors.New("path must be 1 to 512 characters")
	}
	if strings.HasPrefix(raw, "/") {
		return "", errors.New("path must be relative")
	}
	for _, segment := range strings.Split(raw, "/") {
		if segment == "." || segment == ".." {
			return "", errors.New("path must not contain . or .. segments")
		}
		if !validBlogPathSegment.MatchString(segment) {
			return "", fmt.Errorf("path segment %q may only contain letters, digits, dot, dash and underscore", segment)
		}
	}
	return raw, nil
}

func blogETag(content []byte) string {
	sum := sha256.Sum256(content)
	return `"` + hex.EncodeToString(sum[:]) + `"`
}

// blogServedContentType adds the charset text types need on the wire.
func blogServedContentType(stored string) string {
	if strings.HasPrefix(stored, "text/") || stored == "application/json" || stored == "image/svg+xml" {
		return stored + "; charset=utf-8"
	}
	return stored
}

var blogIndexTemplate = template.Must(template.New("blog-index").Parse(`<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>PRism Blog</title>
<style>
:root { color-scheme: light dark; }
body { margin: 0; padding: 2rem 1rem 4rem; font: 16px/1.5 system-ui, -apple-system, "Segoe UI", sans-serif; background: Canvas; color: CanvasText; }
main { max-width: 42rem; margin: 0 auto; }
header { display: flex; align-items: baseline; gap: 1rem; margin-bottom: 2rem; }
header h1 { margin: 0; font-size: 1.75rem; }
header a { color: inherit; opacity: .7; text-decoration: none; }
header a:hover { opacity: 1; }
ul { list-style: none; margin: 0; padding: 0; }
li { padding: 1.25rem 0; border-top: 1px solid color-mix(in srgb, CanvasText 15%, transparent); }
li:last-child { border-bottom: 1px solid color-mix(in srgb, CanvasText 15%, transparent); }
li h2 { margin: 0 0 .25rem; font-size: 1.25rem; }
li h2 a { color: LinkText; text-decoration: none; }
li h2 a:hover { text-decoration: underline; }
li p { margin: 0 0 .25rem; }
.meta { font-size: .875rem; opacity: .7; }
.draft { display: inline-block; margin-left: .5rem; padding: 0 .4rem; border: 1px solid currentColor; border-radius: .25rem; font-size: .75rem; vertical-align: middle; opacity: .7; }
.empty { opacity: .7; }
</style>
</head>
<body>
<main>
<header><a href="/">PRism</a><h1>PRism Blog</h1></header>
{{if not .Posts}}<p class="empty">No posts yet.</p>{{end}}
<ul>
{{range .Posts}}<li>
<h2><a href="/blog/{{.Slug}}/">{{.Title}}</a>{{if .Draft}}<span class="draft">Draft</span>{{end}}</h2>
{{if .Dek}}<p>{{.Dek}}</p>{{end}}
<p class="meta">{{.Date}}{{if .Author}} by {{.Author}}{{end}}</p>
</li>
{{end}}</ul>
</main>
</body>
</html>
`))

type blogIndexEntry struct {
	Slug   string
	Title  string
	Dek    string
	Author string
	Date   string
	Draft  bool
}

func blogPostDate(p db.BlogPost) time.Time {
	if p.PublishedAt != nil {
		return *p.PublishedAt
	}
	return p.CreatedAt
}

func (s *Server) blogStore() (blogStore, bool) {
	store, ok := s.db.(blogStore)
	return store, ok
}

// handleBlog serves /blog (the index), /blog/<slug>/ (the post page) and
// /blog/<slug>/<path> (an asset). Drafts are visible to admins only and
// otherwise indistinguishable from missing posts.
func (s *Server) handleBlog(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	user := auth.GetCurrentUser(r)
	if user == nil {
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}
	store, ok := s.blogStore()
	if !ok {
		http.Error(w, "Blog unsupported by this storage backend", http.StatusNotImplemented)
		return
	}
	isAdmin := s.isAdmin(user)

	rest := strings.TrimPrefix(strings.TrimPrefix(r.URL.Path, blogPath), "/")
	if rest == "" {
		s.serveBlogIndex(w, store, isAdmin)
		return
	}
	slug, filePath, hasSlash := strings.Cut(rest, "/")
	if !validBlogSlug.MatchString(slug) {
		http.NotFound(w, r)
		return
	}
	if !hasSlash {
		http.Redirect(w, r, blogPath+"/"+slug+"/", http.StatusMovedPermanently)
		return
	}
	post, err := store.GetBlogPost(slug)
	if err != nil {
		log.Printf("[BLOG] load post %s: %v", slug, err)
		http.Error(w, "Failed to load post", http.StatusInternalServerError)
		return
	}
	if post == nil || (!post.Published && !isAdmin) {
		http.NotFound(w, r)
		return
	}
	if filePath == "" {
		filePath = blogIndexFile
	} else if filePath, err = cleanBlogPath(filePath); err != nil {
		http.NotFound(w, r)
		return
	}
	asset, err := store.GetBlogAsset(slug, filePath)
	if err != nil {
		log.Printf("[BLOG] load asset %s/%s: %v", slug, filePath, err)
		http.Error(w, "Failed to load file", http.StatusInternalServerError)
		return
	}
	if asset == nil {
		http.NotFound(w, r)
		return
	}
	s.serveBlogAsset(w, r, asset)
}

func (s *Server) serveBlogIndex(w http.ResponseWriter, store blogStore, isAdmin bool) {
	posts, err := store.ListBlogPosts(!isAdmin)
	if err != nil {
		log.Printf("[BLOG] list posts: %v", err)
		http.Error(w, "Failed to load posts", http.StatusInternalServerError)
		return
	}
	entries := make([]blogIndexEntry, 0, len(posts))
	for _, p := range posts {
		entries = append(entries, blogIndexEntry{
			Slug:   p.Slug,
			Title:  p.Title,
			Dek:    p.Dek,
			Author: p.AuthorLogin,
			Date:   blogPostDate(p).UTC().Format("January 2, 2006"),
			Draft:  !p.Published,
		})
	}
	var buf bytes.Buffer
	if err := blogIndexTemplate.Execute(&buf, struct{ Posts []blogIndexEntry }{entries}); err != nil {
		log.Printf("[BLOG] render index: %v", err)
		http.Error(w, "Failed to render index", http.StatusInternalServerError)
		return
	}
	h := w.Header()
	h.Set("Content-Type", "text/html; charset=utf-8")
	h.Set("Content-Security-Policy", "default-src 'none'; style-src 'unsafe-inline'; form-action 'none'")
	h.Set("X-Frame-Options", "DENY")
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Cache-Control", "private, no-cache")
	_, _ = w.Write(buf.Bytes()) // nolint:errcheck
}

func (s *Server) serveBlogAsset(w http.ResponseWriter, r *http.Request, asset *db.BlogAsset) {
	h := w.Header()
	h.Set("Content-Security-Policy", blogPageCSP)
	h.Set("X-Frame-Options", "DENY")
	h.Set("X-Content-Type-Options", "nosniff")
	if asset.Path == blogIndexFile {
		h.Set("Cache-Control", "private, no-cache")
	} else {
		h.Set("Cache-Control", "private, max-age=3600")
	}
	if asset.ETag != "" {
		h.Set("ETag", asset.ETag)
		if match := r.Header.Get("If-None-Match"); match != "" && strings.Contains(match, asset.ETag) {
			w.WriteHeader(http.StatusNotModified)
			return
		}
	}
	content, err := s.blogObjects.Get(r.Context(), asset.StorageObject)
	if err != nil {
		if errors.Is(err, errBlogObjectNotFound) {
			log.Printf("[BLOG] object missing for %s/%s: %s", asset.Slug, asset.Path, asset.StorageObject)
			http.NotFound(w, r)
			return
		}
		log.Printf("[BLOG] read %s: %v", asset.StorageObject, err)
		http.Error(w, "Failed to read file", http.StatusInternalServerError)
		return
	}
	h.Set("Content-Type", blogServedContentType(asset.ContentType))
	h.Set("Content-Length", fmt.Sprint(len(content)))
	_, _ = w.Write(content) // nolint:errcheck
}
