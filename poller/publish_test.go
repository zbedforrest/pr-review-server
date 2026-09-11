package poller

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"pr-review-server/config"
	"pr-review-server/db"
	"pr-review-server/github"
	"pr-review-server/pkg/publisher"
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

func TestPublishPolicy_ReadsSettingsOverDefaults(t *testing.T) {
	database, err := db.NewGormSQLite(":memory:")
	require.NoError(t, err)
	defer database.Close()
	p := &Poller{cfg: &config.Config{}, db: database}

	pol := p.publishPolicy()
	assert.Equal(t, publisher.DefaultPolicy(), pol, "no settings means the shipped policy")

	require.NoError(t, database.SetSetting("publish_inline_cap", "1"))
	require.NoError(t, database.SetSetting("publish_inline_min_severity", "Critical"))
	require.NoError(t, database.SetSetting("publish_show_unverified", "false"))
	pol = p.publishPolicy()
	assert.Equal(t, 1, pol.InlineCap)
	assert.Equal(t, "critical", pol.InlineMinSeverity)
	assert.False(t, pol.ShowUnverified)

	require.NoError(t, database.SetSetting("publish_show_unverified", "not-a-bool"))
	assert.True(t, p.publishPolicy().ShowUnverified, "an unreadable value keeps the default")
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

// A closed, merged or draft PR must never receive bot comments, and neither
// may a PR whose author is not enabled, whoever requested the review. The
// guard has to hold at the real publish entry point.
func TestPublishGitHubReview_DoesNotWriteToClosedDraftOrUnlistedAuthorPRs(t *testing.T) {
	for _, tc := range []struct {
		name, prJSON, author string
	}{
		{"merged", `{"state":"closed","merged":true,"draft":false,"head":{"sha":"abc"}}`, "alice"},
		{"draft", `{"state":"open","merged":false,"draft":true,"head":{"sha":"abc"}}`, "alice"},
		{"head moved", `{"state":"open","merged":false,"draft":false,"head":{"sha":"newer"}}`, "alice"},
		{"author not enabled", `{"state":"open","merged":false,"draft":false,"head":{"sha":"abc"}}`, "mallory"},
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

			report := p.publishGitHubReview(context.Background(), github.PullRequest{Owner: "acme", Repo: "example", Number: 1, CommitSHA: "abc", Author: tc.author}, sidecar)

			assert.Empty(t, writes, "no GitHub writes may happen for a %s PR", tc.name)
			assert.Nil(t, report, "nothing was published, so there is no published score")
		})
	}
}

// gitHubStub answers every GitHub call a publish makes: the PR itself as
// prJSON, empty lists elsewhere, and an id for each write. When failWrites
// is set, POSTs answer 500 so the publish fails after the round is scored.
func gitHubStub(t *testing.T, prJSON string, failWrites bool) (*httptest.Server, func() []string) {
	t.Helper()
	var mu sync.Mutex
	var writes []string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method != http.MethodGet {
			mu.Lock()
			writes = append(writes, r.Method+" "+r.URL.Path)
			mu.Unlock()
			if failWrites {
				w.WriteHeader(http.StatusInternalServerError)
				_, _ = w.Write([]byte(`{"message":"boom"}`))
				return
			}
			_, _ = w.Write([]byte(`{"id":11}`))
			return
		}
		if strings.HasPrefix(r.URL.Path, "/repos/") && strings.Contains(r.URL.Path, "/pulls/") && strings.Count(r.URL.Path, "/") == 5 {
			_, _ = w.Write([]byte(prJSON))
			return
		}
		_, _ = w.Write([]byte(`[]`))
	}))
	t.Cleanup(ts.Close)
	return ts, func() []string {
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), writes...)
	}
}

const openPRJSON = `{"state":"open","merged":false,"draft":false,"head":{"sha":"abc"}}`

// One critical finding and a violated required check: the raw sidecar scores
// 5 - 2 - 1 = 2; with the critical conceded in the ledger the publisher says 4.
const scoredSidecar = `{"schema_version":"1","owner":"acme","repo":"example","pr_number":1,"commit_sha":"abc",
	"required_checks":{"checks_issued":1,"checks_answered":1,"checks_violated":1},
	"findings":[{"id":"f.go:0:abc123def456","severity":"critical","provenance":"agent","state":"confirmed","active":true,"file":"f.go","line":3,"comment":"Real bug."}]}`

func dismissedRow(owner, repo string, number int, fingerprint string) *db.PublishedFinding {
	return &db.PublishedFinding{
		RepoOwner: owner, RepoName: repo, PRNumber: number,
		Kind: db.PublishedKindFinding, Fingerprint: fingerprint, Severity: "critical",
		ReviewedSHA: "old", LastSeenSHA: "old", CommentID: 77, State: db.PublishedStateDismissed,
	}
}

func TestPublishGitHubReview_ReportsTheConfidenceItRendered(t *testing.T) {
	ts, writes := gitHubStub(t, openPRJSON, false)
	database, err := db.NewGormSQLite(":memory:")
	require.NoError(t, err)
	defer database.Close()
	require.NoError(t, database.SetSetting("publish_enabled_authors", "alice"))
	require.NoError(t, database.UpsertPublishedFinding(dismissedRow("acme", "example", 1, "f.go:0:abc123def456")))
	p := &Poller{cfg: &config.Config{}, db: database, ghClientConcrete: github.NewTestClient(ts.URL, "bot")}

	report := p.publishGitHubReview(context.Background(), github.PullRequest{Owner: "acme", Repo: "example", Number: 1, CommitSHA: "abc", Author: "alice"}, []byte(scoredSidecar))

	require.NotNil(t, report, "an open PR by an enabled author is published")
	assert.Equal(t, 4, report.Confidence, "the conceded finding is not counted")
	assert.Equal(t, []string{"POST /repos/acme/example/issues/1/comments"}, writes())
}

func TestPublishGitHubReview_KeepsTheScoreWhenAWriteFails(t *testing.T) {
	ts, writes := gitHubStub(t, openPRJSON, true)
	database, err := db.NewGormSQLite(":memory:")
	require.NoError(t, err)
	defer database.Close()
	require.NoError(t, database.SetSetting("publish_enabled_authors", "alice"))
	require.NoError(t, database.UpsertPublishedFinding(dismissedRow("acme", "example", 1, "f.go:0:abc123def456")))
	p := &Poller{cfg: &config.Config{}, db: database, ghClientConcrete: github.NewTestClient(ts.URL, "bot")}

	report := p.publishGitHubReview(context.Background(), github.PullRequest{Owner: "acme", Repo: "example", Number: 1, CommitSHA: "abc", Author: "alice"}, []byte(scoredSidecar))

	require.NotEmpty(t, writes(), "the publish must have reached GitHub")
	require.NotNil(t, report, "the round was scored before the write, so the score survives the failure")
	assert.Equal(t, 4, report.Confidence)
}

func TestMergeConfidence_PublishedReportWinsOverSidecar(t *testing.T) {
	sidecar := []byte(scoredSidecar)
	p := &Poller{cfg: &config.Config{}, db: NewMockDatabase()}
	pr := github.PullRequest{Owner: "acme", Repo: "example", Number: 1, CommitSHA: "abc", Author: "alice"}

	fromSidecar, err := p.mergeConfidence(pr, nil, sidecar)
	require.NoError(t, err)
	assert.Equal(t, 2, fromSidecar)

	fromReport, err := p.mergeConfidence(pr, &publisher.Report{Confidence: 4}, sidecar)
	require.NoError(t, err)
	assert.Equal(t, 4, fromReport, "a conceded finding dropped at publish time must not be re-counted")

	_, err = p.mergeConfidence(pr, nil, []byte("not json"))
	assert.Error(t, err)
}

func TestMergeConfidence_UnpublishedFallbackHonoursLedgerDismissals(t *testing.T) {
	database, err := db.NewGormSQLite(":memory:")
	require.NoError(t, err)
	defer database.Close()
	require.NoError(t, database.UpsertPublishedFinding(dismissedRow("acme", "example", 1, "f.go:0:abc123def456")))
	p := &Poller{cfg: &config.Config{}, db: database}
	pr := github.PullRequest{Owner: "acme", Repo: "example", Number: 1, CommitSHA: "abc", Author: "alice"}

	score, err := p.mergeConfidence(pr, nil, []byte(scoredSidecar))
	require.NoError(t, err)
	assert.Equal(t, 4, score, "a draft or moved head skips the publish but the concession still stands")

	other := github.PullRequest{Owner: "acme", Repo: "example", Number: 2, CommitSHA: "abc", Author: "alice"}
	score, err = p.mergeConfidence(other, nil, []byte(scoredSidecar))
	require.NoError(t, err)
	assert.Equal(t, 2, score, "another PR's ledger does not apply")
}

// ledgerFailsAfterSummary lets the GitHub writes succeed and then refuses the
// first ledger row, the failure that leaves GitHub showing a score the poller
// must not contradict.
type ledgerFailsAfterSummary struct {
	*db.GormDB
	failures int
}

func (l *ledgerFailsAfterSummary) UpsertPublishedFinding(row *db.PublishedFinding) error {
	if row.Kind == db.PublishedKindSummary && l.failures == 0 {
		l.failures++
		return errors.New("ledger unavailable")
	}
	return l.GormDB.UpsertPublishedFinding(row)
}

func completionWithLedger(t *testing.T, database *db.GormDB, pollerDB db.Database, prJSON string) (*Poller, ReviewJob) {
	t.Helper()
	job := reviewJobWithoutAgent(t, "run-50000000000000000000000000000010")
	ts, _ := gitHubStub(t, strings.Replace(prJSON, `"abc"`, `"`+job.PR.CommitSHA+`"`, 1), false)
	require.NoError(t, database.SetSetting("publish_enabled_authors", job.PR.Author))
	fingerprint := payload.Fingerprint("src/app.go", 3, "Nil dereference on the error path.")
	require.NoError(t, database.UpsertPublishedFinding(dismissedRow(job.PR.Owner, job.PR.Repo, job.PR.Number, fingerprint)))
	require.NoError(t, database.UpsertPR(&db.PR{
		RepoOwner: job.PR.Owner, RepoName: job.PR.Repo, PRNumber: job.PR.Number,
		LastCommitSHA: job.PR.CommitSHA, Status: "generating", Author: job.PR.Author,
	}))
	p := newTestPollerFull(NewMockGitHubClient(), pollerDB, NewMockReviewStorage(), scoredReviewGenerator())
	p.ghClientConcrete = github.NewTestClient(ts.URL, "bot")
	return p, job
}

func TestCompletedReviewStoresThePublishedConfidence(t *testing.T) {
	for _, tc := range []struct {
		name   string
		prJSON string
		wrap   func(*db.GormDB) db.Database
	}{
		{"published", openPRJSON, func(d *db.GormDB) db.Database { return d }},
		{"ledger fails after the summary is written", openPRJSON, func(d *db.GormDB) db.Database { return &ledgerFailsAfterSummary{GormDB: d} }},
		{"draft skips the publish", `{"state":"open","merged":false,"draft":true,"head":{"sha":"abc"}}`, func(d *db.GormDB) db.Database { return d }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			database, err := db.NewGormSQLite(":memory:")
			require.NoError(t, err)
			defer database.Close()
			p, job := completionWithLedger(t, database, tc.wrap(database), tc.prJSON)

			require.NoError(t, p.ProcessReviewJob(context.Background(), job))
			waitForReviewJob(t, p, job)

			pr, err := database.GetPR(job.PR.Owner, job.PR.Repo, job.PR.Number)
			require.NoError(t, err)
			require.NotNil(t, pr)
			assert.Equal(t, "completed", pr.Status)
			require.NotNil(t, pr.MergeConfidence)
			assert.Equal(t, 4, *pr.MergeConfidence, "the conceded critical is not counted; the raw sidecar would say 2")
		})
	}
}

func TestBuildPublishRound_AliasesRewordedFindingsToPriorComments(t *testing.T) {
	pr := github.PullRequest{Owner: "acme", Repo: "example", Number: 7, CommitSHA: "abc", Author: "alice"}
	pl := payload.Payload{Findings: []payload.Finding{
		{ID: "a.go:5:bbbbbbbbbbbb", Severity: "critical", Provenance: "agent", File: "a.go", Line: 52,
			Comment: "Clicking Start in the C2C setup modal fires showMyCamDidNotStart/showMyCamBroadcastStopped immediately after starting, resetting the button to Ready."},
	}}
	comments := []github.ReviewCommentInfo{
		{ID: 501, Author: "prism-pr-review-server[bot]", Path: "a.go", Line: 54,
			Body: "<!-- prism:finding:a.go:5:aaaaaaaaaaaa -->\n**[CRITICAL] Behavior change · every successful Cam To Cam start also fires showMyCamDidNotStart and showMyCamBroadcastStopped, resetting the button to Ready**"},
	}
	previous := []db.PublishedFinding{{RepoOwner: "acme", RepoName: "example", PRNumber: 7, Kind: db.PublishedKindFinding, Fingerprint: "a.go:5:aaaaaaaaaaaa", CommentID: 501, State: db.PublishedStateOpen}}
	r := buildPublishRound(pr, pl, comments, nil, previous, "")
	if r.Findings[0].ID != "a.go:5:aaaaaaaaaaaa" {
		t.Fatalf("finding must take its published identity, got %q", r.Findings[0].ID)
	}
	if r.InlineComments["a.go:5:aaaaaaaaaaaa"] != 501 {
		t.Fatalf("aliased finding must link to its existing comment: %v", r.InlineComments)
	}
}

func TestBuildPublishRound_InactiveRecordsDoNotTakePartInReconciliation(t *testing.T) {
	pr := github.PullRequest{Owner: "acme", Repo: "example", Number: 7, CommitSHA: "abc", Author: "alice"}
	pl := payload.Payload{SchemaVersion: payload.CurrentSchemaVersion, Findings: []payload.Finding{
		{ID: "a.go:5:bbbbbbbbbbbb", Severity: "critical", Provenance: "agent", File: "a.go", Line: 52, State: "confirmed", Active: true,
			Comment: "Clicking Start in the C2C setup modal fires showMyCamDidNotStart immediately after starting, resetting the button to Ready."},
		{ID: "a.go:5:dddddddddddd", Severity: "critical", Provenance: "first-pass", File: "a.go", Line: 54, State: "merged", Active: false,
			Comment: "every successful Cam To Cam start also fires showMyCamDidNotStart and showMyCamBroadcastStopped, resetting the button to Ready"},
	}}
	comments := []github.ReviewCommentInfo{{ID: 501, Author: "prism-pr-review-server[bot]", Path: "a.go", Line: 54,
		Body: "<!-- prism:finding:a.go:5:aaaaaaaaaaaa -->\n**[CRITICAL] Behavior change · every successful Cam To Cam start also fires showMyCamDidNotStart and showMyCamBroadcastStopped, resetting the button to Ready**"}}
	previous := []db.PublishedFinding{{RepoOwner: "acme", RepoName: "example", PRNumber: 7, Kind: db.PublishedKindFinding, Fingerprint: "a.go:5:aaaaaaaaaaaa", CommentID: 501, State: db.PublishedStateOpen}}
	r := buildPublishRound(pr, pl, comments, nil, previous, "")
	var active []string
	for _, f := range r.Findings {
		active = append(active, f.ID)
	}
	if len(r.Findings) != 1 || r.Findings[0].ID != "a.go:5:aaaaaaaaaaaa" {
		t.Fatalf("the active agent finding must take the alias; the inactive merged record must not compete for it or reach the publisher: %v", active)
	}
}
