// Package grader grades PRism's posted comments on the audit rubric
// (GOOD, OK, BAD) so comment quality is measured daily rather than by
// occasional manual audits.
package grader

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"regexp"
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
	// AlreadyPosted marks the earlier comment itself carried into this
	// round, which is never a duplicate of its own prior row.
	AlreadyPosted bool `json:"already_posted,omitempty"`
}

// Prior is a point PRism already raised on the PR, or one the author
// settled on an earlier PR, with what was said in its thread since.
type Prior struct {
	State    string   `json:"state"`
	File     string   `json:"file,omitempty"`
	Lines    string   `json:"lines,omitempty"`
	Headline string   `json:"headline"`
	Verdicts []string `json:"verdicts,omitempty"`
}

// Grade is the judge's verdict on one item.
type Grade struct {
	ID       string `json:"id"`
	Grade    string `json:"grade"`
	Category string `json:"category"`
	Reason   string `json:"reason"`
	// DuplicateOf names what a "duplicate" repeats: another item's id, or
	// DuplicateOfPrior for a point raised in an earlier round.
	DuplicateOf string `json:"duplicate_of,omitempty"`
}

// DuplicateOfPrior marks a duplicate of a point already raised on the PR.
const DuplicateOfPrior = "prior"

// ResolveDuplicates keeps one item of every group of duplicates: taking
// items from the highest severity down, a BAD "duplicate" stays BAD only
// when what it repeats is an earlier point or an item still kept, and is
// otherwise kept as OK, so two items naming each other never both drop.
func ResolveDuplicates(items []Item, grades []Grade) []Grade {
	byID := make(map[string]int, len(grades))
	for i, g := range grades {
		byID[g.ID] = i
	}
	order := make([]Item, len(items))
	copy(order, items)
	sort.SliceStable(order, func(i, j int) bool { return severityRank(order[i].Severity) > severityRank(order[j].Severity) })
	out := append([]Grade(nil), grades...)
	for _, it := range order {
		i, ok := byID[it.ID]
		if !ok || out[i].Grade != GradeBad || out[i].Category != "duplicate" {
			continue
		}
		target := strings.TrimSpace(out[i].DuplicateOf)
		if strings.EqualFold(target, DuplicateOfPrior) {
			continue
		}
		if j, ok := byID[target]; ok && target != it.ID && out[j].Grade != GradeBad {
			continue
		}
		out[i].Grade, out[i].Category = GradeOK, "marginal-value"
		out[i].Reason += " (kept: the item it repeats is not kept)"
	}
	return out
}

func severityRank(s string) int {
	switch strings.ToLower(s) {
	case "critical":
		return 3
	case "medium":
		return 2
	case "low":
		return 1
	}
	return 0
}

// BadCategories and OKCategories are the only categories a grade may carry,
// so the health report's breakdown counts a fixed set; anything else the
// judge says becomes CategoryOther.
var (
	BadCategories = []string{"wrong-claim", "wrong-mechanism", "invented-regression", "pre-existing", "deliberate-choice",
		"speculative", "nitpick", "low-value-test-ask", "severity-inflated", "duplicate", "restates-pr", "noise"}
	OKCategories = []string{"overstated-severity", "overstated-scope", "minor-detail-wrong", "marginal-value"}
)

// CategoryOther replaces a category outside the fixed lists.
const CategoryOther = "other"

//go:embed prompt.md
var promptTemplate string

// maxBodyChars bounds the PR description carried into the prompt.
const maxBodyChars = 6000

// Prompt renders the grading prompt for one PR's items.
func Prompt(title, body, headSHA, baseSHA, baseDir string, items []Item, prior []Prior) (string, error) {
	itemsJSON, err := json.MarshalIndent(items, "", " ")
	if err != nil {
		return "", err
	}
	priorText := "None."
	if len(prior) > 0 {
		b, err := json.MarshalIndent(prior, "", " ")
		if err != nil {
			return "", err
		}
		priorText = string(b)
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
		"{{PRIOR}}", priorText,
		"{{BAD_CATEGORIES}}", quoteList(BadCategories),
		"{{OK_CATEGORIES}}", quoteList(OKCategories),
	).Replace(promptTemplate), nil
}

// ParseReply reads the judge's grades from the first JSON array in reply
// that grades at least one asked item, so prose, brackets or an echoed copy
// of the input around it cannot spoil it. An item graded twice keeps the
// first grade.
func ParseReply(reply string, items []Item) ([]Grade, error) {
	asked := make(map[string]bool, len(items))
	for _, it := range items {
		asked[it.ID] = true
	}
	for i := strings.IndexByte(reply, '['); i >= 0; {
		var raw []Grade
		if err := json.NewDecoder(strings.NewReader(reply[i:])).Decode(&raw); err == nil {
			if out := validGrades(raw, asked); len(out) > 0 {
				return out, nil
			}
		}
		next := strings.IndexByte(reply[i+1:], '[')
		if next < 0 {
			break
		}
		i += next + 1
	}
	return nil, fmt.Errorf("grader: reply graded none of the %d items", len(items))
}

func quoteList(v []string) string {
	q := make([]string, len(v))
	for i, s := range v {
		q[i] = `"` + s + `"`
	}
	return strings.Join(q, ", ")
}

// NormalizeCategory maps a grade's category onto the fixed lists: "-" for
// GOOD, CategoryOther for anything unlisted.
func NormalizeCategory(grade, category string) string {
	category = strings.ToLower(strings.TrimSpace(category))
	allowed := map[string][]string{GradeBad: BadCategories, GradeOK: OKCategories}[grade]
	if allowed == nil {
		return "-"
	}
	for _, c := range allowed {
		if c == category {
			return c
		}
	}
	return CategoryOther
}

var (
	secretAssignment = regexp.MustCompile("(?i)\\b([A-Z0-9_.-]*(?:password|passwd|pwd|_pass|secret|secret[_-]?key|token|api[_-]?key|access[_-]?key|private[_-]?key|credential))\\b([\"']?\\s*[:=]\\s*)([\"'`]?)[^\\s\"'`,;)]{4,}")
	secretProse      = regexp.MustCompile(`(?i)\b((?:password|passwd|secret|token|api key|credential)s?\s+(?:is|was|of)\s+)(["']?)([^\s"',;)]{6,})`)
	secretURL        = regexp.MustCompile(`(://[^\s:/@]+:)[^\s@/]+@`)
	secretToken      = regexp.MustCompile(`\b(?:ghp_|gho_|ghs_|github_pat_|sk-|sk_live_|xox[abpr]-|AKIA)[A-Za-z0-9_-]{8,}|(?i)\bbearer\s+[A-Za-z0-9._~+/-]{12,}`)
)

// RedactSecrets masks values that look like credentials, so a reason that
// quotes one cannot carry it into stored grades or the health report.
func RedactSecrets(s string) string {
	s = secretAssignment.ReplaceAllString(s, "${1}${2}${3}[redacted]")
	s = secretProse.ReplaceAllStringFunc(s, func(m string) string {
		// Prose like "the token is expired" is not a value; only a quoted or
		// digit-bearing word after "is", "was" or "of" is masked.
		parts := secretProse.FindStringSubmatch(m)
		if parts[2] == "" && !strings.ContainsAny(parts[3], "0123456789") {
			return m
		}
		return parts[1] + parts[2] + "[redacted]"
	})
	s = secretURL.ReplaceAllString(s, "${1}[redacted]@")
	return secretToken.ReplaceAllString(s, "[redacted]")
}

func validGrades(raw []Grade, asked map[string]bool) []Grade {
	seen := map[string]bool{}
	var out []Grade
	for _, g := range raw {
		g.Grade = strings.ToUpper(strings.TrimSpace(g.Grade))
		if !asked[g.ID] || seen[g.ID] || (g.Grade != GradeGood && g.Grade != GradeOK && g.Grade != GradeBad) {
			continue
		}
		seen[g.ID] = true
		g.Category = NormalizeCategory(g.Grade, g.Category)
		g.Reason = RedactSecrets(g.Reason)
		out = append(out, g)
	}
	return out
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
