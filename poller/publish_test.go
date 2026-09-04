package poller

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"pr-review-server/config"
	"pr-review-server/db"
	"pr-review-server/github"
	"pr-review-server/pkg/reviewer/payload"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestPublishEnabledFor(t *testing.T) {
	cases := []struct {
		author, enabled string
		want            bool
	}{
		{"alice", "", false},
		{"", "*", false},
		{"alice", "*", true},
		{"alice", "bob, Alice ,carol", true},
		{"dave", "bob,alice", false},
	}
	for _, c := range cases {
		assert.Equal(t, c.want, publishEnabledFor(c.author, c.enabled), "author=%q enabled=%q", c.author, c.enabled)
	}
}

const greptileBody = `<a href="#"><img alt="P1" src="https://greptile-static-assets.s3.amazonaws.com/badges/p1.svg?v=9" align="top"></a> **Nil map write on first session**

The sessions map is assigned before init so the first write panics with a nil map.

<a href="https://app.greptile.com/x"><picture><img alt="Fix"></picture></a>`

func TestBuildPublishRound_TagsReconciledFindingsAndBuildsLinks(t *testing.T) {
	pr := github.PullRequest{Owner: "acme", Repo: "example", Number: 7, CommitSHA: "abcdef1234567890", Author: "alice"}
	pl := payload.Payload{Findings: []payload.Finding{
		{ID: "sum", Severity: "unknown", File: "SUMMARY", Comment: "Verdict: approve with suggestions."},
		{ID: "f1", Severity: "critical", File: "pkg/store/sessions.go", Line: 42, Comment: "Nil map write: sessions[id] is assigned before the map is initialised, so the first write panics."},
		{ID: "f2", Severity: "medium", File: "pkg/api/handler.go", Line: 8, Comment: "Missing error check on Decode."},
	}}
	comments := []github.ReviewCommentInfo{
		{ID: 900, Author: "greptile-apps[bot]", Body: greptileBody, Path: "pkg/store/sessions.go", Line: 44},
		{ID: 901, Author: "greptile-apps[bot]", Body: greptileBody, Path: "pkg/other/unrelated.go", Line: 3},
		{ID: 902, Author: "human", Body: "looks fine", Path: "pkg/api/handler.go", Line: 8},
	}
	patches := map[string]string{
		"pkg/store/sessions.go": "@@ -40,3 +40,4 @@\n a\n+b\n+c\n d\n",
	}
	previous := []db.PublishedFinding{{Kind: db.PublishedKindSummary, Fingerprint: "summary", Rounds: 3}}

	r := buildPublishRound(pr, pl, comments, patches, previous, "https://prism.example")

	assert.Equal(t, "acme", r.Owner)
	assert.Equal(t, 7, r.Number)
	assert.Equal(t, "abcdef1234567890", r.HeadSHA)
	assert.Equal(t, "both", r.SourceTags["f1"], "PRism finding near Greptile's comment on the same file")
	assert.Empty(t, r.SourceTags["f2"], "unmatched PRism finding stays prism-only")
	require.Len(t, r.GreptileOnly, 1)
	assert.Equal(t, int64(901), r.GreptileOnly[0].CommentID)
	assert.Equal(t, "pkg/other/unrelated.go", r.GreptileOnly[0].File)
	assert.True(t, r.Commentable["pkg/store/sessions.go"][41], "added line inside the hunk is commentable")
	assert.False(t, r.Commentable["pkg/store/sessions.go"][99])
	assert.Equal(t, previous, r.Previous)
	assert.Equal(t, "https://prism.example/go/agent?o=acme&r=example&n=7", r.AgentLinkBase)
	assert.Equal(t, "https://prism.example/api/review/acme/example/7?format=html", r.DashboardURL)
}

func TestBuildPublishRound_NoBaseURLDisablesLinks(t *testing.T) {
	r := buildPublishRound(github.PullRequest{Owner: "a", Repo: "b", Number: 1}, payload.Payload{}, nil, nil, nil, "")
	assert.Empty(t, r.AgentLinkBase)
	assert.Empty(t, r.DashboardURL)
}

func TestBuildPublishRound_CarriesRequiredCheckViolation(t *testing.T) {
	pr := github.PullRequest{Owner: "a", Repo: "b", Number: 1}
	quiet := buildPublishRound(pr, payload.Payload{RequiredChecks: &payload.RequiredChecksInfo{Issued: 2, Violated: 0}}, nil, nil, nil, "")
	assert.False(t, quiet.RequiredCheckViolated)
	loud := buildPublishRound(pr, payload.Payload{RequiredChecks: &payload.RequiredChecksInfo{Issued: 2, Violated: 1}}, nil, nil, nil, "")
	assert.True(t, loud.RequiredCheckViolated)
}

func TestPublishTargetReady(t *testing.T) {
	cases := []struct {
		state          string
		draft          bool
		head, reviewed string
		want           bool
	}{
		{"open", false, "abc", "abc", true},
		{"open", false, "ABC", "abc", true},
		{"open", true, "abc", "abc", false},
		{"closed", false, "abc", "abc", false},
		{"", false, "abc", "abc", false},
		{"open", false, "def", "abc", false},
		{"open", false, "", "abc", false},
	}
	for _, c := range cases {
		ok, _ := publishTargetReady(c.state, c.draft, c.head, c.reviewed)
		assert.Equal(t, c.want, ok, "state=%q draft=%v head=%q reviewed=%q", c.state, c.draft, c.head, c.reviewed)
	}
}

// A closed, merged or draft PR must never receive bot comments, no matter what
// the review found: the guard has to hold at the real publish entry point.
func TestPublishGitHubReview_DoesNotWriteToClosedOrDraftPRs(t *testing.T) {
	for _, tc := range []struct {
		name, prJSON string
	}{
		{"merged", `{"state":"closed","merged":true,"draft":false,"head":{"sha":"abc"}}`},
		{"draft", `{"state":"open","merged":false,"draft":true,"head":{"sha":"abc"}}`},
		{"head moved", `{"state":"open","merged":false,"draft":false,"head":{"sha":"newer"}}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var writes []string
			ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodGet {
					writes = append(writes, r.Method+" "+r.URL.Path)
				}
				w.Header().Set("Content-Type", "application/json")
				switch {
				case r.Method == http.MethodGet && r.URL.Path == "/repos/acme/example/pulls/1":
					_, _ = w.Write([]byte(tc.prJSON))
				default:
					_, _ = w.Write([]byte(`[]`))
				}
			}))
			defer ts.Close()

			database, err := db.NewGormSQLite(":memory:")
			require.NoError(t, err)
			defer database.Close()
			require.NoError(t, database.SetSetting("publish_enabled_authors", "alice"))

			p := &Poller{cfg: &config.Config{}, db: database, ghClientConcrete: github.NewTestClient(ts.URL, "bot")}
			sidecar := []byte(`{"schema_version":"1","owner":"acme","repo":"example","pr_number":1,"commit_sha":"abc",
				"findings":[{"id":"f.go:0:abc123def456","severity":"critical","provenance":"agent","file":"f.go","line":3,"comment":"Real bug."}]}`)

			p.publishGitHubReview(context.Background(), github.PullRequest{Owner: "acme", Repo: "example", Number: 1, CommitSHA: "abc", Author: "alice"}, sidecar)

			assert.Empty(t, writes, "no GitHub writes may happen for a %s PR", tc.name)
		})
	}
}
