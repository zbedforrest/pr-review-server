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

	"pr-review-server/pkg/publisher"
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
}

type dumpThread struct {
	ID           string `json:"id"`
	Path         string `json:"path"`
	Line         int    `json:"line"`
	OriginalLine int    `json:"originalLine"`
	IsResolved   bool   `json:"isResolved"`
	IsOutdated   bool   `json:"isOutdated"`
	Comments     struct {
		Nodes []dumpThreadComment `json:"nodes"`
	} `json:"comments"`
}

// prDump is the subset of the GitHub GraphQL dump the replay reads.
type prDump struct {
	Number     int       `json:"number"`
	URL        string    `json:"url"`
	State      string    `json:"state"`
	HeadRefOid string    `json:"headRefOid"`
	Author     dumpActor `json:"author"`
	Reviews    struct {
		Nodes []dumpReview `json:"nodes"`
	} `json:"reviews"`
	ReviewThreads struct {
		Nodes []dumpThread `json:"nodes"`
	} `json:"reviewThreads"`

	Owner string `json:"-"`
	Repo  string `json:"-"`
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
	d.Owner, d.Repo = m[1], m[2]
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
// finding marker on this PR. A published round always posts at least one
// such comment, so the set identifies the bot without a configured name.
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
	return out
}

type round struct {
	SHA  string
	SHA7 string
	At   time.Time
}

// rounds are the bot's review submissions in time order. The same commit
// may appear more than once: each submission was a publication round.
func (d *prDump) rounds(bots map[string]bool) []round {
	var out []round
	for _, r := range d.Reviews.Nodes {
		if !bots[bareLogin(actorLogin(r.Author))] || len(r.Commit.Oid) < 7 || r.State == "PENDING" || r.SubmittedAt.IsZero() {
			continue
		}
		out = append(out, round{SHA: r.Commit.Oid, SHA7: r.Commit.Oid[:7], At: r.SubmittedAt})
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].At.Before(out[j].At) })
	return out
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
		out = append(out, observedRoot{FindingID: id, File: t.Path, Line: line, Text: stripMarkup(root.Body), At: root.CreatedAt, Resolved: t.IsResolved})
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
