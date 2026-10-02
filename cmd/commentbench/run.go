package main

import (
	"context"
	"fmt"
	"hash/fnv"
	"sort"
	"strings"
	"time"

	"pr-review-server/db"
	"pr-review-server/internal/replaykit"
	"pr-review-server/pkg/publisher"
	"pr-review-server/pkg/reviewer/payload"
)

// replyLag is how long after an author reply the reply scan runs.
const replyLag = time.Minute

// Run is everything one case's replay did, for the scorer.
type Run struct {
	Case        *Case
	Rounds      []RoundRun
	Posts       []replaykit.Post
	Rec         *replaykit.Recorder
	Replies     map[int64]db.PublishedReply // by author comment id
	FinalRows   map[string]db.PublishedFinding
	RootFor     map[int64]int64 // historical root comment id -> replayed root comment id
	Synthetic   int             // stub decisions made without a recorded one
	Live        bool            // replies came from the live reply agent
	ReplyErrors []string
}

type RoundRun struct {
	Index    int
	Replayed bool
	Posts    []replaykit.Post
	Summary  string
	Flipped  []db.PublishedFinding // rows this round moved from open to resolved
	Previous []db.PublishedFinding
	After    []db.PublishedFinding
	HeadSHA  string
	PrevSHA  string
}

type event struct {
	at    time.Time
	round int   // -1 for a reply event
	reply int64 // author comment id
}

// replayCase runs one case: every round through the publisher, and after
// every human reply under a PRism root, one reply scan through the reactor.
func replayCase(ctx context.Context, c *Case, policy publisher.Policy, responder func(*Case, *Run) publisher.Responder) (*Run, error) {
	ledger, err := db.NewGormSQLite(":memory:")
	if err != nil {
		return nil, fmt.Errorf("open ledger: %w", err)
	}
	bots := c.bots()
	rp := replaykit.NewPRReplay(c.Owner, c.Repo, c.Number, c.Dump.Author.Login, ledger, policy)
	rp.Bot = c.Bot
	run := &Run{Case: c, Rec: rp.Rec, Replies: map[int64]db.PublishedReply{}, RootFor: map[int64]int64{}}
	rounds := c.Dump.Rounds(bots)

	var events []event
	for i, rd := range rounds {
		events = append(events, event{at: rd.At, round: i})
	}
	for _, hc := range humanReplies(c) {
		events = append(events, event{at: hc.CreatedAt.Add(replyLag), round: -1, reply: int64(hc.DatabaseID)})
	}
	sort.SliceStable(events, func(i, j int) bool { return events[i].at.Before(events[j].at) })

	var now time.Time
	rp.Rec.SetClock(func() time.Time { return now })
	gh := &threadView{c: c, run: run, now: func() time.Time { return now }}
	reactor := publisher.ReplyReactor{
		GH: gh, Ledger: ledger, Mode: publisher.ReplyModeRespond,
		PR:             gh.prState,
		Allowed:        func(string) bool { return true },
		Now:            func() time.Time { return now },
		Holder:         "commentbench",
		ResolveThreads: true,
	}
	reactor.Responder = responder(c, run)

	lastReplayed := ""
	for _, ev := range events {
		now = ev.at
		if ev.round < 0 {
			targets, err := replyTargets(ledger, c)
			if err != nil {
				return nil, err
			}
			if len(targets) == 0 {
				continue
			}
			reactor.Targets = targets
			rep, err := reactor.Run(ctx)
			if err != nil {
				return nil, fmt.Errorf("reply scan: %w", err)
			}
			run.ReplyErrors = append(run.ReplyErrors, rep.Errors...)
			continue
		}
		rd := rounds[ev.round]
		rr := RoundRun{Index: ev.round, HeadSHA: rd.SHA, PrevSHA: lastReplayed}
		raw, ok := c.Sidecars[rd.SHA7]
		if !ok {
			run.Rounds = append(run.Rounds, rr)
			continue
		}
		pl, err := payload.Decode(raw)
		if err != nil {
			run.Rounds = append(run.Rounds, rr)
			continue
		}
		in := replaykit.RoundInput{Index: ev.round, SHA: rd.SHA, At: rd.At, Payload: pl, Comments: c.Dump.ExternalCommentsBefore(bots, rd.At)}
		if lastReplayed != "" && lastReplayed != rd.SHA {
			in.ChangedFiles, in.ChangedKnown = c.Compares[compareKey(lastReplayed, rd.SHA)]
		} else if lastReplayed == rd.SHA {
			in.ChangedKnown = true
		}
		res, err := rp.PublishRound(ctx, in)
		if err != nil {
			return nil, err
		}
		rr.Replayed, rr.Posts, rr.Summary, rr.Previous, rr.After = true, res.Posts, res.Summary, res.Previous, res.After
		rr.Flipped = flipped(res.Previous, res.After)
		run.Rounds = append(run.Rounds, rr)
		run.Posts = append(run.Posts, res.Posts...)
		lastReplayed = rd.SHA
	}
	rows, err := ledger.ListPublishedRepliesForPR(c.Owner, c.Repo, c.Number)
	if err != nil {
		return nil, err
	}
	for _, r := range rows {
		run.Replies[r.AuthorCommentID] = r
	}
	final, err := ledger.GetPublishedFindingsForPR(c.Owner, c.Repo, c.Number)
	if err != nil {
		return nil, err
	}
	run.FinalRows = map[string]db.PublishedFinding{}
	for _, r := range final {
		if replaykit.IsFindingRow(r) {
			run.FinalRows[r.Fingerprint] = r
		}
	}
	gh.mapRoots()
	return run, nil
}

func flipped(previous, after []db.PublishedFinding) []db.PublishedFinding {
	wasOpen := map[string]bool{}
	for _, row := range previous {
		if replaykit.IsFindingRow(row) && row.State == db.PublishedStateOpen {
			wasOpen[row.Fingerprint] = true
		}
	}
	var out []db.PublishedFinding
	for _, row := range after {
		if wasOpen[row.Fingerprint] && replaykit.IsFindingRow(row) && (row.State == db.PublishedStateResolved || row.State == db.PublishedStateFixed) {
			out = append(out, row)
		}
	}
	return out
}

// replyTargets mirrors ListPublishedReplyTargets for the one PR under replay:
// every inline finding row with a comment id.
func replyTargets(ledger *db.GormDB, c *Case) ([]db.PublishedReplyTarget, error) {
	rows, err := ledger.GetPublishedFindingsForPR(c.Owner, c.Repo, c.Number)
	if err != nil {
		return nil, err
	}
	t := db.PublishedReplyTarget{RepoOwner: c.Owner, RepoName: c.Repo, PRNumber: c.Number, Roots: map[int64]string{}}
	for _, r := range rows {
		if r.Kind == db.PublishedKindFinding && r.CommentID != 0 {
			t.Roots[r.CommentID] = r.Fingerprint
		}
	}
	if len(t.Roots) == 0 {
		return nil, nil
	}
	return []db.PublishedReplyTarget{t}, nil
}

// humanReplies are the non-bot comments in threads whose root is a PRism
// finding.
func humanReplies(c *Case) []replaykit.ThreadComment {
	bots := c.bots()
	var out []replaykit.ThreadComment
	for _, t := range c.Dump.ReviewThreads.Nodes {
		if len(t.Comments.Nodes) < 2 || !isPrismRoot(t.Comments.Nodes[0], bots) {
			continue
		}
		for _, cm := range t.Comments.Nodes[1:] {
			if !bots[replaykit.BareLogin(replaykit.ActorLogin(cm.Author))] {
				out = append(out, cm)
			}
		}
	}
	return out
}

func isPrismRoot(cm replaykit.ThreadComment, bots map[string]bool) bool {
	if cm.ReplyTo != nil || !bots[replaykit.BareLogin(replaykit.ActorLogin(cm.Author))] {
		return false
	}
	_, ok := publisher.FindingIDFromBody(cm.Body)
	return ok
}

func userID(login string) int64 {
	h := fnv.New64a()
	_, _ = h.Write([]byte(strings.ToLower(replaykit.BareLogin(login))))
	return int64(h.Sum64() >> 2)
}

// threadView is the reply side of the fake GitHub: the replay's own roots and
// replies plus every human comment written by now, with replies to a
// historical PRism root re-pointed at the root the replay posted for the
// same defect.
type threadView struct {
	c   *Case
	run *Run
	now func() time.Time
}

func (v *threadView) ListThread(_ context.Context, _, _ string, _ int) ([]publisher.ThreadComment, error) {
	v.mapRoots()
	now := v.now()
	bot := userID(v.c.Bot)
	var out []publisher.ThreadComment
	for _, p := range v.run.Rec.Posts {
		out = append(out, publisher.ThreadComment{ID: p.CommentID, ReviewID: 1, AuthorID: bot, Author: v.c.Bot, Body: p.Body})
	}
	for _, r := range v.run.Rec.Replies {
		out = append(out, publisher.ThreadComment{ID: r.CommentID, InReplyToID: r.RootCommentID, AuthorID: bot, Author: v.c.Bot, Body: r.Body, CreatedAt: r.At})
	}
	bots := v.c.bots()
	for _, t := range v.c.Dump.ReviewThreads.Nodes {
		if len(t.Comments.Nodes) == 0 {
			continue
		}
		rootID := int64(t.Comments.Nodes[0].DatabaseID)
		replayRoot, mapped := v.run.RootFor[rootID]
		for _, cm := range t.Comments.Nodes[1:] {
			login := replaykit.ActorLogin(cm.Author)
			if bots[replaykit.BareLogin(login)] || cm.CreatedAt.After(now) {
				continue
			}
			tc := publisher.ThreadComment{ID: int64(cm.DatabaseID), AuthorID: userID(login), Author: login, Body: cm.Body, CreatedAt: cm.CreatedAt, InReplyToID: rootID}
			if mapped {
				tc.InReplyToID = replayRoot
			}
			out = append(out, tc)
		}
	}
	return out, nil
}

func (v *threadView) React(ctx context.Context, owner, repo string, commentID int64) error {
	return v.run.Rec.React(ctx, owner, repo, commentID)
}

func (v *threadView) PostReply(ctx context.Context, owner, repo string, number int, rootCommentID int64, body string) (int64, error) {
	return v.run.Rec.PostReply(ctx, owner, repo, number, rootCommentID, body)
}

// The reactor resolves a conceded finding's thread through the recorder, so
// the resolution is scored with the publisher's own.
func (v *threadView) ListReviewThreads(ctx context.Context, owner, repo string, number int) ([]publisher.ReviewThread, error) {
	return v.run.Rec.ListReviewThreads(ctx, owner, repo, number)
}

func (v *threadView) ResolveThread(ctx context.Context, owner, repo, nodeID string) error {
	return v.run.Rec.ResolveThread(ctx, owner, repo, nodeID)
}

func (v *threadView) UnresolveThread(ctx context.Context, owner, repo, nodeID string) error {
	return v.run.Rec.UnresolveThread(ctx, owner, repo, nodeID)
}

func (v *threadView) prState(context.Context, string, string, int) (publisher.PRState, error) {
	now := v.now()
	head := ""
	for _, cm := range v.c.Dump.Commits.Nodes {
		if !cm.Commit.CommittedDate.After(now) {
			head = cm.Commit.Oid
		}
	}
	if head == "" {
		for _, rd := range v.c.Dump.Rounds(v.c.bots()) {
			if !rd.At.After(now) {
				head = rd.SHA
			}
		}
	}
	return publisher.PRState{Open: true, AuthorID: userID(v.c.Dump.Author.Login), AuthorLogin: v.c.Dump.Author.Login, UpdatedAt: now, HeadSHA: head, BaseRef: "main"}, nil
}

// mapRoots points each historical PRism root at the replayed root for the
// same defect: the post matched to its expectation, else the latest post of
// the same defect, else the latest post with the same marker or alias.
func (v *threadView) mapRoots() {
	m := matchPosts(v.c, v.run.Posts)
	byDefect := map[int64]int64{}
	for _, p := range v.run.Posts {
		if fe, ok := m.byPost[p.CommentID]; ok {
			byDefect[fe.Defect] = p.CommentID
		}
	}
	for _, t := range v.c.Dump.ReviewThreads.Nodes {
		if len(t.Comments.Nodes) == 0 || !isPrismRoot(t.Comments.Nodes[0], v.c.bots()) {
			continue
		}
		root := t.Comments.Nodes[0]
		id := int64(root.DatabaseID)
		if fe := findingExpect(v.c, id); fe != nil {
			if pid, ok := m.byExpect[id]; ok {
				v.run.RootFor[id] = pid
				continue
			}
			if pid, ok := byDefect[fe.Defect]; ok {
				v.run.RootFor[id] = pid
				continue
			}
		}
		marker, _ := publisher.FindingIDFromBody(root.Body)
		text := replaykit.StripMarkup(root.Body)
		line := t.OriginalLine
		if line == 0 {
			line = t.Line
		}
		for i := len(v.run.Posts) - 1; i >= 0; i-- {
			p := v.run.Posts[i]
			if p.FindingID == marker || replaykit.SameDefect(p.File, p.Line, p.RawText, nil, t.Path, line, text, nil) {
				v.run.RootFor[id] = p.CommentID
				break
			}
		}
	}
}

func findingExpect(c *Case, commentID int64) *FindingExpect {
	for i := range c.Expect.Findings {
		if c.Expect.Findings[i].CommentID == commentID {
			return &c.Expect.Findings[i]
		}
	}
	return nil
}

// postMatch pairs replayed posts with finding expectations, one to one,
// within the round the expectation belongs to: same finding id first, then
// the alias rule on the sidecar text.
type postMatch struct {
	byPost   map[int64]*FindingExpect // replayed comment id -> expectation
	byExpect map[int64]int64          // historical comment id -> replayed comment id
}

func matchPosts(c *Case, posts []replaykit.Post) postMatch {
	m := postMatch{byPost: map[int64]*FindingExpect{}, byExpect: map[int64]int64{}}
	byRound := map[int][]replaykit.Post{}
	for _, p := range posts {
		byRound[p.Round] = append(byRound[p.Round], p)
	}
	pass := func(earlier bool, match func(p replaykit.Post, fe *FindingExpect) bool) {
		for i := range c.Expect.Findings {
			fe := &c.Expect.Findings[i]
			if !fe.mapped() || (earlier && fe.Expect != ExpectPost) {
				continue
			}
			from := fe.Round
			if earlier {
				from = 0
			}
			for r := fe.Round; r >= from; r-- {
				if _, done := m.byExpect[fe.CommentID]; done {
					break
				}
				for _, p := range byRound[r] {
					if _, taken := m.byPost[p.CommentID]; taken {
						continue
					}
					if match(p, fe) {
						m.byPost[p.CommentID] = fe
						m.byExpect[fe.CommentID] = p.CommentID
						break
					}
				}
			}
		}
	}
	sameID := func(p replaykit.Post, fe *FindingExpect) bool { return p.FindingID == fe.FindingID }
	alias := func(p replaykit.Post, fe *FindingExpect) bool {
		return replaykit.SameDefect(p.File, p.Line, p.RawText, p.Subjects, fe.File, fe.Line, fe.Text, fe.Subjects)
	}
	pass(false, sameID)
	pass(false, alias)
	// A finding the replay raised in an earlier round than history did still
	// counts as posted.
	pass(true, sameID)
	pass(true, alias)
	return m
}
