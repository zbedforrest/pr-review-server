package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"pr-review-server/internal/replaykit"
)

func TestExpectationForLabels(t *testing.T) {
	cases := []struct {
		label  auditComment
		expect string
		reason string
	}{
		{auditComment{Correctness: "correct", FirstRaisedBy: "prism", AddsContext: true}, ExpectPost, ""},
		{auditComment{Correctness: "correct", FirstRaisedBy: "prism", AddsContext: true, RedundantRepost: true, RepostOfCommentID: 7}, ExpectSuppress, ReasonRepost},
		{auditComment{Correctness: "correct", FirstRaisedBy: "prism", AddsContext: true, AddressedBeforePost: true}, ExpectSuppress, ReasonAddressed},
		{auditComment{Correctness: "correct", FirstRaisedBy: "greptile", AddsContext: true}, ExpectSuppress, ReasonExternal},
		{auditComment{Correctness: "correct", FirstRaisedBy: "human", AddsContext: true}, ExpectSuppress, ReasonExternal},
		{auditComment{Correctness: "incorrect", FirstRaisedBy: "prism", AddsContext: true}, ExpectSuppress, ReasonIncorrect},
		{auditComment{Correctness: "true_but_immaterial", FirstRaisedBy: "prism"}, ExpectSuppress, ReasonNoContext},
	}
	for i, c := range cases {
		if expect, reason, _ := expectationFor(c.label); expect != c.expect || reason != c.reason {
			t.Errorf("case %d: got %s/%s, want %s/%s", i, expect, reason, c.expect, c.reason)
		}
	}
}

func TestAcceptedActionsFollowTheVerdict(t *testing.T) {
	concede := &RecordedDecision{Decision: "concede"}
	wrong := &auditComment{Correctness: "incorrect"}
	cases := []struct {
		class, quality string
		rec            *RecordedDecision
		label          *auditComment
		accept         []string
		dismissed      bool
	}{
		{"intentional_behavior", "good", concede, nil, []string{ActionReact, ActionConcede, ActionWithdraw}, true},
		{"intentional_behavior", "missing_when_needed", nil, nil, []string{ActionReact, ActionConcede, ActionWithdraw}, true},
		{"intentional_behavior", "unneeded", concede, nil, []string{ActionReact}, true},
		{"fix_claim", "good", concede, nil, []string{ActionConcede}, false},
		{"fix_claim", "missing_when_needed", nil, nil, []string{ActionConcede}, false},
		{"pushback", "held_wrongly", nil, nil, []string{ActionConcede, ActionWithdraw}, true},
		{"pushback", "conceded_wrongly", concede, nil, []string{ActionHold}, false},
		{"pushback", "argued_against_itself", nil, nil, []string{ActionWithdraw}, true},
		{"pushback", "missing_when_needed", nil, wrong, []string{ActionConcede, ActionWithdraw}, true},
		{"pushback", "missing_when_needed", nil, nil, []string{ActionHold}, false},
		{"question", "missing_when_needed", nil, nil, []string{ActionAnswer}, false},
		{"ack", "unneeded", nil, nil, []string{ActionReact}, false},
	}
	for _, c := range cases {
		accept, dismissed := acceptedActions(auditResponse{Class: c.class, PrismReplyQuality: c.quality}, c.rec, c.label)
		if !reflect.DeepEqual(accept, c.accept) || dismissed != c.dismissed {
			t.Errorf("%s/%s: got %v dismissed=%t, want %v dismissed=%t", c.class, c.quality, accept, dismissed, c.accept, c.dismissed)
		}
	}
}

func TestExamplesCoverNamedCommentsOrTheWholePR(t *testing.T) {
	ex := indexExamples([]auditPattern{{Pattern: "repeat_after_fix", Votes: []struct {
		Real              bool     `json:"real"`
		ExamplesConfirmed []string `json:"examples_confirmed"`
		ExamplesRefuted   []string `json:"examples_refuted"`
	}{{ExamplesConfirmed: []string{"acme/example#5 (4000000001 reposted after 4000000002 by another bot)", "#6 has no owner"},
		ExamplesRefuted: []string{"acme/example#7 (a single run)"}}}}})
	prism := map[int64]bool{4000000001: true}
	confirmed := normaliseExamples(ex.confirmed["acme/example#5"], prism)
	if got := covers(confirmed, 4000000001); len(got) != 1 {
		t.Fatalf("named PRism comment not covered: %v", got)
	}
	if got := covers(confirmed, 4000000003); len(got) != 0 {
		t.Fatalf("an unnamed comment is covered: %v", got)
	}
	if !wholePR(normaliseExamples(ex.refuted["acme/example#7"], prism)) {
		t.Fatal("an example naming no comment must cover the PR")
	}
	if len(ex.confirmed) != 1 {
		t.Fatalf("an example without owner/repo must be ignored: %v", ex.confirmed)
	}
}

// writeInputs lays a fixture out the way the builder reads real data: one
// dump file, a sidecar cache with compares, and an audit entry.
func writeInputs(t *testing.T, c *Case) (dumps string, store *replaykit.Store) {
	t.Helper()
	dir := t.TempDir()
	dumps = filepath.Join(dir, "dumps")
	if err := os.MkdirAll(dumps, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := writeJSON(filepath.Join(dumps, fmt.Sprintf("acme__example__%d.json", c.Number)), c.Dump); err != nil {
		t.Fatal(err)
	}
	store, err := replaykit.NewStore(filepath.Join(dir, "sidecars"), nil)
	if err != nil {
		t.Fatal(err)
	}
	for sha7, raw := range c.Sidecars {
		if err := os.WriteFile(filepath.Join(dir, "sidecars", replaykit.SidecarName("acme", "example", c.Number, sha7)), raw, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	for key, files := range c.Compares {
		base, head, _ := strings.Cut(key, "...")
		raw, _ := json.Marshal(replaykit.CompareResult{Files: files})
		if err := os.WriteFile(filepath.Join(dir, "sidecars", "compare", fmt.Sprintf("acme_example_%s_%s.json", base, head)), raw, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dumps, store
}

func TestBuildReproducesThePushbackFixture(t *testing.T) {
	fixture, err := loadCase(filepath.Join("testdata", "cases", "pushback_reword.json"))
	if err != nil {
		t.Fatal(err)
	}
	dumps, store := writeInputs(t, fixture)
	apr := auditPR{Key: "acme/example#11", ReadOK: true,
		Comments: []auditComment{
			{CommentID: 101, Path: "svc/retry.go", Line: 42, Correctness: "correct", FirstRaisedBy: "prism", AddsContext: true},
			{CommentID: 102, Path: "svc/cache.go", Line: 15, Correctness: "correct", FirstRaisedBy: "prism", AddsContext: true},
			{CommentID: 103, Path: "svc/retry.go", Line: 43, Correctness: "correct", FirstRaisedBy: "prism", RedundantRepost: true, RepostOfCommentID: 101},
		},
		HumanResponses: []auditResponse{
			{CommentID: 201, Login: "dev-ana", Created: fixture.Expect.Replies[0].At, Class: "intentional_behavior", PrismReplied: true, PrismReplyQuality: "good"},
			{CommentID: 202, Login: "dev-ana", Created: fixture.Expect.Replies[1].At, Class: "fix_claim", Quote: "Fixed in 2222222, the key now includes the tenant id.", PrismReplied: true, PrismReplyQuality: "good"},
		}}
	confirmed := examples{confirmed: map[string][]example{"acme/example#11": {{Pattern: "repeat_after_pushback", IDs: map[int64]bool{}}}}}
	c, entry, err := buildCase(BuildOptions{DumpsDir: dumps, Store: store, ReplyLedger: fixture.Recorded}, apr, confirmed)
	if err != nil {
		t.Fatal(err)
	}
	if c.Tier != TierGold || entry.ExpectPost != 2 || entry.ExpectSuppress != 1 {
		t.Fatalf("tier %s, post %d, suppress %d (%v)", c.Tier, entry.ExpectPost, entry.ExpectSuppress, c.TierWhy)
	}
	type key struct {
		ID      int64
		Round   int
		Finding string
		Expect  string
		Reason  string
		Defect  int64
	}
	keys := func(fs []FindingExpect) []key {
		var out []key
		for _, f := range fs {
			out = append(out, key{f.CommentID, f.Round, f.FindingID, f.Expect, f.Reason, f.Defect})
		}
		return out
	}
	if got, want := keys(c.Expect.Findings), keys(fixture.Expect.Findings); !reflect.DeepEqual(got, want) {
		t.Fatalf("findings\n got %+v\nwant %+v", got, want)
	}
	if !reflect.DeepEqual(c.Expect.Summaries, fixture.Expect.Summaries) {
		t.Fatalf("summaries\n got %+v\nwant %+v", c.Expect.Summaries, fixture.Expect.Summaries)
	}
	if !reflect.DeepEqual(c.Expect.Resolutions, fixture.Expect.Resolutions) {
		t.Fatalf("resolutions\n got %+v\nwant %+v", c.Expect.Resolutions, fixture.Expect.Resolutions)
	}
	for i, r := range c.Expect.Replies {
		w := fixture.Expect.Replies[i]
		if r.CommentID != w.CommentID || r.RootCommentID != w.RootCommentID || !reflect.DeepEqual(r.Accept, w.Accept) || r.WantDismissed != w.WantDismissed || !r.Gold {
			t.Fatalf("reply %d\n got %+v\nwant %+v", i, r, w)
		}
	}
}

func TestBuildExcludesARefutedPR(t *testing.T) {
	fixture, err := loadCase(filepath.Join("testdata", "cases", "pushback_reword.json"))
	if err != nil {
		t.Fatal(err)
	}
	dumps, store := writeInputs(t, fixture)
	apr := auditPR{Key: "acme/example#11", ReadOK: true, Comments: []auditComment{{CommentID: 101, Correctness: "correct", FirstRaisedBy: "prism", AddsContext: true}}}
	ex := examples{confirmed: map[string][]example{}, refuted: map[string][]example{"acme/example#11": {{Pattern: "false_positive", IDs: map[int64]bool{}}}}}
	c, _, err := buildCase(BuildOptions{DumpsDir: dumps, Store: store}, apr, ex)
	if err != nil {
		t.Fatal(err)
	}
	if c.Tier != TierExcluded {
		t.Fatalf("tier = %s, want excluded", c.Tier)
	}
}

func TestParseSince(t *testing.T) {
	if n, o, f, ok := parseSince("x\n**Since last review:** 2 new · 1 still open · 3 fixed\n"); !ok || n != 2 || o != 1 || f != 3 {
		t.Fatalf("got %d %d %d %t", n, o, f, ok)
	}
	if _, _, _, ok := parseSince("first round"); ok {
		t.Fatal("a first-round summary has no counts")
	}
}
