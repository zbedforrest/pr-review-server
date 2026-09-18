package github

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/google/go-github/v57/github"
	"golang.org/x/oauth2"
)

// UserReviewResult is the review GitHub created on the human's behalf.
type UserReviewResult struct {
	ReviewID int64
	HTMLURL  string
	State    string
	HeadSHA  string
}

// UserReviewError classifies a failed user-token call. Code is one of
// own_pr, pr_closed, no_permission, reauth_required, rate_limited,
// validation or github_error; Status is GitHub's HTTP status (0 when the
// request never completed).
type UserReviewError struct {
	Status     int
	Code       string
	Message    string
	RetryAfter time.Duration
}

func (e *UserReviewError) Error() string {
	if e.Status == 0 {
		return fmt.Sprintf("github %s: %s", e.Code, e.Message)
	}
	return fmt.Sprintf("github %s (status %d): %s", e.Code, e.Status, e.Message)
}

var userReviewEvents = map[string]bool{"APPROVE": true, "REQUEST_CHANGES": true, "COMMENT": true}

// newUserClient builds a throwaway REST client for one call with the human's
// own token; the App installation client is never involved.
func newUserClient(ctx context.Context, apiBase, token string) (*github.Client, error) {
	httpClient := oauth2.NewClient(ctx, oauth2.StaticTokenSource(&oauth2.Token{AccessToken: token}))
	httpClient.Timeout = 20 * time.Second
	gh := github.NewClient(httpClient)
	if apiBase == "" {
		apiBase = githubAPIBase
	}
	if apiBase != githubAPIBase {
		u, err := url.Parse(strings.TrimRight(apiBase, "/") + "/")
		if err != nil {
			return nil, err
		}
		gh.BaseURL = u
	}
	return gh, nil
}

// SubmitReviewAsUser posts one review with the caller's own token so GitHub
// attributes it to the human. commitSHA pins the reviewed head.
func SubmitReviewAsUser(ctx context.Context, apiBase, token, owner, repo string, number int, commitSHA, event, body string) (*UserReviewResult, error) {
	if !userReviewEvents[event] {
		return nil, &UserReviewError{Code: "validation", Message: fmt.Sprintf("unknown review event %q", event)}
	}
	if event != "APPROVE" && strings.TrimSpace(body) == "" {
		return nil, &UserReviewError{Code: "validation", Message: "a review body is required for " + event}
	}
	if token == "" {
		return nil, &UserReviewError{Code: "reauth_required", Message: "no GitHub token"}
	}
	gh, err := newUserClient(ctx, apiBase, token)
	if err != nil {
		return nil, &UserReviewError{Code: "github_error", Message: err.Error()}
	}
	req := &github.PullRequestReviewRequest{Event: github.String(event)}
	if commitSHA != "" {
		req.CommitID = github.String(commitSHA)
	}
	if body != "" {
		req.Body = github.String(body)
	}
	review, _, err := gh.PullRequests.CreateReview(ctx, owner, repo, number, req)
	if err != nil {
		return nil, mapUserReviewError(err)
	}
	return &UserReviewResult{
		ReviewID: review.GetID(),
		HTMLURL:  review.GetHTMLURL(),
		State:    review.GetState(),
		HeadSHA:  review.GetCommitID(),
	}, nil
}

// GetPRHeadAsUser reads the PR's current head and state with the caller's
// token, which also proves the token can see the repository.
func GetPRHeadAsUser(ctx context.Context, apiBase, token, owner, repo string, number int) (headSHA, state string, merged bool, err error) {
	if token == "" {
		return "", "", false, &UserReviewError{Code: "reauth_required", Message: "no GitHub token"}
	}
	gh, err := newUserClient(ctx, apiBase, token)
	if err != nil {
		return "", "", false, &UserReviewError{Code: "github_error", Message: err.Error()}
	}
	pr, _, err := gh.PullRequests.Get(ctx, owner, repo, number)
	if err != nil {
		return "", "", false, mapUserReviewError(err)
	}
	return pr.GetHead().GetSHA(), pr.GetState(), pr.GetMerged(), nil
}

func mapUserReviewError(err error) error {
	var rateErr *github.RateLimitError
	if errors.As(err, &rateErr) {
		return &UserReviewError{Status: statusOf(rateErr.Response), Code: "rate_limited", Message: rateErr.Message, RetryAfter: time.Until(rateErr.Rate.Reset.Time)}
	}
	var abuseErr *github.AbuseRateLimitError
	if errors.As(err, &abuseErr) {
		retry := time.Minute
		if abuseErr.RetryAfter != nil {
			retry = *abuseErr.RetryAfter
		}
		return &UserReviewError{Status: statusOf(abuseErr.Response), Code: "rate_limited", Message: abuseErr.Message, RetryAfter: retry}
	}
	var ghErr *github.ErrorResponse
	if !errors.As(err, &ghErr) {
		return &UserReviewError{Code: "github_error", Message: err.Error()}
	}
	status := statusOf(ghErr.Response)
	message := ghErr.Message
	for _, e := range ghErr.Errors {
		if e.Message != "" {
			message += "; " + e.Message
		}
	}
	code := "github_error"
	switch status {
	case http.StatusUnauthorized:
		code = "reauth_required"
	case http.StatusForbidden, http.StatusTooManyRequests:
		code = "no_permission"
		if ghErr.Response != nil && (ghErr.Response.Header.Get("X-RateLimit-Remaining") == "0" || ghErr.Response.Header.Get("Retry-After") != "" || status == http.StatusTooManyRequests) {
			code = "rate_limited"
		}
	case http.StatusNotFound:
		// GitHub hides repositories the token cannot see.
		code = "no_permission"
	case http.StatusUnprocessableEntity:
		lower := strings.ToLower(message)
		switch {
		case strings.Contains(lower, "own pull request"):
			code = "own_pr"
		case strings.Contains(lower, "closed"), strings.Contains(lower, "merged"):
			code = "pr_closed"
		default:
			code = "validation"
		}
	}
	return &UserReviewError{Status: status, Code: code, Message: message}
}

func statusOf(resp *http.Response) int {
	if resp == nil {
		return 0
	}
	return resp.StatusCode
}
