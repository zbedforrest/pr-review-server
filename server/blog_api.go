package server

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"mime"
	"net/http"
	"strings"
	"time"

	"pr-review-server/auth"
	"pr-review-server/db"
)

// Admin API for the blog:
//
//	GET    /api/blog/posts                      list posts (drafts for admins only)
//	GET    /api/blog/posts/<slug>               one post and its files
//	PUT    /api/blog/posts/<slug>               create or update {title, dek, published}
//	DELETE /api/blog/posts/<slug>               remove the post and its files
//	PUT    /api/blog/posts/<slug>/files/<path>  upload one file (raw body, Content-Type header)
//	DELETE /api/blog/posts/<slug>/files/<path>  remove one file
//
// Reads need a session; writes need an admin.

const (
	blogAPIPath = "/api/blog/posts"

	blogMaxFileBytes = 15 << 20
	blogMaxPostBytes = 64 << 20
	blogMaxFiles     = 200
	maxBlogTitleLen  = 200
	maxBlogDekLen    = 500
)

var blogAssetContentTypes = map[string]bool{
	"image/png":        true,
	"image/jpeg":       true,
	"image/gif":        true,
	"image/webp":       true,
	"image/svg+xml":    true,
	"video/webm":       true,
	"video/mp4":        true,
	"text/css":         true,
	"font/woff2":       true,
	"application/json": true,
	"text/plain":       true,
}

type blogPostJSON struct {
	Slug        string  `json:"slug"`
	Title       string  `json:"title"`
	Dek         string  `json:"dek"`
	AuthorLogin string  `json:"author_login"`
	Published   bool    `json:"published"`
	PublishedAt *string `json:"published_at"`
	CreatedAt   string  `json:"created_at"`
	UpdatedAt   string  `json:"updated_at"`
	HasIndex    bool    `json:"has_index"`
	FileCount   int     `json:"file_count"`
	SizeBytes   int64   `json:"size_bytes"`
	URL         string  `json:"url"`
}

type blogFileJSON struct {
	Path        string `json:"path"`
	ContentType string `json:"content_type"`
	SizeBytes   int64  `json:"size_bytes"`
	UploadedAt  string `json:"uploaded_at"`
}

func blogPostToJSON(p db.BlogPost, assets []db.BlogAsset) blogPostJSON {
	out := blogPostJSON{
		Slug:        p.Slug,
		Title:       p.Title,
		Dek:         p.Dek,
		AuthorLogin: p.AuthorLogin,
		Published:   p.Published,
		CreatedAt:   p.CreatedAt.UTC().Format(time.RFC3339),
		UpdatedAt:   p.UpdatedAt.UTC().Format(time.RFC3339),
		HasIndex:    p.IndexObject != "",
		FileCount:   len(assets),
		URL:         blogPath + "/" + p.Slug + "/",
	}
	if p.PublishedAt != nil {
		stamp := p.PublishedAt.UTC().Format(time.RFC3339)
		out.PublishedAt = &stamp
	}
	for _, a := range assets {
		out.SizeBytes += a.SizeBytes
	}
	return out
}

func blogFileToJSON(a db.BlogAsset) blogFileJSON {
	return blogFileJSON{
		Path:        a.Path,
		ContentType: a.ContentType,
		SizeBytes:   a.SizeBytes,
		UploadedAt:  a.UploadedAt.UTC().Format(time.RFC3339),
	}
}

func blogAudit(actor, action, slug, path string, bytes int) {
	log.Printf("[BLOG] actor=%s action=%s slug=%s path=%s bytes=%d", actor, action, slug, path, bytes)
}

func (s *Server) handleBlogAPI(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-cache, no-store, must-revalidate")

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
	if r.Method != http.MethodGet && !isAdmin {
		log.Printf("[BLOG] denied actor=%s method=%s path=%s", user.GitHubUsername, r.Method, r.URL.Path)
		http.Error(w, "admin required", http.StatusForbidden)
		return
	}

	rest := strings.TrimPrefix(strings.TrimPrefix(r.URL.Path, blogAPIPath), "/")
	if rest == "" {
		if r.Method != http.MethodGet {
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}
		s.listBlogPosts(w, store, isAdmin)
		return
	}
	slug, tail, hasTail := strings.Cut(rest, "/")
	if !validBlogSlug.MatchString(slug) {
		http.Error(w, "slug must match ^[a-z0-9][a-z0-9-]{1,80}$", http.StatusBadRequest)
		return
	}
	if !hasTail {
		switch r.Method {
		case http.MethodGet:
			s.getBlogPost(w, store, slug, isAdmin)
		case http.MethodPut:
			s.putBlogPost(w, r, store, user, slug)
		case http.MethodDelete:
			s.deleteBlogPost(w, r, store, user, slug)
		default:
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		}
		return
	}
	filePath, isFile := strings.CutPrefix(tail, "files/")
	if !isFile {
		http.NotFound(w, r)
		return
	}
	switch r.Method {
	case http.MethodPut:
		s.putBlogFile(w, r, store, user, slug, filePath)
	case http.MethodDelete:
		s.deleteBlogFile(w, r, store, user, slug, filePath)
	default:
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
	}
}

func writeBlogJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body) // nolint:errcheck
}

func (s *Server) listBlogPosts(w http.ResponseWriter, store blogStore, isAdmin bool) {
	posts, err := store.ListBlogPosts(!isAdmin)
	if err != nil {
		log.Printf("[BLOG] list posts: %v", err)
		http.Error(w, "Failed to list posts", http.StatusInternalServerError)
		return
	}
	out := make([]blogPostJSON, 0, len(posts))
	for _, p := range posts {
		assets, err := store.ListBlogAssets(p.Slug)
		if err != nil {
			log.Printf("[BLOG] list assets %s: %v", p.Slug, err)
			http.Error(w, "Failed to list posts", http.StatusInternalServerError)
			return
		}
		out = append(out, blogPostToJSON(p, assets))
	}
	writeBlogJSON(w, http.StatusOK, map[string]any{"posts": out})
}

func (s *Server) loadBlogPost(w http.ResponseWriter, store blogStore, slug string, isAdmin bool) (*db.BlogPost, []db.BlogAsset, bool) {
	post, err := store.GetBlogPost(slug)
	if err != nil {
		log.Printf("[BLOG] load post %s: %v", slug, err)
		http.Error(w, "Failed to load post", http.StatusInternalServerError)
		return nil, nil, false
	}
	if post == nil || (!post.Published && !isAdmin) {
		http.Error(w, "post not found", http.StatusNotFound)
		return nil, nil, false
	}
	assets, err := store.ListBlogAssets(slug)
	if err != nil {
		log.Printf("[BLOG] list assets %s: %v", slug, err)
		http.Error(w, "Failed to load post", http.StatusInternalServerError)
		return nil, nil, false
	}
	return post, assets, true
}

func (s *Server) getBlogPost(w http.ResponseWriter, store blogStore, slug string, isAdmin bool) {
	post, assets, ok := s.loadBlogPost(w, store, slug, isAdmin)
	if !ok {
		return
	}
	files := make([]blogFileJSON, 0, len(assets))
	for _, a := range assets {
		files = append(files, blogFileToJSON(a))
	}
	writeBlogJSON(w, http.StatusOK, map[string]any{"post": blogPostToJSON(*post, assets), "files": files})
}

func (s *Server) putBlogPost(w http.ResponseWriter, r *http.Request, store blogStore, user *db.User, slug string) {
	var req struct {
		Title     string `json:"title"`
		Dek       string `json:"dek"`
		Published bool   `json:"published"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&req); err != nil {
		http.Error(w, fmt.Sprintf("Invalid request: %v", err), http.StatusBadRequest)
		return
	}
	title := strings.TrimSpace(req.Title)
	dek := strings.TrimSpace(req.Dek)
	if title == "" || len(title) > maxBlogTitleLen {
		http.Error(w, fmt.Sprintf("title must be 1 to %d characters", maxBlogTitleLen), http.StatusBadRequest)
		return
	}
	if len(dek) > maxBlogDekLen {
		http.Error(w, fmt.Sprintf("dek must be at most %d characters", maxBlogDekLen), http.StatusBadRequest)
		return
	}

	post, err := store.GetBlogPost(slug)
	if err != nil {
		log.Printf("[BLOG] load post %s: %v", slug, err)
		http.Error(w, "Failed to load post", http.StatusInternalServerError)
		return
	}
	action, status := "update", http.StatusOK
	if post == nil {
		post = &db.BlogPost{Slug: slug, AuthorLogin: user.GitHubUsername}
		action, status = "create", http.StatusCreated
	}
	if req.Published && post.IndexObject == "" {
		http.Error(w, "upload index.html before publishing", http.StatusConflict)
		return
	}
	post.Title = title
	post.Dek = dek
	if req.Published && !post.Published && post.PublishedAt == nil {
		now := time.Now()
		post.PublishedAt = &now
	}
	post.Published = req.Published
	if err := store.SaveBlogPost(post); err != nil {
		log.Printf("[BLOG] save post %s: %v", slug, err)
		http.Error(w, "Failed to save post", http.StatusInternalServerError)
		return
	}
	blogAudit(user.GitHubUsername, action, slug, "", 0)

	saved, assets, ok := s.loadBlogPost(w, store, slug, true)
	if !ok {
		return
	}
	writeBlogJSON(w, status, map[string]any{"post": blogPostToJSON(*saved, assets)})
}

func (s *Server) deleteBlogPost(w http.ResponseWriter, r *http.Request, store blogStore, user *db.User, slug string) {
	post, assets, ok := s.loadBlogPost(w, store, slug, true)
	if !ok {
		return
	}
	if err := store.DeleteBlogPost(post.Slug); err != nil {
		log.Printf("[BLOG] delete post %s: %v", slug, err)
		http.Error(w, "Failed to delete post", http.StatusInternalServerError)
		return
	}
	var total int64
	for _, a := range assets {
		total += a.SizeBytes
		s.removeBlogObject(r, a.StorageObject)
	}
	blogAudit(user.GitHubUsername, "delete", slug, "", int(total))
	writeBlogJSON(w, http.StatusOK, map[string]any{"status": "deleted", "slug": slug})
}

// removeBlogObject is best effort: once the rows are gone an orphaned object
// is unreachable and only costs storage, while a missing object behind a live
// row would 404 for readers.
func (s *Server) removeBlogObject(r *http.Request, key string) {
	if err := s.blogObjects.Delete(r.Context(), key); err != nil {
		log.Printf("[BLOG] orphaned object %s: %v", key, err)
	}
}

func (s *Server) deleteBlogFile(w http.ResponseWriter, r *http.Request, store blogStore, user *db.User, slug, rawPath string) {
	filePath, err := cleanBlogPath(rawPath)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	post, _, ok := s.loadBlogPost(w, store, slug, true)
	if !ok {
		return
	}
	asset, err := store.GetBlogAsset(slug, filePath)
	if err != nil {
		log.Printf("[BLOG] load asset %s/%s: %v", slug, filePath, err)
		http.Error(w, "Failed to load file", http.StatusInternalServerError)
		return
	}
	if asset == nil {
		http.Error(w, "file not found", http.StatusNotFound)
		return
	}
	if filePath == blogIndexFile {
		post.IndexObject = ""
		post.Published = false
		if err := store.SaveBlogPost(post); err != nil {
			log.Printf("[BLOG] save post %s: %v", slug, err)
			http.Error(w, "Failed to save post", http.StatusInternalServerError)
			return
		}
	}
	if err := store.DeleteBlogAsset(slug, filePath); err != nil {
		log.Printf("[BLOG] delete asset %s/%s: %v", slug, filePath, err)
		http.Error(w, "Failed to delete file", http.StatusInternalServerError)
		return
	}
	s.removeBlogObject(r, asset.StorageObject)
	blogAudit(user.GitHubUsername, "delete-file", slug, filePath, int(asset.SizeBytes))
	writeBlogJSON(w, http.StatusOK, map[string]any{"status": "deleted", "slug": slug, "path": filePath})
}

// blogUploadContentType validates the declared type against the allowlist and
// returns the bare media type that is stored.
func blogUploadContentType(header, filePath string) (string, error) {
	mediaType, _, err := mime.ParseMediaType(header)
	if err != nil {
		return "", errors.New("missing or invalid Content-Type header")
	}
	if filePath == blogIndexFile {
		if mediaType != "text/html" {
			return "", errors.New("index.html must be uploaded as text/html")
		}
		return mediaType, nil
	}
	if !blogAssetContentTypes[mediaType] {
		return "", fmt.Errorf("content type %s is not allowed for assets", mediaType)
	}
	return mediaType, nil
}

func (s *Server) putBlogFile(w http.ResponseWriter, r *http.Request, store blogStore, user *db.User, slug, rawPath string) {
	filePath, err := cleanBlogPath(rawPath)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	contentType, err := blogUploadContentType(r.Header.Get("Content-Type"), filePath)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if r.ContentLength > blogMaxFileBytes {
		http.Error(w, fmt.Sprintf("file exceeds %d bytes", blogMaxFileBytes), http.StatusRequestEntityTooLarge)
		return
	}
	content, err := io.ReadAll(http.MaxBytesReader(w, r.Body, blogMaxFileBytes))
	if err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			http.Error(w, fmt.Sprintf("file exceeds %d bytes", blogMaxFileBytes), http.StatusRequestEntityTooLarge)
			return
		}
		http.Error(w, "Failed to read upload", http.StatusBadRequest)
		return
	}
	if len(content) == 0 {
		http.Error(w, "empty file", http.StatusBadRequest)
		return
	}

	// One upload at a time per process keeps the quota check and the row
	// write together; the limits are guardrails for admins, not a hard cap
	// across instances.
	s.blogUploadMu.Lock()
	defer s.blogUploadMu.Unlock()

	post, assets, ok := s.loadBlogPost(w, store, slug, true)
	if !ok {
		return
	}
	var otherBytes int64
	otherFiles := 0
	previousObject := ""
	for _, a := range assets {
		if a.Path == filePath {
			previousObject = a.StorageObject
			continue
		}
		otherBytes += a.SizeBytes
		otherFiles++
	}
	if otherFiles+1 > blogMaxFiles {
		http.Error(w, fmt.Sprintf("post may hold at most %d files", blogMaxFiles), http.StatusRequestEntityTooLarge)
		return
	}
	if otherBytes+int64(len(content)) > blogMaxPostBytes {
		http.Error(w, fmt.Sprintf("post exceeds %d bytes", blogMaxPostBytes), http.StatusRequestEntityTooLarge)
		return
	}

	etag := blogETag(content)
	key := blogObjectKey(slug, filePath, strings.Trim(etag, `"`))
	if err := s.blogObjects.Put(r.Context(), key, blogServedContentType(contentType), content); err != nil {
		log.Printf("[BLOG] store %s: %v", key, err)
		http.Error(w, "Failed to store file", http.StatusInternalServerError)
		return
	}
	asset := db.BlogAsset{
		Slug:          slug,
		Path:          filePath,
		ContentType:   contentType,
		SizeBytes:     int64(len(content)),
		StorageObject: key,
		ETag:          etag,
		UploadedAt:    time.Now(),
	}
	if err := store.UpsertBlogAsset(&asset); err != nil {
		log.Printf("[BLOG] record %s: %v", key, err)
		s.removeBlogObject(r, key)
		http.Error(w, "Failed to record file", http.StatusInternalServerError)
		return
	}
	if filePath == blogIndexFile && post.IndexObject != key {
		post.IndexObject = key
		if err := store.SaveBlogPost(post); err != nil {
			log.Printf("[BLOG] save post %s: %v", slug, err)
			http.Error(w, "Failed to save post", http.StatusInternalServerError)
			return
		}
	}
	if previousObject != "" && previousObject != key {
		s.removeBlogObject(r, previousObject)
	}
	blogAudit(user.GitHubUsername, "upload", slug, filePath, len(content))
	writeBlogJSON(w, http.StatusOK, map[string]any{"file": blogFileToJSON(asset)})
}
