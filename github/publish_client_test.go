package github

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestCreateReviewSendsDraftCommentsAndResolvesIDs(t *testing.T) {
	var reviewReq map[string]any
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/repos/acme/example/pulls/7/reviews":
			body, _ := io.ReadAll(r.Body)
			if err := json.Unmarshal(body, &reviewReq); err != nil {
				t.Errorf("bad review body: %v", err)
			}
			fmt.Fprint(w, `{"id": 9001, "state": "COMMENTED"}`)
		case r.Method == http.MethodGet && r.URL.Path == "/repos/acme/example/pulls/7/reviews/9001/comments":
			fmt.Fprint(w, `[{"id": 31, "path": "a.go", "line": 10}, {"id": 32, "path": "b.go", "line": 22, "start_line": 20}]`)
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer ts.Close()

	c := NewTestClient(ts.URL, "tester")
	reviewID, ids, err := c.CreateReview(context.Background(), "acme", "example", 7, "abc123", "", []ReviewCommentInput{
		{Path: "a.go", Line: 10, Body: "first"},
		{Path: "b.go", Line: 22, StartLine: 20, Body: "second"},
	})
	if err != nil {
		t.Fatalf("CreateReview: %v", err)
	}
	if reviewID != 9001 || len(ids) != 2 || ids[0] != 31 || ids[1] != 32 {
		t.Fatalf("reviewID=%d ids=%v", reviewID, ids)
	}
	if reviewReq["event"] != "COMMENT" || reviewReq["commit_id"] != "abc123" {
		t.Errorf("review request = %v", reviewReq)
	}
	comments := reviewReq["comments"].([]any)
	first := comments[0].(map[string]any)
	second := comments[1].(map[string]any)
	if first["side"] != "RIGHT" || first["line"] != float64(10) || first["path"] != "a.go" || first["body"] != "first" {
		t.Errorf("first comment = %v", first)
	}
	if _, has := first["start_line"]; has {
		t.Errorf("single-line comment must not send start_line: %v", first)
	}
	if second["start_line"] != float64(20) || second["start_side"] != "RIGHT" || second["line"] != float64(22) {
		t.Errorf("second comment = %v", second)
	}
}

func TestIssueCommentCreateEditList(t *testing.T) {
	var edited string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/repos/acme/example/issues/7/comments":
			fmt.Fprint(w, `{"id": 501}`)
		case r.Method == http.MethodPatch && r.URL.Path == "/repos/acme/example/issues/comments/501":
			body, _ := io.ReadAll(r.Body)
			edited = string(body)
			fmt.Fprint(w, `{"id": 501}`)
		case r.Method == http.MethodGet && r.URL.Path == "/repos/acme/example/issues/7/comments":
			if r.URL.Query().Get("page") == "2" {
				fmt.Fprint(w, `[{"id": 2, "body": "second", "user": {"login": "bob"}}]`)
				return
			}
			w.Header().Set("Link", fmt.Sprintf(`<%s/repos/acme/example/issues/7/comments?page=2&per_page=100>; rel="next"`, "http://"+r.Host))
			fmt.Fprint(w, `[{"id": 1, "body": "first", "user": {"login": "alice"}}]`)
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer ts.Close()

	c := NewTestClient(ts.URL, "tester")
	ctx := context.Background()
	id, err := c.CreateIssueComment(ctx, "acme", "example", 7, "hello")
	if err != nil || id != 501 {
		t.Fatalf("CreateIssueComment = %d, %v", id, err)
	}
	if err := c.EditIssueComment(ctx, "acme", "example", 501, "updated"); err != nil {
		t.Fatalf("EditIssueComment: %v", err)
	}
	if !strings.Contains(edited, `"body":"updated"`) {
		t.Errorf("edit body = %s", edited)
	}
	list, err := c.ListIssueComments(ctx, "acme", "example", 7)
	if err != nil {
		t.Fatalf("ListIssueComments: %v", err)
	}
	if len(list) != 2 || list[0].ID != 1 || list[0].Author != "alice" || list[1].Body != "second" {
		t.Errorf("list = %+v", list)
	}
}

func TestListReviewCommentsAndFilePatches(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/repos/acme/example/pulls/7/comments":
			fmt.Fprint(w, `[{"id": 31, "path": "a.go", "line": 10, "body": "x", "user": {"login": "prism[bot]"}},
				{"id": 40, "path": "a.go", "line": 10, "body": "reply", "in_reply_to_id": 31, "start_line": 8, "user": {"login": "dev"}}]`)
		case r.Method == http.MethodGet && r.URL.Path == "/repos/acme/example/pulls/7/files":
			if r.URL.Query().Get("per_page") != "100" {
				t.Errorf("expected per_page=100, got %s", r.URL.RawQuery)
			}
			fmt.Fprint(w, `[{"filename": "a.go", "patch": "@@ -1 +1 @@\n-a\n+b"}, {"filename": "bin.png"}]`)
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer ts.Close()

	c := NewTestClient(ts.URL, "tester")
	ctx := context.Background()
	rc, err := c.ListReviewComments(ctx, "acme", "example", 7)
	if err != nil {
		t.Fatalf("ListReviewComments: %v", err)
	}
	if len(rc) != 2 || rc[0].ID != 31 || rc[0].Author != "prism[bot]" || rc[0].Path != "a.go" || rc[0].Line != 10 || rc[1].InReplyToID != 31 || rc[1].StartLine != 8 {
		t.Errorf("review comments = %+v", rc)
	}
	patches, err := c.GetPRFilePatches(ctx, "acme", "example", 7)
	if err != nil {
		t.Fatalf("GetPRFilePatches: %v", err)
	}
	if patches["a.go"] != "@@ -1 +1 @@\n-a\n+b" {
		t.Errorf("patches = %v", patches)
	}
	if _, ok := patches["bin.png"]; ok {
		t.Errorf("files without a patch must be omitted: %v", patches)
	}
}
