package approval

import (
	"context"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// Preload is the first prompt of an investigation: every review artifact,
// every extracted concern, the pull request diff and the code at the review
// anchors, under short aliases the answer may use in place of full IDs.
type Preload struct {
	Text string
	// Evidence, Concerns and Revisions map an alias to its ID or SHA.
	Evidence, Concerns, Revisions map[string]string
	// Auto maps an evidence ID to the server's rationale for an artifact it
	// classifies as non-actionable from its structure alone.
	Auto map[string]string

	evidenceAlias, concernAlias, revisionAlias map[string]string
	// linked holds artifacts tied to a canonical concern, which are always
	// classified as carrying it.
	linked map[string]bool
}

const (
	preloadContext    = 40
	preloadMergeGap   = 10
	preloadChunk      = 400
	preloadAnchorDiff = 32 << 10
	preloadFullLines  = 1500
)

var preloadHunk = regexp.MustCompile(`^@@ -([0-9]+)(?:,([0-9]+))? \+([0-9]+)(?:,([0-9]+))? @@`)

func (p Preload) evidenceID(ref string) string { return p.resolve(p.Evidence, ref) }
func (p Preload) concernID(ref string) string  { return p.resolve(p.Concerns, ref) }

func (p Preload) resolve(aliases map[string]string, ref string) string {
	ref = strings.TrimSpace(ref)
	if id, ok := aliases[strings.ToUpper(ref)]; ok {
		return id
	}
	return ref
}

// revision resolves an alias to its SHA and lowercases a full SHA; anything
// else is returned unchanged so the revision checks reject it.
func (p Preload) revision(ref string) string {
	ref = strings.TrimSpace(ref)
	if sha, ok := p.Revisions[strings.ToUpper(ref)]; ok {
		return sha
	}
	if fullSHA.MatchString(ref) {
		return strings.ToLower(ref)
	}
	return ref
}

func (p Preload) revisionName(sha string) string {
	if alias, ok := p.revisionAlias[sha]; ok {
		return alias
	}
	return "unknown"
}

// BuildPreload renders the snapshot and the code it points at as one plain
// text message. Evidence over PreloadEvidenceBytes is a limit; the diff and
// code sections stop at their budgets without becoming coverage gaps, since
// the read tools stay available.
func BuildPreload(ctx context.Context, s Snapshot, repo Repository) (Preload, error) {
	total := 0
	for _, e := range s.Evidence {
		total += len(e.Body)
	}
	if total > PreloadEvidenceBytes {
		return Preload{}, investigationLimit(LimitEvidence, "%d bytes of review evidence exceed the %d byte preload budget", total, PreloadEvidenceBytes)
	}
	reportActivity(ctx, Activity{Stage: "repository"})
	p := Preload{Evidence: map[string]string{}, Concerns: map[string]string{}, Revisions: map[string]string{}, Auto: map[string]string{}, evidenceAlias: map[string]string{}, concernAlias: map[string]string{}, revisionAlias: map[string]string{}, linked: map[string]bool{}}
	for i, e := range s.Evidence {
		if len(e.ConcernIDs) > 0 {
			p.linked[e.ID] = true
		}
		alias := "E" + strconv.Itoa(i+1)
		p.Evidence[alias], p.evidenceAlias[e.ID] = e.ID, alias
		if reason := autoClassification(e); reason != "" {
			p.Auto[e.ID] = reason
		}
	}
	for i, c := range s.Concerns {
		for _, id := range c.EvidenceIDs {
			p.linked[id] = true
		}
		alias := "C" + strconv.Itoa(i+1)
		p.Concerns[alias], p.concernAlias[c.ID] = c.ID, alias
	}
	addRevision := func(alias, sha string) {
		if sha == "" {
			return
		}
		p.Revisions[alias] = sha
		if _, ok := p.revisionAlias[sha]; !ok {
			p.revisionAlias[sha] = alias
		}
	}
	addRevision("H", s.Revision.Head)
	addRevision("B", s.Revision.Base)
	addRevision("M", s.Revision.MergeBase)
	others := append([]string(nil), s.AllowedRevisions...)
	sort.Strings(others)
	extra := 0
	for _, sha := range others {
		if _, ok := p.revisionAlias[sha]; !ok && sha != "" {
			extra++
			addRevision("R"+strconv.Itoa(extra), sha)
		}
	}

	var out strings.Builder
	nonce := s.Digest
	if len(nonce) > 8 {
		nonce = nonce[:8]
	}
	out.WriteString("Revisions:")
	for i, alias := range []string{"H", "B", "M"} {
		if sha, ok := p.Revisions[alias]; ok {
			if i > 0 {
				out.WriteString(",")
			}
			fmt.Fprintf(&out, " %s=%s %s", alias, sha, []string{"head", "base", "merge base"}[i])
		}
	}
	for i := 1; ; i++ {
		sha, ok := p.Revisions["R"+strconv.Itoa(i)]
		if !ok {
			break
		}
		fmt.Fprintf(&out, ", R%d=%s", i, sha)
	}
	out.WriteString("\n\nChecks on H:\n")
	if len(s.Checks) == 0 {
		out.WriteString("none reported\n")
	}
	for _, check := range s.Checks {
		fmt.Fprintf(&out, "%s: %s", check.Name, check.State)
		if check.SHA != s.Revision.Head {
			fmt.Fprintf(&out, " (on %s)", p.revisionName(check.SHA))
		}
		out.WriteString("\n")
	}

	out.WriteString("\nConcerns (each needs a verdict):\n")
	if len(s.Concerns) == 0 {
		out.WriteString("none extracted\n")
	}
	for _, c := range s.Concerns {
		fmt.Fprintf(&out, "%s id=%s severity=%s impact=%s revision=%s anchor=%s sources=%s\nclaim: %s\n", p.concernAlias[c.ID], c.ID, orUnknown(c.OriginalSeverity), orUnknown(c.Impact), p.revisionName(c.OriginalRevision), anchorText(c.Path, c.StartLine, c.EndLine), p.aliases(p.evidenceAlias, c.EvidenceIDs), c.Claim)
	}

	sourced := map[string][]string{}
	for _, c := range s.Concerns {
		for _, id := range c.EvidenceIDs {
			sourced[id] = append(sourced[id], c.ID)
		}
	}
	sources := map[string]Source{}
	for _, source := range s.Sources {
		sources[source.ID] = source
	}
	out.WriteString("\nReview artifacts:\n")
	for _, e := range s.Evidence {
		alias := p.evidenceAlias[e.ID]
		source, ok := sources[e.SourceID]
		login, provider, verified := "unknown", "unknown", "unverified"
		if ok {
			login, provider = orUnknown(source.Login), orUnknown(source.Provider)
			if source.Verified {
				verified = "verified"
			}
		}
		fmt.Fprintf(&out, "=== %s %s id=%s kind=%s by %s (%s, %s) reviewed=%s", alias, nonce, e.ID, e.Kind, login, provider, verified, p.revisionName(e.ReviewedSHA))
		if e.Resolved {
			out.WriteString(" resolved")
		}
		if e.Path != "" {
			out.WriteString(" anchor=" + anchorText(e.Path, e.StartLine, e.EndLine))
		}
		if ids := append(append([]string(nil), e.ConcernIDs...), sourced[e.ID]...); len(ids) > 0 {
			out.WriteString(" concerns=" + p.aliases(p.concernAlias, dedupe(ids)))
		}
		if reason := p.Auto[e.ID]; reason != "" {
			out.WriteString(" auto: " + reason)
		}
		out.WriteString("\n" + e.Body)
		if !strings.HasSuffix(e.Body, "\n") {
			out.WriteString("\n")
		}
		fmt.Fprintf(&out, "=== end %s %s\n", alias, nonce)
	}

	if repo == nil {
		out.WriteString("\nPull request diff M..H:\nCode was not available.\n\nCode at review anchors:\nCode was not available.\n")
		p.Text = out.String()
		return p, nil
	}
	files := preloadDiff(ctx, s, repo, &out)
	if err := ctx.Err(); err != nil {
		return Preload{}, err
	}
	preloadCode(ctx, s, p, repo, files, &out)
	if err := ctx.Err(); err != nil {
		return Preload{}, err
	}
	p.Text = out.String()
	return p, nil
}

func dedupe(ids []string) []string {
	seen := map[string]bool{}
	out := ids[:0]
	for _, id := range ids {
		if !seen[id] {
			seen[id] = true
			out = append(out, id)
		}
	}
	return out
}

func orUnknown(text string) string {
	if strings.TrimSpace(text) == "" {
		return "unknown"
	}
	return text
}

func anchorText(path string, start, end int) string {
	if path == "" {
		return "none"
	}
	if start < 1 {
		return path
	}
	if end < start {
		end = start
	}
	return fmt.Sprintf("%s:%d-%d", path, start, end)
}

func (p Preload) aliases(index map[string]string, ids []string) string {
	names := make([]string, 0, len(ids))
	for _, id := range ids {
		if alias, ok := index[id]; ok {
			names = append(names, alias)
		} else {
			names = append(names, id)
		}
	}
	if len(names) == 0 {
		return "none"
	}
	return strings.Join(names, ",")
}

// autoClassification names artifacts that carry no review text of their own;
// change requests in bare reviews are already captured by the snapshot flags.
func autoClassification(e Evidence) string {
	switch e.Kind {
	case "review_request":
		return "Review request metadata."
	case "review":
		if !strings.HasPrefix(e.Body, "Review state:") {
			return ""
		}
		rest := ""
		if newline := strings.IndexByte(e.Body, '\n'); newline >= 0 {
			rest = e.Body[newline+1:]
		}
		if strings.TrimSpace(rest) == "" {
			return "Review state only; no review text."
		}
	}
	return ""
}

type diffFile struct {
	path           string
	text           string
	added, removed int
	deleted        bool
}

// readFullDiff reads every page of one diff, bounded by limit bytes.
func readFullDiff(ctx context.Context, repo Repository, req ReadRequest, limit int) (string, bool, error) {
	var text strings.Builder
	seen := map[string]bool{}
	for {
		page, err := repo.Read(ctx, "read_diff", req)
		if err != nil {
			return text.String(), false, err
		}
		if text.Len()+len(page.Text) > limit {
			text.WriteString(page.Text[:limit-text.Len()])
			return text.String(), true, nil
		}
		text.WriteString(page.Text)
		if page.NextCursor == "" {
			return text.String(), false, nil
		}
		if seen[page.NextCursor] || page.NextCursor == req.Cursor {
			return text.String(), false, fmt.Errorf("invalid diff pagination")
		}
		seen[page.NextCursor] = true
		req.Cursor = page.NextCursor
	}
}

func splitDiff(text string) []diffFile {
	var files []diffFile
	if text == "" {
		return nil
	}
	for _, chunk := range strings.SplitAfter(text, "\n") {
		if strings.HasPrefix(chunk, "diff --git ") || len(files) == 0 {
			files = append(files, diffFile{})
		}
		f := &files[len(files)-1]
		f.text += chunk
	}
	for i := range files {
		f := &files[i]
		inHunk := false
		for _, line := range strings.Split(f.text, "\n") {
			switch {
			case strings.HasPrefix(line, "@@"):
				inHunk = true
			case !inHunk && strings.HasPrefix(line, "+++ "):
				if p := strings.TrimPrefix(line, "+++ "); p == "/dev/null" {
					f.deleted = true
				} else {
					f.path = strings.TrimPrefix(p, "b/")
				}
			case !inHunk && strings.HasPrefix(line, "--- ") && f.path == "":
				if p := strings.TrimPrefix(line, "--- "); p != "/dev/null" {
					f.path = strings.TrimPrefix(p, "a/")
				}
			case inHunk && strings.HasPrefix(line, "+"):
				f.added++
			case inHunk && strings.HasPrefix(line, "-"):
				f.removed++
			}
		}
		if f.path == "" {
			if fields := strings.Fields(strings.SplitN(f.text, "\n", 2)[0]); len(fields) == 4 {
				f.path = strings.TrimPrefix(fields[3], "b/")
			}
		}
	}
	return files
}

func anchorPaths(s Snapshot) map[string]bool {
	paths := map[string]bool{}
	for _, c := range s.Concerns {
		if c.Path != "" {
			paths[c.Path] = true
		}
	}
	for _, e := range s.Evidence {
		if e.Path != "" {
			paths[e.Path] = true
		}
	}
	return paths
}

// preloadDiff writes the merge-base to head diff, anchor files first, and
// returns its files in that order.
func preloadDiff(ctx context.Context, s Snapshot, repo Repository, out *strings.Builder) []diffFile {
	out.WriteString("\nPull request diff M..H:\n")
	text, _, err := readFullDiff(ctx, repo, ReadRequest{Revision: s.Revision.Head, OtherRevision: s.Revision.MergeBase}, 4<<20)
	if err != nil {
		out.WriteString("The diff could not be read; use read_diff.\n")
		return nil
	}
	files := splitDiff(text)
	anchors := anchorPaths(s)
	sort.SliceStable(files, func(i, j int) bool { return anchors[files[i].path] && !anchors[files[j].path] })
	used := 0
	var omitted []diffFile
	for _, f := range files {
		if used+len(f.text) > PreloadDiffBytes {
			omitted = append(omitted, f)
			continue
		}
		out.WriteString(f.text)
		if !strings.HasSuffix(f.text, "\n") {
			out.WriteString("\n")
		}
		used += len(f.text)
	}
	if len(files) == 0 {
		out.WriteString("No changes.\n")
	}
	if len(omitted) > 0 {
		out.WriteString("Omitted from this diff (read them with read_diff):\n")
		for _, f := range omitted {
			fmt.Fprintf(out, "%s +%d -%d\n", f.path, f.added, f.removed)
		}
	}
	return files
}

type codeAnchor struct {
	revision, path string
	start, end     int
}

type codeUnit struct {
	revision, path string
	start, end     int
	priority, seq  int
	diff, full     bool
}

func severityRank(severity string) int {
	switch strings.ToLower(severity) {
	case "critical":
		return 0
	case "high":
		return 1
	case "medium":
		return 2
	case "low":
		return 3
	}
	return 4
}

func codeAnchors(s Snapshot) []codeAnchor {
	concerns := append([]Concern(nil), s.Concerns...)
	sort.SliceStable(concerns, func(i, j int) bool {
		if a, b := severityRank(concerns[i].OriginalSeverity), severityRank(concerns[j].OriginalSeverity); a != b {
			return a < b
		}
		return concerns[i].ID < concerns[j].ID
	})
	var anchors []codeAnchor
	for _, c := range concerns {
		if c.Path != "" && c.StartLine > 0 && allowedRevision(s, c.OriginalRevision) {
			anchors = append(anchors, codeAnchor{c.OriginalRevision, c.Path, c.StartLine, max(c.EndLine, c.StartLine)})
		}
	}
	verified := map[string]bool{}
	for _, source := range s.Sources {
		verified[source.ID] = source.Verified
	}
	var inline []Evidence
	for _, e := range s.Evidence {
		if e.Kind == "inline_comment" && e.Path != "" && e.StartLine > 0 && allowedRevision(s, e.ReviewedSHA) {
			inline = append(inline, e)
		}
	}
	sort.SliceStable(inline, func(i, j int) bool { return verified[inline[i].SourceID] && !verified[inline[j].SourceID] })
	for _, e := range inline {
		anchors = append(anchors, codeAnchor{e.ReviewedSHA, e.Path, e.StartLine, max(e.EndLine, e.StartLine)})
	}
	return anchors
}

// mapLine follows an old line through a diff's hunk headers to the new side;
// a line inside a hunk maps to the hunk's new start.
func mapLine(diff string, line int) int {
	offset := 0
	for _, text := range strings.Split(diff, "\n") {
		m := preloadHunk.FindStringSubmatch(text)
		if m == nil {
			continue
		}
		oldStart, _ := strconv.Atoi(m[1])
		newStart, _ := strconv.Atoi(m[3])
		oldCount, newCount := 1, 1
		if m[2] != "" {
			oldCount, _ = strconv.Atoi(m[2])
		}
		if m[4] != "" {
			newCount, _ = strconv.Atoi(m[4])
		}
		// A zero count means the range sits after the start line.
		oldFirst, oldEnd, newEnd := oldStart, oldStart+oldCount, newStart+newCount
		if oldCount == 0 {
			oldFirst, oldEnd = oldStart+1, oldStart+1
		}
		if newCount == 0 {
			newEnd = newStart + 1
		}
		if line < oldFirst {
			return line + offset
		}
		if line < oldEnd {
			return max(newStart, 1)
		}
		offset = newEnd - oldEnd
	}
	return line + offset
}

func preloadCode(ctx context.Context, s Snapshot, p Preload, repo Repository, files []diffFile, out *strings.Builder) {
	out.WriteString("\nCode at review anchors:\n")
	head := s.Revision.Head
	var units []codeUnit
	windows := map[string][][2]int{}
	addWindow := func(rev, path string, start, end, priority int) {
		start = max(start, 1)
		units = append(units, codeUnit{revision: rev, path: path, start: start, end: end, priority: priority, seq: len(units)})
		windows[rev+"\x00"+path] = append(windows[rev+"\x00"+path], [2]int{start, end})
	}
	diffs := map[string]string{}
	anchors := codeAnchors(s)
	for i, a := range anchors {
		addWindow(a.revision, a.path, a.start-preloadContext, a.end+preloadContext, i)
		if a.revision == head {
			continue
		}
		key := a.revision + "\x00" + a.path
		diff, ok := diffs[key]
		if !ok {
			text, truncated, err := readFullDiff(ctx, repo, ReadRequest{Revision: head, OtherRevision: a.revision, Path: a.path}, preloadAnchorDiff)
			if err != nil {
				continue
			}
			if truncated {
				text += "\n[diff truncated; use read_diff for the rest]\n"
			}
			diff = text
			diffs[key] = diff
			units = append(units, codeUnit{revision: a.revision, path: a.path, priority: i, seq: len(units), diff: true})
		}
		addWindow(head, a.path, mapLine(diff, a.start)-preloadContext, mapLine(diff, a.end)+preloadContext, i)
	}
	for i, f := range files {
		if f.deleted || f.path == "" {
			continue
		}
		if _, err := repo.Read(ctx, "read_file", ReadRequest{Revision: head, Path: f.path, StartLine: preloadFullLines, EndLine: preloadFullLines}); err == nil {
			continue
		}
		for _, gap := range subtractWindows([2]int{1, preloadFullLines}, windows[head+"\x00"+f.path]) {
			units = append(units, codeUnit{revision: head, path: f.path, start: gap[0], end: gap[1], priority: len(anchors) + i, seq: len(units), full: true})
		}
	}
	used := 0
	for _, u := range mergeUnits(units) {
		if ctx.Err() != nil {
			return
		}
		text := ""
		if u.diff {
			text = fmt.Sprintf("--- diff %s..H %s\n%s", p.revisionName(u.revision), u.path, diffs[u.revision+"\x00"+u.path])
		} else {
			text = renderWindow(ctx, p, repo, u)
		}
		if text == "" || used+len(text) > PreloadCodeBytes {
			continue
		}
		out.WriteString(text)
		if !strings.HasSuffix(text, "\n") {
			out.WriteString("\n")
		}
		used += len(text)
	}
}

// subtractWindows returns the parts of span that no window covers.
func subtractWindows(span [2]int, windows [][2]int) [][2]int {
	sorted := append([][2]int(nil), windows...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i][0] < sorted[j][0] })
	var gaps [][2]int
	next := span[0]
	for _, w := range sorted {
		if w[0] > next {
			gaps = append(gaps, [2]int{next, min(w[0]-1, span[1])})
		}
		if next = max(next, w[1]+1); next > span[1] {
			return gaps
		}
	}
	return append(gaps, [2]int{next, span[1]})
}

// mergeUnits joins code windows of one tier (anchor or full file) on the same
// revision and path that overlap or sit within preloadMergeGap lines, keeping
// the best priority, and orders the result by priority. Anchor windows never
// absorb full-file ranges, so a large file cannot displace a later anchor.
func mergeUnits(units []codeUnit) []codeUnit {
	groups := map[string][]codeUnit{}
	var order []string
	var merged []codeUnit
	for _, u := range units {
		if u.diff {
			merged = append(merged, u)
			continue
		}
		key := fmt.Sprintf("%t\x00%s\x00%s", u.full, u.revision, u.path)
		if _, ok := groups[key]; !ok {
			order = append(order, key)
		}
		groups[key] = append(groups[key], u)
	}
	for _, key := range order {
		group := groups[key]
		sort.SliceStable(group, func(i, j int) bool { return group[i].start < group[j].start })
		current := group[0]
		for _, u := range group[1:] {
			if u.start <= current.end+preloadMergeGap+1 {
				current.end = max(current.end, u.end)
				current.priority, current.seq = min(current.priority, u.priority), min(current.seq, u.seq)
				continue
			}
			merged = append(merged, current)
			current = u
		}
		merged = append(merged, current)
	}
	sort.SliceStable(merged, func(i, j int) bool {
		if merged[i].priority != merged[j].priority {
			return merged[i].priority < merged[j].priority
		}
		return merged[i].seq < merged[j].seq
	})
	return merged
}

// renderWindow reads one window in chunks and renders it with line numbers.
// A full-file range that cannot be read is skipped silently.
func renderWindow(ctx context.Context, p Preload, repo Repository, u codeUnit) string {
	var body strings.Builder
	last := 0
	for start := u.start; start <= u.end; start += preloadChunk {
		end := min(u.end, start+preloadChunk-1)
		r, err := repo.Read(ctx, "read_file", ReadRequest{Revision: u.revision, Path: u.path, StartLine: start, EndLine: end})
		if err != nil {
			break
		}
		lines := strings.Split(r.Text, "\n")
		for i, line := range lines {
			fmt.Fprintf(&body, "%5d| %s\n", start+i, line)
		}
		last = start + len(lines) - 1
		if len(lines) < end-start+1 {
			break
		}
	}
	if last == 0 {
		if u.full {
			return ""
		}
		return fmt.Sprintf("--- %s:%s lines %d-%d are not available at this revision.\n", p.revisionName(u.revision), u.path, u.start, u.end)
	}
	return fmt.Sprintf("--- %s:%s lines %d-%d\n%s", p.revisionName(u.revision), u.path, u.start, last, body.String())
}
