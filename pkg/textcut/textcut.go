// Package textcut shortens one line of prose for a headline or a bullet
// without leaving a reader inside a half-finished clause or an open
// delimiter.
package textcut

import (
	"sort"
	"strings"
	"unicode"
	"unicode/utf8"
)

// Marker ends a cut: an editorial omission mark the agent's own prose never
// produces, so a reader can tell a cut from a sentence left hanging. Inside
// a text it stands for a dropped parenthetical.
const Marker = " […]"

// ClauseBoundaries are the joints an effect sentence is most often built
// around; cutting at the last one before the limit keeps a line readable.
var ClauseBoundaries = []string{", so ", "; ", " because ", ", which ", ", and ", ", causing ", ", leaving ", ", "}

// Options selects the cut rules beyond the balanced cut.
type Options struct {
	// KeepProblemClause prefers the clause that states the problem: a text
	// whose "but", "yet" or "however" clause does not fit loses its leading
	// context clause (replaced by the marker) rather than that clause, and
	// no cut ends on a connective or subordinator ("however", "only",
	// "when", "because" ...), which would leave the problem unsaid. The
	// leading clause is kept when HeadStatesProblem says it states the
	// defect, or when the trailing clause opens with a qualifier or an
	// anaphor ("but only when", "but this flag"), which reads as nothing
	// on its own.
	KeepProblemClause bool
	// HeadStatesProblem reports whether a leading clause already says what
	// goes wrong; nil treats every leading clause as context.
	HeadStatesProblem func(string) bool
	// MarkDroppedLead renders a dropped leading clause as the marker plus its
	// connective ("[…] but ..."), the pre-2026-10-14 form; off, the text
	// starts at the problem clause, capitalised and unmarked.
	MarkDroppedLead bool
}

// Clamp cuts s to max runes. A text within the limit is returned unchanged.
// A longer one first loses its parentheticals, left to right, when that
// alone brings it within the limit, so "X (aside) does Y, so Z" keeps its
// consequence. Otherwise it is cut before the limit at the end of a
// sentence, else a clause joint, else a word, never inside an open pair of
// parentheses, brackets, code span or double quotes, and ends with Marker.
// When no balanced cut exists in the window, the cut lands on the last
// word and the open pairs are closed after the marker.
func Clamp(s string, max int) string {
	return ClampWith(s, max, Options{})
}

// ClampWith is Clamp under the given options.
func ClampWith(s string, max int, opts Options) string {
	out := clampWith(s, max, opts)
	if opts.KeepProblemClause && !opts.MarkDroppedLead {
		return StandAlone(out)
	}
	return out
}

func clampWith(s string, max int, opts Options) string {
	if len([]rune(s)) <= max {
		return s
	}
	if reduced, ok := dropParentheticals(s, max); ok {
		return reduced
	}
	if opts.KeepProblemClause {
		if tail, ok := problemClause(s, opts); ok {
			lead, budget := "", max
			if opts.MarkDroppedLead {
				lead, budget = leadMarker, max-len([]rune(leadMarker))
			}
			if budget >= minTailRunes {
				if len([]rune(tail)) <= budget {
					return lead + tail
				}
				if reduced, ok := dropParentheticals(tail, budget); ok {
					return lead + reduced
				}
				// A tail cut at both ends says less than a right cut that
				// keeps more of the text.
				fromTail, fromRight := lead+cutRight(tail, budget, opts), cutRight(s, max, opts)
				if len([]rune(fromTail)) >= len([]rune(fromRight)) {
					return fromTail
				}
				return fromRight
			}
		}
	}
	return cutRight(s, max, opts)
}

// leadMarker opens a text whose leading clause was dropped.
const leadMarker = "[…] "

// minTailRunes is the least room a problem clause gets; under it a marker
// and a few words would say less than a cut from the right.
const minTailRunes = 40

// problemJoints introduce the clause a sentence builds up to: the contrast
// after the context. Each is matched case-insensitively outside open pairs.
var problemJoints = []string{", but ", "; but ", " but ", ", yet ", "; yet ", ", however, ", "; however, ", " however, ", ", whereas "}

// tailOpeners are the words a trailing clause cannot open with and still
// stand alone: a qualifier narrows the leading clause's problem, an anaphor
// points back at it.
var tailOpeners = map[string]bool{
	"only": true, "just": true, "merely": true, "solely": true, "except": true, "unless": true, "if": true,
	"when": true, "whenever": true, "where": true, "while": true, "until": true, "not": true,
	"this": true, "that": true, "these": true, "those": true, "it": true, "its": true, "they": true,
	"their": true, "there": true, "such": true, "then": true,
}

// subordinators open a clause that cannot be the problem on its own
// ("Where A but B, C" states its problem in C).
var subordinators = map[string]bool{
	"where": true, "when": true, "whenever": true, "while": true, "if": true, "unless": true, "although": true,
	"though": true, "because": true, "since": true, "after": true, "before": true, "once": true, "as": true,
	"given": true, "whereas": true, "until": true, "even": true,
}

// coordinators join two clauses that each stand alone; one at the start of
// a line is dropped.
var coordinators = map[string]bool{"but": true, "yet": true, "and": true, "or": true, "nor": true, "so": true, "however": true, "whereas": true}

// clauseOpeners are the words a problem clause cannot open with: a
// subordinator or a relative leaves the clause hanging on what preceded it.
var clauseOpeners = map[string]bool{
	"which": true, "because": true, "although": true, "though": true, "since": true, "whereas": true, "and": true,
	"or": true, "nor": true, "so": true, "as": true, "thus": true, "hence": true, "rather": true, "also": true,
	"even": true, "still": true, "instead": true, "yet": true, "but": true, "however": true,
}

type joint struct{ at, end int }

// problemClause is the text from the first contrast joint, in text order,
// at which the context before it and the clause after it have a few words,
// the context does not already state the problem and the clause stands
// alone. With MarkDroppedLead the clause keeps its connective; otherwise
// it starts after the connective, capitalised, and a joint inside an
// unclosed subordinate clause yields the main clause that follows it.
func problemClause(s string, opts Options) (string, bool) {
	lower := asciiLower(s)
	balanced := balancedPrefixes(s)
	var joints []joint
	for _, j := range problemJoints {
		for from := 0; ; {
			i := strings.Index(lower[from:], j)
			if i < 0 {
				break
			}
			i += from
			if balanced[i] {
				joints = append(joints, joint{i, i + len(j)})
			}
			from = i + 1
		}
	}
	sort.Slice(joints, func(a, b int) bool { return joints[a].at < joints[b].at })
	for _, j := range joints {
		head := s[:j.at]
		if len(strings.Fields(head)) < 2 {
			continue
		}
		if opts.HeadStatesProblem != nil && opts.HeadStatesProblem(head) {
			return "", false
		}
		if opts.MarkDroppedLead {
			tail := strings.TrimLeft(s[j.at:], " ,;")
			words := strings.Fields(tail)
			if len(words) < 4 || tailOpeners[cleanWord(words[1])] {
				continue
			}
			return tail, true
		}
		// A head that opens with a subordinator may still be open at the
		// joint even when a list comma sits inside it ("Where a, b and c
		// share X but differ by Y, Z"), so the main clause is tried first.
		if opensWithSubordinator(head) {
			if main, ok := mainClauseAfter(s, j.end, balanced); ok {
				return capitalise(main), true
			}
			if subordinateOpen(head, balanced) {
				continue
			}
		}
		clause := strings.TrimLeft(s[j.end:], " ,;")
		if words := strings.Fields(clause); len(words) < 3 || tailOpeners[cleanWord(words[0])] || clauseOpeners[cleanWord(words[0])] {
			continue
		}
		return capitalise(clause), true
	}
	return "", false
}

// subordinateOpen reports whether head is a subordinate clause still open
// at its end: it starts with a subordinator and no balanced comma has
// closed it.
func subordinateOpen(head string, balanced []bool) bool {
	return opensWithSubordinator(head) && balancedComma(head, 0, balanced) < 0
}

func opensWithSubordinator(head string) bool {
	words := strings.Fields(head)
	return len(words) > 0 && subordinators[cleanWord(words[0])]
}

// mainClauseAfter is the clause after the first balanced comma at or past
// from whose first word can open a line, when it has a few words; a
// participle ("leaving", "causing") continues the clause before it.
func mainClauseAfter(s string, from int, balanced []bool) (string, bool) {
	for i := balancedComma(s, from, balanced); i >= 0; i = balancedComma(s, i+1, balanced) {
		clause := strings.TrimLeft(s[i+1:], " ,;")
		words := strings.Fields(clause)
		if len(words) < 4 {
			return "", false
		}
		if first := cleanWord(words[0]); tailOpeners[first] || clauseOpeners[first] || participle(first) {
			continue
		}
		return clause, true
	}
	return "", false
}

func participle(w string) bool {
	return strings.HasSuffix(w, "ing") && !strings.HasSuffix(w, "thing")
}

// balancedComma is the offset of the first ", " at or past from that sits
// outside every open pair, or -1.
func balancedComma(s string, from int, balanced []bool) int {
	for i := from; ; {
		j := strings.Index(s[i:], ", ")
		if j < 0 {
			return -1
		}
		if balanced[i+j] {
			return i + j
		}
		i += j + 1
	}
}

func cleanWord(w string) string {
	return strings.Trim(strings.ToLower(w), "`,;:.")
}

// lowercaseNames are tools and branches written in lowercase that a line
// may open with.
var lowercaseNames = map[string]bool{
	"npm": true, "npx": true, "pnpm": true, "yarn": true, "pip": true, "git": true, "go": true, "node": true,
	"docker": true, "kubectl": true, "make": true, "main": true, "master": true,
}

// capitalise upper-cases the first letter of s when its first word is a
// plain lowercase word, hyphens and apostrophes included ("free-approved",
// "don't"); an identifier ("purchaser_id", "isMicBlocked"), a code span, a
// number or a lowercase tool or branch name ("npm", "main") is left alone.
func capitalise(s string) string {
	words := strings.Fields(s)
	if len(words) == 0 {
		return s
	}
	if lowercaseNames[cleanWord(words[0])] {
		return s
	}
	for _, r := range strings.TrimRight(words[0], ",;:.") {
		if strings.ContainsRune("-'’", r) {
			continue
		}
		if !unicode.IsLetter(r) || unicode.IsUpper(r) {
			return s
		}
	}
	r, n := utf8.DecodeRuneInString(s)
	return string(unicode.ToUpper(r)) + s[n:]
}

// StandAlone makes s open a line: a leading marker or lowercase
// coordinating connective ("[…] but", "however,") is dropped and the first
// word is capitalised when it is a plain lowercase function word or
// connective. Prose that already opens a line ("So far ...", "Nor does
// ...") is left alone, as is a text that is nothing but the marker.
func StandAlone(s string) string {
	out, stripped := strings.TrimSpace(s), false
	for {
		rest, ok := stripLead(out, stripped)
		if !ok {
			break
		}
		out, stripped = rest, true
	}
	if out == "" {
		return s
	}
	if words := strings.Fields(out); stripped || functionWords[cleanWord(words[0])] || tailOpeners[cleanWord(words[0])] || OpensWithMarkerOrConnective(out) {
		return capitalise(out)
	}
	return out
}

// stripLead drops a leading marker, or a coordinator that is lowercase or
// follows a stripped marker.
func stripLead(s string, afterMarker bool) (string, bool) {
	if mark := strings.TrimSpace(Marker); strings.HasPrefix(s, mark) {
		return strings.TrimLeft(s[len(mark):], " ,;:"), true
	}
	words := strings.Fields(s)
	if len(words) < 2 || !coordinators[cleanWord(words[0])] {
		return s, false
	}
	if first := strings.Trim(words[0], ",;:"); !afterMarker && first != strings.ToLower(first) {
		return s, false
	}
	return strings.TrimLeft(strings.TrimPrefix(s, words[0]), " ,;:"), true
}

// OpensWithMarkerOrConnective reports whether s starts with the marker or
// a lowercase connective, which is how a clamp that dropped the leading
// clause used to read.
func OpensWithMarkerOrConnective(s string) bool {
	s = strings.TrimSpace(s)
	if strings.HasPrefix(s, strings.TrimSpace(Marker)) {
		return true
	}
	words := strings.Fields(s)
	if len(words) == 0 {
		return false
	}
	first := strings.Trim(words[0], ",;:")
	if first != strings.ToLower(first) {
		return false
	}
	return coordinators[first] || subordinators[first] || clauseOpeners[first] || first == "only" || first == "that" || first == "which"
}

// asciiLower lowers the ASCII letters only, so every byte offset in the
// result is an offset in s.
func asciiLower(s string) string {
	return strings.Map(func(r rune) rune {
		if 'A' <= r && r <= 'Z' {
			return r + 'a' - 'A'
		}
		return r
	}, s)
}

// cutRight cuts s from the right to max runes: at the end of a sentence,
// else a clause joint, else a word, outside every open pair, and ends with
// Marker; with KeepProblemClause never on a connective.
func cutRight(s string, max int, opts Options) string {
	runes := []rune(s)
	budget := max - len([]rune(Marker))
	if budget < 1 {
		return strings.TrimSpace(Marker)
	}
	window := string(runes[:budget])
	balanced := balancedPrefixes(window)
	floor := len(window) / 3
	cut := -1
	// A whole first sentence reads better than a longer cut mid-sentence,
	// so a sentence end counts from a quarter of the window on.
	for i := len(window) - 1; i >= len(window)/4; i-- {
		if end, ok := sentenceEnd(window, i); ok && end <= len(window) && balanced[end] {
			cut = end
			break
		}
	}
	if cut < 0 {
		for _, b := range ClauseBoundaries {
			if i := strings.LastIndex(window, b); i > cut && i >= floor && balanced[i] {
				cut = i
			}
		}
		if cut >= 0 && opts.KeepProblemClause {
			cut = beforeConnectives(window, cut, floor)
		}
	}
	if cut < 0 {
		cut = wordCut(window, floor, balanced)
	}
	if cut >= 0 {
		return strings.TrimRight(window[:cut], " ,;:") + Marker
	}
	head := window
	if i := strings.LastIndex(head, " "); i > 0 {
		head = head[:i]
	}
	for {
		head = strings.TrimRight(head, " ,;:")
		if opts.KeepProblemClause && strings.Contains(head, " ") && functionWords[lastWord(head)] {
			head = head[:strings.LastIndex(head, " ")]
			continue
		}
		closed := head + Marker + closers(head)
		if len([]rune(closed)) <= max || head == "" {
			return closed
		}
		if i := strings.LastIndex(head, " "); i > 0 {
			head = head[:i]
		} else {
			r := []rune(head)
			head = string(r[:len(r)-1])
		}
	}
}

// wordCut is the last balanced space at or beyond floor whose preceding
// word is not a function word, so a cut does not end on "via" or "the";
// -1 when there is none.
func wordCut(window string, floor int, balanced []bool) int {
	for i := len(window) - 1; i >= floor; i-- {
		if window[i] != ' ' || !balanced[i] {
			continue
		}
		if !functionWords[lastWord(window[:i])] {
			return i
		}
	}
	return -1
}

// beforeConnectives moves a clause cut back over the function words it
// would otherwise end on ("However", "only when"), or to -1 when that
// drops it under floor.
func beforeConnectives(window string, cut, floor int) int {
	for cut >= floor {
		head := strings.TrimRight(window[:cut], " ,;:")
		if !functionWords[lastWord(head)] {
			return cut
		}
		cut = strings.LastIndex(head, " ")
	}
	return -1
}

func lastWord(s string) string {
	return strings.ToLower(strings.Trim(s[strings.LastIndex(s, " ")+1:], "`,;:"))
}

var functionWords = map[string]bool{
	"a": true, "an": true, "the": true, "of": true, "to": true, "in": true, "on": true, "at": true, "by": true,
	"for": true, "from": true, "via": true, "with": true, "without": true, "into": true, "onto": true, "over": true,
	"under": true, "per": true, "as": true, "than": true, "and": true, "or": true, "but": true, "nor": true,
	"so": true, "yet": true, "is": true, "are": true, "was": true, "were": true, "be": true, "been": true,
	"that": true, "which": true, "who": true, "whose": true, "when": true, "where": true, "while": true,
	"not": true, "no": true, "its": true, "their": true, "this": true, "these": true, "those": true,
	"however": true, "only": true, "unless": true, "because": true, "although": true, "though": true,
	"whereas": true, "since": true, "if": true, "then": true, "thus": true, "hence": true, "until": true,
	"even": true, "still": true, "also": true, "either": true, "neither": true, "whether": true,
}

// FirstSentence returns the leading sentence of line: up to the first
// period, exclamation or question mark that ends a word and sits outside
// every open pair; failing that, the first one anywhere; failing that, the
// whole line.
func FirstSentence(line string) string {
	balanced := balancedPrefixes(line)
	first := -1
	for i := 0; i < len(line); i++ {
		end, ok := sentenceEnd(line, i)
		if !ok {
			continue
		}
		if balanced[end] {
			return line[:end]
		}
		if first < 0 {
			first = end
		}
	}
	if first > 0 {
		return line[:first]
	}
	return line
}

// sentenceEnd reports whether a sentence ends at byte i: a period,
// exclamation or question mark, then any closing quotes, then a space or
// the end of the text; a closing bracket is not a sentence end, so
// "(e.g.)" stays inside its sentence. end is the offset past the quotes.
func sentenceEnd(s string, i int) (end int, ok bool) {
	switch s[i] {
	case '.', '!', '?':
	default:
		return 0, false
	}
	end = i + 1
	for end < len(s) {
		r, n := utf8.DecodeRuneInString(s[end:])
		if !strings.ContainsRune(`'"”’`, r) {
			break
		}
		end += n
	}
	if end == len(s) || s[end] == ' ' {
		return end, true
	}
	return 0, false
}

// pairs tracks the delimiters open at a point in the text. Inside a code
// span only the closing backtick counts; single quotes are apostrophes
// more often than pairs, so they are not tracked.
type pairs struct {
	stack []rune
}

func (p *pairs) feed(c rune) {
	if n := len(p.stack); n > 0 && p.stack[n-1] == '`' {
		if c == '`' {
			p.stack = p.stack[:n-1]
		}
		return
	}
	switch c {
	case '`', '(', '[', '“':
		p.stack = append(p.stack, c)
	case ')':
		p.pop('(')
	case ']':
		p.pop('[')
	case '”':
		p.pop('“')
	case '"':
		if n := len(p.stack); n > 0 && p.stack[n-1] == '"' {
			p.stack = p.stack[:n-1]
		} else {
			p.stack = append(p.stack, c)
		}
	}
}

func (p *pairs) pop(opener rune) {
	if n := len(p.stack); n > 0 && p.stack[n-1] == opener {
		p.stack = p.stack[:n-1]
	}
}

// balancedPrefixes reports, for every byte offset i, whether s[:i] leaves
// no pair open.
func balancedPrefixes(s string) []bool {
	out := make([]bool, len(s)+1)
	out[0] = true
	var p pairs
	for i, c := range s {
		p.feed(c)
		out[i+len(string(c))] = len(p.stack) == 0
	}
	return out
}

var closerOf = map[rune]string{'`': "`", '(': ")", '[': "]", '“': "”", '"': `"`}

func closers(s string) string {
	var p pairs
	for _, c := range s {
		p.feed(c)
	}
	var b strings.Builder
	for i := len(p.stack) - 1; i >= 0; i-- {
		b.WriteString(closerOf[p.stack[i]])
	}
	return b.String()
}

// dropParentheticals replaces the outermost "(...)" groups outside code
// spans with the marker, left to right, until s fits max runes.
func dropParentheticals(s string, max int) (string, bool) {
	for {
		start, end, found := firstParenthetical(s)
		if !found {
			return s, false
		}
		s = s[:start] + strings.TrimSpace(Marker) + s[end:]
		if len([]rune(s)) <= max {
			return strings.TrimSuffix(s, "."), true
		}
	}
}

// firstParenthetical locates the first balanced parenthesis group outside
// a code span, as byte offsets [start, end).
func firstParenthetical(s string) (start, end int, found bool) {
	var p pairs
	start = -1
	for i, c := range s {
		p.feed(c)
		switch {
		case c == '(' && start < 0 && len(p.stack) == 1 && p.stack[0] == '(':
			start = i
		case c == ')' && start >= 0 && len(p.stack) == 0:
			return start, i + 1, true
		}
	}
	return 0, 0, false
}
