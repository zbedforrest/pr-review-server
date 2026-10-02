package feedback

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"pr-review-server/db"
)

func TestLexiconLabelsAnonymisedReplies(t *testing.T) {
	cases := []struct {
		body string
		want Label
	}{
		{"Good catch, fixed in 3f2a1c9. Thanks!", Happy},
		{"👍", Happy},
		{"Appreciate the pointer, I had missed that the handler re-reads the body.", Happy},
		{"Fixed in the latest push.", Neutral},
		{"The retry lives in the caller, see fetchWithBackoff; this path cannot loop.", Neutral},
		{"This is wrong, the lock is already held by the caller.", Frustrated},
		{"False positive: the migration is idempotent and this is covered by the test above.", Frustrated},
		{"No, this is not an issue, the value is validated upstream.", Frustrated},
		{"This is the third time it flagged the same line after I explained it. Please stop.", VeryFrustrated},
		{"Useless noise, can someone turn this bot off on my PRs?", VeryFrustrated},
		{"WRONG again?! The nil check is RIGHT THERE!!", VeryFrustrated},
		{"Thanks, but the wording is a bit misleading here.", Neutral},
		{"This is wrong, the JSON is already validated by the HTTP layer.", Frustrated},
		{"This is the second time this file changed in the release.", Neutral},
		{"Third time it flagged the same line after I explained it.", VeryFrustrated},
	}
	for _, c := range cases {
		got := Lexicon(Item{Source: SourceReply, Body: c.body})
		assert.Equal(t, c.want, got, "%q", c.body)
	}
}

func TestLexiconLabelsReactionsByContent(t *testing.T) {
	for content, want := range map[string]Label{"+1": Happy, "heart": Happy, "hooray": Happy, "-1": Frustrated, "confused": Frustrated, "laugh": Neutral, "eyes": Neutral, "rocket": Neutral} {
		assert.Equal(t, want, Lexicon(Item{Source: SourceReaction, Reaction: content}), content)
	}
}

func TestParseLabelTakesTheFirstLabelAndKeepsVeryApart(t *testing.T) {
	for answer, want := range map[string]Label{
		"very_frustrated":                      VeryFrustrated,
		"Frustrated.":                          Frustrated,
		"**Happy**: the author thanks the bot": Happy,
		"Very frustrated":                      VeryFrustrated,
		"very-frustrated, asks to stop":        VeryFrustrated,
		"```\nneutral\n```":                    Neutral,
	} {
		got, ok := ParseLabel(answer)
		require.True(t, ok, answer)
		assert.Equal(t, want, got, answer)
	}
	for _, answer := range []string{"I cannot tell.", "not happy", "this is not frustrated", "happiness"} {
		_, ok := ParseLabel(answer)
		assert.False(t, ok, answer)
	}
}

func TestModelClassifierFallsBackToTheLexicon(t *testing.T) {
	item := Item{Source: SourceReply, Body: "This is wrong, the lock is already held by the caller.", ReplyClass: "pushback"}
	calls := 0
	failing := ModelClassifier{Name: "flash", Ask: func(_ context.Context, prompt string) (string, error) {
		calls++
		assert.Contains(t, prompt, "Reply classifier hint: pushback")
		assert.Contains(t, prompt, item.Body)
		return "", errors.New("quota")
	}}
	label, by := failing.Classify(context.Background(), item)
	assert.Equal(t, Frustrated, label)
	assert.Equal(t, "lexicon", by)

	garbled := ModelClassifier{Name: "flash", Ask: func(context.Context, string) (string, error) { return "cannot say", nil }}
	label, by = garbled.Classify(context.Background(), item)
	assert.Equal(t, Frustrated, label)
	assert.Equal(t, "lexicon", by)

	model := ModelClassifier{Name: "flash", Ask: func(context.Context, string) (string, error) { return "neutral", nil }}
	label, by = model.Classify(context.Background(), item)
	assert.Equal(t, Neutral, label)
	assert.Equal(t, "flash", by)

	label, by = model.Classify(context.Background(), Item{Source: SourceReaction, Reaction: "-1"})
	assert.Equal(t, Frustrated, label)
	assert.Equal(t, "lexicon", by, "reactions never spend a model call")
	assert.Equal(t, 1, calls)
}

func TestQuoteNeutralisesLinksAndHTML(t *testing.T) {
	assert.Equal(t, "see [this] (https://example.invalid) now", Quote("see [this](https://example.invalid) <img src=x onerror=alert(1)> now"))
}

func TestQuoteFlattensAndTruncatesAtAWordBoundary(t *testing.T) {
	body := "> PRism said something\n```go\nx := 1\n```\nThe   retry lives in the caller,\nsee fetchWithBackoff.\n"
	assert.Equal(t, "The retry lives in the caller, see fetchWithBackoff.", Quote(body))
	long := strings.Repeat("word ", 60)
	q := Quote(long)
	assert.True(t, strings.HasSuffix(q, "..."), q)
	assert.LessOrEqual(t, len([]rune(q)), quoteRunes+3)
	assert.False(t, strings.HasSuffix(strings.TrimSuffix(q, "..."), " "))
}

type fakeGitHub struct {
	review       map[int][]Comment
	issue        map[int][]Comment
	reactions    map[int64][]Reaction
	calls        int
	failReaction int64
}

func (f *fakeGitHub) ListReviewComments(_ context.Context, _, _ string, n int) ([]Comment, error) {
	f.calls++
	return f.review[n], nil
}
func (f *fakeGitHub) ListIssueComments(_ context.Context, _, _ string, n int) ([]Comment, error) {
	f.calls++
	return f.issue[n], nil
}
func (f *fakeGitHub) ListReviewCommentReactions(_ context.Context, _, _ string, id int64) ([]Reaction, error) {
	f.calls++
	if id == f.failReaction {
		return nil, errors.New("403 from GitHub")
	}
	return f.reactions[id], nil
}
func (f *fakeGitHub) ListIssueCommentReactions(_ context.Context, _, _ string, id int64) ([]Reaction, error) {
	f.calls++
	return f.reactions[id], nil
}

type memStore struct {
	targets []db.FeedbackTarget
	rows    map[db.FeedbackKey]db.FeedbackItem
}

func (m *memStore) ListFeedbackTargets(time.Time) ([]db.FeedbackTarget, error) { return m.targets, nil }
func (m *memStore) KnownFeedbackItems(_, _ string, n int) (map[db.FeedbackKey]bool, error) {
	out := map[db.FeedbackKey]bool{}
	for k, r := range m.rows {
		if r.PRNumber == n {
			out[k] = true
		}
	}
	return out, nil
}
func (m *memStore) SaveFeedbackItems(items []db.FeedbackItem) (int, error) {
	n := 0
	for _, it := range items {
		k := db.FeedbackKey{Source: it.Source, ItemID: it.ItemID}
		if _, dup := m.rows[k]; dup {
			continue
		}
		m.rows[k] = it
		n++
	}
	return n, nil
}

type countingClassifier struct{ calls int }

func (c *countingClassifier) Classify(_ context.Context, it Item) (Label, string) {
	c.calls++
	return Lexicon(it), "counted"
}

func fixtureScanner(now time.Time) (Scanner, *fakeGitHub, *memStore, *countingClassifier) {
	recent := now.Add(-3 * time.Hour)
	old := now.Add(-10 * 24 * time.Hour)
	gh := &fakeGitHub{
		review: map[int][]Comment{42: {
			{ID: 100, Author: "prism-bot[bot]", IsBot: true, Body: "finding", CreatedAt: old, Reactions: 2},
			{ID: 101, InReplyToID: 100, Author: "dana-dev", Body: "This is the third time it flagged the same line after I explained it. Please stop.", CreatedAt: recent},
			{ID: 102, InReplyToID: 100, Author: "prism-bot[bot]", IsBot: true, Body: "Understood, withdrawn.", CreatedAt: recent, Reactions: 1},
			{ID: 103, InReplyToID: 100, Author: "sam-q", Body: "Good catch, fixed in 3f2a1c9.", CreatedAt: old},
			{ID: 104, InReplyToID: 999, Author: "sam-q", Body: "Not a PRism thread, this is wrong.", CreatedAt: recent},
		}},
		issue: map[int][]Comment{42: {
			{ID: 200, Author: "prism-bot[bot]", IsBot: true, Body: "summary", CreatedAt: old, Reactions: 1},
			{ID: 201, Author: "sam-q", Body: "@prism-bot thanks, the summary was helpful today", CreatedAt: recent},
			{ID: 202, Author: "sam-q", Body: "Rebased on main.", CreatedAt: recent},
		}},
		reactions: map[int64][]Reaction{
			100: {{ID: 300, User: "dana-dev", Content: "-1", CreatedAt: recent}, {ID: 301, User: "prism-bot[bot]", IsBot: true, Content: "+1", CreatedAt: recent}},
			102: {{ID: 303, User: "dana-dev", Content: "heart", CreatedAt: recent}},
			200: {{ID: 302, User: "lee-ops", Content: "heart", CreatedAt: recent}},
		},
	}
	store := &memStore{rows: map[db.FeedbackKey]db.FeedbackItem{}, targets: []db.FeedbackTarget{{
		RepoOwner: "acme", RepoName: "example", PRNumber: 42,
		Roots: map[int64]bool{100: true}, Summaries: map[int64]bool{200: true}, ReplyClasses: map[int64]string{101: "pushback"},
	}}}
	classifier := &countingClassifier{}
	s := Scanner{Store: store, GitHub: gh, Classifier: classifier, Handle: "prism-bot", Now: func() time.Time { return now }}
	return s, gh, store, classifier
}

func TestScannerCollectsRepliesMentionsAndReactionsFromHumansOnly(t *testing.T) {
	now := time.Date(2026, 10, 2, 14, 0, 0, 0, time.UTC)
	s, _, store, classifier := fixtureScanner(now)
	res, err := s.Run(context.Background())
	require.NoError(t, err)
	assert.Equal(t, 1, res.Targets)
	assert.Equal(t, 0, res.Errors)

	got := map[db.FeedbackKey]db.FeedbackItem{}
	for _, it := range res.Added {
		got[db.FeedbackKey{Source: it.Source, ItemID: it.ItemID}] = store.rows[db.FeedbackKey{Source: it.Source, ItemID: it.ItemID}]
	}
	require.Len(t, got, 5, "%+v", res.Added)
	assert.Equal(t, string(Happy), got[db.FeedbackKey{Source: SourceReaction, ItemID: 303}].Label, "reactions on PRism's own in-thread reply count")
	reply := got[db.FeedbackKey{Source: SourceReply, ItemID: 101}]
	assert.Equal(t, "dana-dev", reply.Author)
	assert.Equal(t, string(VeryFrustrated), reply.Label)
	assert.Equal(t, "pushback", reply.ReplyClass)
	assert.Equal(t, int64(100), reply.CommentID)
	assert.Equal(t, "https://github.com/acme/example/pull/42#discussion_r101", reply.URL)
	assert.Equal(t, string(Happy), got[db.FeedbackKey{Source: SourceComment, ItemID: 201}].Label)
	assert.Equal(t, "https://github.com/acme/example/pull/42#issuecomment-201", got[db.FeedbackKey{Source: SourceComment, ItemID: 201}].URL)
	assert.Equal(t, string(Frustrated), got[db.FeedbackKey{Source: SourceReaction, ItemID: 300}].Label)
	assert.Equal(t, "-1", got[db.FeedbackKey{Source: SourceReaction, ItemID: 300}].Reaction)
	assert.Equal(t, string(Happy), got[db.FeedbackKey{Source: SourceReaction, ItemID: 302}].Label)
	assert.Equal(t, 2, classifier.calls, "only text items reach the classifier")
}

func TestScannerIsIdempotentAcrossRuns(t *testing.T) {
	now := time.Date(2026, 10, 2, 14, 0, 0, 0, time.UTC)
	s, gh, store, classifier := fixtureScanner(now)
	first, err := s.Run(context.Background())
	require.NoError(t, err)
	require.Len(t, first.Added, 5)
	rows, calls := len(store.rows), classifier.calls

	s.Now = func() time.Time { return now.Add(24 * time.Hour) }
	second, err := s.Run(context.Background())
	require.NoError(t, err)
	assert.Empty(t, second.Added)
	assert.Equal(t, 5, second.Seen, "the same items are listed again")
	assert.Equal(t, rows, len(store.rows))
	assert.Equal(t, calls, classifier.calls, "stored items are not re-classified")
	assert.Greater(t, gh.calls, 0)
}

func TestScannerModelCapFallsBackToLexicon(t *testing.T) {
	now := time.Date(2026, 10, 2, 14, 0, 0, 0, time.UTC)
	s, _, store, classifier := fixtureScanner(now)
	s.MaxModelCalls = 1
	_, err := s.Run(context.Background())
	require.NoError(t, err)
	assert.Equal(t, 1, classifier.calls)
	byName := map[string]int{}
	for _, r := range store.rows {
		byName[r.Classifier]++
	}
	assert.Equal(t, map[string]int{"counted": 1, "lexicon": 4}, byName)
}

func TestScannerKeepsThePRWhenOneReactionPageFails(t *testing.T) {
	now := time.Date(2026, 10, 2, 14, 0, 0, 0, time.UTC)
	s, gh, _, _ := fixtureScanner(now)
	gh.failReaction = 100
	res, err := s.Run(context.Background())
	require.NoError(t, err)
	assert.Equal(t, 0, res.Errors)
	assert.Equal(t, 1, res.Skipped)
	assert.Len(t, res.Added, 4, "only the failed page's reaction is missing")
}

func TestScannerStopsSpendingModelCallsNearTheDeadline(t *testing.T) {
	now := time.Date(2026, 10, 2, 14, 0, 0, 0, time.UTC)
	s, _, _, classifier := fixtureScanner(now)
	ctx, cancel := context.WithTimeout(context.Background(), modelReserve/2)
	defer cancel()
	_, err := s.Run(ctx)
	require.NoError(t, err)
	assert.Equal(t, 0, classifier.calls)
}

func TestDigestMarkdownAndHealthMetrics(t *testing.T) {
	now := time.Date(2026, 10, 2, 14, 0, 0, 0, time.UTC)
	items := []db.FeedbackItem{
		{Source: SourceReply, Author: "dana-dev", Body: "This is the third time it flagged the same line after I explained it. Please stop.", Label: string(VeryFrustrated), RepoOwner: "acme", RepoName: "example", PRNumber: 42, URL: "https://github.com/acme/example/pull/42#discussion_r101", CreatedAt: now.Add(-time.Hour)},
		{Source: SourceReaction, Author: "lee-ops", Reaction: "-1", Body: "-1", Label: string(Frustrated), RepoOwner: "acme", RepoName: "example", PRNumber: 43, URL: "https://github.com/acme/example/pull/43#discussion_r500", CreatedAt: now.Add(-2 * time.Hour)},
		{Source: SourceComment, Author: "sam-q", Body: "@prism-bot thanks, the summary was helpful today", Label: string(Happy), RepoOwner: "acme", RepoName: "example", PRNumber: 42, URL: "https://github.com/acme/example/pull/42#issuecomment-201", CreatedAt: now.Add(-3 * time.Hour)},
		{Source: SourceReply, Author: "sam-q", Body: "Fixed in the latest push.", Label: string(Neutral), RepoOwner: "acme", RepoName: "example", PRNumber: 42, URL: "u", CreatedAt: now.Add(-4 * time.Hour)},
	}
	d := NewDigest(items, now.Add(-24*time.Hour), now, 1)
	assert.Equal(t, map[string]int{"happy": 1, "neutral": 1, "frustrated": 1, "very_frustrated": 1}, d.Counts)
	md := d.Markdown()
	for _, want := range []string{"# PRism author feedback: last 1 day", "1 happy, 1 neutral, 1 frustrated, 1 very frustrated", "## very frustrated (1)", "@dana-dev on acme/example#42 (reply)", "reacted -1", "## happy (1)"} {
		assert.Contains(t, md, want)
	}
	m := d.HealthMetrics(true, "")
	require.Len(t, m.Frustrated, 2)
	assert.Equal(t, "dana-dev", m.Frustrated[0].Author)
	assert.Equal(t, "reacted -1", m.Frustrated[1].Quote)
	require.NotNil(t, m.Happy)
	assert.Equal(t, "sam-q", m.Happy.Author)

	empty := NewDigest(nil, now.Add(-7*24*time.Hour), now, 7)
	assert.Contains(t, empty.Markdown(), "last 7 days")
	assert.Contains(t, empty.Markdown(), "No author feedback in the window.")
	assert.NotNil(t, empty.Items)
}

func TestNamesBotNeedsTheWordOrTheHandle(t *testing.T) {
	s := Scanner{Handle: "prism-bot"}
	assert.True(t, s.namesBot("@prism-bot please re-review"))
	assert.True(t, s.namesBot("PRism flagged this already"))
	assert.True(t, s.namesBot("thanks prism."))
	assert.False(t, s.namesBot("see my-prism-fork"))
	assert.False(t, s.namesBot("switched the highlighter to prism.js"))
	assert.False(t, s.namesBot("a prismatic palette"))
}

func TestTruncateBytesKeepsUTF8Whole(t *testing.T) {
	s := strings.Repeat("é", 10)
	got := truncateBytes(s, 7)
	assert.True(t, utf8.ValidString(got))
	assert.Equal(t, 6, len(got))
	assert.Equal(t, "abc", truncateBytes("abc", 3))
}
