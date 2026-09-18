package github

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func userReviewErr(t *testing.T, err error) *UserReviewError {
	t.Helper()
	var ure *UserReviewError
	if !errors.As(err, &ure) {
		t.Fatalf("expected *UserReviewError, got %T: %v", err, err)
	}
	return ure
}

func TestSubmitReviewAsUser_Approve(t *testing.T) {
	var got map[string]any
	var authz string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/repos/acme/example/pulls/7/reviews" {
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
			return
		}
		authz = r.Header.Get("Authorization")
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &got)
		fmt.Fprint(w, `{"id": 2233, "state": "APPROVED", "commit_id": "abc1234", "html_url": "https://github.com/acme/example/pull/7#pullrequestreview-2233"}`)
	}))
	defer ts.Close()

	res, err := SubmitReviewAsUser(context.Background(), ts.URL, "gho_user", "acme", "example", 7, "abc1234", "APPROVE", "")
	if err != nil {
		t.Fatalf("SubmitReviewAsUser: %v", err)
	}
	if authz != "Bearer gho_user" {
		t.Errorf("authorization = %q", authz)
	}
	if got["event"] != "APPROVE" || got["commit_id"] != "abc1234" {
		t.Errorf("request = %v", got)
	}
	if _, has := got["body"]; has {
		t.Errorf("empty body must be omitted: %v", got)
	}
	if res.ReviewID != 2233 || res.State != "APPROVED" || res.HeadSHA != "abc1234" || res.HTMLURL == "" {
		t.Errorf("result = %+v", res)
	}
}

func TestSubmitReviewAsUser_RequestChangesRequiresBody(t *testing.T) {
	called := false
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { called = true }))
	defer ts.Close()

	for _, event := range []string{"REQUEST_CHANGES", "COMMENT"} {
		_, err := SubmitReviewAsUser(context.Background(), ts.URL, "gho_user", "acme", "example", 7, "abc", event, "   ")
		if ure := userReviewErr(t, err); ure.Code != "validation" {
			t.Errorf("%s: code = %q", event, ure.Code)
		}
	}
	_, err := SubmitReviewAsUser(context.Background(), ts.URL, "gho_user", "acme", "example", 7, "abc", "MERGE", "x")
	if ure := userReviewErr(t, err); ure.Code != "validation" {
		t.Errorf("unknown event code = %q", ure.Code)
	}
	if called {
		t.Error("validation failures must not reach GitHub")
	}
}

func statusServer(t *testing.T, status int, body string, headers map[string]string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		for k, v := range headers {
			w.Header().Set(k, v)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		fmt.Fprint(w, body)
	}))
}

func TestSubmitReviewAsUser_MapsOwnPR422(t *testing.T) {
	ts := statusServer(t, 422, `{"message":"Unprocessable Entity","errors":["Review Can not approve your own pull request"]}`, nil)
	defer ts.Close()
	_, err := SubmitReviewAsUser(context.Background(), ts.URL, "gho", "acme", "example", 7, "abc", "APPROVE", "")
	ure := userReviewErr(t, err)
	if ure.Code != "own_pr" || ure.Status != 422 {
		t.Errorf("got %+v", ure)
	}
}

func TestSubmitReviewAsUser_MapsClosed422AndOtherValidation(t *testing.T) {
	ts := statusServer(t, 422, `{"message":"Validation Failed","errors":[{"message":"Can not approve a closed pull request"}]}`, nil)
	defer ts.Close()
	_, err := SubmitReviewAsUser(context.Background(), ts.URL, "gho", "acme", "example", 7, "abc", "APPROVE", "")
	if ure := userReviewErr(t, err); ure.Code != "pr_closed" {
		t.Errorf("closed: got %+v", ure)
	}

	other := statusServer(t, 422, `{"message":"Validation Failed","errors":[{"message":"commit_id is not part of the pull request"}]}`, nil)
	defer other.Close()
	_, err = SubmitReviewAsUser(context.Background(), other.URL, "gho", "acme", "example", 7, "abc", "APPROVE", "")
	if ure := userReviewErr(t, err); ure.Code != "validation" {
		t.Errorf("other: got %+v", ure)
	}
}

func TestSubmitReviewAsUser_Maps403ToNoPermissionAnd404(t *testing.T) {
	ts := statusServer(t, 403, `{"message":"Resource not accessible by integration"}`, nil)
	defer ts.Close()
	_, err := SubmitReviewAsUser(context.Background(), ts.URL, "gho", "acme", "example", 7, "abc", "APPROVE", "")
	if ure := userReviewErr(t, err); ure.Code != "no_permission" || ure.Status != 403 {
		t.Errorf("403: got %+v", ure)
	}

	nf := statusServer(t, 404, `{"message":"Not Found"}`, nil)
	defer nf.Close()
	_, err = SubmitReviewAsUser(context.Background(), nf.URL, "gho", "acme", "example", 7, "abc", "APPROVE", "")
	if ure := userReviewErr(t, err); ure.Code != "no_permission" || ure.Status != 404 {
		t.Errorf("404: got %+v", ure)
	}
}

func TestSubmitReviewAsUser_Maps401ToReauth(t *testing.T) {
	ts := statusServer(t, 401, `{"message":"Bad credentials"}`, nil)
	defer ts.Close()
	_, err := SubmitReviewAsUser(context.Background(), ts.URL, "gho", "acme", "example", 7, "abc", "COMMENT", "hi")
	if ure := userReviewErr(t, err); ure.Code != "reauth_required" || ure.Status != 401 {
		t.Errorf("got %+v", ure)
	}
	_, err = SubmitReviewAsUser(context.Background(), ts.URL, "", "acme", "example", 7, "abc", "COMMENT", "hi")
	if ure := userReviewErr(t, err); ure.Code != "reauth_required" {
		t.Errorf("empty token: got %+v", ure)
	}
}

func TestSubmitReviewAsUser_MapsRateLimit(t *testing.T) {
	reset := time.Now().Add(20 * time.Minute).Unix()
	ts := statusServer(t, 403, `{"message":"API rate limit exceeded"}`, map[string]string{
		"X-RateLimit-Remaining": "0", "X-RateLimit-Limit": "5000", "X-RateLimit-Reset": fmt.Sprint(reset),
	})
	defer ts.Close()
	_, err := SubmitReviewAsUser(context.Background(), ts.URL, "gho", "acme", "example", 7, "abc", "APPROVE", "")
	ure := userReviewErr(t, err)
	if ure.Code != "rate_limited" || ure.RetryAfter < 15*time.Minute {
		t.Errorf("got %+v", ure)
	}

	abuse := statusServer(t, 403, `{"message":"You have exceeded a secondary rate limit","documentation_url":"https://docs.github.com/rest/overview/rate-limits-for-the-rest-api#about-secondary-rate-limits"}`, map[string]string{"Retry-After": "30"})
	defer abuse.Close()
	_, err = SubmitReviewAsUser(context.Background(), abuse.URL, "gho", "acme", "example", 7, "abc", "APPROVE", "")
	if ure := userReviewErr(t, err); ure.Code != "rate_limited" || ure.RetryAfter != 30*time.Second {
		t.Errorf("abuse: got %+v", ure)
	}
}

func TestSubmitReviewAsUser_NetworkErrorIsGitHubError(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	ts.Close()
	_, err := SubmitReviewAsUser(context.Background(), ts.URL, "gho", "acme", "example", 7, "abc", "APPROVE", "")
	if ure := userReviewErr(t, err); ure.Code != "github_error" || ure.Status != 0 {
		t.Errorf("got %+v", ure)
	}
}

func TestGetPRHeadAsUser(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/repos/acme/example/pulls/7" || r.Header.Get("Authorization") != "Bearer gho" {
			t.Errorf("unexpected %s %s auth=%q", r.Method, r.URL.Path, r.Header.Get("Authorization"))
		}
		fmt.Fprint(w, `{"number": 7, "state": "closed", "merged": true, "draft": true, "head": {"sha": "def5678"}}`)
	}))
	defer ts.Close()
	head, err := GetPRHeadAsUser(context.Background(), ts.URL, "gho", "acme", "example", 7)
	if err != nil || head.SHA != "def5678" || head.State != "closed" || !head.Merged || !head.Draft {
		t.Fatalf("got %+v err=%v", head, err)
	}
	nf := statusServer(t, 404, `{"message":"Not Found"}`, nil)
	defer nf.Close()
	_, err = GetPRHeadAsUser(context.Background(), nf.URL, "gho", "acme", "example", 7)
	if ure := userReviewErr(t, err); ure.Code != "no_permission" {
		t.Errorf("404: got %+v", ure)
	}
}
