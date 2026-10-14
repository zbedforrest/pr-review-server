// Package textcut shortens one line of prose for a headline or a bullet
// without leaving a reader inside a half-finished clause or an open
// delimiter.
package textcut

import (
	"sort"
	"strings"
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
	if len([]rune(s)) <= max {
		return s
	}
	if reduced, ok := dropParentheticals(s, max); ok {
		return reduced
	}
	if opts.KeepProblemClause {
		if tail, ok := problemClause(s, opts.HeadStatesProblem); ok {
			budget := max - len([]rune(leadMarker))
			if budget >= minTailRunes {
				if len([]rune(tail)) <= budget {
					return leadMarker + tail
				}
				if reduced, ok := dropParentheticals(tail, budget); ok {
					return leadMarker + reduced
				}
				// A tail cut at both ends says less than a right cut that
				// keeps more of the text.
				fromTail, fromRight := leadMarker+cutRight(tail, budget, opts), cutRight(s, max, opts)
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

// problemClause is the text from the first contrast joint, in text order,
// at which the context before it and the clause after it have a few words,
// the context does not already state the problem and the clause stands
// alone.
func problemClause(s string, headStatesProblem func(string) bool) (string, bool) {
	lower := asciiLower(s)
	balanced := balancedPrefixes(s)
	var joints []int
	for _, j := range problemJoints {
		for from := 0; ; {
			i := strings.Index(lower[from:], j)
			if i < 0 {
				break
			}
			i += from
			if balanced[i] {
				joints = append(joints, i)
			}
			from = i + 1
		}
	}
	sort.Ints(joints)
	for _, at := range joints {
		head, tail := s[:at], strings.TrimLeft(s[at:], " ,;")
		if len(strings.Fields(head)) < 2 || len(strings.Fields(tail)) < 4 {
			continue
		}
		if headStatesProblem != nil && headStatesProblem(head) {
			return "", false
		}
		if words := strings.Fields(tail); tailOpeners[strings.Trim(strings.ToLower(words[1]), "`,;:")] {
			continue
		}
		return tail, true
	}
	return "", false
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
