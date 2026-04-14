package github

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestBatchGetReviewerGroups(t *testing.T) {
	// Mock response data matches the timeline-based structure expected by client.go
	mockResponse := `{
		"data": {
			"pr0": {
				"pullRequest": {
					"number": 101,
					"timelineItems": {
						"nodes": [
							{
								"requestedReviewer": {
									"__typename": "Team",
									"name": "backend-team",
									"slug": "backend-team"
								}
							},
							{
								"requestedReviewer": {
									"__typename": "User",
									"login": "other-user"
								}
							}
						]
					}
				}
			},
			"pr1": {
				"pullRequest": {
					"number": 102,
					"timelineItems": {
						"nodes": [
							{
								"requestedReviewer": {
									"__typename": "User",
									"login": "test-user"
								}
							}
						]
					}
				}
			},
			"pr2": {
				"pullRequest": {
					"number": 103,
					"timelineItems": {
						"nodes": []
					}
				}
			}
		}
	}`

	// Start mock server
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" {
			t.Errorf("Expected POST request, got %s", r.Method)
		}
		w.WriteHeader(http.StatusOK)
		if _, err := w.Write([]byte(mockResponse)); err != nil {
			t.Errorf("failed to write response: %v", err)
		}
	}))
	defer ts.Close()

	// Since we can't easily change the URL in the client code (it's hardcoded),
	// we'll actually use a custom Transport in the client's http.Client to redirect the request to our mock server.

	client := NewClient("dummy-token", "test-user")
	client.httpClient = &http.Client{
		Transport: &redirectTransport{
			targetURL: ts.URL,
		},
	}

	prs := []PullRequest{
		{Owner: "owner", Repo: "repo", Number: 101},
		{Owner: "owner", Repo: "repo", Number: 102},
		{Owner: "owner", Repo: "repo", Number: 103},
	}

	results, err := client.BatchGetReviewerGroups(context.Background(), prs)
	if err != nil {
		t.Fatalf("BatchGetReviewerGroups failed: %v", err)
	}

	// Verify PR 101: Has team "backend-team" and user "other-user" in RequestedUsers
	data101, ok := results["owner/repo/101"]
	if !ok {
		t.Fatal("Result for PR 101 not found")
	}
	if len(data101.ReviewerGroups) != 1 || data101.ReviewerGroups[0] != "backend-team" {
		t.Errorf("PR 101: Expected ReviewerGroups ['backend-team'], got %v", data101.ReviewerGroups)
	}
	if len(data101.RequestedUsers) != 1 || data101.RequestedUsers[0] != "other-user" {
		t.Errorf("PR 101: Expected RequestedUsers ['other-user'], got %v", data101.RequestedUsers)
	}

	// Verify PR 102: Personal request for "test-user" -> no teams, but in RequestedUsers
	data102, ok := results["owner/repo/102"]
	if !ok {
		t.Fatal("Result for PR 102 not found")
	}
	if len(data102.ReviewerGroups) != 0 {
		t.Errorf("PR 102: Expected empty ReviewerGroups, got %v", data102.ReviewerGroups)
	}
	if len(data102.RequestedUsers) != 1 || data102.RequestedUsers[0] != "test-user" {
		t.Errorf("PR 102: Expected RequestedUsers ['test-user'], got %v", data102.RequestedUsers)
	}

	// Verify PR 103: No requests -> empty
	data103, ok := results["owner/repo/103"]
	if !ok {
		t.Fatal("Result for PR 103 not found")
	}
	if len(data103.ReviewerGroups) != 0 {
		t.Errorf("PR 103: Expected empty ReviewerGroups, got %v", data103.ReviewerGroups)
	}
	if len(data103.RequestedUsers) != 0 {
		t.Errorf("PR 103: Expected empty RequestedUsers, got %v", data103.RequestedUsers)
	}
}

// redirectTransport redirects all requests to the target URL
type redirectTransport struct {
	targetURL string
}

func (t *redirectTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	// Create a new request to the target URL
	newReq, err := http.NewRequest(req.Method, t.targetURL, req.Body)
	if err != nil {
		return nil, err
	}
	newReq.Header = req.Header
	return http.DefaultTransport.RoundTrip(newReq)
}
