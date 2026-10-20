// Package verify checks merged ensemble findings against repository evidence
// before they publish.
package verify

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"pr-review-server/pkg/reviewer/types"
)

// Item kinds.
const (
	KindHead = "head"
	KindHunk = "hunk"
	KindBase = "base"
	KindRef  = "ref"
	KindPR   = "pr"
	KindNote = "note"
)

// Limits bound what one packet may hold.
type Limits struct {
	HeadContext  int
	BaseContext  int
	MaxSpanLines int
	MaxHunkLines int
	RefContext   int
	RefPerSymbol int
	RefTotal     int
	MaxSymbols   int
	PRTextChars  int
	MaxFileBytes int
	MaxLineRunes int
}

var DefaultLimits = Limits{
	HeadContext:  30,
	BaseContext:  10,
	MaxSpanLines: 100,
	MaxHunkLines: 200,
	RefContext:   3,
	RefPerSymbol: 4,
	RefTotal:     12,
	MaxSymbols:   6,
	PRTextChars:  4000,
	MaxFileBytes: 1 << 20,
	MaxLineRunes: 300,
}

// Finding is one merged finding to verify; Key is how the model refers to it.
type Finding struct {
	Key     string
	Comment types.LineComment
}

// Input names the immutable revisions a packet is read at.
type Input struct {
	Head     string
	Base     string
	PRText   string
	Findings []Finding
}

// Item is one piece of evidence the model may cite by ID.
type Item struct {
	ID    string
	Kind  string
	Path  string
	Rev   string
	Start int
	End   int
	Text  string
}

// Packet is the evidence for one verify call. Items are shared across
// findings; ByFinding lists the IDs built for each finding key, and Shared
// lists IDs every finding sees (the PR text).
type Packet struct {
	Items     []Item
	ByFinding map[string][]string
	Shared    []string
}

// Item returns the item with the given ID.
func (p *Packet) Item(id string) (Item, bool) {
	n, err := strconv.Atoi(strings.TrimPrefix(id, "P"))
	if err != nil || !strings.HasPrefix(id, "P") || n < 1 || n > len(p.Items) {
		return Item{}, false
	}
	return p.Items[n-1], true
}

type builder struct {
	ctx   context.Context
	git   Git
	in    Input
	lim   Limits
	pkt   *Packet
	byKey map[string]string
	files map[string]fileRead
	diffs map[string]fileDiff
}

type fileRead struct {
	lines []string
	err   error
}

type fileDiff struct {
	newFile bool
	hunks   []hunk
	err     error
}

type hunk struct {
	oldStart, oldCount, newStart, newCount int
	text                                   string
}

// Build assembles the evidence packet for every finding in in.
func Build(ctx context.Context, git Git, in Input, lim Limits) (*Packet, error) {
	b := &builder{
		ctx: ctx, git: git, in: in, lim: lim,
		pkt:   &Packet{ByFinding: map[string][]string{}},
		byKey: map[string]string{},
		files: map[string]fileRead{},
		diffs: map[string]fileDiff{},
	}
	if text := strings.TrimSpace(in.PRText); text != "" {
		if r := []rune(text); len(r) > lim.PRTextChars {
			text = string(r[:lim.PRTextChars]) + "\n[truncated]"
		}
		b.pkt.Shared = append(b.pkt.Shared, b.add(Item{Kind: KindPR, Text: text}))
	}
	for _, f := range in.Findings {
		ids, err := b.finding(f.Comment)
		if err != nil {
			return nil, err
		}
		b.pkt.ByFinding[f.Key] = ids
	}
	return b.pkt, nil
}

func (b *builder) add(it Item) string {
	key := strings.Join([]string{it.Kind, it.Path, it.Rev, strconv.Itoa(it.Start), strconv.Itoa(it.End), it.Text}, "\x00")
	if id, ok := b.byKey[key]; ok {
		return id
	}
	it.ID = "P" + strconv.Itoa(len(b.pkt.Items)+1)
	b.pkt.Items = append(b.pkt.Items, it)
	b.byKey[key] = it.ID
	return it.ID
}

func (b *builder) note(path, text string) string {
	return b.add(Item{Kind: KindNote, Path: path, Text: text})
}

func (b *builder) finding(c types.LineComment) ([]string, error) {
	var ids []string
	path, line := c.FilePath, c.LineNumber

	head, err := b.read(b.in.Head, path)
	if err := b.ctx.Err(); err != nil {
		return nil, err
	}
	var headStart, headEnd int
	switch {
	case err != nil:
		ids = append(ids, b.note(path, "head source unavailable: "+reason(err)))
	default:
		headStart, headEnd = clamp(line-b.lim.HeadContext, line+b.lim.HeadContext, len(head))
		ids = append(ids, b.add(Item{Kind: KindHead, Path: path, Rev: b.in.Head, Start: headStart, End: headEnd, Text: b.numbered(head, headStart, headEnd)}))
	}

	ids = append(ids, b.baseItems(path, line)...)
	if err := b.ctx.Err(); err != nil {
		return nil, err
	}

	ids = append(ids, b.subjects(c, path)...)
	if err := b.ctx.Err(); err != nil {
		return nil, err
	}

	refs, err := b.refs(c, path, headStart, headEnd)
	if err != nil {
		return nil, err
	}
	return append(ids, refs...), nil
}

// subjects adds the code of each contract subject that lives in another
// file, around the subject's name when it appears there. A subject path the
// repository does not hold (an untracked dependency, generated code) is
// said so, which keeps a library claim from being dropped for lack of it.
func (b *builder) subjects(c types.LineComment, path string) []string {
	if c.FindingContract == nil {
		return nil
	}
	var ids []string
	seen := map[string]bool{path: true}
	for _, s := range c.FindingContract.Subjects {
		p := strings.TrimLeft(strings.TrimPrefix(strings.TrimSpace(s.Path), "./"), "/")
		if p == "" || seen[p] {
			continue
		}
		seen[p] = true
		lines, err := b.read(b.in.Head, p)
		if err != nil {
			ids = append(ids, b.note(p, "subject source not in the repository at head: "+reason(err)))
			continue
		}
		at := 1
		if s.Name != "" {
			for i, l := range lines {
				if strings.Contains(l, s.Name) {
					at = i + 1
					break
				}
			}
		}
		start, end := clamp(at-b.lim.BaseContext, at+b.lim.HeadContext, len(lines))
		ids = append(ids, b.add(Item{Kind: KindHead, Path: p, Rev: b.in.Head, Start: start, End: end, Text: b.numbered(lines, start, end)}))
	}
	return ids
}

// baseItems adds the hunk covering line and the base text it replaced. A line
// outside every hunk is mapped through the preceding hunks' offsets.
func (b *builder) baseItems(path string, line int) []string {
	d := b.diff(path)
	if d.err != nil {
		return []string{b.note(path, "diff unavailable: "+reason(d.err))}
	}
	if d.newFile {
		return []string{b.note(path, "file is new in this PR; it does not exist at the merge-base")}
	}
	var ids []string
	baseLine, covering := line, (*hunk)(nil)
	for i := range d.hunks {
		h := &d.hunks[i]
		if line >= h.newStart && line < h.newStart+max(h.newCount, 1) {
			covering = h
			break
		}
		if h.newStart > line {
			break
		}
		baseLine += h.oldCount - h.newCount
	}
	base, err := b.read(b.in.Base, path)
	if err != nil {
		return append(ids, b.note(path, "base unavailable: "+reason(err)))
	}
	var start, end int
	if covering != nil {
		ids = append(ids, b.add(Item{Kind: KindHunk, Path: path, Start: covering.newStart, End: covering.newStart + covering.newCount - 1, Text: b.clip(firstLines(covering.text, b.lim.MaxHunkLines))}))
		spanEnd := covering.oldStart + max(covering.oldCount, 1) - 1
		spanEnd = min(spanEnd, covering.oldStart+b.lim.MaxSpanLines)
		start, end = clamp(covering.oldStart-b.lim.BaseContext, spanEnd+b.lim.BaseContext, len(base))
	} else {
		start, end = clamp(baseLine-b.lim.HeadContext, baseLine+b.lim.HeadContext, len(base))
	}
	if start > end {
		return append(ids, b.note(path, "base has no lines at the mapped location"))
	}
	return append(ids, b.add(Item{Kind: KindBase, Path: path, Rev: b.in.Base, Start: start, End: end, Text: b.numbered(base, start, end)}))
}

func (b *builder) refs(c types.LineComment, path string, headStart, headEnd int) ([]string, error) {
	var ids []string
	total := 0
	for _, sym := range symbols(c, b.lim.MaxSymbols) {
		if total >= b.lim.RefTotal {
			break
		}
		hits, err := b.git.Grep(b.ctx, b.in.Head, sym, 50)
		if err := b.ctx.Err(); err != nil {
			return nil, err
		}
		if err != nil {
			ids = append(ids, b.note("", fmt.Sprintf("references for %s unavailable: %s", sym, reason(err))))
			continue
		}
		if len(hits) == 0 {
			ids = append(ids, b.note("", fmt.Sprintf("no references to %s at head", sym)))
			continue
		}
		taken := 0
		for _, h := range orderHits(hits, path) {
			if taken >= b.lim.RefPerSymbol || total >= b.lim.RefTotal {
				break
			}
			if h.Path == path && h.Line >= headStart && h.Line <= headEnd {
				continue
			}
			lines, err := b.read(b.in.Head, h.Path)
			if err != nil {
				continue
			}
			s, e := clamp(h.Line-b.lim.RefContext, h.Line+b.lim.RefContext, len(lines))
			ids = append(ids, b.add(Item{Kind: KindRef, Path: h.Path, Rev: b.in.Head, Start: s, End: e, Text: "reference to " + sym + "\n" + b.numbered(lines, s, e)}))
			taken++
			total++
		}
		if extra := len(hits) - taken; extra > 0 && taken == b.lim.RefPerSymbol {
			ids = append(ids, b.note("", fmt.Sprintf("%s has at least %d more references not shown", sym, extra)))
		}
	}
	return ids, nil
}

// orderHits puts the finding's own file first, then the rest in grep order.
func orderHits(hits []Hit, path string) []Hit {
	out := make([]Hit, 0, len(hits))
	for _, h := range hits {
		if h.Path == path {
			out = append(out, h)
		}
	}
	for _, h := range hits {
		if h.Path != path {
			out = append(out, h)
		}
	}
	return out
}

func (b *builder) read(rev, path string) ([]string, error) {
	key := rev + ":" + path
	if r, ok := b.files[key]; ok {
		return r.lines, r.err
	}
	var r fileRead
	text, err := b.git.Show(b.ctx, rev, path)
	switch {
	case err != nil:
		r.err = err
	case len(text) > b.lim.MaxFileBytes:
		r.err = errTooLarge
	default:
		r.lines = strings.Split(strings.TrimSuffix(text, "\n"), "\n")
	}
	b.files[key] = r
	return r.lines, r.err
}

var errTooLarge = errors.New("file too large")

func (b *builder) diff(path string) fileDiff {
	if d, ok := b.diffs[path]; ok {
		return d
	}
	text, err := b.git.Diff(b.ctx, b.in.Base, b.in.Head, path)
	d := fileDiff{err: err}
	if err == nil {
		d = parseDiff(text)
	}
	b.diffs[path] = d
	return d
}

var hunkHeader = regexp.MustCompile(`^@@ -(\d+)(?:,(\d+))? \+(\d+)(?:,(\d+))? @@`)

func parseDiff(text string) fileDiff {
	var d fileDiff
	var cur *hunk
	var body strings.Builder
	flush := func() {
		if cur != nil {
			cur.text = strings.TrimSuffix(body.String(), "\n")
			d.hunks = append(d.hunks, *cur)
		}
		body.Reset()
	}
	for _, line := range strings.Split(text, "\n") {
		if cur == nil && (strings.HasPrefix(line, "new file mode") || line == "--- /dev/null") {
			d.newFile = true
		}
		if m := hunkHeader.FindStringSubmatch(line); m != nil {
			flush()
			cur = &hunk{oldStart: atoi(m[1], 0), oldCount: atoi(m[2], 1), newStart: atoi(m[3], 0), newCount: atoi(m[4], 1)}
		}
		if cur != nil {
			body.WriteString(line + "\n")
		}
	}
	flush()
	return d
}

func atoi(s string, def int) int {
	if s == "" {
		return def
	}
	n, _ := strconv.Atoi(s)
	return n
}

var (
	backticked = regexp.MustCompile("`([^`\n]+)`")
	identifier = regexp.MustCompile(`[A-Za-z_][A-Za-z0-9_]{2,}`)
	compound   = regexp.MustCompile(`^(?:[A-Za-z0-9]*_[A-Za-z0-9_]*|[a-z]+[A-Z][A-Za-z0-9]*|[A-Z][a-z0-9]+[A-Z][A-Za-z0-9]*)$`)
)

var stopIdents = map[string]bool{
	"true": true, "false": true, "null": true, "nil": true, "None": true, "self": true, "this": true,
	"return": true, "const": true, "let": true, "var": true, "func": true, "def": true, "class": true,
	"import": true, "from": true, "async": true, "await": true, "undefined": true,
}

// symbols returns the identifiers worth looking up: the contract's named
// subjects, then identifiers in code spans and compound names in the headline.
func symbols(c types.LineComment, limit int) []string {
	var out []string
	seen := map[string]bool{}
	push := func(s string) {
		if s == "" || seen[s] || stopIdents[s] || len(out) >= limit {
			return
		}
		seen[s] = true
		out = append(out, s)
	}
	headline := firstLine(c.CommentBody)
	if c.FindingContract != nil {
		for _, s := range c.FindingContract.Subjects {
			for _, id := range identifier.FindAllString(s.Name, -1) {
				push(id)
			}
		}
		if c.FindingContract.Headline != "" {
			headline = c.FindingContract.Headline
		}
	}
	for _, m := range backticked.FindAllStringSubmatch(headline, -1) {
		for _, id := range identifier.FindAllString(m[1], -1) {
			push(id)
		}
	}
	for _, id := range identifier.FindAllString(backticked.ReplaceAllString(headline, " "), -1) {
		if compound.MatchString(id) {
			push(id)
		}
	}
	return out
}

func firstLine(s string) string {
	line, _, _ := strings.Cut(strings.TrimSpace(s), "\n")
	return line
}

func firstLines(s string, n int) string {
	lines := strings.Split(s, "\n")
	if len(lines) <= n {
		return s
	}
	return strings.Join(lines[:n], "\n") + fmt.Sprintf("\n[%d more lines]", len(lines)-n)
}

func clamp(start, end, n int) (int, int) {
	return max(start, 1), min(end, n)
}

func (b *builder) numbered(lines []string, start, end int) string {
	var out strings.Builder
	for i := start; i <= end; i++ {
		fmt.Fprintf(&out, "%5d| %s\n", i, clipLine(lines[i-1], b.lim.MaxLineRunes))
	}
	return strings.TrimSuffix(out.String(), "\n")
}

func (b *builder) clip(text string) string {
	lines := strings.Split(text, "\n")
	for i, l := range lines {
		lines[i] = clipLine(l, b.lim.MaxLineRunes)
	}
	return strings.Join(lines, "\n")
}

// clipLine cuts a line past n runes; minified bundles hold whole files on
// one line.
func clipLine(line string, n int) string {
	if n <= 0 || len(line) <= n {
		return line
	}
	r := []rune(line)
	if len(r) <= n {
		return line
	}
	return string(r[:n]) + fmt.Sprintf(" [%d more chars]", len(r)-n)
}

func reason(err error) string {
	switch {
	case errors.Is(err, ErrNotFound):
		return "path not found at that revision"
	case errors.Is(err, errTooLarge):
		return "file too large"
	}
	return "git error"
}
