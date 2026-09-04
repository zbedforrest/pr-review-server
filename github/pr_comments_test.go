package github

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// GetPRComments must walk every page of both comment endpoints; busy PRs have
// far more than one page of review comments.
func TestGetPRComments_PaginatesBothEndpoints(t *testing.T) {
	var ts *httptest.Server
	ts = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("per_page") != "100" {
			t.Errorf("expected per_page=100, got %q on %s", r.URL.Query().Get("per_page"), r.URL.Path)
		}
		page := r.URL.Query().Get("page")
		if page == "" {
			page = "1"
		}
		kind := "issue"
		if strings.Contains(r.URL.Path, "/pulls/") {
			kind = "review"
		}
		w.Header().Set("Content-Type", "application/json")
		switch page {
		case "1":
			w.Header().Set("Link", fmt.Sprintf(`<%s%s?per_page=100&page=2>; rel="next"`, ts.URL, r.URL.Path))
			fmt.Fprintf(w, `[{"id":1,"body":"%s one","user":{"login":"a"}},{"id":2,"body":"%s two","user":{"login":"b"}}]`, kind, kind)
		default:
			fmt.Fprintf(w, `[{"id":3,"body":"%s three","user":{"login":"c"},"path":"f.go","line":7}]`, kind)
		}
	}))
	defer ts.Close()

	client := NewTestClient(ts.URL, "test-user")
	got, err := client.GetPRComments("tok", "o", "r", 1)
	if err != nil {
		t.Fatalf("GetPRComments: %v", err)
	}
	if len(got) != 6 {
		t.Fatalf("expected 6 comments across both endpoints and pages, got %d", len(got))
	}
	var reviewThird *PRComment
	for i := range got {
		if got[i].Body == "review three" {
			reviewThird = &got[i]
		}
	}
	if reviewThird == nil || reviewThird.Path != "f.go" || reviewThird.Line != 7 {
		t.Fatalf("second-page review comment not returned with its location: %+v", reviewThird)
	}
}
