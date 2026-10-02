package main

import (
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"pr-review-server/db"
	"pr-review-server/internal/replaykit"
)

const (
	DiffPostedShouldNot  = "posted_should_not"
	DiffSuppressedShould = "suppressed_should_post"
	DiffRepost           = "repost"
	DiffWrongFixed       = "wrong_fixed"
	DiffSummary          = "summary_mismatch"
	DiffReply            = "wrong_reply"
	DiffUnresolved       = "not_resolved"
)

// Diff is one place the replay departed from the expectation.
type Diff struct {
	Kind      string `json:"kind"`
	Round     int    `json:"round"`
	CommentID int64  `json:"comment_id,omitempty"`
	Detail    string `json:"detail"`
}

type ClassScore struct {
	Expected int `json:"expected"`
	Correct  int `json:"correct"`
}

// CaseScore is one case's counts; Aggregate sums them per tier.
type CaseScore struct {
	ID             string `json:"id"`
	Tier           string `json:"tier"`
	Rounds         int    `json:"rounds"`
	RoundsReplayed int    `json:"rounds_replayed"`

	ShouldPost        int `json:"should_post"`
	ShouldSuppress    int `json:"should_suppress"`
	PostedShould      int `json:"posted_should"`
	PostedShouldNot   int `json:"posted_should_not"`
	Suppressed        int `json:"suppressed_should"`
	SuppressedShould  int `json:"suppressed_should_post"`
	UnlabeledPosts    int `json:"unlabeled_posts"`
	UnmappedExpected  int `json:"unmapped_expectations"`
	Reposts           int `json:"reposts"`
	Fixed             int `json:"fixed"`
	WrongFixed        int `json:"wrong_fixed"`
	FixedUnconfirmed  int `json:"fixed_unconfirmed"`
	SummariesScored   int `json:"summaries_scored"`
	SummariesCorrect  int `json:"summaries_correct"`
	ResolutionsWanted int `json:"resolutions_expected"`
	ResolutionsDone   int `json:"resolutions_done"`
	RepliesScored     int `json:"replies_scored"`
	RepliesCorrect    int `json:"replies_correct"`
	RepliesNoThread   int `json:"replies_without_thread"`
	SyntheticReplies  int `json:"synthetic_reply_decisions"`

	ByClass map[string]*ClassScore `json:"reply_by_class,omitempty"`
	Diffs   []Diff                 `json:"diffs,omitempty"`
}

var sinceLastRe = regexp.MustCompile(`(\d+) new\D+(\d+) still open\D+(\d+) fixed`)

func parseSince(summary string) (newN, open, fixed int, ok bool) {
	m := sinceLastRe.FindStringSubmatch(summary)
	if m == nil {
		return 0, 0, 0, false
	}
	a, _ := strconv.Atoi(m[1])
	b, _ := strconv.Atoi(m[2])
	c, _ := strconv.Atoi(m[3])
	return a, b, c, true
}

func scoreCase(run *Run) CaseScore {
	c := run.Case
	s := CaseScore{ID: c.ID, Tier: c.Tier, Rounds: len(run.Rounds), ByClass: map[string]*ClassScore{}, SyntheticReplies: run.Synthetic}
	replayed := map[int]bool{}
	for _, rr := range run.Rounds {
		if rr.Replayed {
			s.RoundsReplayed++
			replayed[rr.Index] = true
		}
	}
	m := matchPosts(c, run.Posts)

	for _, fe := range c.Expect.Findings {
		if !fe.mapped() || !replayed[fe.Round] {
			s.UnmappedExpected++
			continue
		}
		_, posted := m.byExpect[fe.CommentID]
		switch {
		case fe.Expect == ExpectPost && posted:
			s.ShouldPost++
			s.PostedShould++
		case fe.Expect == ExpectPost:
			s.ShouldPost++
			s.SuppressedShould++
			s.Diffs = append(s.Diffs, Diff{Kind: DiffSuppressedShould, Round: fe.Round, CommentID: fe.CommentID, Detail: fe.File})
		case posted:
			s.ShouldSuppress++
			s.PostedShouldNot++
			s.Diffs = append(s.Diffs, Diff{Kind: DiffPostedShouldNot, Round: fe.Round, CommentID: fe.CommentID, Detail: fe.Reason + " " + fe.Detail})
		default:
			s.ShouldSuppress++
			s.Suppressed++
		}
	}

	var earlier []replaykit.Post
	for _, rr := range run.Rounds {
		for _, p := range rr.Posts {
			if _, ok := m.byPost[p.CommentID]; !ok {
				s.UnlabeledPosts++
			}
			if k := replaykit.ClassifyRepost(p, earlier); k != replaykit.RepostNone {
				s.Reposts++
				kind := "same marker"
				if k == replaykit.RepostSameDefect {
					kind = "same defect"
				}
				s.Diffs = append(s.Diffs, Diff{Kind: DiffRepost, Round: rr.Index, Detail: kind + " " + p.File})
			}
		}
		earlier = append(earlier, rr.Posts...)
	}

	scoreFixed(run, m, &s)
	scoreSummaries(run, m, &s)
	scoreResolutions(run, m, &s)
	scoreReplies(run, &s)
	sort.SliceStable(s.Diffs, func(i, j int) bool { return s.Diffs[i].Round < s.Diffs[j].Round })
	return s
}

// scoreFixed checks every row the publisher moved out of open. A flip is
// wrong when the case says the defect was not fixed by then and either its
// cited file did not change or the defect comes back in a later round; a
// flip on a changed file with no contrary evidence is only unconfirmed.
func scoreFixed(run *Run, m postMatch, s *CaseScore) {
	c := run.Case
	defectOfFP := map[string]*FindingExpect{}
	for _, p := range run.Posts {
		if fe, ok := m.byPost[p.CommentID]; ok {
			if _, seen := defectOfFP[p.FindingID]; !seen {
				defectOfFP[p.FindingID] = fe
			}
		}
	}
	fixedBy := map[int64]int{}
	for _, r := range c.Expect.Resolutions {
		if r.Why != "author verdict" && r.FixedBy >= 0 {
			fixedBy[r.Defect] = r.FixedBy
		}
	}
	for _, rr := range run.Rounds {
		for _, row := range rr.Flipped {
			s.Fixed++
			fe := defectOfFP[row.Fingerprint]
			if fe == nil {
				continue
			}
			if by, ok := fixedBy[fe.Defect]; ok && by <= rr.Index {
				continue
			}
			returns := false
			for _, later := range c.Expect.Findings {
				if later.Defect == fe.Defect && later.Round > rr.Index {
					returns = true
				}
			}
			changed, known := c.Compares[compareKey(row.LastSeenSHA, rr.HeadSHA)]
			why := ""
			switch {
			case raises(c.payload(rr.Index), fe):
				why = "still raised under new wording"
			case returns:
				why = "defect returns in a later round"
			case row.LastSeenSHA == rr.HeadSHA || (known && !replaykit.FileChanged(changed, replaykit.FileOfFingerprint(row.Fingerprint))):
				why = "cited file unchanged"
			}
			if why != "" {
				s.WrongFixed++
				s.Diffs = append(s.Diffs, Diff{Kind: DiffWrongFixed, Round: rr.Index, CommentID: fe.CommentID, Detail: why})
				continue
			}
			s.FixedUnconfirmed++
		}
	}
}

// scoreSummaries compares each later round's "Since last review" line with
// the expectation, over labelled findings only: rows the audit has no label
// for (folded notes, annotations, unlabelled posts) are taken out of the
// rendered counts first.
func scoreSummaries(run *Run, m postMatch, s *CaseScore) {
	labelled := map[string]bool{}
	for _, p := range run.Posts {
		if _, ok := m.byPost[p.CommentID]; ok {
			labelled[p.FindingID] = true
		}
	}
	byRound := map[int]RoundRun{}
	for _, rr := range run.Rounds {
		byRound[rr.Index] = rr
	}
	for _, se := range run.Case.Expect.Summaries {
		rr, ok := byRound[se.Round]
		if !ok || !rr.Replayed {
			continue
		}
		s.SummariesScored++
		n, _, f, ok := parseSince(rr.Summary)
		before := map[string]bool{}
		for _, row := range rr.Previous {
			before[row.Fingerprint] = true
		}
		for _, row := range rr.After {
			if replaykit.IsFindingRow(row) && !before[row.Fingerprint] && !labelled[row.Fingerprint] && n > 0 {
				n--
			}
		}
		for _, row := range rr.Flipped {
			if !labelled[row.Fingerprint] && f > 0 {
				f--
			}
		}
		if ok && n == se.New && f == se.Fixed {
			s.SummariesCorrect++
			continue
		}
		s.Diffs = append(s.Diffs, Diff{Kind: DiffSummary, Round: se.Round, Detail: fmt.Sprintf("labelled findings: want %d new, %d fixed; got %d new, %d fixed", se.New, se.Fixed, n, f)})
	}
}

func scoreResolutions(run *Run, m postMatch, s *CaseScore) {
	s.ResolutionsWanted = len(run.Case.Expect.Resolutions)
	if !replaykit.PublisherCanResolve() {
		return
	}
	resolved := map[int64]bool{}
	for _, id := range run.Rec.Resolved {
		resolved[id] = true
	}
	for _, r := range run.Case.Expect.Resolutions {
		done := false
		for _, p := range run.Posts {
			if fe, ok := m.byPost[p.CommentID]; ok && fe.Defect == r.Defect && resolved[p.CommentID] {
				done = true
			}
		}
		if id, ok := run.RootFor[r.CommentID]; ok && resolved[id] {
			done = true
		}
		if done {
			s.ResolutionsDone++
			continue
		}
		s.Diffs = append(s.Diffs, Diff{Kind: DiffUnresolved, Round: r.FixedBy, CommentID: r.CommentID, Detail: r.Why})
	}
}

// replyAction names what the reactor did about one author comment.
func replyAction(row db.PublishedReply, ok bool) string {
	switch {
	case !ok:
		return ActionNone
	case row.Outcome == "posted" && row.Decision != "":
		return row.Decision
	case row.Action == "reacted":
		return ActionReact
	}
	return ActionNone
}

func scoreReplies(run *Run, s *CaseScore) {
	for _, re := range run.Case.Expect.Replies {
		row, ok := run.Replies[re.CommentID]
		_, rootPosted := run.RootFor[re.RootCommentID]
		if !ok && !rootPosted {
			s.RepliesNoThread++
			continue
		}
		got := replyAction(row, ok)
		cs := s.ByClass[re.Class]
		if cs == nil {
			cs = &ClassScore{}
			s.ByClass[re.Class] = cs
		}
		cs.Expected++
		s.RepliesScored++
		correct := contains(re.Accept, got)
		if correct && re.WantDismissed {
			fr, has := run.FinalRows[row.Fingerprint]
			correct = ok && has && fr.State == db.PublishedStateDismissed
		}
		if correct {
			cs.Correct++
			s.RepliesCorrect++
			continue
		}
		want := fmt.Sprint(re.Accept)
		if re.WantDismissed {
			want += " and dismissed"
		}
		why := row.Outcome
		if !ok && !strings.EqualFold(re.Author, run.Case.Dump.Author.Login) {
			why = "reply by someone other than the PR author"
		}
		s.Diffs = append(s.Diffs, Diff{Kind: DiffReply, Round: -1, CommentID: re.CommentID, Detail: fmt.Sprintf("%s (%s): want %s, got %s (%s)", re.Class, re.Quality, want, got, why)})
	}
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

// TierMetrics are the aggregate numbers for one tier. Ratios are nil when
// their denominator is zero; resolution recall is nil until the publisher
// can resolve threads.
type TierMetrics struct {
	Cases             int                    `json:"cases"`
	Rounds            int                    `json:"rounds_replayed"`
	ShouldPost        int                    `json:"should_post"`
	ShouldSuppress    int                    `json:"should_suppress"`
	PostPrecision     *float64               `json:"post_precision"`
	PostRecall        *float64               `json:"post_recall"`
	SuppressionRecall *float64               `json:"suppression_recall"`
	UnlabeledPosts    int                    `json:"unlabeled_posts"`
	UnmappedExpected  int                    `json:"unmapped_expectations"`
	Reposts           int                    `json:"reposts"`
	Fixed             int                    `json:"fixed"`
	WrongFixed        int                    `json:"wrong_fixed"`
	FixedUnconfirmed  int                    `json:"fixed_unconfirmed"`
	SummariesScored   int                    `json:"summaries_scored"`
	SummaryCorrect    *float64               `json:"summary_count_correct"`
	ResolutionsWanted int                    `json:"resolutions_expected"`
	ResolutionRecall  *float64               `json:"thread_resolution_recall"`
	ResolutionNote    string                 `json:"thread_resolution_note,omitempty"`
	RepliesScored     int                    `json:"replies_scored"`
	RepliesNoThread   int                    `json:"replies_without_thread"`
	ReplyAccuracy     *float64               `json:"reply_accuracy"`
	ReplyByClass      map[string]*ClassScore `json:"reply_by_class"`
	SyntheticReplies  int                    `json:"synthetic_reply_decisions"`
}

func ratio(n, d int) *float64 {
	if d == 0 {
		return nil
	}
	v := float64(int(float64(n)/float64(d)*1000+0.5)) / 1000
	return &v
}

func aggregate(scores []CaseScore) TierMetrics {
	var t TierMetrics
	t.ReplyByClass = map[string]*ClassScore{}
	var tp, fp, fn, sup, sumOK, resDone, repOK int
	for _, s := range scores {
		t.Cases++
		t.Rounds += s.RoundsReplayed
		t.ShouldPost += s.ShouldPost
		t.ShouldSuppress += s.ShouldSuppress
		tp, fp, fn, sup = tp+s.PostedShould, fp+s.PostedShouldNot, fn+s.SuppressedShould, sup+s.Suppressed
		t.UnlabeledPosts += s.UnlabeledPosts
		t.UnmappedExpected += s.UnmappedExpected
		t.Reposts += s.Reposts
		t.Fixed += s.Fixed
		t.WrongFixed += s.WrongFixed
		t.FixedUnconfirmed += s.FixedUnconfirmed
		t.SummariesScored += s.SummariesScored
		sumOK += s.SummariesCorrect
		t.ResolutionsWanted += s.ResolutionsWanted
		resDone += s.ResolutionsDone
		t.RepliesScored += s.RepliesScored
		repOK += s.RepliesCorrect
		t.RepliesNoThread += s.RepliesNoThread
		t.SyntheticReplies += s.SyntheticReplies
		for k, v := range s.ByClass {
			cs := t.ReplyByClass[k]
			if cs == nil {
				cs = &ClassScore{}
				t.ReplyByClass[k] = cs
			}
			cs.Expected += v.Expected
			cs.Correct += v.Correct
		}
	}
	t.PostPrecision = ratio(tp, tp+fp)
	t.PostRecall = ratio(tp, tp+fn)
	t.SuppressionRecall = ratio(sup, sup+fp)
	t.SummaryCorrect = ratio(sumOK, t.SummariesScored)
	t.ReplyAccuracy = ratio(repOK, t.RepliesScored)
	if replaykit.PublisherCanResolve() {
		t.ResolutionRecall = ratio(resDone, t.ResolutionsWanted)
	} else {
		t.ResolutionNote = "not applicable: the publisher cannot resolve threads yet"
	}
	return t
}
