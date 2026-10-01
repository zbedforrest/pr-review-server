package github

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func approvalFixtureClient(t *testing.T, failPage bool, partialGraphQL bool) *Client {
	t.Helper()
	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method != "GET" && r.URL.Path != "/graphql" {
			t.Errorf("unexpected write: %s %s", r.Method, r.URL.Path)
		}
		head, base := strings.Repeat("a", 40), strings.Repeat("b", 40)
		switch {
		case r.URL.Path == "/repos/acme/example/pulls/1":
			fmt.Fprintf(w, `{"state":"open","head":{"sha":%q},"base":{"sha":%q}}`, head, base)
		case strings.HasSuffix(r.URL.Path, "/reviews"):
			if r.URL.Query().Get("page") == "1" {
				w.Header().Set("Link", fmt.Sprintf(`<%s%s?per_page=100&page=2>; rel="next"`, server.URL, r.URL.Path))
				fmt.Fprint(w, `[{"id":1,"state":"COMMENTED"}]`)
			} else if failPage {
				w.WriteHeader(500)
				fmt.Fprint(w, `{"message":"failed"}`)
			} else {
				fmt.Fprint(w, `[{"id":2,"state":"APPROVED"}]`)
			}
		case strings.Contains(r.URL.Path, "/compare/"):
			fmt.Fprintf(w, `{"merge_base_commit":{"sha":%q}}`, base)
		case strings.HasSuffix(r.URL.Path, "/check-suites"):
			fmt.Fprint(w, `{"check_suites":[]}`)
		case strings.HasSuffix(r.URL.Path, "/check-runs"):
			fmt.Fprint(w, `{"check_runs":[]}`)
		case strings.HasSuffix(r.URL.Path, "/requested_reviewers"):
			fmt.Fprint(w, `{"users":[],"teams":[]}`)
		case r.URL.Path == "/graphql":
			var body struct {
				Variables map[string]any `json:"variables"`
			}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Error(err)
			}
			if partialGraphQL {
				fmt.Fprint(w, `{"data":{"repository":null},"errors":[{"message":"partial"}]}`)
			} else if body.Variables["id"] != nil {
				fmt.Fprint(w, `{"data":{"node":{"comments":{"nodes":[{"fullDatabaseId":12}],"pageInfo":{"hasNextPage":false}}}}}`)
			} else {
				fmt.Fprint(w, `{"data":{"repository":{"pullRequest":{"reviewThreads":{"nodes":[{"id":"thread-1","isResolved":true,"isOutdated":false,"comments":{"nodes":[{"fullDatabaseId":11}],"pageInfo":{"hasNextPage":true,"endCursor":"reply-page-2"}}}],"pageInfo":{"hasNextPage":false}}}}}}`)
			}
		default:
			fmt.Fprint(w, `[]`)
		}
	}))
	t.Cleanup(server.Close)
	client := NewClient("fixture-token", "fixture")
	client.gh.BaseURL, _ = url.Parse(server.URL + "/")
	return client
}

func TestApprovalEvidencePaginatesReviewsAndNestedReplies(t *testing.T) {
	client := approvalFixtureClient(t, false, false)
	result, err := client.CollectApprovalEvidence(context.Background(), "acme", "example", 1, ApprovalReadLimits{})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Reviews) != 2 || len(result.Threads) != 1 || len(result.Threads[0].Comments) != 2 {
		t.Fatalf("missing paginated evidence: %+v", result)
	}
	for _, ep := range result.Endpoints {
		if !ep.Complete {
			t.Fatalf("incomplete %s: %s", ep.Name, ep.Error)
		}
	}
}

func TestApprovalEvidenceFailuresAndCeilingsRemainIncomplete(t *testing.T) {
	for _, test := range []struct {
		name                        string
		pageFailure, graphqlFailure bool
		limits                      ApprovalReadLimits
	}{{name: "second review page", pageFailure: true}, {name: "graphql partial", graphqlFailure: true}, {name: "page ceiling", limits: ApprovalReadLimits{Pages: 1}}, {name: "byte ceiling", limits: ApprovalReadLimits{Bytes: 250}}} {
		t.Run(test.name, func(t *testing.T) {
			client := approvalFixtureClient(t, test.pageFailure, test.graphqlFailure)
			result, err := client.CollectApprovalEvidence(context.Background(), "acme", "example", 1, test.limits)
			if err != nil {
				return
			}
			for _, ep := range result.Endpoints {
				if !ep.Complete {
					return
				}
			}
			t.Fatal("partial evidence reported complete")
		})
	}
}

func TestApprovalEvidenceReportsRateLimitsAndReadsReviewersOnce(t *testing.T) {
	for _, limited := range []bool{false, true} {
		t.Run(fmt.Sprint("limited=", limited), func(t *testing.T) {
			var reviewerReads, afterLimit atomic.Int32
			var tripped atomic.Bool
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				if tripped.Load() {
					afterLimit.Add(1)
				}
				head, base := strings.Repeat("a", 40), strings.Repeat("b", 40)
				switch {
				case r.URL.Path == "/repos/acme/example/pulls/1":
					fmt.Fprintf(w, `{"state":"open","head":{"sha":%q},"base":{"sha":%q}}`, head, base)
				case strings.HasSuffix(r.URL.Path, "/check-runs") && limited:
					tripped.Store(true)
					w.Header().Set("X-RateLimit-Limit", "5000")
					w.Header().Set("X-RateLimit-Remaining", "0")
					w.Header().Set("X-RateLimit-Reset", fmt.Sprint(time.Now().Add(time.Minute).Unix()))
					w.WriteHeader(http.StatusForbidden)
					fmt.Fprint(w, `{"message":"API rate limit exceeded"}`)
				case strings.HasSuffix(r.URL.Path, "/check-runs"):
					fmt.Fprint(w, `{"check_runs":[]}`)
				case strings.HasSuffix(r.URL.Path, "/check-suites"):
					fmt.Fprint(w, `{"check_suites":[]}`)
				case strings.Contains(r.URL.Path, "/compare/"):
					fmt.Fprintf(w, `{"merge_base_commit":{"sha":%q}}`, base)
				case strings.HasSuffix(r.URL.Path, "/requested_reviewers"):
					reviewerReads.Add(1)
					fmt.Fprint(w, `{"users":[{"id":7,"login":"someone"}],"teams":[{"id":8,"slug":"group"}]}`)
				case r.URL.Path == "/graphql":
					fmt.Fprint(w, `{"data":{"repository":{"pullRequest":{"reviewThreads":{"nodes":[],"pageInfo":{"hasNextPage":false}}}}}}`)
				default:
					fmt.Fprint(w, `[]`)
				}
			}))
			defer server.Close()
			client := NewClient("fixture-token", "fixture")
			client.gh.BaseURL, _ = url.Parse(server.URL + "/")
			result, err := client.CollectApprovalEvidence(context.Background(), "acme", "example", 1, ApprovalReadLimits{})
			if limited {
				if _, ok := RateLimited(err, time.Now()); !ok || err.Error() != "evidence request failed" || afterLimit.Load() != 0 {
					t.Fatalf("err=%v requests after the limit=%d", err, afterLimit.Load())
				}
				return
			}
			if err != nil || reviewerReads.Load() != 1 || len(result.RequestedUsers) != 1 || len(result.RequestedTeams) != 1 {
				t.Fatalf("err=%v reads=%d result=%+v", err, reviewerReads.Load(), result)
			}
			for _, ep := range result.Endpoints {
				if !ep.Complete {
					t.Fatalf("incomplete %s: %s", ep.Name, ep.Error)
				}
			}
		})
	}
}

func TestApprovalEvidenceRejectsTargetPathInjection(t *testing.T) {
	client := approvalFixtureClient(t, false, false)
	for _, owner := range []string{"..", "acme/other", "acme?x=y"} {
		if _, err := client.CollectApprovalEvidence(context.Background(), owner, "example", 1, ApprovalReadLimits{}); err == nil {
			t.Fatal("invalid owner accepted")
		}
	}
}

func TestApprovalEvidenceFullDatabaseIDs(t *testing.T) {
	for _, value := range []string{`3000000001`, `"3000000001"`} {
		var connection approvalCommentIDs
		body := `{"nodes":[{"fullDatabaseId":` + value + `}],"pageInfo":{"hasNextPage":false}}`
		if err := json.Unmarshal([]byte(body), &connection); err != nil {
			t.Fatal(err)
		}
		if int64(connection.Nodes[0].ID) != 3000000001 {
			t.Fatal("64-bit comment identity lost")
		}
	}
}
