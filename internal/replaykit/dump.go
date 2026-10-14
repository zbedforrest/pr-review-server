package replaykit

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

	"pr-review-server/pkg/publisher"
	"pr-review-server/pkg/reviewer/reconcile"
)

// FlexInt64 reads a GraphQL databaseId whether the dump stored it as a number
// or as a string.
type FlexInt64 int64

func (n *FlexInt64) UnmarshalJSON(b []byte) error {
	s := strings.Trim(strings.TrimSpace(string(b)), `"`)
	if s == "" || s == "null" {
		*n = 0
		return nil
	}
	v, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return err
	}
	*n = FlexInt64(v)
	return nil
}

type Actor struct {
	Login string `json:"login"`
}

type Review struct {
	Author      *Actor    `json:"author"`
	DatabaseID  FlexInt64 `json:"databaseId"`
	State       string    `json:"state"`
	SubmittedAt time.Time `json:"submittedAt"`
	Commit      struct {
		Oid string `json:"oid"`
	} `json:"commit"`
}

type ThreadComment struct {
	DatabaseID FlexInt64 `json:"databaseId"`
	Author     *Actor    `json:"author"`
	Body       string    `json:"body"`
	CreatedAt  time.Time `json:"createdAt"`
	Path       string    `json:"path"`
	ReplyTo    *struct {
		DatabaseID FlexInt64 `json:"databaseId"`
	} `json:"replyTo"`
	OriginalCommit struct {
		Oid string `json:"oid"`
	} `json:"originalCommit"`
}

// Commit is one entry of the PR's commit list.
type Commit struct {
	Commit struct {
		Oid             string    `json:"oid"`
		CommittedDate   time.Time `json:"committedDate"`
		MessageHeadline string    `json:"messageHeadline"`
	} `json:"commit"`
}

// IssueComment is one PR conversation comment.
type IssueComment struct {
	DatabaseID FlexInt64 `json:"databaseId"`
	Author     *Actor    `json:"author"`
	Body       string    `json:"body"`
	CreatedAt  time.Time `json:"createdAt"`
	UpdatedAt  time.Time `json:"updatedAt"`
}

// Thread is one review thread. IsResolved is nil when the dump did not
// carry resolution state (a REST export before the GraphQL fill).
type Thread struct {
	ID           string `json:"id"`
	Path         string `json:"path"`
	Line         int    `json:"line"`
	OriginalLine int    `json:"originalLine"`
	IsResolved   *bool  `json:"isResolved"`
	IsOutdated   bool   `json:"isOutdated"`
	Comments     struct {
		Nodes []ThreadComment `json:"nodes"`
	} `json:"comments"`
}

// Resolved reports whether the thread is resolved; unknown reads as open.
func (t Thread) Resolved() bool { return t.IsResolved != nil && *t.IsResolved }

// PRDump is the subset of the GitHub GraphQL dump the replay reads.
type PRDump struct {
	Number     int    `json:"number"`
	URL        string `json:"url"`
	State      string `json:"state"`
	Merged     bool   `json:"merged"`
	HeadRefOid string `json:"headRefOid"`
	Author     Actor  `json:"author"`
	// Source is "rest" for a dump rebuilt from the REST API, whose thread
	// resolution flags are placeholders until ResolutionFilled is true.
	Source           string          `json:"source"`
	ResolutionFilled json.RawMessage `json:"resolution_filled"`
	Reviews          struct {
		Nodes []Review `json:"nodes"`
	} `json:"reviews"`
	ReviewThreads struct {
		Nodes []Thread `json:"nodes"`
	} `json:"reviewThreads"`
	Commits struct {
		Nodes []Commit `json:"nodes"`
	} `json:"commits"`
	// Comments are the PR's issue comments; the bot's sticky summary among
	// them names the last head a round reviewed.
	Comments struct {
		Nodes []IssueComment `json:"nodes"`
	} `json:"comments"`

	Owner string `json:"-"`
	Repo  string `json:"-"`
}

var pullURLRe = regexp.MustCompile(`github\.com/([^/]+)/([^/]+)/pull/(\d+)`)

func LoadDump(path string) (*PRDump, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var d PRDump
	if err := json.Unmarshal(raw, &d); err != nil {
		return nil, fmt.Errorf("%s: %w", filepath.Base(path), err)
	}
	m := pullURLRe.FindStringSubmatch(d.URL)
	if m == nil {
		return nil, fmt.Errorf("%s: no pull request url", filepath.Base(path))
	}
	d.Owner, d.Repo = m[1], m[2]
	if d.Number == 0 {
		d.Number, _ = strconv.Atoi(m[3])
	}
	return &d, nil
}

func LoadDumps(dir string) ([]*PRDump, error) {
	paths, err := filepath.Glob(filepath.Join(dir, "*.json"))
	if err != nil {
		return nil, err
	}
	if len(paths) == 0 {
		return nil, fmt.Errorf("no dumps found: %s has no *.json files", dir)
	}
	sort.Strings(paths)
	var out []*PRDump
	for _, p := range paths {
		d, err := LoadDump(p)
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
func (d *PRDump) Key() string { return fmt.Sprintf("%s#%d", d.Repo, d.Number) }

// ResolutionKnown reports whether the dump's IsResolved flags are real: a
// GraphQL dump always carries them, a REST dump only after the fill pass.
func (d *PRDump) ResolutionKnown() bool {
	return d.Source != "rest" || strings.TrimSpace(string(d.ResolutionFilled)) == "true"
}

func BareLogin(login string) string {
	return strings.TrimSuffix(login, "[bot]")
}

func ActorLogin(a *Actor) string {
	if a == nil {
		return ""
	}
	return a.Login
}

// botLogins returns the logins that posted a root comment carrying the PRism
// finding marker on this PR, or its sticky summary, so the set identifies
// the bot without a configured name.
func (d *PRDump) BotLogins() map[string]bool {
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
			out[BareLogin(ActorLogin(root.Author))] = true
		}
	}
	// A summary-only PR has no marked root; the sticky summary names the bot.
	for _, c := range d.Comments.Nodes {
		if strings.Contains(c.Body, publisher.SummaryMarker) {
			out[BareLogin(ActorLogin(c.Author))] = true
		}
	}
	return out
}

// Round is one bot review submission: a publication round.
type Round struct {
	SHA  string
	SHA7 string
	At   time.Time
}

// Rounds are the bot's review submissions in time order. The same commit
// may appear more than once: each submission was a publication round.
func (d *PRDump) Rounds(bots map[string]bool) []Round {
	var out []Round
	for _, r := range d.Reviews.Nodes {
		if !bots[BareLogin(ActorLogin(r.Author))] || len(r.Commit.Oid) < 7 || r.State == "PENDING" || r.SubmittedAt.IsZero() {
			continue
		}
		out = append(out, Round{SHA: r.Commit.Oid, SHA7: r.Commit.Oid[:7], At: r.SubmittedAt})
	}
	// A round that posted no inline comment left no review submission; the
	// sticky summary's footer still names the head it reviewed, so that head
	// is a round too when no submission covers it.
	if sha, at, ok := d.summaryRound(bots); ok {
		covered := false
		for _, r := range out {
			if strings.HasPrefix(r.SHA, sha) {
				covered = true
			}
		}
		if !covered {
			out = append(out, Round{SHA: sha, SHA7: sha, At: at})
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].At.Before(out[j].At) })
	return out
}

// summaryFooterRe reads the head out of the sticky summary's footer.
var summaryFooterRe = regexp.MustCompile(`reviewed ([0-9a-f]{7,40})`)

// summaryRound is the head the bot's sticky summary says it reviewed and
// when that summary was last written.
func (d *PRDump) summaryRound(bots map[string]bool) (string, time.Time, bool) {
	// Comments come oldest first; the newest sticky summary names the head
	// the last round reviewed.
	sha, at, found := "", time.Time{}, false
	for _, c := range d.Comments.Nodes {
		if !bots[BareLogin(ActorLogin(c.Author))] || !strings.Contains(c.Body, publisher.SummaryMarker) {
			continue
		}
		m := summaryFooterRe.FindStringSubmatch(c.Body)
		if m == nil {
			continue
		}
		when := c.UpdatedAt
		if when.IsZero() {
			when = c.CreatedAt
		}
		if !found || !when.Before(at) {
			sha, at, found = m[1][:7], when, true
		}
	}
	return sha, at, found
}

// ObservedRoot is a PRism root comment as GitHub recorded it, used for the
// historical counterpart of every replayed metric.
type ObservedRoot struct {
	FindingID string
	File      string
	Line      int
	Text      string
	At        time.Time
	Resolved  bool
}

func (d *PRDump) ObservedRoots(bots map[string]bool) []ObservedRoot {
	var out []ObservedRoot
	for _, t := range d.ReviewThreads.Nodes {
		if len(t.Comments.Nodes) == 0 {
			continue
		}
		root := t.Comments.Nodes[0]
		if root.ReplyTo != nil || !bots[BareLogin(ActorLogin(root.Author))] {
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
		out = append(out, ObservedRoot{FindingID: id, File: t.Path, Line: line, Text: StripMarkup(root.Body), At: root.CreatedAt, Resolved: t.Resolved()})
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].At.Before(out[j].At) })
	return out
}

// ExternalRoot is a root comment on the PR that PRism did not post, with
// the thread's resolution state as the dump records it.
type ExternalRoot struct {
	CommentID     int64
	Author        string
	File          string
	Line          int
	Text          string
	At            time.Time
	Resolved      bool
	ResolvedKnown bool
}

// ExternalRoots lists every root comment on the PR by someone other than
// the bot, with the thread's original line and the prose the reconciler's
// parser reads out of the body, oldest first.
func (d *PRDump) ExternalRoots(bots map[string]bool) []ExternalRoot {
	var out []ExternalRoot
	for _, t := range d.ReviewThreads.Nodes {
		if len(t.Comments.Nodes) == 0 {
			continue
		}
		root := t.Comments.Nodes[0]
		login := ActorLogin(root.Author)
		if root.ReplyTo != nil || bots[BareLogin(login)] || t.Path == "" {
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
		out = append(out, ExternalRoot{CommentID: int64(root.DatabaseID), Author: login, File: t.Path, Line: line, Text: strings.TrimSpace(parsed[0].Title + " " + parsed[0].Body), At: root.CreatedAt, Resolved: t.Resolved(), ResolvedKnown: t.IsResolved != nil})
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].At.Before(out[j].At) })
	return out
}

var (
	htmlTagRe    = regexp.MustCompile(`<[^>]+>`)
	markdownLink = regexp.MustCompile(`\[([^\]]*)\]\([^)]*\)`)
)

// StripMarkup reduces a rendered comment body to its prose so observed roots
// can be compared the way raw agent text is.
func StripMarkup(body string) string {
	body = htmlTagRe.ReplaceAllString(body, " ")
	body = markdownLink.ReplaceAllString(body, "$1")
	return strings.Join(strings.Fields(body), " ")
}
