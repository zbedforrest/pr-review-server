package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"pr-review-server/internal/replaykit"
	"pr-review-server/pkg/publisher"
	"pr-review-server/pkg/reviewer/reconcile"
)

// flexInt64 reads a GraphQL databaseId whether the dump stored it as a number
// or as a string.
type flexInt64 int64

func (n *flexInt64) UnmarshalJSON(b []byte) error {
	s := strings.Trim(strings.TrimSpace(string(b)), `"`)
	if s == "" || s == "null" {
		*n = 0
		return nil
	}
	v, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return err
	}
	*n = flexInt64(v)
	return nil
}

type dumpActor struct {
	Login string `json:"login"`
}

type dumpReview struct {
	Author      *dumpActor `json:"author"`
	DatabaseID  flexInt64  `json:"databaseId"`
	State       string     `json:"state"`
	SubmittedAt time.Time  `json:"submittedAt"`
	Commit      struct {
		Oid string `json:"oid"`
	} `json:"commit"`
}

type dumpThreadComment struct {
	DatabaseID flexInt64  `json:"databaseId"`
	Author     *dumpActor `json:"author"`
	Body       string     `json:"body"`
	CreatedAt  time.Time  `json:"createdAt"`
	Path       string     `json:"path"`
	ReplyTo    *struct {
		DatabaseID flexInt64 `json:"databaseId"`
	} `json:"replyTo"`
	OriginalCommit struct {
		Oid string `json:"oid"`
	} `json:"originalCommit"`
}

// dumpIssueComment is one conversation comment on the PR; the bot's sticky
// summary is one of them.
type dumpIssueComment struct {
	DatabaseID flexInt64  `json:"databaseId"`
	Author     *dumpActor `json:"author"`
	Body       string     `json:"body"`
	CreatedAt  time.Time  `json:"createdAt"`
	UpdatedAt  time.Time  `json:"updatedAt"`
}

type dumpCommit struct {
	Commit struct {
		Oid string `json:"oid"`
	} `json:"commit"`
}

// dumpThread is one review thread. IsResolved is nil when the dump did not
// carry resolution state (a REST export before the GraphQL fill).
type dumpThread struct {
	ID           string `json:"id"`
	Path         string `json:"path"`
	Line         int    `json:"line"`
	OriginalLine int    `json:"originalLine"`
	IsResolved   *bool  `json:"isResolved"`
	IsOutdated   bool   `json:"isOutdated"`
	Comments     struct {
		Nodes []dumpThreadComment `json:"nodes"`
	} `json:"comments"`
}

func (t dumpThread) resolved() bool { return t.IsResolved != nil && *t.IsResolved }

// prDump is the subset of the GitHub GraphQL dump the replay reads.
type prDump struct {
	Number     int       `json:"number"`
	URL        string    `json:"url"`
	State      string    `json:"state"`
	HeadRefOid string    `json:"headRefOid"`
	Body       string    `json:"body"`
	Author     dumpActor `json:"author"`
	Reviews    struct {
		Nodes []dumpReview `json:"nodes"`
	} `json:"reviews"`
	ReviewThreads struct {
		Nodes []dumpThread `json:"nodes"`
	} `json:"reviewThreads"`
	// Comments are the PR's issue comments; the bot's sticky summary among
	// them names the last head a round reviewed.
	Comments struct {
		Nodes []dumpIssueComment `json:"nodes"`
	} `json:"comments"`
	Commits struct {
		Nodes []dumpCommit `json:"nodes"`
	} `json:"commits"`

	Owner string `json:"-"`
	Repo  string `json:"-"`
	// Path is the file the dump was loaded from.
	Path string `json:"-"`
}

var pullURLRe = regexp.MustCompile(`github\.com/([^/]+)/([^/]+)/pull/(\d+)`)

func loadDump(path string) (*prDump, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var d prDump
	if err := json.Unmarshal(raw, &d); err != nil {
		return nil, fmt.Errorf("%s: %w", filepath.Base(path), err)
	}
	m := pullURLRe.FindStringSubmatch(d.URL)
	if m == nil {
		return nil, fmt.Errorf("%s: no pull request url", filepath.Base(path))
	}
	d.Owner, d.Repo, d.Path = m[1], m[2], path
	if d.Number == 0 {
		d.Number, _ = strconv.Atoi(m[3])
	}
	return &d, nil
}

func loadDumps(dir string) ([]*prDump, error) {
	paths, err := filepath.Glob(filepath.Join(dir, "*.json"))
	if err != nil {
		return nil, err
	}
	if len(paths) == 0 {
		return nil, fmt.Errorf("no dumps found: %s has no *.json files", dir)
	}
	sort.Strings(paths)
	var out []*prDump
	for _, p := range paths {
		d, err := loadDump(p)
		if err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("no dumps loaded from %s", dir)
	}
	return out, nil
}

// key is the anonymised PR id used in every report: repo#number, no owner.
func (d *prDump) key() string { return fmt.Sprintf("%s#%d", d.Repo, d.Number) }

func bareLogin(login string) string {
	return strings.TrimSuffix(login, "[bot]")
}

func actorLogin(a *dumpActor) string {
	if a == nil {
		return ""
	}
	return a.Login
}

// botLogins returns the logins that posted a root comment carrying the PRism
// finding marker on this PR, or the sticky summary. A published round
// always writes one of the two, so the set identifies the bot without a
// configured name.
func (d *prDump) botLogins() map[string]bool {
	out := map[string]bool{}
	for _, t := range d.ReviewThreads.Nodes {
		if len(t.Comments.Nodes) == 0 {
			continue
		}
		root := t.Comments.Nodes[0]
		if root.ReplyTo != nil {
			continue
		}
		if _, ok := publisher.FindingIDFromBody(root.Body); ok {
			out[bareLogin(actorLogin(root.Author))] = true
		}
	}
	// A summary-only PR has no marked root; the sticky summary names the bot.
	for _, c := range d.Comments.Nodes {
		if strings.Contains(c.Body, publisher.SummaryMarker) {
			out[bareLogin(actorLogin(c.Author))] = true
		}
	}
	return out
}

type round struct {
	SHA  string
	SHA7 string
	At   time.Time
}

// rounds are the bot's review submissions in time order, plus the round
// the sticky summary records when no review carries its head (a round that
// posted nothing inline). The same commit may appear more than once: each
// submission was a publication round.
func (d *prDump) rounds(bots map[string]bool) []round {
	var out []round
	seen := map[string]bool{}
	for _, r := range d.Reviews.Nodes {
		if !bots[bareLogin(actorLogin(r.Author))] || len(r.Commit.Oid) < 7 || r.State == "PENDING" || r.SubmittedAt.IsZero() {
			continue
		}
		out = append(out, round{SHA: r.Commit.Oid, SHA7: r.Commit.Oid[:7], At: r.SubmittedAt})
		seen[r.Commit.Oid[:7]] = true
	}
	for _, r := range d.summaryRounds(bots) {
		if !seen[r.SHA7] {
			out = append(out, r)
			seen[r.SHA7] = true
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].At.Before(out[j].At) })
	return out
}

var (
	summaryReviewedRe = regexp.MustCompile(`reviewed ([0-9a-f]{7})`)
	summaryRoundsRe   = regexp.MustCompile(`Reviews \((\d+)\)`)
)

// summaryRounds reads the round the bot's sticky summary records: its
// footer names the reviewed head, and the comment's creation time is the
// round's time when it was the only round, its last edit otherwise (the
// edit rewrote it for the latest head; earlier summary-only heads are
// lost). The short sha is widened through the PR's commit list.
func (d *prDump) summaryRounds(bots map[string]bool) []round {
	var out []round
	for _, c := range d.Comments.Nodes {
		if !bots[bareLogin(actorLogin(c.Author))] || !strings.Contains(c.Body, publisher.SummaryMarker) {
			continue
		}
		m := summaryReviewedRe.FindStringSubmatch(c.Body)
		if m == nil {
			continue
		}
		at := c.CreatedAt
		if n := summaryRoundsRe.FindStringSubmatch(c.Body); n != nil && n[1] != "1" && !c.UpdatedAt.IsZero() {
			at = c.UpdatedAt
		}
		if at.IsZero() {
			continue
		}
		out = append(out, round{SHA: d.fullSHA(m[1]), SHA7: m[1], At: at})
	}
	return out
}

func (d *prDump) fullSHA(sha7 string) string {
	for _, c := range d.Commits.Nodes {
		if strings.HasPrefix(c.Commit.Oid, sha7) {
			return c.Commit.Oid
		}
	}
	if strings.HasPrefix(d.HeadRefOid, sha7) {
		return d.HeadRefOid
	}
	return sha7
}

// summaryNote is one bullet of a rendered summary that reached GitHub
// without an inline root: a folded note or a bullet linked to the file.
type summaryNote struct {
	File string
	Line int
	Text string
}

var summaryNoteRe = regexp.MustCompile(`(?m)^- (.*?) — \[` + "`" + `[^` + "`" + `]*` + "`" + `\]\(https://github\.com/[^/]+/[^/]+/blob/[^/]+/([^#)]+)#L(\d+)\)`)

// summaryNotes parses the bullets of a rendered summary body that link to
// the file rather than to a review comment: the points the round raised
// without a thread.
func summaryNotes(body string) []summaryNote {
	var out []summaryNote
	for _, m := range summaryNoteRe.FindAllStringSubmatch(body, -1) {
		line, _ := strconv.Atoi(m[3])
		out = append(out, summaryNote{File: m[2], Line: line, Text: stripMarkup(m[1])})
	}
	return out
}

// observedNotes are the bullets of the bot's newest sticky summary as
// GitHub holds it now, stamped with that summary's last edit.
func (d *prDump) observedNotes(bots map[string]bool) ([]summaryNote, time.Time) {
	var notes []summaryNote
	var newest time.Time
	for _, c := range d.Comments.Nodes {
		if !bots[bareLogin(actorLogin(c.Author))] || !strings.Contains(c.Body, publisher.SummaryMarker) {
			continue
		}
		at := c.UpdatedAt
		if at.IsZero() {
			at = c.CreatedAt
		}
		if at.Before(newest) {
			continue
		}
		notes, newest = summaryNotes(c.Body), at
	}
	return notes, newest
}

// observedRoot is a PRism root comment as GitHub recorded it, used for the
// historical counterpart of every replayed metric.
type observedRoot struct {
	FindingID string
	File      string
	Line      int
	Text      string
	At        time.Time
	Resolved  bool
	Commit    string
	// Replies are the bot's own replies in the thread, in time order.
	Replies []timedText
}

type timedText struct {
	At   time.Time
	Text string
}

// earlierReplies are the bot's replies in the thread of root id written
// before this root was posted: what PRism had said on that point by then.
func (r observedRoot) earlierReplies(id string, d *prDump, bots map[string]bool) []string {
	var out []string
	for _, o := range d.observedRoots(bots) {
		if o.FindingID != id {
			continue
		}
		for _, reply := range o.Replies {
			if reply.At.Before(r.At) {
				out = append(out, reply.Text)
			}
		}
	}
	return out
}

func (d *prDump) observedRoots(bots map[string]bool) []observedRoot {
	var out []observedRoot
	for _, t := range d.ReviewThreads.Nodes {
		if len(t.Comments.Nodes) == 0 {
			continue
		}
		root := t.Comments.Nodes[0]
		if root.ReplyTo != nil || !bots[bareLogin(actorLogin(root.Author))] {
			continue
		}
		id, ok := publisher.FindingIDFromBody(root.Body)
		if !ok {
			continue
		}
		line := t.OriginalLine
		if line == 0 {
			line = t.Line
		}
		o := observedRoot{FindingID: id, File: t.Path, Line: line, Text: stripMarkup(root.Body), At: root.CreatedAt, Resolved: t.resolved(), Commit: root.OriginalCommit.Oid}
		for _, c := range t.Comments.Nodes[1:] {
			if bots[bareLogin(actorLogin(c.Author))] && c.ReplyTo != nil {
				o.Replies = append(o.Replies, timedText{At: c.CreatedAt, Text: stripMarkup(c.Body)})
			}
		}
		out = append(out, o)
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].At.Before(out[j].At) })
	return out
}

// externalRoots lists every root comment on the PR by someone other than
// the bot, oldest first, for the external_duplicates metrics.
func (d *prDump) externalRoots(bots map[string]bool) []replaykit.ExternalRoot {
	var out []replaykit.ExternalRoot
	for _, t := range d.ReviewThreads.Nodes {
		if len(t.Comments.Nodes) == 0 {
			continue
		}
		root := t.Comments.Nodes[0]
		login := actorLogin(root.Author)
		if root.ReplyTo != nil || bots[bareLogin(login)] || t.Path == "" {
			continue
		}
		line := t.OriginalLine
		if line == 0 {
			line = t.Line
		}
		parsed := reconcile.ParseExternalComments([]reconcile.ExternalComment{{ID: int64(root.DatabaseID), Author: login, Body: root.Body, Path: t.Path, Line: line}}, nil)
		if len(parsed) == 0 {
			continue
		}
		out = append(out, replaykit.ExternalRoot{CommentID: int64(root.DatabaseID), Author: login, File: t.Path, Line: line, Text: strings.TrimSpace(parsed[0].Title + " " + parsed[0].Body), At: root.CreatedAt, Resolved: t.resolved(), ResolvedKnown: t.IsResolved != nil})
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].At.Before(out[j].At) })
	return out
}

var (
	htmlTagRe    = regexp.MustCompile(`<[^>]+>`)
	markdownLink = regexp.MustCompile(`\[([^\]]*)\]\([^)]*\)`)
)

// stripMarkup reduces a rendered comment body to its prose so observed roots
// can be compared the way raw agent text is.
func stripMarkup(body string) string {
	body = htmlTagRe.ReplaceAllString(body, " ")
	body = markdownLink.ReplaceAllString(body, "$1")
	return strings.Join(strings.Fields(body), " ")
}
