package grader

import (
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func items(ids ...string) []Item {
	var out []Item
	for _, id := range ids {
		out = append(out, Item{ID: id, Kind: KindInline, Comment: "c"})
	}
	return out
}

func TestParseReplyKeepsOnlyValidGradesForAskedIDs(t *testing.T) {
	reply := "Here you go:\n" + `[
 {"id":"c1","grade":"bad","category":"wrong-claim","reason":"x.go:3"},
 {"id":"c1","grade":"GOOD","category":"-","reason":"dup"},
 {"id":"c2","grade":"MAYBE","category":"-","reason":"r"},
 {"id":"c9","grade":"OK","category":"-","reason":"not asked"},
 {"id":"c3","grade":"OK","category":"perf-overstated","reason":"y.go:1"}
]`
	got, err := ParseReply(reply, items("c1", "c2", "c3"))
	require.NoError(t, err)
	require.Len(t, got, 2)
	assert.Equal(t, Grade{ID: "c1", Grade: GradeBad, Category: "wrong-claim", Reason: "x.go:3"}, got[0])
	assert.Equal(t, "c3", got[1].ID)
}

func TestParseReplyFailsWhenNothingUsable(t *testing.T) {
	_, err := ParseReply("no json here", items("c1"))
	assert.Error(t, err)
	_, err = ParseReply(`[{"id":"zz","grade":"OK"}]`, items("c1"))
	assert.Error(t, err)
}

func TestSelectRoundRobinsKindsUnderTheCap(t *testing.T) {
	var cands []Candidate
	for i := 0; i < 6; i++ {
		cands = append(cands, Candidate{Item: Item{ID: "i", Kind: KindInline}})
	}
	cands = append(cands, Candidate{Item: Item{ID: "n", Kind: KindSummaryNote}}, Candidate{Item: Item{ID: "r", Kind: KindReply}})

	kept, dropped := Select(cands, 4)
	assert.Equal(t, 4, dropped)
	var kinds []string
	for _, c := range kept {
		kinds = append(kinds, c.Item.Kind)
	}
	assert.ElementsMatch(t, []string{KindInline, KindInline, KindSummaryNote, KindReply}, kinds)

	all, none := Select(cands, 0)
	assert.Len(t, all, len(cands))
	assert.Zero(t, none)
}

func TestByPRGroupsByHeadInFirstSeenOrder(t *testing.T) {
	a1 := Candidate{Owner: "o", Repo: "r", PRNumber: 1, HeadSHA: "h1", Item: Item{ID: "a1"}}
	b := Candidate{Owner: "o", Repo: "r", PRNumber: 2, HeadSHA: "h2", Item: Item{ID: "b"}}
	a2 := Candidate{Owner: "o", Repo: "r", PRNumber: 1, HeadSHA: "h1", Item: Item{ID: "a2"}}
	got := ByPR([]Candidate{a1, b, a2})
	require.Len(t, got, 2)
	assert.Equal(t, []Candidate{a1, a2}, got[0])
	assert.Equal(t, []Candidate{b}, got[1])
}

func TestPromptFillsPlaceholdersAndTruncatesTheBody(t *testing.T) {
	body := strings.Repeat("x", maxBodyChars+50)
	p, err := Prompt("Fix login", body, "headsha", "basesha", "/tmp/base", items("c1"))
	require.NoError(t, err)
	for _, want := range []string{"Fix login", "headsha", "basesha", "/tmp/base", `"id": "c1"`, "[truncated]"} {
		assert.Contains(t, p, want)
	}
	assert.NotContains(t, p, "{{")
	assert.NotContains(t, p, strings.Repeat("x", maxBodyChars+1))
}

func TestPromptTruncatesTheBodyOnACharacterBoundary(t *testing.T) {
	body := strings.Repeat("é", maxBodyChars+1)
	p, err := Prompt("t", body, "h", "b", "d", items("c1"))
	require.NoError(t, err)
	assert.Contains(t, p, strings.Repeat("é", maxBodyChars)+"\n[truncated]")
	assert.True(t, utf8.ValidString(p))
}

func TestParseReplyIgnoresBracketsAroundTheArray(t *testing.T) {
	reply := "Checked arr[i] first.\n" + `[{"id":"c1","grade":"OK","category":"-","reason":"see x.go:2 [line 2]"}]` + "\nDone [all graded]."
	got, err := ParseReply(reply, items("c1"))
	require.NoError(t, err)
	require.Len(t, got, 1)
	assert.Equal(t, "see x.go:2 [line 2]", got[0].Reason)
}

func TestParseReplySkipsAnEchoedCopyOfTheItems(t *testing.T) {
	reply := `Items: [{"id":"c1","kind":"inline","comment":"x"}]` + "\n" + `[{"id":"c1","grade":"GOOD","category":"-","reason":"a.go:1"}]`
	got, err := ParseReply(reply, items("c1"))
	require.NoError(t, err)
	require.Len(t, got, 1)
	assert.Equal(t, GradeGood, got[0].Grade)
}
