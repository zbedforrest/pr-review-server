package server

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"pr-review-server/db"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

var (
	blogAdmin  = &db.User{ID: 1, GitHubUsername: "admin"}
	blogMember = &db.User{ID: 2, GitHubUsername: "member"}
)

func newBlogTestServer(t *testing.T) *Server {
	t.Helper()
	server, database := newTestServer(t, "admin")
	t.Cleanup(func() { _ = database.Close() })
	server.cfg.GitHubAppClientID = "app"
	server.cfg.AdminLogins = []string{"admin"}
	server.cfg.BlogLocalDir = t.TempDir()
	server.blogObjects = newBlogObjectStore(server.cfg, nil)
	return server
}

func blogRequest(t *testing.T, server *Server, user *db.User, method, target string, body []byte, headers map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, target, bytes.NewReader(body))
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	if user != nil {
		req = addUserToRequest(req, user)
	}
	w := httptest.NewRecorder()
	if strings.HasPrefix(target, blogAPIPath) {
		server.handleBlogAPI(w, req)
	} else {
		server.handleBlog(w, req)
	}
	return w
}

func putBlogMeta(t *testing.T, server *Server, user *db.User, slug string, title string, published bool) *httptest.ResponseRecorder {
	t.Helper()
	body, _ := json.Marshal(map[string]any{"title": title, "dek": "About " + title, "published": published})
	return blogRequest(t, server, user, http.MethodPut, blogAPIPath+"/"+slug, body, map[string]string{"Content-Type": "application/json"})
}

func putBlogFile(t *testing.T, server *Server, user *db.User, slug, path, contentType string, content []byte) *httptest.ResponseRecorder {
	t.Helper()
	return blogRequest(t, server, user, http.MethodPut, blogAPIPath+"/"+slug+"/files/"+path, content, map[string]string{"Content-Type": contentType})
}

// storedObjects lists the object files under a post's storage directory.
func storedObjects(t *testing.T, server *Server, slug string) []string {
	t.Helper()
	var found []string
	root := filepath.Join(server.cfg.BlogLocalDir, "blog", slug)
	_ = filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err == nil && !info.IsDir() {
			rel, _ := filepath.Rel(root, path)
			found = append(found, filepath.ToSlash(rel))
		}
		return nil
	})
	return found
}

const testPostHTML = `<!doctype html><html><body><h1>Hi</h1><img src="img/a.png"></body></html>`

var testPNG = []byte("\x89PNG\r\n\x1a\nfake")

// createPublishedPost uploads a page with one image and publishes it.
func createPublishedPost(t *testing.T, server *Server, slug string) {
	t.Helper()
	require.Equal(t, http.StatusCreated, putBlogMeta(t, server, blogAdmin, slug, "Post "+slug, false).Code)
	require.Equal(t, http.StatusOK, putBlogFile(t, server, blogAdmin, slug, "index.html", "text/html; charset=utf-8", []byte(testPostHTML)).Code)
	require.Equal(t, http.StatusOK, putBlogFile(t, server, blogAdmin, slug, "img/a.png", "image/png", testPNG).Code)
	require.Equal(t, http.StatusOK, putBlogMeta(t, server, blogAdmin, slug, "Post "+slug, true).Code)
}

func TestBlogIndex_HidesDraftsFromMembersAndShowsThemToAdmins(t *testing.T) {
	server := newBlogTestServer(t)
	createPublishedPost(t, server, "live-post")
	require.Equal(t, http.StatusCreated, putBlogMeta(t, server, blogAdmin, "draft-post", "Secret draft", false).Code)

	w := blogRequest(t, server, blogMember, http.MethodGet, "/blog", nil, nil)
	require.Equal(t, http.StatusOK, w.Code)
	assert.Equal(t, "text/html; charset=utf-8", w.Header().Get("Content-Type"))
	assert.Contains(t, w.Body.String(), "PRism Blog")
	assert.Contains(t, w.Body.String(), `href="/blog/live-post/"`)
	assert.Contains(t, w.Body.String(), "Post live-post")
	assert.Contains(t, w.Body.String(), "by admin")
	assert.NotContains(t, w.Body.String(), "Secret draft")

	w = blogRequest(t, server, blogAdmin, http.MethodGet, "/blog/", nil, nil)
	require.Equal(t, http.StatusOK, w.Code)
	assert.Contains(t, w.Body.String(), "Secret draft")
	assert.Contains(t, w.Body.String(), `class="draft"`)
}

func TestBlogIndex_EscapesTitles(t *testing.T) {
	server := newBlogTestServer(t)
	require.Equal(t, http.StatusCreated, putBlogMeta(t, server, blogAdmin, "xss", `<script>alert(1)</script>`, false).Code)

	w := blogRequest(t, server, blogAdmin, http.MethodGet, "/blog", nil, nil)
	require.Equal(t, http.StatusOK, w.Code)
	assert.NotContains(t, w.Body.String(), "<script>alert")
	assert.Contains(t, w.Body.String(), "&lt;script&gt;")
}

func TestBlogPage_ServesIndexHTMLWithStrictHeaders(t *testing.T) {
	server := newBlogTestServer(t)
	createPublishedPost(t, server, "hello")

	w := blogRequest(t, server, blogMember, http.MethodGet, "/blog/hello/", nil, nil)
	require.Equal(t, http.StatusOK, w.Code)
	assert.Equal(t, testPostHTML, w.Body.String())
	assert.Equal(t, "text/html; charset=utf-8", w.Header().Get("Content-Type"))
	assert.Equal(t, blogPageCSP, w.Header().Get("Content-Security-Policy"))
	assert.Contains(t, blogPageCSP, "style-src 'self' 'unsafe-inline'", "linked same-origin stylesheets must load")
	assert.NotContains(t, blogPageCSP, "script-src")
	assert.Equal(t, "DENY", w.Header().Get("X-Frame-Options"))
	assert.Equal(t, "nosniff", w.Header().Get("X-Content-Type-Options"))
	assert.NotEmpty(t, w.Header().Get("ETag"))

	w = blogRequest(t, server, blogMember, http.MethodGet, "/blog/hello/index.html", nil, nil)
	assert.Equal(t, http.StatusOK, w.Code)
	assert.Equal(t, blogPageCSP, w.Header().Get("Content-Security-Policy"))
}

func TestBlogAsset_ContentTypeCacheAndETag(t *testing.T) {
	server := newBlogTestServer(t)
	createPublishedPost(t, server, "hello")

	w := blogRequest(t, server, blogMember, http.MethodGet, "/blog/hello/img/a.png", nil, nil)
	require.Equal(t, http.StatusOK, w.Code)
	assert.Equal(t, testPNG, w.Body.Bytes())
	assert.Equal(t, "image/png", w.Header().Get("Content-Type"))
	assert.Equal(t, "private, max-age=3600", w.Header().Get("Cache-Control"))
	etag := w.Header().Get("ETag")
	assert.Equal(t, blogETag(testPNG), etag)

	w = blogRequest(t, server, blogMember, http.MethodGet, "/blog/hello/img/a.png", nil, map[string]string{"If-None-Match": etag})
	assert.Equal(t, http.StatusNotModified, w.Code)
	assert.Empty(t, w.Body.Bytes())
	w = blogRequest(t, server, blogMember, http.MethodGet, "/blog/hello/img/a.png", nil, map[string]string{"If-None-Match": `"other", W/` + etag})
	assert.Equal(t, http.StatusNotModified, w.Code, "weak tags in a list match")
	w = blogRequest(t, server, blogMember, http.MethodGet, "/blog/hello/img/a.png", nil, map[string]string{"If-None-Match": `"x` + strings.Trim(etag, `"`) + `y"`})
	assert.Equal(t, http.StatusOK, w.Code, "a tag that merely contains ours is not a match")

	w = blogRequest(t, server, blogMember, http.MethodHead, "/blog/hello/img/a.png", nil, nil)
	assert.Equal(t, http.StatusOK, w.Code)
	assert.Empty(t, w.Body.Bytes())
	assert.Equal(t, fmt.Sprint(len(testPNG)), w.Header().Get("Content-Length"))
	assert.Equal(t, "image/png", w.Header().Get("Content-Type"))

	w = blogRequest(t, server, blogMember, http.MethodGet, "/blog/hello/img/missing.png", nil, nil)
	assert.Equal(t, http.StatusNotFound, w.Code)
}

func TestBlogPage_RedirectsToTrailingSlash(t *testing.T) {
	server := newBlogTestServer(t)
	createPublishedPost(t, server, "hello")

	w := blogRequest(t, server, blogMember, http.MethodGet, "/blog/hello", nil, nil)
	assert.Equal(t, http.StatusMovedPermanently, w.Code)
	assert.Equal(t, "/blog/hello/", w.Header().Get("Location"))

	w = blogRequest(t, server, blogMember, http.MethodGet, "/blog/does-not-exist", nil, nil)
	assert.Equal(t, http.StatusMovedPermanently, w.Code, "the redirect must not reveal whether a slug exists")
}

func TestBlogPage_DraftsAreNotFoundForMembers(t *testing.T) {
	server := newBlogTestServer(t)
	require.Equal(t, http.StatusCreated, putBlogMeta(t, server, blogAdmin, "draft", "Draft", false).Code)
	require.Equal(t, http.StatusOK, putBlogFile(t, server, blogAdmin, "draft", "index.html", "text/html", []byte(testPostHTML)).Code)

	assert.Equal(t, http.StatusNotFound, blogRequest(t, server, blogMember, http.MethodGet, "/blog/draft/", nil, nil).Code)
	assert.Equal(t, http.StatusOK, blogRequest(t, server, blogAdmin, http.MethodGet, "/blog/draft/", nil, nil).Code)
	assert.Equal(t, http.StatusNotFound, blogRequest(t, server, blogMember, http.MethodGet, blogAPIPath+"/draft", nil, nil).Code)
	assert.Equal(t, http.StatusOK, blogRequest(t, server, blogAdmin, http.MethodGet, blogAPIPath+"/draft", nil, nil).Code)
}

func TestBlogPage_RejectsBadSlugsAndTraversal(t *testing.T) {
	server := newBlogTestServer(t)
	createPublishedPost(t, server, "hello")

	for _, target := range []string{
		"/blog/Hello/", "/blog/-bad/", "/blog/a/", "/blog/" + strings.Repeat("a", 82) + "/",
		"/blog/hello/../hello/index.html", "/blog/hello/img/../../etc/passwd", "/blog/hello//img/a.png", "/blog/hello/.hidden",
	} {
		w := blogRequest(t, server, blogAdmin, http.MethodGet, target, nil, nil)
		assert.Equal(t, http.StatusNotFound, w.Code, target)
	}
	assert.Equal(t, http.StatusUnauthorized, blogRequest(t, server, nil, http.MethodGet, "/blog", nil, nil).Code)
	assert.Equal(t, http.StatusMethodNotAllowed, blogRequest(t, server, blogAdmin, http.MethodPost, "/blog", nil, nil).Code)
}

func TestCleanBlogPath(t *testing.T) {
	for _, ok := range []string{"index.html", "img/a.png", "fonts/Inter-Bold.woff2", "a/b/c/d.css", "video_1.mp4"} {
		got, err := cleanBlogPath(ok)
		assert.NoError(t, err, ok)
		assert.Equal(t, ok, got)
	}
	for _, bad := range []string{"", "/abs.png", "../up.png", "img/../x.png", "./x.png", "img//x.png", "img/", ".env", "a\\b.png", "sp ace.png", "x\x00.png", strings.Repeat("a", 513)} {
		_, err := cleanBlogPath(bad)
		assert.Error(t, err, "%q", bad)
	}
}

func TestBlogAPI_AdminGate(t *testing.T) {
	server := newBlogTestServer(t)
	createPublishedPost(t, server, "hello")

	assert.Equal(t, http.StatusForbidden, putBlogMeta(t, server, blogMember, "hello", "Hijack", true).Code)
	assert.Equal(t, http.StatusForbidden, putBlogFile(t, server, blogMember, "hello", "index.html", "text/html", []byte("<p>x</p>")).Code)
	assert.Equal(t, http.StatusForbidden, blogRequest(t, server, blogMember, http.MethodDelete, blogAPIPath+"/hello", nil, nil).Code)
	assert.Equal(t, http.StatusUnauthorized, blogRequest(t, server, nil, http.MethodGet, blogAPIPath, nil, nil).Code)

	w := blogRequest(t, server, blogMember, http.MethodGet, "/blog/hello/", nil, nil)
	assert.Equal(t, http.StatusOK, w.Code)
	assert.Equal(t, testPostHTML, w.Body.String(), "a member's rejected writes must not change the post")
}

func TestBlogAPI_ListShowsDraftsToAdminsOnly(t *testing.T) {
	server := newBlogTestServer(t)
	createPublishedPost(t, server, "live-post")
	require.Equal(t, http.StatusCreated, putBlogMeta(t, server, blogAdmin, "draft-post", "Draft", false).Code)

	var resp struct {
		Posts []blogPostJSON `json:"posts"`
	}
	w := blogRequest(t, server, blogMember, http.MethodGet, blogAPIPath, nil, nil)
	require.Equal(t, http.StatusOK, w.Code)
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	require.Len(t, resp.Posts, 1)
	assert.Equal(t, "live-post", resp.Posts[0].Slug)
	assert.Equal(t, 2, resp.Posts[0].FileCount)
	assert.EqualValues(t, len(testPostHTML)+len(testPNG), resp.Posts[0].SizeBytes)
	assert.True(t, resp.Posts[0].HasIndex)
	assert.Equal(t, "/blog/live-post/", resp.Posts[0].URL)

	w = blogRequest(t, server, blogAdmin, http.MethodGet, blogAPIPath, nil, nil)
	require.Equal(t, http.StatusOK, w.Code)
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	require.Len(t, resp.Posts, 2)
	bySlug := map[string]blogPostJSON{}
	for _, p := range resp.Posts {
		bySlug[p.Slug] = p
	}
	assert.Contains(t, bySlug, "live-post")
	assert.False(t, bySlug["draft-post"].HasIndex)
	assert.False(t, bySlug["draft-post"].Published)
}

func TestBlogAPI_MetadataValidation(t *testing.T) {
	server := newBlogTestServer(t)

	assert.Equal(t, http.StatusBadRequest, putBlogMeta(t, server, blogAdmin, "Bad_Slug", "x", false).Code)
	assert.Equal(t, http.StatusBadRequest, putBlogMeta(t, server, blogAdmin, "ok", "   ", false).Code)
	assert.Equal(t, http.StatusBadRequest, putBlogMeta(t, server, blogAdmin, "ok", strings.Repeat("t", 201), false).Code)
	assert.Equal(t, http.StatusConflict, putBlogMeta(t, server, blogAdmin, "ok", "No page yet", true).Code, "publishing needs index.html")

	w := putBlogMeta(t, server, blogAdmin, "ok", "Fine", false)
	require.Equal(t, http.StatusCreated, w.Code)
	var created struct {
		Post blogPostJSON `json:"post"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &created))
	assert.Equal(t, "admin", created.Post.AuthorLogin)
	assert.Nil(t, created.Post.PublishedAt)

	require.Equal(t, http.StatusOK, putBlogFile(t, server, blogAdmin, "ok", "index.html", "text/html", []byte(testPostHTML)).Code)
	w = putBlogMeta(t, server, blogAdmin, "ok", "Fine", true)
	require.Equal(t, http.StatusOK, w.Code)
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &created))
	assert.True(t, created.Post.Published)
	assert.NotNil(t, created.Post.PublishedAt)
}

func TestBlogAPI_UploadValidation(t *testing.T) {
	server := newBlogTestServer(t)
	require.Equal(t, http.StatusCreated, putBlogMeta(t, server, blogAdmin, "hello", "Hello", false).Code)

	assert.Equal(t, http.StatusNotFound, putBlogFile(t, server, blogAdmin, "nope", "index.html", "text/html", []byte("x")).Code, "metadata first")
	assert.Equal(t, http.StatusBadRequest, putBlogFile(t, server, blogAdmin, "hello", "index.html", "image/png", []byte("x")).Code, "index.html must be html")
	assert.Equal(t, http.StatusBadRequest, putBlogFile(t, server, blogAdmin, "hello", "page2.html", "text/html", []byte("x")).Code, "only index.html may be html")
	assert.Equal(t, http.StatusBadRequest, putBlogFile(t, server, blogAdmin, "hello", "run.js", "application/javascript", []byte("x")).Code)
	assert.Equal(t, http.StatusBadRequest, putBlogFile(t, server, blogAdmin, "hello", "img/a.png", "", testPNG).Code, "content type required")
	assert.Equal(t, http.StatusBadRequest, putBlogFile(t, server, blogAdmin, "hello", "img/a.png", "image/png", nil).Code, "empty body")
	assert.Equal(t, http.StatusBadRequest, putBlogFile(t, server, blogAdmin, "hello", "../escape.png", "image/png", testPNG).Code)
	assert.Equal(t, http.StatusBadRequest, putBlogFile(t, server, blogAdmin, "hello", "img/../../escape.png", "image/png", testPNG).Code)
	assert.Equal(t, http.StatusBadRequest, blogRequest(t, server, blogAdmin, http.MethodPut, blogAPIPath+"/hello/files//abs.png", testPNG, map[string]string{"Content-Type": "image/png"}).Code)

	entries, err := os.ReadDir(server.cfg.BlogLocalDir)
	require.NoError(t, err)
	assert.Empty(t, entries, "rejected uploads must not touch storage")
}

func TestBlogAPI_UploadSizeLimits(t *testing.T) {
	server := newBlogTestServer(t)
	require.Equal(t, http.StatusCreated, putBlogMeta(t, server, blogAdmin, "hello", "Hello", false).Code)

	big := bytes.Repeat([]byte("a"), blogMaxFileBytes+1)
	w := putBlogFile(t, server, blogAdmin, "hello", "img/big.png", "image/png", big)
	assert.Equal(t, http.StatusRequestEntityTooLarge, w.Code)

	req := httptest.NewRequest(http.MethodPut, blogAPIPath+"/hello/files/img/big.png", strings.NewReader("tiny"))
	req.Header.Set("Content-Type", "image/png")
	req.ContentLength = blogMaxFileBytes + 1
	rec := httptest.NewRecorder()
	server.handleBlogAPI(rec, addUserToRequest(req, blogAdmin))
	assert.Equal(t, http.StatusRequestEntityTooLarge, rec.Code, "a declared oversize length is refused before reading")

	exact := bytes.Repeat([]byte("a"), blogMaxFileBytes)
	assert.Equal(t, http.StatusOK, putBlogFile(t, server, blogAdmin, "hello", "img/max.png", "image/png", exact).Code)
}

func TestBlogAPI_FileCountLimit(t *testing.T) {
	server := newBlogTestServer(t)
	require.Equal(t, http.StatusCreated, putBlogMeta(t, server, blogAdmin, "many", "Many", false).Code)

	for i := 0; i < blogMaxFiles; i++ {
		require.Equal(t, http.StatusOK, putBlogFile(t, server, blogAdmin, "many", fmt.Sprintf("img/%d.png", i), "image/png", testPNG).Code)
	}
	assert.Equal(t, http.StatusRequestEntityTooLarge, putBlogFile(t, server, blogAdmin, "many", "img/extra.png", "image/png", testPNG).Code)
	assert.Equal(t, http.StatusOK, putBlogFile(t, server, blogAdmin, "many", "img/0.png", "image/png", testPNG).Code, "replacing a file does not count as a new one")
}

func TestBlogAPI_ReuploadReplacesContent(t *testing.T) {
	server := newBlogTestServer(t)
	createPublishedPost(t, server, "hello")

	updated := []byte("<!doctype html><p>v2</p>")
	require.Equal(t, http.StatusOK, putBlogFile(t, server, blogAdmin, "hello", "index.html", "text/html", updated).Code)

	w := blogRequest(t, server, blogMember, http.MethodGet, "/blog/hello/", nil, nil)
	require.Equal(t, http.StatusOK, w.Code)
	assert.Equal(t, string(updated), w.Body.String())
	assert.Equal(t, blogETag(updated), w.Header().Get("ETag"))

	objects := storedObjects(t, server, "hello")
	assert.Len(t, objects, 2, "the replaced index.html object is removed: %v", objects)
	for _, obj := range objects {
		assert.NotEqual(t, "index.html."+strings.Trim(blogETag([]byte(testPostHTML)), `"`)[:16], obj)
	}
}

func TestBlogAPI_DeleteFile(t *testing.T) {
	server := newBlogTestServer(t)
	createPublishedPost(t, server, "hello")

	assert.Equal(t, http.StatusForbidden, blogRequest(t, server, blogMember, http.MethodDelete, blogAPIPath+"/hello/files/img/a.png", nil, nil).Code)
	assert.Equal(t, http.StatusNotFound, blogRequest(t, server, blogAdmin, http.MethodDelete, blogAPIPath+"/hello/files/img/zzz.png", nil, nil).Code)
	assert.Equal(t, http.StatusBadRequest, blogRequest(t, server, blogAdmin, http.MethodDelete, blogAPIPath+"/hello/files/../a.png", nil, nil).Code)

	require.Equal(t, http.StatusOK, blogRequest(t, server, blogAdmin, http.MethodDelete, blogAPIPath+"/hello/files/img/a.png", nil, nil).Code)
	assert.Equal(t, http.StatusNotFound, blogRequest(t, server, blogMember, http.MethodGet, "/blog/hello/img/a.png", nil, nil).Code)
	assert.Equal(t, http.StatusOK, blogRequest(t, server, blogMember, http.MethodGet, "/blog/hello/", nil, nil).Code, "the page stays published")
	assert.Len(t, storedObjects(t, server, "hello"), 1)

	require.Equal(t, http.StatusOK, blogRequest(t, server, blogAdmin, http.MethodDelete, blogAPIPath+"/hello/files/index.html", nil, nil).Code)
	assert.Equal(t, http.StatusNotFound, blogRequest(t, server, blogMember, http.MethodGet, "/blog/hello/", nil, nil).Code, "a post without a page is unpublished")
	w := blogRequest(t, server, blogAdmin, http.MethodGet, blogAPIPath+"/hello", nil, nil)
	require.Equal(t, http.StatusOK, w.Code)
	var detail struct {
		Post blogPostJSON `json:"post"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &detail))
	assert.False(t, detail.Post.Published)
	assert.False(t, detail.Post.HasIndex)
	assert.Empty(t, storedObjects(t, server, "hello"))
}

func TestBlogAPI_DeleteRemovesRowsAndObjects(t *testing.T) {
	server := newBlogTestServer(t)
	createPublishedPost(t, server, "hello")
	require.Len(t, storedObjects(t, server, "hello"), 2)

	assert.Equal(t, http.StatusNotFound, blogRequest(t, server, blogAdmin, http.MethodDelete, blogAPIPath+"/missing", nil, nil).Code)
	w := blogRequest(t, server, blogAdmin, http.MethodDelete, blogAPIPath+"/hello", nil, nil)
	require.Equal(t, http.StatusOK, w.Code)

	assert.Equal(t, http.StatusNotFound, blogRequest(t, server, blogAdmin, http.MethodGet, "/blog/hello/", nil, nil).Code)
	assert.Equal(t, http.StatusNotFound, blogRequest(t, server, blogAdmin, http.MethodGet, "/blog/hello/img/a.png", nil, nil).Code)
	assert.Empty(t, storedObjects(t, server, "hello"))
}

func TestBlogAPI_UnknownRoutes(t *testing.T) {
	server := newBlogTestServer(t)
	createPublishedPost(t, server, "hello")

	assert.Equal(t, http.StatusMethodNotAllowed, blogRequest(t, server, blogAdmin, http.MethodPost, blogAPIPath, nil, nil).Code)
	assert.Equal(t, http.StatusMethodNotAllowed, blogRequest(t, server, blogAdmin, http.MethodPost, blogAPIPath+"/hello", nil, nil).Code)
	assert.Equal(t, http.StatusNotFound, blogRequest(t, server, blogAdmin, http.MethodPut, blogAPIPath+"/hello/other/x", nil, nil).Code)
	assert.Equal(t, http.StatusMethodNotAllowed, blogRequest(t, server, blogAdmin, http.MethodGet, blogAPIPath+"/hello/files/index.html", nil, nil).Code)
	assert.Equal(t, http.StatusMethodNotAllowed, blogRequest(t, server, blogAdmin, http.MethodPost, blogAPIPath+"/hello/files/index.html", nil, nil).Code)
}

func TestReactApp_StillServesDashboardForOtherPaths(t *testing.T) {
	server := newBlogTestServer(t)
	for _, target := range []string{"/", "/settings", "/usage-stats", "/blogger"} {
		req := httptest.NewRequest(http.MethodGet, target, nil)
		w := httptest.NewRecorder()
		server.handleReactApp(w, req)
		assert.Equal(t, http.StatusOK, w.Code, target)
		assert.Equal(t, "text/html; charset=utf-8", w.Header().Get("Content-Type"), target)
		assert.Contains(t, strings.ToLower(w.Body.String()), "<!doctype html>", target)
	}
}

func TestEtagMatches(t *testing.T) {
	assert.True(t, etagMatches(`"abc"`, `"abc"`))
	assert.True(t, etagMatches(`W/"abc"`, `"abc"`))
	assert.True(t, etagMatches(`"x", "abc"`, `"abc"`))
	assert.True(t, etagMatches(`*`, `"abc"`))
	assert.False(t, etagMatches(``, `"abc"`))
	assert.False(t, etagMatches(`"abcd"`, `"abc"`))
	assert.False(t, etagMatches(`"zabcz"`, `"abc"`))
}

func TestLocalBlogObjects_RefusesKeysOutsideRoot(t *testing.T) {
	store := localBlogObjects{dir: t.TempDir()}
	_, err := store.Get(t.Context(), "../outside")
	assert.Error(t, err)
	assert.Error(t, store.Put(t.Context(), "../outside", "text/plain", []byte("x")))
}
