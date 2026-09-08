package tickets

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func newJiraServer(t *testing.T, handler http.HandlerFunc) (*httptest.Server, *JiraFetcher) {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	return srv, &JiraFetcher{BaseURL: srv.URL + "/", Email: "bot@acme.example", APIToken: "tok", HTTP: srv.Client()}
}

func TestJiraFetcherRequestsIssueWithBasicAuthAndRenderedFields(t *testing.T) {
	var gotPath, gotQuery string
	var gotUser, gotPass string
	var gotAuthOK bool
	srv, f := newJiraServer(t, func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotQuery = r.URL.RawQuery
		gotUser, gotPass, gotAuthOK = r.BasicAuth()
		fmt.Fprint(w, `{"key":"XO-370","fields":{"summary":"Retry policy","status":{"name":"In Review"},"issuetype":{"name":"Story"}},
		  "renderedFields":{"description":"<p>Keep <b>three</b> retries&nbsp;max.</p>",
		    "comment":{"comments":[{"author":{"displayName":"Alice"},"created":"2026-08-30T10:00:00.000+0000","body":"<p>Decided: no jitter.</p>"}]}}}`)
	})
	_ = srv

	ticket, err := f.Fetch(context.Background(), "XO-370")
	require.NoError(t, err)
	assert.Equal(t, "/rest/api/3/issue/XO-370", gotPath)
	assert.Contains(t, gotQuery, "fields=summary%2Cstatus%2Cissuetype%2Cdescription%2Ccomment")
	assert.Contains(t, gotQuery, "expand=renderedFields")
	assert.True(t, gotAuthOK)
	assert.Equal(t, "bot@acme.example", gotUser)
	assert.Equal(t, "tok", gotPass)

	assert.Equal(t, "XO-370", ticket.Key)
	assert.Equal(t, "Retry policy", ticket.Summary)
	assert.Equal(t, "In Review", ticket.Status)
	assert.Equal(t, "Story", ticket.Type)
	assert.Equal(t, srv.URL+"/browse/XO-370", ticket.URL)
	assert.Equal(t, "Keep three retries max.", ticket.Description)
	require.Len(t, ticket.Comments, 1)
	assert.Equal(t, Comment{Author: "Alice", Created: "2026-08-30T10:00:00.000+0000", Body: "Decided: no jitter."}, ticket.Comments[0])
}

func TestJiraFetcherFallsBackToADFWhenRenderedFieldsMissing(t *testing.T) {
	_, f := newJiraServer(t, func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"key":"XO-1","fields":{"summary":"S","status":{"name":"Done"},"issuetype":{"name":"Bug"},
		  "description":{"type":"doc","version":1,"content":[{"type":"paragraph","content":[{"type":"text","text":"adf body"}]}]},
		  "comment":{"comments":[{"author":{"displayName":"Bob"},"created":"2026-08-01T00:00:00.000+0000",
		    "body":{"type":"doc","version":1,"content":[{"type":"paragraph","content":[{"type":"text","text":"adf comment"}]}]}}]}}}`)
	})
	ticket, err := f.Fetch(context.Background(), "XO-1")
	require.NoError(t, err)
	assert.Equal(t, "adf body", ticket.Description)
	require.Len(t, ticket.Comments, 1)
	assert.Equal(t, "Bob", ticket.Comments[0].Author)
	assert.Equal(t, "adf comment", ticket.Comments[0].Body)
}

func TestJiraFetcherReturnsTypedNotFound(t *testing.T) {
	_, f := newJiraServer(t, func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"errorMessages":["Issue does not exist"]}`, http.StatusNotFound)
	})
	_, err := f.Fetch(context.Background(), "XO-404")
	assert.True(t, errors.Is(err, ErrNotFound), "expected ErrNotFound, got %v", err)
}

func TestJiraFetcherReportsOtherStatuses(t *testing.T) {
	_, f := newJiraServer(t, func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "nope", http.StatusForbidden)
	})
	_, err := f.Fetch(context.Background(), "XO-403")
	require.Error(t, err)
	assert.False(t, errors.Is(err, ErrNotFound))
	assert.Contains(t, err.Error(), "403")
}

func TestJiraFetcherKeepsNewestTenCommentsInChronologicalOrderAndCapsBodies(t *testing.T) {
	var comments []string
	for i := 1; i <= 12; i++ {
		comments = append(comments, fmt.Sprintf(`{"author":{"displayName":"U%d"},"created":"2026-08-%02dT00:00:00.000+0000","body":"<p>c%d %s</p>"}`,
			i, i, i, strings.Repeat("x", 700)))
	}
	_, f := newJiraServer(t, func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, `{"key":"XO-2","fields":{"summary":"S","status":{"name":"Open"},"issuetype":{"name":"Task"}},
		  "renderedFields":{"description":"<p>%s</p>","comment":{"comments":[%s]}}}`,
			strings.Repeat("d", 3500), strings.Join(comments, ","))
	})
	ticket, err := f.Fetch(context.Background(), "XO-2")
	require.NoError(t, err)
	require.Len(t, ticket.Comments, 10)
	assert.Equal(t, "U3", ticket.Comments[0].Author)
	assert.Equal(t, "U12", ticket.Comments[9].Author)
	for _, c := range ticket.Comments {
		assert.LessOrEqual(t, len([]rune(c.Body)), 600)
	}
	assert.Equal(t, 3000, len([]rune(ticket.Description)))
}

func TestJiraFetcherHonorsContextCancellation(t *testing.T) {
	_, f := newJiraServer(t, func(w http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
	})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := f.Fetch(ctx, "XO-3")
	require.Error(t, err)
}

type fakeFetcher struct {
	tickets map[string]Ticket
	errs    map[string]error
	calls   []string
}

func (f *fakeFetcher) Fetch(ctx context.Context, key string) (Ticket, error) {
	f.calls = append(f.calls, key)
	if err, ok := f.errs[key]; ok {
		return Ticket{}, err
	}
	return f.tickets[key], nil
}

func TestFetchAllSkipsFailuresAndLogsThem(t *testing.T) {
	f := &fakeFetcher{
		tickets: map[string]Ticket{"XO-1": {Key: "XO-1", Summary: "one"}, "XO-3": {Key: "XO-3", Summary: "three"}},
		errs:    map[string]error{"XO-2": ErrNotFound},
	}
	var logged []string
	logf := func(format string, args ...any) { logged = append(logged, fmt.Sprintf(format, args...)) }

	got := FetchAll(context.Background(), f, []string{"XO-1", "XO-2", "XO-3"}, logf)
	require.Len(t, got, 2)
	assert.Equal(t, "XO-1", got[0].Key)
	assert.Equal(t, "XO-3", got[1].Key)
	assert.Equal(t, []string{"XO-1", "XO-2", "XO-3"}, f.calls)
	require.Len(t, logged, 1)
	assert.Contains(t, logged[0], "XO-2")
}

func TestFetchAllWithNilFetcherOrNoKeysReturnsNothing(t *testing.T) {
	assert.Empty(t, FetchAll(context.Background(), nil, []string{"XO-1"}, nil))
	assert.Empty(t, FetchAll(context.Background(), &fakeFetcher{}, nil, nil))
}
