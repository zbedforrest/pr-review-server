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
}

// Commit is one entry of the PR's commit list.
type Commit struct {
	Commit struct {
		Oid             string    `json:"oid"`
		CommittedDate   time.Time `json:"committedDate"`
		MessageHeadline string    `json:"messageHeadline"`
	} `json:"commit"`
}

type Thread struct {
	ID           string `json:"id"`
	Path         string `json:"path"`
	Line         int    `json:"line"`
	OriginalLine int    `json:"originalLine"`
	IsResolved   bool   `json:"isResolved"`
	IsOutdated   bool   `json:"isOutdated"`
	Comments     struct {
		Nodes []ThreadComment `json:"nodes"`
	} `json:"comments"`
}

// PRDump is the subset of the GitHub GraphQL dump the replay reads.
type PRDump struct {
	Number     int    `json:"number"`
	URL        string `json:"url"`
	State      string `json:"state"`
	HeadRefOid string `json:"headRefOid"`
	Author     Actor  `json:"author"`
	Reviews    struct {
		Nodes []Review `json:"nodes"`
	} `json:"reviews"`
	ReviewThreads struct {
		Nodes []Thread `json:"nodes"`
	} `json:"reviewThreads"`
	Commits struct {
		Nodes []Commit `json:"nodes"`
	} `json:"commits"`

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
// finding marker on this PR. A published round always posts at least one
// such comment, so the set identifies the bot without a configured name.
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
	sort.SliceStable(out, func(i, j int) bool { return out[i].At.Before(out[j].At) })
	return out
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
		out = append(out, ObservedRoot{FindingID: id, File: t.Path, Line: line, Text: StripMarkup(root.Body), At: root.CreatedAt, Resolved: t.IsResolved})
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
