package feedback

import (
	"context"
	"fmt"
	"log"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	"pr-review-server/db"
)

// Item is one piece of feedback before or after classification.
type Item struct {
	Source     string
	ItemID     int64
	CommentID  int64
	RepoOwner  string
	RepoName   string
	PRNumber   int
	Author     string
	Body       string
	ReplyClass string
	Reaction   string
	Label      Label
	Classifier string
	URL        string
	CreatedAt  time.Time
}

// Comment is a PR comment as the scan sees it: inline (review) or
// conversation (issue).
type Comment struct {
	ID          int64
	InReplyToID int64
	Author      string
	IsBot       bool
	Body        string
	CreatedAt   time.Time
	Reactions   int
}

type Reaction struct {
	ID        int64
	User      string
	IsBot     bool
	Content   string
	CreatedAt time.Time
}

// GitHub is the read-only slice of the API the scan uses.
type GitHub interface {
	ListReviewComments(ctx context.Context, owner, repo string, number int) ([]Comment, error)
	ListIssueComments(ctx context.Context, owner, repo string, number int) ([]Comment, error)
	ListReviewCommentReactions(ctx context.Context, owner, repo string, commentID int64) ([]Reaction, error)
	ListIssueCommentReactions(ctx context.Context, owner, repo string, commentID int64) ([]Reaction, error)
}

type Store interface {
	ListFeedbackTargets(recentSince time.Time) ([]db.FeedbackTarget, error)
	KnownFeedbackItems(owner, repo string, number int) (map[db.FeedbackKey]bool, error)
	SaveFeedbackItems([]db.FeedbackItem) (int, error)
}

// Scanner finds feedback newer than Lookback on the PRs the ledger knows,
// classifies what it has not stored yet and stores it. It never writes to
// GitHub.
type Scanner struct {
	Store      Store
	GitHub     GitHub
	Classifier Classifier
	// Handle is the bot login authors mention; conversation comments count
	// when they name it (or say "prism").
	Handle   string
	Lookback time.Duration
	// MaxTargets bounds GitHub calls per run; MaxModelCalls bounds classifier
	// spend, the rest of a run falls back to the lexicon.
	MaxTargets    int
	MaxModelCalls int
	Now           func() time.Time
}

type Result struct {
	Targets int
	Seen    int
	Added   []Item
	Errors  int
}

const (
	defaultLookback      = 7 * 24 * time.Hour
	defaultMaxTargets    = 200
	defaultMaxModelCalls = 150
)

func (s Scanner) Run(ctx context.Context) (Result, error) {
	now := time.Now().UTC()
	if s.Now != nil {
		now = s.Now()
	}
	lookback, maxTargets, maxModel := s.Lookback, s.MaxTargets, s.MaxModelCalls
	if lookback <= 0 {
		lookback = defaultLookback
	}
	if maxTargets <= 0 {
		maxTargets = defaultMaxTargets
	}
	if maxModel <= 0 {
		maxModel = defaultMaxModelCalls
	}
	since := now.Add(-lookback)
	classifier := s.Classifier
	if classifier == nil {
		classifier = LexiconClassifier{}
	}

	targets, err := s.Store.ListFeedbackTargets(since)
	if err != nil {
		return Result{}, err
	}
	if len(targets) > maxTargets {
		log.Printf("[FEEDBACK] scanning the %d most recently commented PRs of %d", maxTargets, len(targets))
		targets = targets[:maxTargets]
	}
	res := Result{Targets: len(targets)}
	modelCalls := 0
	for _, t := range targets {
		if ctx.Err() != nil {
			return res, ctx.Err()
		}
		items, err := s.collect(ctx, t, since)
		if err != nil {
			log.Printf("[FEEDBACK] %s/%s#%d: %v", t.RepoOwner, t.RepoName, t.PRNumber, err)
			res.Errors++
			continue
		}
		res.Seen += len(items)
		known, err := s.Store.KnownFeedbackItems(t.RepoOwner, t.RepoName, t.PRNumber)
		if err != nil {
			return res, err
		}
		var fresh []db.FeedbackItem
		var added []Item
		for _, it := range items {
			if known[db.FeedbackKey{Source: it.Source, ItemID: it.ItemID}] {
				continue
			}
			var c Classifier = LexiconClassifier{}
			if it.Source != SourceReaction && modelCalls < maxModel {
				c, modelCalls = classifier, modelCalls+1
			}
			it.Label, it.Classifier = c.Classify(ctx, it)
			fresh = append(fresh, toRow(it, now))
			added = append(added, it)
		}
		if len(fresh) == 0 {
			continue
		}
		// Only the rows this run inserted count as new; a concurrent run may
		// have stored some of them first.
		inserted, err := s.Store.SaveFeedbackItems(fresh)
		if err != nil {
			return res, err
		}
		if inserted == len(fresh) {
			res.Added = append(res.Added, added...)
		} else if inserted > 0 {
			nowKnown, err := s.Store.KnownFeedbackItems(t.RepoOwner, t.RepoName, t.PRNumber)
			if err != nil {
				return res, err
			}
			for _, it := range added {
				if nowKnown[db.FeedbackKey{Source: it.Source, ItemID: it.ItemID}] && !known[db.FeedbackKey{Source: it.Source, ItemID: it.ItemID}] {
					res.Added = append(res.Added, it)
				}
			}
		}
	}
	return res, nil
}

func (s Scanner) collect(ctx context.Context, t db.FeedbackTarget, since time.Time) ([]Item, error) {
	var items []Item
	base := Item{RepoOwner: t.RepoOwner, RepoName: t.RepoName, PRNumber: t.PRNumber}
	inline, err := s.GitHub.ListReviewComments(ctx, t.RepoOwner, t.RepoName, t.PRNumber)
	if err != nil {
		return nil, err
	}
	for _, c := range inline {
		if t.Roots[c.ID] && c.Reactions > 0 {
			reactions, err := s.GitHub.ListReviewCommentReactions(ctx, t.RepoOwner, t.RepoName, c.ID)
			if err != nil {
				return nil, err
			}
			items = append(items, reactionItems(base, c.ID, reactions, since, discussionURL(t, c.ID))...)
		}
		if !t.Roots[c.InReplyToID] || isBot(c) || c.CreatedAt.Before(since) {
			continue
		}
		it := base
		it.Source, it.ItemID, it.CommentID = SourceReply, c.ID, c.InReplyToID
		it.Author, it.Body, it.CreatedAt = c.Author, c.Body, c.CreatedAt
		it.ReplyClass, it.URL = t.ReplyClasses[c.ID], discussionURL(t, c.ID)
		items = append(items, it)
	}
	conversation, err := s.GitHub.ListIssueComments(ctx, t.RepoOwner, t.RepoName, t.PRNumber)
	if err != nil {
		return nil, err
	}
	for _, c := range conversation {
		if t.Summaries[c.ID] && c.Reactions > 0 {
			reactions, err := s.GitHub.ListIssueCommentReactions(ctx, t.RepoOwner, t.RepoName, c.ID)
			if err != nil {
				return nil, err
			}
			items = append(items, reactionItems(base, c.ID, reactions, since, issueCommentURL(t, c.ID))...)
		}
		if isBot(c) || c.CreatedAt.Before(since) || !s.namesBot(c.Body) {
			continue
		}
		it := base
		it.Source, it.ItemID, it.CommentID = SourceComment, c.ID, c.ID
		it.Author, it.Body, it.CreatedAt, it.URL = c.Author, c.Body, c.CreatedAt, issueCommentURL(t, c.ID)
		items = append(items, it)
	}
	return items, nil
}

func reactionItems(base Item, commentID int64, reactions []Reaction, since time.Time, url string) []Item {
	var items []Item
	for _, r := range reactions {
		if r.IsBot || strings.HasSuffix(r.User, "[bot]") || r.CreatedAt.Before(since) {
			continue
		}
		it := base
		it.Source, it.ItemID, it.CommentID = SourceReaction, r.ID, commentID
		it.Author, it.Reaction, it.Body, it.CreatedAt, it.URL = r.User, r.Content, r.Content, r.CreatedAt, url
		items = append(items, it)
	}
	return items
}

// prismWordRe matches the product name as a word, not prism.js or a
// hyphenated identifier.
var prismWordRe = regexp.MustCompile(`(?i)(?:^|[^\w.\-])prism(?:$|[^\w.\-]|\.(?:\s|$))`)

func (s Scanner) namesBot(body string) bool {
	if s.Handle != "" && strings.Contains(strings.ToLower(body), "@"+strings.ToLower(s.Handle)) {
		return true
	}
	return prismWordRe.MatchString(body)
}

func isBot(c Comment) bool {
	return c.IsBot || strings.HasSuffix(c.Author, "[bot]")
}

func discussionURL(t db.FeedbackTarget, id int64) string {
	return fmt.Sprintf("https://github.com/%s/%s/pull/%d#discussion_r%d", t.RepoOwner, t.RepoName, t.PRNumber, id)
}

func issueCommentURL(t db.FeedbackTarget, id int64) string {
	return fmt.Sprintf("https://github.com/%s/%s/pull/%d#issuecomment-%d", t.RepoOwner, t.RepoName, t.PRNumber, id)
}

const maxStoredBody = 4000

func toRow(it Item, observed time.Time) db.FeedbackItem {
	return db.FeedbackItem{
		Source: it.Source, ItemID: it.ItemID, CommentID: it.CommentID,
		RepoOwner: it.RepoOwner, RepoName: it.RepoName, PRNumber: it.PRNumber,
		Author: it.Author, Body: truncateBytes(it.Body, maxStoredBody), ReplyClass: it.ReplyClass, Reaction: it.Reaction,
		Label: string(it.Label), Classifier: it.Classifier, URL: it.URL,
		CreatedAt: it.CreatedAt.UTC(), ObservedAt: observed.UTC(),
	}
}

// truncateBytes cuts at most max bytes without splitting a UTF-8 sequence.
func truncateBytes(s string, max int) string {
	if len(s) <= max {
		return s
	}
	cut := max
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut]
}
