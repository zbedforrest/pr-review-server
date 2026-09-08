package publisher

import (
	"context"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"

	"pr-review-server/db"
)

// ReplyClass is what an author's reply to one of our inline comments amounts
// to. Only questions and pushback ever earn a text response; everything else
// is acknowledged with a reaction.
type ReplyClass string

const (
	ReplyResolution ReplyClass = "resolution"
	ReplyQuestion   ReplyClass = "question"
	ReplyPushback   ReplyClass = "pushback"
	ReplyOther      ReplyClass = "other"
)

// ThreadComment is one review comment on a PR, as much of it as reply
// handling needs.
type ThreadComment struct {
	ID          int64
	InReplyToID int64
	AuthorID    int64
	Author      string
	Body        string
	CreatedAt   time.Time
}

// AuthorReply is a PR author's reply under one of our published findings.
type AuthorReply struct {
	CommentID     int64
	RootCommentID int64
	Fingerprint   string
	Body          string
	CreatedAt     time.Time
	Class         ReplyClass
}

var (
	resolutionOpeners = regexp.MustCompile(`^(fixed|done|addressed|resolved|updated|removed|handled|good catch|accepted for now)\b`)
	acknowledgements  = regexp.MustCompile(`^(ok(ay)?|thanks?( you)?|thx|ty|ack(nowledged)?|will fix|will do|noted|sure|yep|yes|sounds good|makes sense|got it)\b`)
	minPushbackRunes  = 20
)

// ClassifyReply uses anchored whole-message rules for the unambiguous cases
// and leaves everything else as pushback, which the model handles later.
// "Not fixed" and "Fixed?" deliberately fall through the resolution rule.
func ClassifyReply(body string) ReplyClass {
	text := strings.ToLower(strings.TrimSpace(body))
	if strings.Contains(text, "?") {
		return ReplyQuestion
	}
	if resolutionOpeners.MatchString(text) {
		return ReplyResolution
	}
	if acknowledgements.MatchString(text) || len([]rune(text)) < minPushbackRunes {
		return ReplyOther
	}
	return ReplyPushback
}

// FindAuthorReplies returns the PR author's replies under our root comments,
// oldest first, ignoring anything created before since. roots maps our
// comment ids (from the ledger) to finding fingerprints; markers alone are
// never trusted as ownership.
func FindAuthorReplies(comments []ThreadComment, roots map[int64]string, authorID int64, since time.Time) []AuthorReply {
	var out []AuthorReply
	for _, c := range comments {
		fp, ok := roots[c.InReplyToID]
		if !ok || c.AuthorID != authorID || c.CreatedAt.Before(since) {
			continue
		}
		out = append(out, AuthorReply{
			CommentID:     c.ID,
			RootCommentID: c.InReplyToID,
			Fingerprint:   fp,
			Body:          c.Body,
			CreatedAt:     c.CreatedAt,
			Class:         ClassifyReply(c.Body),
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.Before(out[j].CreatedAt) })
	return out
}

const (
	ReplyModeOff     = "off"
	ReplyModeObserve = "observe"
	ReplyModeReact   = "react"

	replyActionObserved = "observed"
	replyActionReacted  = "reacted"
)

// ReplyGitHub is the slice of GitHub the reactor needs.
type ReplyGitHub interface {
	ListThread(ctx context.Context, owner, repo string, number int) ([]ThreadComment, error)
	React(ctx context.Context, owner, repo string, commentID int64) error
}

// ReplyLedger persists what we did about each author reply.
type ReplyLedger interface {
	ListPublishedReplyTargets() ([]db.PublishedReplyTarget, error)
	GetPublishedReplyIDsForPR(owner, repo string, number int) (map[int64]bool, error)
	RecordPublishedReply(*db.PublishedReply) (bool, error)
}

// PRState is the live state of a PR that gates any reaction.
type PRState struct {
	Open        bool
	Draft       bool
	AuthorID    int64
	AuthorLogin string
}

// ReplyReactor acknowledges PR authors' replies under our inline comments.
// Mode observe records them; react also adds a 👍. Since is the activation
// cutoff so enabling the feature never answers historical threads.
type ReplyReactor struct {
	GH      ReplyGitHub
	Ledger  ReplyLedger
	PR      func(ctx context.Context, owner, repo string, number int) (PRState, error)
	Allowed func(authorLogin string) bool
	Mode    string
	Since   time.Time
	Targets []db.PublishedReplyTarget // optional pre-filtered subset; nil means all ledger targets
}

type ReplyReport struct {
	PRsScanned int
	Reacted    int
	Recorded   int
	Errors     []string
}

func (r ReplyReactor) Run(ctx context.Context) (ReplyReport, error) {
	var rep ReplyReport
	targets := r.Targets
	if targets == nil {
		var err error
		if targets, err = r.Ledger.ListPublishedReplyTargets(); err != nil {
			return rep, err
		}
	}
	for _, t := range targets {
		if err := r.scan(ctx, t, &rep); err != nil {
			rep.Errors = append(rep.Errors, fmt.Sprintf("%s/%s#%d: %v", t.RepoOwner, t.RepoName, t.PRNumber, err))
		}
	}
	return rep, nil
}

func (r ReplyReactor) scan(ctx context.Context, t db.PublishedReplyTarget, rep *ReplyReport) error {
	state, err := r.PR(ctx, t.RepoOwner, t.RepoName, t.PRNumber)
	if err != nil {
		return err
	}
	if !state.Open || state.Draft || !r.Allowed(state.AuthorLogin) {
		return nil
	}
	rep.PRsScanned++
	comments, err := r.GH.ListThread(ctx, t.RepoOwner, t.RepoName, t.PRNumber)
	if err != nil {
		return err
	}
	seen, err := r.Ledger.GetPublishedReplyIDsForPR(t.RepoOwner, t.RepoName, t.PRNumber)
	if err != nil {
		return err
	}
	for _, reply := range FindAuthorReplies(comments, t.Roots, state.AuthorID, r.Since) {
		if seen[reply.CommentID] {
			continue
		}
		action := replyActionObserved
		if r.Mode == ReplyModeReact {
			if err := r.GH.React(ctx, t.RepoOwner, t.RepoName, reply.CommentID); err != nil {
				return err
			}
			action = replyActionReacted
			rep.Reacted++
		}
		created, err := r.Ledger.RecordPublishedReply(&db.PublishedReply{
			RepoOwner: t.RepoOwner, RepoName: t.RepoName, PRNumber: t.PRNumber,
			RootCommentID: reply.RootCommentID, AuthorCommentID: reply.CommentID,
			Fingerprint: reply.Fingerprint, AuthorID: state.AuthorID,
			Class: string(reply.Class), Action: action, Body: reply.Body, CreatedAt: reply.CreatedAt,
		})
		if err != nil {
			return err
		}
		if created {
			rep.Recorded++
		}
	}
	return nil
}
