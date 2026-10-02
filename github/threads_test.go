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

func graphqlServer(t *testing.T, handle func(query string, vars map[string]any) string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/graphql" {
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
			return
		}
		body, _ := io.ReadAll(r.Body)
		var req struct {
			Query     string         `json:"query"`
			Variables map[string]any `json:"variables"`
		}
		if err := json.Unmarshal(body, &req); err != nil {
			t.Errorf("bad graphql body: %v", err)
		}
		fmt.Fprint(w, handle(req.Query, req.Variables))
	}))
}

func TestListReviewThreads_PagesAndReadsRootIDs(t *testing.T) {
	var cursors []any
	ts := graphqlServer(t, func(query string, vars map[string]any) string {
		if !strings.Contains(query, "reviewThreads(first:100,after:$after)") || vars["owner"] != "acme" || vars["repo"] != "example" || vars["number"] != float64(7) {
			t.Errorf("query=%q vars=%v", query, vars)
		}
		cursors = append(cursors, vars["after"])
		if vars["after"] == nil {
			return `{"data":{"repository":{"pullRequest":{"reviewThreads":{"nodes":[
				{"id":"T1","isResolved":false,"isOutdated":true,"comments":{"nodes":[{"fullDatabaseId":"31"}]}},
				{"id":"T2","isResolved":true,"isOutdated":false,"comments":{"nodes":[{"fullDatabaseId":32}]}}
			],"pageInfo":{"hasNextPage":true,"endCursor":"c1"}}}}}}`
		}
		return `{"data":{"repository":{"pullRequest":{"reviewThreads":{"nodes":[
			{"id":"T3","isResolved":false,"isOutdated":false,"comments":{"nodes":[]}}
		],"pageInfo":{"hasNextPage":false,"endCursor":""}}}}}}`
	})
	defer ts.Close()

	got, err := NewTestClient(ts.URL, "bot").ListReviewThreads(context.Background(), "acme", "example", 7)
	if err != nil {
		t.Fatalf("ListReviewThreads: %v", err)
	}
	want := []ReviewThreadInfo{
		{NodeID: "T1", RootCommentID: 31, Outdated: true},
		{NodeID: "T2", RootCommentID: 32, Resolved: true},
		{NodeID: "T3"},
	}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("threads = %+v, want %+v", got, want)
	}
	if len(cursors) != 2 || cursors[0] != nil || cursors[1] != "c1" {
		t.Fatalf("cursors = %v", cursors)
	}
}

func TestResolveAndUnresolveThread_SendTheMutationAndConfirmTheState(t *testing.T) {
	var queries []string
	ts := graphqlServer(t, func(query string, vars map[string]any) string {
		queries = append(queries, query)
		if vars["id"] != "T1" {
			t.Errorf("vars = %v", vars)
		}
		if strings.Contains(query, "unresolveReviewThread") {
			return `{"data":{"unresolveReviewThread":{"thread":{"id":"T1","isResolved":false}}}}`
		}
		return `{"data":{"resolveReviewThread":{"thread":{"id":"T1","isResolved":true}}}}`
	})
	defer ts.Close()

	c := NewTestClient(ts.URL, "bot")
	if err := c.ResolveThread(context.Background(), "acme", "example", "T1"); err != nil {
		t.Fatalf("ResolveThread: %v", err)
	}
	if err := c.UnresolveThread(context.Background(), "acme", "example", "T1"); err != nil {
		t.Fatalf("UnresolveThread: %v", err)
	}
	if len(queries) != 2 || !strings.Contains(queries[0], "resolveReviewThread(input:{threadId:$id})") || !strings.Contains(queries[1], "unresolveReviewThread(input:{threadId:$id})") {
		t.Fatalf("queries = %q", queries)
	}
}

func TestResolveThread_FailsOnErrorsAndUnconfirmedState(t *testing.T) {
	cases := map[string]string{
		"graphql error":  `{"data":null,"errors":[{"message":"Resource not accessible by integration"}]}`,
		"still open":     `{"data":{"resolveReviewThread":{"thread":{"id":"T1","isResolved":false}}}}`,
		"missing thread": `{"data":{"resolveReviewThread":{"thread":null}}}`,
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			ts := graphqlServer(t, func(string, map[string]any) string { return body })
			defer ts.Close()
			if err := NewTestClient(ts.URL, "bot").ResolveThread(context.Background(), "acme", "example", "T1"); err == nil {
				t.Fatal("expected an error")
			}
		})
	}
}
