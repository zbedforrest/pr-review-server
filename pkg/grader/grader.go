// Package grader grades PRism's posted comments on the audit rubric
// (GOOD, OK, BAD) so comment quality is measured daily rather than by
// occasional manual audits.
package grader

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

// Item kinds.
const (
	KindInline      = "inline"
	KindSummaryNote = "summary_note"
	KindReply       = "reply"
)

// Grades.
const (
	GradeGood = "GOOD"
	GradeOK   = "OK"
	GradeBad  = "BAD"
)

// Item is one posted comment to grade. ID is unique within a PR batch.
type Item struct {
	ID            string `json:"id"`
	Kind          string `json:"kind"`
	File          string `json:"file,omitempty"`
	Line          int    `json:"line,omitempty"`
	Severity      string `json:"severity,omitempty"`
	Comment       string `json:"comment"`
	AuthorComment string `json:"author_comment,omitempty"`
}

// Grade is the judge's verdict on one item.
type Grade struct {
	ID       string `json:"id"`
	Grade    string `json:"grade"`
	Category string `json:"category"`
	Reason   string `json:"reason"`
}

//go:embed prompt.md
var promptTemplate string

// maxBodyChars bounds the PR description carried into the prompt.
const maxBodyChars = 6000

// Prompt renders the grading prompt for one PR's items.
func Prompt(title, body, headSHA, baseSHA, baseDir string, items []Item) (string, error) {
	itemsJSON, err := json.MarshalIndent(items, "", " ")
	if err != nil {
		return "", err
	}
	if r := []rune(body); len(r) > maxBodyChars {
		body = string(r[:maxBodyChars]) + "\n[truncated]"
	}
	return strings.NewReplacer(
		"{{TITLE}}", title,
		"{{BODY}}", body,
		"{{HEAD}}", headSHA,
		"{{BASE}}", baseSHA,
		"{{BASE_DIR}}", baseDir,
		"{{ITEMS}}", string(itemsJSON),
	).Replace(promptTemplate), nil
}

// ParseReply reads the judge's JSON array, keeping only well-formed grades
// for ids that were asked about; an item graded twice keeps the first.
func ParseReply(reply string, items []Item) ([]Grade, error) {
	raw, err := firstGradeArray(reply)
	if err != nil {
		return nil, err
	}
	asked := make(map[string]bool, len(items))
	for _, it := range items {
		asked[it.ID] = true
	}
	seen := map[string]bool{}
	var out []Grade
	for _, g := range raw {
		g.Grade = strings.ToUpper(strings.TrimSpace(g.Grade))
		if !asked[g.ID] || seen[g.ID] || (g.Grade != GradeGood && g.Grade != GradeOK && g.Grade != GradeBad) {
			continue
		}
		seen[g.ID] = true
		out = append(out, g)
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("grader: reply graded none of the %d items", len(items))
	}
	return out, nil
}

// firstGradeArray decodes the first JSON array of objects in reply, starting
// at each "[" in turn; a decoder stops at the end of one value, so prose or
// brackets after the array cannot spoil it.
func firstGradeArray(reply string) ([]Grade, error) {
	for i := strings.IndexByte(reply, '['); i >= 0; {
		var raw []Grade
		if err := json.NewDecoder(strings.NewReader(reply[i:])).Decode(&raw); err == nil && len(raw) > 0 {
			return raw, nil
		}
		next := strings.IndexByte(reply[i+1:], '[')
		if next < 0 {
			break
		}
		i += next + 1
	}
	return nil, fmt.Errorf("grader: no JSON array of grades in reply")
}

// Candidate is one posted comment eligible for grading, with where it lives.
type Candidate struct {
	Ref              string // stable key stored with the grade
	Owner, Repo      string
	PRNumber         int
	HeadSHA, BaseSHA string
	CommentURL       string
	Item             Item
}

// Select caps the day's candidates at max, round-robin across kinds
// so a busy day of inline comments cannot crowd out summary notes and
// replies. It returns the kept candidates and the number dropped.
func Select(cands []Candidate, max int) ([]Candidate, int) {
	if max <= 0 || len(cands) <= max {
		return cands, 0
	}
	byKind := map[string][]Candidate{}
	var kinds []string
	for _, c := range cands {
		if _, ok := byKind[c.Item.Kind]; !ok {
			kinds = append(kinds, c.Item.Kind)
		}
		byKind[c.Item.Kind] = append(byKind[c.Item.Kind], c)
	}
	sort.Strings(kinds)
	var kept []Candidate
	for i := 0; len(kept) < max; i++ {
		progressed := false
		for _, k := range kinds {
			if i < len(byKind[k]) && len(kept) < max {
				kept = append(kept, byKind[k][i])
				progressed = true
			}
		}
		if !progressed {
			break
		}
	}
	return kept, len(cands) - len(kept)
}

// ByPR groups candidates into one batch per PR head, in first-seen order.
func ByPR(cands []Candidate) [][]Candidate {
	idx := map[string]int{}
	var out [][]Candidate
	for _, c := range cands {
		k := fmt.Sprintf("%s/%s#%d@%s", c.Owner, c.Repo, c.PRNumber, c.HeadSHA)
		i, ok := idx[k]
		if !ok {
			i = len(out)
			idx[k] = i
			out = append(out, nil)
		}
		out[i] = append(out[i], c)
	}
	return out
}
