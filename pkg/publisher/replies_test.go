package publisher

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"pr-review-server/db"
)

func TestClassifyReply(t *testing.T) {
	cases := map[string]ReplyClass{
		"Fixed":  ReplyResolution,
		"fixed.": ReplyResolution,
		"Done":   ReplyResolution,
		"Addressed in 9de3bed. The 15 s fast-abort stays for obvious misconfig.": ReplyResolution,
		"Accepted for now: XO-291 hasn't landed yet.":                            ReplyResolution,
		"Good catch, removed the extra call.":                                    ReplyResolution,
		"Thanks!":                                                                ReplyOther,
		"ok":                                                                     ReplyOther,
		"Will fix":                                                               ReplyOther,
		"Not fixed, the guard still runs twice.":                                 ReplyPushback,
		"Fixed?":                                                                 ReplyQuestion,
		"Fixed this, but why is the guard insufficient?":                                         ReplyQuestion,
		"`DmVideos` perform toggle is not active on prod.":                                       ReplyPushback,
		"Disabling standard shows does not pass a price of 0, it passes the last enabled price.": ReplyPushback,
	}
	for body, want := range cases {
		if got := ClassifyReply(body); got != want {
			t.Errorf("ClassifyReply(%q) = %s, want %s", body, got, want)
		}
	}
}

func TestFindAuthorRepliesKeepsOnlyTheAuthorsRepliesToOurRoots(t *testing.T) {
	t0 := time.Date(2026, 9, 8, 12, 0, 0, 0, time.UTC)
	roots := map[int64]string{100: "a.go:1:abc", 200: "b.go:4:def"}
	comments := []ThreadComment{
		{ID: 100, AuthorID: 1, Body: "<!-- prism:finding:a.go:1:abc -->\nours"},
		{ID: 101, InReplyToID: 100, AuthorID: 42, Body: "Fixed", CreatedAt: t0.Add(time.Minute)},
		{ID: 102, InReplyToID: 100, AuthorID: 7, Body: "third party pushback", CreatedAt: t0.Add(time.Minute)},
		{ID: 103, InReplyToID: 100, AuthorID: 1, Body: "our own reply", CreatedAt: t0.Add(time.Minute)},
		{ID: 104, InReplyToID: 999, AuthorID: 42, Body: "reply to someone else's thread", CreatedAt: t0.Add(time.Minute)},
		{ID: 105, InReplyToID: 200, AuthorID: 42, Body: "Why does this matter?", CreatedAt: t0.Add(-time.Hour)},
		{ID: 106, InReplyToID: 200, AuthorID: 42, Body: "Is this reachable?", CreatedAt: t0.Add(2 * time.Minute)},
	}
	got := FindAuthorReplies(comments, roots, 42, t0)
	if len(got) != 2 {
		t.Fatalf("got %d replies, want 2: %+v", len(got), got)
	}
	if got[0].CommentID != 101 || got[0].RootCommentID != 100 || got[0].Fingerprint != "a.go:1:abc" || got[0].Class != ReplyResolution {
		t.Errorf("first reply = %+v", got[0])
	}
	if got[1].CommentID != 106 || got[1].Fingerprint != "b.go:4:def" || got[1].Class != ReplyQuestion {
		t.Errorf("second reply = %+v", got[1])
	}
}

type fakeReplyGH struct {
	threads   map[string][]ThreadComment
	reactions []int64
	listed    []string
	failList  map[string]bool
	posted    []string
	postGate  chan struct{}
	mu        sync.Mutex
}

func (f *fakeReplyGH) ListThread(_ context.Context, owner, repo string, number int) ([]ThreadComment, error) {
	key := fmt.Sprintf("%s/%s#%d", owner, repo, number)
	f.listed = append(f.listed, key)
	if f.failList[key] {
		return nil, fmt.Errorf("boom")
	}
	return f.threads[key], nil
}

func (f *fakeReplyGH) React(_ context.Context, _, _ string, commentID int64) error {
	f.reactions = append(f.reactions, commentID)
	return nil
}

func (f *fakeReplyGH) PostReply(_ context.Context, owner, repo string, number int, rootCommentID int64, body string) (int64, error) {
	key := fmt.Sprintf("%s/%s#%d", owner, repo, number)
	if f.postGate != nil {
		f.postGate <- struct{}{}
		<-f.postGate
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.posted = append(f.posted, body)
	id := int64(5000 + len(f.posted))
	f.threads[key] = append(f.threads[key], ThreadComment{ID: id, InReplyToID: rootCommentID, AuthorID: 1, Body: body, CreatedAt: time.Now()})
	return id, nil
}

type fakeReplyLedger struct {
	targets  []db.PublishedReplyTarget
	rows     []db.PublishedReply
	unlinked []db.UnlinkedPublishedFinding
	linked   map[uint]int64
	states   map[string]string
	mu       sync.Mutex

	failOutcomeOnce bool
}

func (f *fakeReplyLedger) ListUnlinkedPublishedFindings() ([]db.UnlinkedPublishedFinding, error) {
	return f.unlinked, nil
}
func (f *fakeReplyLedger) LinkPublishedFindingComment(id uint, commentID int64) error {
	if f.linked == nil {
		f.linked = map[uint]int64{}
	}
	f.linked[id] = commentID
	return nil
}

func (f *fakeReplyLedger) ListPublishedReplyTargets() ([]db.PublishedReplyTarget, error) {
	return f.targets, nil
}
func (f *fakeReplyLedger) ListPublishedRepliesForPR(_, _ string, number int) ([]db.PublishedReply, error) {
	var out []db.PublishedReply
	for _, r := range f.rows {
		if r.PRNumber == number {
			out = append(out, r)
		}
	}
	return out, nil
}
func (f *fakeReplyLedger) SetPublishedReplyOutcome(_, _ string, _ int, authorCommentID int64, outcome string) error {
	if f.failOutcomeOnce {
		f.failOutcomeOnce = false
		return fmt.Errorf("db blip")
	}
	for i := range f.rows {
		if f.rows[i].AuthorCommentID == authorCommentID {
			f.rows[i].Outcome = outcome
		}
	}
	return nil
}
func (f *fakeReplyLedger) ClaimPublishedReply(_, _ string, _ int, authorCommentID int64, holder string, now time.Time, lease time.Duration) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for i := range f.rows {
		if f.rows[i].AuthorCommentID != authorCommentID {
			continue
		}
		r := &f.rows[i]
		if r.Outcome != "" || (r.ClaimedAt != nil && !r.ClaimedAt.Before(now.Add(-lease))) {
			return false, nil
		}
		at := now
		r.ClaimedBy, r.ClaimedAt = holder, &at
		return true, nil
	}
	return false, fmt.Errorf("no row")
}
func (f *fakeReplyLedger) ReleasePublishedReplyClaim(_, _ string, _ int, authorCommentID int64, holder string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	for i := range f.rows {
		if f.rows[i].AuthorCommentID == authorCommentID && f.rows[i].ClaimedBy == holder {
			f.rows[i].ClaimedBy, f.rows[i].ClaimedAt = "", nil
		}
	}
	return nil
}
func (f *fakeReplyLedger) IncrementPublishedReplyAttempts(_, _ string, _ int, authorCommentID int64) (int, error) {
	for i := range f.rows {
		if f.rows[i].AuthorCommentID == authorCommentID {
			f.rows[i].Attempts++
			return f.rows[i].Attempts, nil
		}
	}
	return 0, fmt.Errorf("no row")
}
func (f *fakeReplyLedger) SetPublishedReplyDecision(_, _ string, _ int, authorCommentID int64, d db.ReplyDecisionRecord) error {
	for i := range f.rows {
		if f.rows[i].AuthorCommentID == authorCommentID {
			f.rows[i].Decision, f.rows[i].ReplyBody, f.rows[i].Cited, f.rows[i].Model = d.Decision, d.ReplyBody, d.Cited, d.Model
			f.rows[i].DecisionHead, f.rows[i].DecisionThread = d.Head, d.Thread
		}
	}
	return nil
}
func (f *fakeReplyLedger) MarkPublishedReplyPosted(_, _ string, _ int, authorCommentID, replyCommentID int64, at time.Time) error {
	for i := range f.rows {
		if f.rows[i].AuthorCommentID == authorCommentID {
			f.rows[i].ReplyCommentID = replyCommentID
			f.rows[i].RepliedAt = &at
		}
	}
	return nil
}
func (f *fakeReplyLedger) ListPublishedRepliesForRoot(_, _ string, _ int, rootCommentID int64) ([]db.PublishedReply, error) {
	var out []db.PublishedReply
	for _, r := range f.rows {
		if r.RootCommentID == rootCommentID {
			out = append(out, r)
		}
	}
	return out, nil
}
func (f *fakeReplyLedger) CountPublishedTextRepliesSince(_, _ string, number int, since time.Time) (int, error) {
	n := 0
	for _, r := range f.rows {
		if r.PRNumber == number && r.ReplyCommentID != 0 && r.RepliedAt != nil && !r.RepliedAt.Before(since) {
			n++
		}
	}
	return n, nil
}
func (f *fakeReplyLedger) SetPublishedFindingState(_, _ string, _ int, fingerprint, state string) error {
	if f.states == nil {
		f.states = map[string]string{}
	}
	f.states[fingerprint] = state
	return nil
}
func (f *fakeReplyLedger) RecordPublishedReply(r *db.PublishedReply) (bool, error) {
	for _, x := range f.rows {
		if x.AuthorCommentID == r.AuthorCommentID {
			return false, nil
		}
	}
	f.rows = append(f.rows, *r)
	return true, nil
}

func reactorFixture(mode string) (*ReplyReactor, *fakeReplyGH, *fakeReplyLedger) {
	t0 := time.Date(2026, 9, 8, 12, 0, 0, 0, time.UTC)
	gh := &fakeReplyGH{threads: map[string][]ThreadComment{
		"acme/example#7": {
			{ID: 100, AuthorID: 1, Body: "ours"},
			{ID: 101, InReplyToID: 100, AuthorID: 42, Body: "Fixed", CreatedAt: t0.Add(time.Minute)},
			{ID: 102, InReplyToID: 100, AuthorID: 42, Body: "Is this reachable at all?", CreatedAt: t0.Add(2 * time.Minute)},
			{ID: 103, InReplyToID: 100, AuthorID: 42, Body: "old", CreatedAt: t0.Add(-time.Hour)},
		},
		"acme/example#8": {
			{ID: 200, AuthorID: 1, Body: "ours"},
			{ID: 201, InReplyToID: 200, AuthorID: 43, Body: "Fixed", CreatedAt: t0.Add(time.Minute)},
		},
		"acme/example#9": {
			{ID: 300, AuthorID: 1, Body: "ours"},
			{ID: 301, InReplyToID: 300, AuthorID: 44, Body: "Fixed", CreatedAt: t0.Add(time.Minute)},
		},
	}}
	ledger := &fakeReplyLedger{targets: []db.PublishedReplyTarget{
		{RepoOwner: "acme", RepoName: "example", PRNumber: 7, Roots: map[int64]string{100: "a.go:1:abc"}},
		{RepoOwner: "acme", RepoName: "example", PRNumber: 8, Roots: map[int64]string{200: "b.go:1:abc"}},
		{RepoOwner: "acme", RepoName: "example", PRNumber: 9, Roots: map[int64]string{300: "c.go:1:abc"}},
	}}
	states := map[int]PRState{
		7: {Open: true, AuthorID: 42, AuthorLogin: "pilot"},
		8: {Open: true, Draft: true, AuthorID: 43, AuthorLogin: "pilot"},
		9: {Open: true, AuthorID: 44, AuthorLogin: "stranger"},
	}
	r := &ReplyReactor{
		GH: gh, Ledger: ledger, Mode: mode, Since: t0,
		PR:      func(_ context.Context, _, _ string, number int) (PRState, error) { return states[number], nil },
		Allowed: func(login string) bool { return login == "pilot" },
	}
	return r, gh, ledger
}

func TestReplyReactor_ReactsOnceToEachNewAuthorReplyOnEligiblePRs(t *testing.T) {
	r, gh, ledger := reactorFixture("react")
	rep, err := r.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if fmt.Sprint(gh.reactions) != "[101 102]" {
		t.Fatalf("reactions = %v, want only PR 7's new replies (8 is draft, 9 not allowlisted)", gh.reactions)
	}
	if rep.Reacted != 2 || len(ledger.rows) != 2 || ledger.rows[0].Class != "resolution" || ledger.rows[1].Class != "question" || ledger.rows[0].Action != "reacted" {
		t.Errorf("report=%+v rows=%+v", rep, ledger.rows)
	}

	gh.reactions = nil
	rep, err = r.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(gh.reactions) != 0 || rep.Reacted != 0 {
		t.Errorf("second cycle must be a no-op, got reactions=%v report=%+v", gh.reactions, rep)
	}
}

func TestReplyReactor_ObserveRecordsWithoutReacting(t *testing.T) {
	r, gh, ledger := reactorFixture("observe")
	if _, err := r.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(gh.reactions) != 0 || len(ledger.rows) != 2 || ledger.rows[0].Action != "observed" {
		t.Errorf("observe mode: reactions=%v rows=%+v", gh.reactions, ledger.rows)
	}
}

func TestMatchUnlinkedRootsUsesReviewIDAndMarkerTogether(t *testing.T) {
	rows := []db.UnlinkedPublishedFinding{
		{ID: 1, ReviewID: 500, Fingerprint: "a.go:1:abc"},
		{ID: 2, ReviewID: 500, Fingerprint: "b.go:2:def"},
		{ID: 3, ReviewID: 501, Fingerprint: "c.go:3:ghi"},
	}
	comments := []ThreadComment{
		{ID: 100, ReviewID: 500, Body: FindingMarker("a.go:1:abc") + "\nours"},
		{ID: 101, ReviewID: 500, InReplyToID: 100, Body: FindingMarker("a.go:1:abc") + " quoted back by the author"},
		{ID: 102, ReviewID: 999, Body: FindingMarker("b.go:2:def") + "\nsame marker, someone else's review"},
		{ID: 103, ReviewID: 501, Body: "no marker"},
	}
	got := MatchUnlinkedRoots(rows, comments)
	if len(got) != 1 || got[1] != 100 {
		t.Fatalf("MatchUnlinkedRoots = %v, want only row 1 -> comment 100", got)
	}
}

func TestReplyReactor_LinkRootsRecordsCommentIDsForReviewPostedFindings(t *testing.T) {
	gh := &fakeReplyGH{threads: map[string][]ThreadComment{
		"acme/example#7": {
			{ID: 100, ReviewID: 500, AuthorID: 1, Body: FindingMarker("a.go:1:abc") + "\nours"},
			{ID: 110, ReviewID: 500, AuthorID: 1, Body: FindingMarker("z.go:9:zzz") + "\nours, not in the ledger"},
		},
		"acme/example#8": {
			{ID: 200, ReviewID: 600, AuthorID: 1, Body: "marker gone"},
		},
	}}
	ledger := &fakeReplyLedger{unlinked: []db.UnlinkedPublishedFinding{
		{ID: 1, RepoOwner: "acme", RepoName: "example", PRNumber: 7, ReviewID: 500, Fingerprint: "a.go:1:abc"},
		{ID: 2, RepoOwner: "acme", RepoName: "example", PRNumber: 8, ReviewID: 600, Fingerprint: "b.go:2:def"},
	}}
	r := &ReplyReactor{GH: gh, Ledger: ledger}
	rep := r.LinkRoots(context.Background(), ledger.unlinked)
	if rep.Linked != 1 || rep.Unmatched != 1 || rep.PRsListed != 2 || len(rep.Errors) != 0 {
		t.Fatalf("report = %+v", rep)
	}
	if ledger.linked[1] != 100 || len(ledger.linked) != 1 {
		t.Errorf("linked = %v, want {1:100}", ledger.linked)
	}
}

func TestReplyReactor_ReportCountsWhatWasSkippedAndWhy(t *testing.T) {
	r, _, ledger := reactorFixture("react")
	rep, err := r.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if rep.PRsSkipped["draft"] != 1 || rep.PRsSkipped["not_allowlisted"] != 1 || rep.PRsScanned != 1 {
		t.Errorf("skip breakdown = %v scanned=%d", rep.PRsSkipped, rep.PRsScanned)
	}
	if rep.RepliesSeen != 2 || rep.AlreadyHandled != 0 || len(rep.Handled) != 2 || rep.Handled[1].Class != "question" {
		t.Errorf("replies: seen=%d already=%d handled=%+v", rep.RepliesSeen, rep.AlreadyHandled, rep.Handled)
	}
	rep, err = r.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if rep.RepliesSeen != 2 || rep.AlreadyHandled != 2 || len(rep.Handled) != 0 || len(ledger.rows) != 2 {
		t.Errorf("second cycle: seen=%d already=%d handled=%d", rep.RepliesSeen, rep.AlreadyHandled, len(rep.Handled))
	}
}

func TestReplyReactor_SkipsUnchangedPRsByLiveUpdatedAtUntilFullScan(t *testing.T) {
	t0 := time.Date(2026, 9, 9, 17, 0, 0, 0, time.UTC)
	r, gh, _ := reactorFixture("react")
	updated := t0
	r.PR = func(_ context.Context, _, _ string, number int) (PRState, error) {
		return PRState{Open: true, AuthorID: 42, AuthorLogin: "pilot", UpdatedAt: updated}, nil
	}
	r.Ledger.(*fakeReplyLedger).targets = r.Ledger.(*fakeReplyLedger).targets[:1]
	r.LastScanned = map[string]time.Time{}

	rep, _ := r.Run(context.Background())
	if len(gh.listed) != 1 || rep.PRsScanned != 1 {
		t.Fatalf("first cycle must list the thread: listed=%v report=%+v", gh.listed, rep)
	}
	rep, _ = r.Run(context.Background())
	if len(gh.listed) != 1 || rep.PRsSkipped["unchanged"] != 1 {
		t.Fatalf("unchanged updated_at must skip the listing: listed=%v report=%+v", gh.listed, rep)
	}
	updated = t0.Add(time.Minute)
	rep, _ = r.Run(context.Background())
	if len(gh.listed) != 2 || rep.PRsScanned != 1 {
		t.Fatalf("a moved updated_at must rescan: listed=%v report=%+v", gh.listed, rep)
	}
	r.Full = true
	rep, _ = r.Run(context.Background())
	if len(gh.listed) != 3 {
		t.Fatalf("a full scan ignores the watermark: listed=%v", gh.listed)
	}
}

func TestReplyReactor_FailedScanDoesNotAdvanceTheWatermark(t *testing.T) {
	t0 := time.Date(2026, 9, 9, 17, 0, 0, 0, time.UTC)
	r, gh, ledger := reactorFixture("react")
	ledger.targets = ledger.targets[:1]
	r.PR = func(_ context.Context, _, _ string, _ int) (PRState, error) {
		return PRState{Open: true, AuthorID: 42, AuthorLogin: "pilot", UpdatedAt: t0}, nil
	}
	r.LastScanned = map[string]time.Time{}
	gh.failList = map[string]bool{"acme/example#7": true}

	rep, _ := r.Run(context.Background())
	if len(rep.Errors) != 1 || !r.LastScanned["acme/example#7"].IsZero() {
		t.Fatalf("errored target must stay unscanned: report=%+v watermarks=%v", rep, r.LastScanned)
	}
	gh.failList = nil
	rep, _ = r.Run(context.Background())
	if rep.PRsScanned != 1 || !r.LastScanned["acme/example#7"].Equal(t0) {
		t.Fatalf("retry next cycle and then advance: report=%+v watermarks=%v", rep, r.LastScanned)
	}
}

func TestTextEligibilityRules(t *testing.T) {
	now := time.Date(2026, 9, 9, 18, 0, 0, 0, time.UTC)
	posted := now.Add(-time.Hour)
	fresh := AuthorReply{RootCommentID: 100, Class: ReplyPushback, CreatedAt: now.Add(-time.Minute)}
	cases := []struct {
		name   string
		reply  AuthorReply
		prior  []db.PublishedReply
		prDay  int
		reason string
	}{
		{"pushback gets text", fresh, nil, 0, ""},
		{"question gets text", AuthorReply{Class: ReplyQuestion, CreatedAt: now}, nil, 0, ""},
		{"resolution never", AuthorReply{Class: ReplyResolution, CreatedAt: now}, nil, 0, "class"},
		{"other never", AuthorReply{Class: ReplyOther, CreatedAt: now}, nil, 0, "class"},
		{"stale", AuthorReply{Class: ReplyPushback, CreatedAt: now.Add(-25 * time.Hour)}, nil, 0, "stale"},
		{"thread cap", fresh, []db.PublishedReply{{ReplyCommentID: 1, RepliedAt: &posted}, {ReplyCommentID: 2, RepliedAt: &posted}}, 0, "thread_cap"},
		{"one prior text is fine", fresh, []db.PublishedReply{{ReplyCommentID: 1, RepliedAt: &posted}, {Decision: "abstain"}}, 0, ""},
		{"conceded ends it", fresh, []db.PublishedReply{{ReplyCommentID: 1, RepliedAt: &posted, Decision: DecisionConcede}}, 0, "conceded"},
		{"pr daily cap", fresh, nil, 10, "pr_cap"},
	}
	for _, c := range cases {
		if got := TextEligibility(c.reply, c.prior, c.prDay, now, DefaultTextPolicy()); got != c.reason {
			t.Errorf("%s: reason=%q want %q", c.name, got, c.reason)
		}
	}
}

func respondFixture(mode string, decide Responder) (*ReplyReactor, *fakeReplyGH, *fakeReplyLedger) {
	t0 := time.Date(2026, 9, 9, 18, 0, 0, 0, time.UTC)
	gh := &fakeReplyGH{threads: map[string][]ThreadComment{
		"acme/example#7": {
			{ID: 100, AuthorID: 1, Body: FindingMarker("a.go:1:abc") + "\n**[MEDIUM] Nil deref.**", CreatedAt: t0.Add(-time.Hour)},
			{ID: 101, InReplyToID: 100, AuthorID: 42, Body: "This can't be nil here, the caller guards it.", CreatedAt: t0.Add(-time.Minute)},
		},
	}}
	ledger := &fakeReplyLedger{targets: []db.PublishedReplyTarget{
		{RepoOwner: "acme", RepoName: "example", PRNumber: 7, Roots: map[int64]string{100: "a.go:1:abc"}},
	}}
	r := &ReplyReactor{
		GH: gh, Ledger: ledger, Mode: mode, Since: t0.Add(-2 * time.Hour), Responder: decide,
		Now: func() time.Time { return t0 },
		PR: func(_ context.Context, _, _ string, _ int) (PRState, error) {
			return PRState{Open: true, AuthorID: 42, AuthorLogin: "pilot", HeadSHA: "head1"}, nil
		},
		Allowed: func(login string) bool { return login == "pilot" },
	}
	return r, gh, ledger
}

func TestReplyReactor_RespondPostsAHoldWithMarkerAndRecordsIt(t *testing.T) {
	var got ReplyRequest
	r, gh, ledger := respondFixture(ReplyModeRespond, func(_ context.Context, req ReplyRequest) (ReplyDecision, error) {
		got = req
		return ReplyDecision{Decision: DecisionHold, Reply: "The guard is on the other branch; line 12 reaches here with nil.", Cited: []EvidenceRef{{File: "a.go", Line: 12}}, Model: "m"}, nil
	})
	rep, err := r.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if got.Fingerprint != "a.go:1:abc" || got.HeadSHA != "head1" || got.Reply.CommentID != 101 || len(got.Thread) != 2 || got.Thread[0].ID != 100 {
		t.Fatalf("request = %+v", got)
	}
	if len(gh.reactions) != 1 || len(gh.posted) != 1 || !strings.HasSuffix(gh.posted[0], ReplyMarker(101)) || !strings.HasPrefix(gh.posted[0], "The guard") {
		t.Fatalf("reactions=%v posted=%q", gh.reactions, gh.posted)
	}
	if rep.Responded != 1 || ledger.rows[0].Decision != DecisionHold || ledger.rows[0].ReplyCommentID != 5001 || ledger.rows[0].RepliedAt == nil {
		t.Errorf("report=%+v row=%+v", rep, ledger.rows[0])
	}
	if ledger.states["a.go:1:abc"] != "" {
		t.Errorf("a hold must not change the finding state")
	}

	gh.posted = nil
	rep, _ = r.Run(context.Background())
	if len(gh.posted) != 0 || rep.Responded != 0 {
		t.Errorf("second cycle must not answer again: posted=%v rep=%+v", gh.posted, rep)
	}
}

func TestReplyReactor_ConcedeDismissesTheFinding(t *testing.T) {
	r, gh, ledger := respondFixture(ReplyModeRespond, func(_ context.Context, _ ReplyRequest) (ReplyDecision, error) {
		return ReplyDecision{Decision: DecisionConcede, Reply: "You're right, the caller guards it. Withdrawn."}, nil
	})
	if _, err := r.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(gh.posted) != 1 || ledger.states["a.go:1:abc"] != db.PublishedStateDismissed {
		t.Fatalf("posted=%v states=%v", gh.posted, ledger.states)
	}
}

func TestReplyReactor_ShadowRecordsTheDecisionWithoutPosting(t *testing.T) {
	r, gh, ledger := respondFixture(ReplyModeShadow, func(_ context.Context, _ ReplyRequest) (ReplyDecision, error) {
		return ReplyDecision{Decision: DecisionConcede, Reply: "Withdrawn."}, nil
	})
	rep, _ := r.Run(context.Background())
	if len(gh.posted) != 0 || len(gh.reactions) != 1 || rep.Shadowed != 1 || ledger.rows[0].Decision != DecisionConcede || ledger.rows[0].ReplyBody != "Withdrawn." {
		t.Fatalf("posted=%v reactions=%v rep=%+v row=%+v", gh.posted, gh.reactions, rep, ledger.rows[0])
	}
	if ledger.states["a.go:1:abc"] != "" {
		t.Errorf("shadow mode must not change finding state")
	}
}

func TestReplyReactor_AbstainAndOverlongRepliesAreNotPosted(t *testing.T) {
	r, gh, ledger := respondFixture(ReplyModeRespond, func(_ context.Context, _ ReplyRequest) (ReplyDecision, error) {
		return ReplyDecision{Decision: DecisionHold, Reply: strings.Repeat("x", 601)}, nil
	})
	rep, _ := r.Run(context.Background())
	if len(gh.posted) != 0 || rep.TextSkipped["too_long"] != 1 || ledger.rows[0].Decision != DecisionHold {
		t.Fatalf("posted=%v rep=%+v row=%+v", gh.posted, rep, ledger.rows[0])
	}

	r, gh, _ = respondFixture(ReplyModeRespond, func(_ context.Context, _ ReplyRequest) (ReplyDecision, error) {
		return ReplyDecision{Decision: DecisionAbstain}, nil
	})
	rep, _ = r.Run(context.Background())
	if len(gh.posted) != 0 || rep.Abstained != 1 {
		t.Fatalf("posted=%v rep=%+v", gh.posted, rep)
	}
}

func TestReplyReactor_DiscardsTheReplyWhenTheThreadOrHeadMovedMeanwhile(t *testing.T) {
	var gh *fakeReplyGH
	r, gh, _ := respondFixture(ReplyModeRespond, func(_ context.Context, _ ReplyRequest) (ReplyDecision, error) {
		gh.threads["acme/example#7"] = append(gh.threads["acme/example#7"], ThreadComment{ID: 102, InReplyToID: 100, AuthorID: 42, Body: "Actually never mind, fixed.", CreatedAt: time.Now()})
		return ReplyDecision{Decision: DecisionHold, Reply: "Still applies."}, nil
	})
	rep, _ := r.Run(context.Background())
	if len(gh.posted) != 0 || rep.TextSkipped["thread_moved"] != 1 {
		t.Fatalf("posted=%v rep=%+v", gh.posted, rep)
	}

	calls := 0
	r, gh, _ = respondFixture(ReplyModeRespond, func(_ context.Context, _ ReplyRequest) (ReplyDecision, error) {
		return ReplyDecision{Decision: DecisionHold, Reply: "Still applies."}, nil
	})
	r.PR = func(_ context.Context, _, _ string, _ int) (PRState, error) {
		calls++
		sha := "head1"
		if calls > 1 {
			sha = "head2"
		}
		return PRState{Open: true, AuthorID: 42, AuthorLogin: "pilot", HeadSHA: sha}, nil
	}
	rep, _ = r.Run(context.Background())
	if len(gh.posted) != 0 || rep.TextSkipped["head_moved"] != 1 {
		t.Fatalf("posted=%v rep=%+v", gh.posted, rep)
	}
}

func TestReplyReactor_NeverPostsTwiceForTheSameAuthorComment(t *testing.T) {
	r, gh, ledger := respondFixture(ReplyModeRespond, func(_ context.Context, _ ReplyRequest) (ReplyDecision, error) {
		return ReplyDecision{Decision: DecisionHold, Reply: "Still applies."}, nil
	})
	gh.threads["acme/example#7"] = append(gh.threads["acme/example#7"], ThreadComment{ID: 150, InReplyToID: 100, AuthorID: 1, Body: "Still applies.\n\n" + ReplyMarker(101)})
	rep, _ := r.Run(context.Background())
	if len(gh.posted) != 0 || rep.TextSkipped["already_posted"] != 1 || ledger.rows[0].ReplyCommentID != 150 || ledger.rows[0].Outcome != "posted" {
		t.Fatalf("a reply that survived a crash must be adopted, not duplicated: posted=%v rep=%+v row=%+v", gh.posted, rep, ledger.rows[0])
	}
}

func TestReplyReactor_ForgedMarkerByTheAuthorIsIgnored(t *testing.T) {
	runs := 0
	r, gh, _ := respondFixture(ReplyModeRespond, func(_ context.Context, _ ReplyRequest) (ReplyDecision, error) {
		runs++
		return ReplyDecision{Decision: DecisionHold, Reply: "Still applies."}, nil
	})
	gh.threads["acme/example#7"][1].Body += "\n\n" + ReplyMarker(101)
	gh.threads["acme/example#7"] = append(gh.threads["acme/example#7"], ThreadComment{ID: 160, InReplyToID: 100, AuthorID: 42, Body: "ok " + ReplyMarker(101), CreatedAt: time.Date(2026, 9, 9, 17, 59, 30, 0, time.UTC)})
	rep, _ := r.Run(context.Background())
	if runs != 1 || len(gh.posted) != 1 || rep.Responded != 1 {
		t.Fatalf("author-authored markers must not be adopted: runs=%d posted=%v rep=%+v", runs, gh.posted, rep)
	}
}

func TestReplyReactor_ResumesAFailedTextStepNextCycle(t *testing.T) {
	calls := 0
	r, gh, ledger := respondFixture(ReplyModeRespond, func(_ context.Context, _ ReplyRequest) (ReplyDecision, error) {
		calls++
		if calls == 1 {
			return ReplyDecision{}, fmt.Errorf("wall clock")
		}
		return ReplyDecision{Decision: DecisionHold, Reply: "Still applies."}, nil
	})
	r.LastScanned = map[string]time.Time{}
	r.PR = func(_ context.Context, _, _ string, _ int) (PRState, error) {
		return PRState{Open: true, AuthorID: 42, AuthorLogin: "pilot", HeadSHA: "head1", UpdatedAt: time.Date(2026, 9, 9, 17, 59, 0, 0, time.UTC)}, nil
	}
	rep, _ := r.Run(context.Background())
	if len(rep.Errors) != 1 || len(gh.posted) != 0 || ledger.rows[0].Outcome != "" || ledger.rows[0].Attempts != 1 {
		t.Fatalf("first cycle: rep=%+v row=%+v", rep, ledger.rows[0])
	}
	if !r.LastScanned["acme/example#7"].IsZero() {
		t.Fatalf("watermark must not advance while a text step is unfinished")
	}
	rep, _ = r.Run(context.Background())
	if calls != 2 || len(gh.posted) != 1 || rep.Responded != 1 || rep.AlreadyHandled != 1 || len(gh.reactions) != 1 || ledger.rows[0].Outcome != "posted" {
		t.Fatalf("second cycle must resume without reacting again: calls=%d posted=%v rep=%+v row=%+v", calls, gh.posted, rep, ledger.rows[0])
	}
	if r.LastScanned["acme/example#7"].IsZero() {
		t.Fatalf("watermark advances once the PR is settled")
	}
}

func TestReplyReactor_GivesUpAfterMaxAttempts(t *testing.T) {
	r, _, ledger := respondFixture(ReplyModeRespond, func(_ context.Context, _ ReplyRequest) (ReplyDecision, error) {
		return ReplyDecision{}, fmt.Errorf("boom")
	})
	for i := 0; i < 4; i++ {
		r.Run(context.Background())
	}
	if ledger.rows[0].Outcome != "failed" || ledger.rows[0].Attempts != 3 {
		t.Fatalf("row=%+v", ledger.rows[0])
	}
}

func TestReplyReactor_CrashAfterPostIsRecoveredFromThePersistedDecision(t *testing.T) {
	runs := 0
	r, gh, ledger := respondFixture(ReplyModeRespond, func(_ context.Context, _ ReplyRequest) (ReplyDecision, error) {
		runs++
		return ReplyDecision{Decision: DecisionConcede, Reply: "Withdrawn."}, nil
	})
	t0 := time.Date(2026, 9, 9, 17, 59, 0, 0, time.UTC)
	ledger.rows = []db.PublishedReply{{RepoOwner: "acme", RepoName: "example", PRNumber: 7, RootCommentID: 100, AuthorCommentID: 101,
		Fingerprint: "a.go:1:abc", Class: "pushback", Action: "reacted", Decision: DecisionConcede, ReplyBody: "Withdrawn.", CreatedAt: t0}}
	gh.threads["acme/example#7"] = append(gh.threads["acme/example#7"], ThreadComment{ID: 170, InReplyToID: 100, AuthorID: 1, Body: "Withdrawn.\n\n" + ReplyMarker(101), CreatedAt: t0.Add(time.Minute)})
	rep, _ := r.Run(context.Background())
	if runs != 0 || len(gh.posted) != 0 || ledger.rows[0].ReplyCommentID != 170 || ledger.rows[0].Outcome != "posted" || ledger.states["a.go:1:abc"] != db.PublishedStateDismissed {
		t.Fatalf("runs=%d posted=%v rep=%+v row=%+v states=%v", runs, gh.posted, rep, ledger.rows[0], ledger.states)
	}
}

func TestReplyReactor_AnswersTwoSiblingsInOneScan(t *testing.T) {
	r, gh, ledger := respondFixture(ReplyModeRespond, func(_ context.Context, req ReplyRequest) (ReplyDecision, error) {
		return ReplyDecision{Decision: DecisionAnswer, Reply: fmt.Sprintf("Answer to %d.", req.Reply.CommentID)}, nil
	})
	t0 := time.Date(2026, 9, 9, 17, 59, 30, 0, time.UTC)
	gh.threads["acme/example#7"] = append(gh.threads["acme/example#7"], ThreadComment{ID: 102, InReplyToID: 100, AuthorID: 42, Body: "And is the retry path covered as well?", CreatedAt: t0})
	rep, _ := r.Run(context.Background())
	if rep.Responded != 2 || len(gh.posted) != 2 || ledger.rows[1].Outcome != "posted" {
		t.Fatalf("both siblings must be answered: rep=%+v posted=%v rows=%+v", rep, gh.posted, ledger.rows)
	}
}

func TestReplyReactor_AuthorEditDuringTheModelRunDropsTheReply(t *testing.T) {
	var gh *fakeReplyGH
	r, gh, ledger := respondFixture(ReplyModeRespond, func(_ context.Context, _ ReplyRequest) (ReplyDecision, error) {
		gh.threads["acme/example#7"][1].Body = "Never mind, I see it now."
		return ReplyDecision{Decision: DecisionHold, Reply: "Still applies."}, nil
	})
	rep, _ := r.Run(context.Background())
	if len(gh.posted) != 0 || rep.TextSkipped["thread_moved"] != 1 || ledger.rows[0].Outcome != "skipped:thread_moved" {
		t.Fatalf("posted=%v rep=%+v row=%+v", gh.posted, rep, ledger.rows[0])
	}
}

func TestReplyReactor_BackgroundDispatchRunsOncePerReplyAndReportsOutcomes(t *testing.T) {
	var tasks []func()
	var outcomes []ReplyOutcome
	r, gh, ledger := respondFixture(ReplyModeRespond, func(_ context.Context, _ ReplyRequest) (ReplyDecision, error) {
		return ReplyDecision{Decision: DecisionHold, Reply: "Still applies."}, nil
	})
	r.Background = func(task func()) { tasks = append(tasks, task) }
	r.InFlight = &ReplyInFlight{}
	r.OnOutcome = func(o ReplyOutcome, err error) { outcomes = append(outcomes, o) }
	r.LastScanned = map[string]time.Time{}
	r.PR = func(_ context.Context, _, _ string, _ int) (PRState, error) {
		return PRState{Open: true, AuthorID: 42, AuthorLogin: "pilot", HeadSHA: "head1", UpdatedAt: time.Date(2026, 9, 9, 17, 59, 0, 0, time.UTC)}, nil
	}

	rep, _ := r.Run(context.Background())
	if rep.Dispatched != 1 || len(tasks) != 1 || len(gh.posted) != 0 || len(gh.reactions) != 1 {
		t.Fatalf("first scan must react and dispatch: rep=%+v tasks=%d", rep, len(tasks))
	}
	rep, _ = r.Run(context.Background())
	if rep.Dispatched != 0 || len(tasks) != 1 || !r.LastScanned["acme/example#7"].IsZero() {
		t.Fatalf("an in-flight reply is not dispatched again and holds the watermark: rep=%+v tasks=%d", rep, len(tasks))
	}
	tasks[0]()
	if len(gh.posted) != 1 || len(outcomes) != 1 || !outcomes[0].Posted || ledger.rows[0].Outcome != "posted" {
		t.Fatalf("posted=%v outcomes=%+v row=%+v", gh.posted, outcomes, ledger.rows[0])
	}
	rep, _ = r.Run(context.Background())
	if rep.Dispatched != 0 || r.LastScanned["acme/example#7"].IsZero() {
		t.Fatalf("settled PR advances the watermark: rep=%+v", rep)
	}
}

func TestReplyReactor_ResumedDecisionIsDroppedWhenHeadOrThreadMoved(t *testing.T) {
	runs := 0
	r, gh, ledger := respondFixture(ReplyModeRespond, func(_ context.Context, _ ReplyRequest) (ReplyDecision, error) {
		runs++
		return ReplyDecision{Decision: DecisionHold, Reply: "Still applies."}, nil
	})
	t0 := time.Date(2026, 9, 9, 17, 59, 0, 0, time.UTC)
	ledger.rows = []db.PublishedReply{{RepoOwner: "acme", RepoName: "example", PRNumber: 7, RootCommentID: 100, AuthorCommentID: 101,
		Fingerprint: "a.go:1:abc", Class: "pushback", Action: "reacted", Decision: DecisionHold, ReplyBody: "Still applies.",
		DecisionHead: "head0", DecisionThread: "stale", CreatedAt: t0}}
	rep, _ := r.Run(context.Background())
	if runs != 0 || len(gh.posted) != 0 || rep.TextSkipped["head_moved"] != 1 || ledger.rows[0].Outcome != "skipped:head_moved" {
		t.Fatalf("runs=%d posted=%v rep=%+v row=%+v", runs, gh.posted, rep, ledger.rows[0])
	}
}

func TestReplyReactor_LiveModeIsReReadBeforePosting(t *testing.T) {
	live := ReplyModeRespond
	r, gh, ledger := respondFixture(ReplyModeRespond, func(_ context.Context, _ ReplyRequest) (ReplyDecision, error) {
		live = ReplyModeShadow
		return ReplyDecision{Decision: DecisionHold, Reply: "Still applies."}, nil
	})
	r.Live = func() (string, func(string) bool, error) { return live, nil, nil }
	rep, _ := r.Run(context.Background())
	if len(gh.posted) != 0 || rep.Shadowed != 1 || ledger.rows[0].Outcome != "shadowed" {
		t.Fatalf("a mode turned down mid-run must not post: posted=%v rep=%+v row=%+v", gh.posted, rep, ledger.rows[0])
	}
}

func TestReplyReactor_AnotherInstancesPostIsAdoptedAtTheFinalReRead(t *testing.T) {
	var gh *fakeReplyGH
	r, gh, ledger := respondFixture(ReplyModeRespond, func(_ context.Context, _ ReplyRequest) (ReplyDecision, error) {
		gh.threads["acme/example#7"] = append(gh.threads["acme/example#7"], ThreadComment{ID: 180, InReplyToID: 100, AuthorID: 1, Body: "Still applies.\n\n" + ReplyMarker(101), CreatedAt: time.Now()})
		return ReplyDecision{Decision: DecisionHold, Reply: "Still applies."}, nil
	})
	rep, _ := r.Run(context.Background())
	if len(gh.posted) != 0 || rep.Responded != 0 || ledger.rows[0].ReplyCommentID != 180 || ledger.rows[0].Outcome != "posted" {
		t.Fatalf("posted=%v rep=%+v row=%+v", gh.posted, rep, ledger.rows[0])
	}
}

func TestReplyReactor_ConcurrentSiblingsRespectTheThreadCap(t *testing.T) {
	var tasks []func()
	r, gh, ledger := respondFixture(ReplyModeRespond, func(_ context.Context, req ReplyRequest) (ReplyDecision, error) {
		return ReplyDecision{Decision: DecisionAnswer, Reply: fmt.Sprintf("Answer to %d.", req.Reply.CommentID)}, nil
	})
	gh.threads["acme/example#7"] = append(gh.threads["acme/example#7"], ThreadComment{ID: 102, InReplyToID: 100, AuthorID: 42, Body: "And is the retry path covered as well?", CreatedAt: time.Date(2026, 9, 9, 17, 59, 30, 0, time.UTC)})
	r.Text = TextPolicy{MaxPerThread: 1, MaxPerPRPerDay: 10, MaxAge: 24 * time.Hour, MaxChars: 600, MaxAttempts: 3}
	r.Background = func(task func()) { tasks = append(tasks, task) }
	r.InFlight = &ReplyInFlight{}
	var outcomes []ReplyOutcome
	var mu sync.Mutex
	r.OnOutcome = func(o ReplyOutcome, _ error) { mu.Lock(); outcomes = append(outcomes, o); mu.Unlock() }
	gh.postGate = make(chan struct{})

	rep, _ := r.Run(context.Background())
	if rep.Dispatched != 2 || len(tasks) != 2 {
		t.Fatalf("rep=%+v tasks=%d", rep, len(tasks))
	}
	var wg sync.WaitGroup
	for _, task := range tasks {
		wg.Add(1)
		go func(task func()) { defer wg.Done(); task() }(task)
	}
	<-gh.postGate // the first task is about to post; the second must be waiting on the PR lock, not racing
	gh.postGate <- struct{}{}
	wg.Wait()
	if len(gh.posted) != 1 {
		t.Fatalf("thread cap of 1 must hold across concurrent siblings, posted=%v", gh.posted)
	}
	got := map[string]int{}
	for _, o := range outcomes {
		got[o.Outcome]++
	}
	if got["posted"] != 1 || got["ineligible:thread_cap"] != 1 {
		t.Fatalf("outcomes=%v rows=%+v", got, ledger.rows)
	}
}

func TestReplyReactor_LiveReadErrorLeavesTheStepUnfinished(t *testing.T) {
	r, gh, ledger := respondFixture(ReplyModeRespond, func(_ context.Context, _ ReplyRequest) (ReplyDecision, error) {
		return ReplyDecision{Decision: DecisionHold, Reply: "Still applies."}, nil
	})
	r.Live = func() (string, func(string) bool, error) { return "", nil, fmt.Errorf("db down") }
	rep, _ := r.Run(context.Background())
	if len(gh.posted) != 0 || len(rep.Errors) != 1 || ledger.rows[0].Outcome != "" || ledger.rows[0].Decision != DecisionHold || ledger.rows[0].ClaimedBy != "" {
		t.Fatalf("a settings blip must not become a permanent skip: posted=%v rep=%+v row=%+v", gh.posted, rep, ledger.rows[0])
	}
}

func TestReplyReactor_ARowClaimedByAnotherInstanceIsLeftAlone(t *testing.T) {
	runs := 0
	r, gh, ledger := respondFixture(ReplyModeRespond, func(_ context.Context, _ ReplyRequest) (ReplyDecision, error) {
		runs++
		return ReplyDecision{Decision: DecisionHold, Reply: "Still applies."}, nil
	})
	t0 := time.Date(2026, 9, 9, 17, 59, 0, 0, time.UTC)
	claimed := t0.Add(-time.Minute)
	ledger.rows = []db.PublishedReply{{RepoOwner: "acme", RepoName: "example", PRNumber: 7, RootCommentID: 100, AuthorCommentID: 101,
		Fingerprint: "a.go:1:abc", Class: "pushback", Action: "reacted", CreatedAt: t0, ClaimedBy: "other", ClaimedAt: &claimed}}
	r.Holder = "me"
	rep, _ := r.Run(context.Background())
	if runs != 0 || len(gh.posted) != 0 || rep.TextSkipped["claimed_elsewhere"] != 1 || ledger.rows[0].ClaimedBy != "other" {
		t.Fatalf("runs=%d posted=%v rep=%+v row=%+v", runs, gh.posted, rep, ledger.rows[0])
	}
	r.Now = func() time.Time { return t0.Add(20 * time.Minute) }
	rep, _ = r.Run(context.Background())
	if runs != 1 || len(gh.posted) != 1 || ledger.rows[0].Outcome != "posted" {
		t.Fatalf("a stale claim is taken over: runs=%d posted=%v rep=%+v row=%+v", runs, gh.posted, rep, ledger.rows[0])
	}
}

func TestReplyReactor_AdoptionRunsEvenWhenTheReplyIsNoLongerEligible(t *testing.T) {
	runs := 0
	r, gh, ledger := respondFixture(ReplyModeRespond, func(_ context.Context, _ ReplyRequest) (ReplyDecision, error) {
		runs++
		return ReplyDecision{Decision: DecisionConcede, Reply: "Withdrawn."}, nil
	})
	old := time.Date(2026, 9, 7, 12, 0, 0, 0, time.UTC)
	gh.threads["acme/example#7"][1].CreatedAt = old
	gh.threads["acme/example#7"] = append(gh.threads["acme/example#7"], ThreadComment{ID: 190, InReplyToID: 100, AuthorID: 1, Body: "Withdrawn.\n\n" + ReplyMarker(101), CreatedAt: old.Add(time.Minute)})
	ledger.rows = []db.PublishedReply{{RepoOwner: "acme", RepoName: "example", PRNumber: 7, RootCommentID: 100, AuthorCommentID: 101,
		Fingerprint: "a.go:1:abc", Class: "pushback", Action: "reacted", Decision: DecisionConcede, ReplyBody: "Withdrawn.", CreatedAt: old}}
	r.Since = old.Add(-time.Hour)
	r.Run(context.Background())
	if runs != 0 || ledger.rows[0].ReplyCommentID != 190 || ledger.rows[0].Outcome != "posted" || ledger.states["a.go:1:abc"] != db.PublishedStateDismissed {
		t.Fatalf("a posted concession must be adopted and applied even after the reply aged out: row=%+v states=%v", ledger.rows[0], ledger.states)
	}
}

func TestReplyReactor_FailedOutcomeWriteReleasesTheClaim(t *testing.T) {
	r, _, ledger := respondFixture(ReplyModeShadow, func(_ context.Context, _ ReplyRequest) (ReplyDecision, error) {
		return ReplyDecision{Decision: DecisionHold, Reply: "Still applies."}, nil
	})
	ledger.failOutcomeOnce = true
	rep, _ := r.Run(context.Background())
	if len(rep.Errors) != 1 || ledger.rows[0].Outcome != "" || ledger.rows[0].ClaimedBy != "" {
		t.Fatalf("rep=%+v row=%+v", rep, ledger.rows[0])
	}
	rep, _ = r.Run(context.Background())
	if rep.Shadowed != 1 || ledger.rows[0].Outcome != "shadowed" {
		t.Fatalf("resume must finish without a second model run: rep=%+v row=%+v", rep, ledger.rows[0])
	}
}

func TestReplyReactor_CancelledRunIsNotCountedAsAnAttempt(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	r, _, ledger := respondFixture(ReplyModeRespond, func(_ context.Context, _ ReplyRequest) (ReplyDecision, error) {
		cancel()
		return ReplyDecision{}, context.Canceled
	})
	r.Run(ctx)
	if ledger.rows[0].Attempts != 0 || ledger.rows[0].Outcome != "" {
		t.Fatalf("row=%+v", ledger.rows[0])
	}
}
