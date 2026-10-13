// Package textcut shortens one line of prose for a headline or a bullet
// without leaving a reader inside a half-finished clause or an open
// delimiter.
package textcut

import "strings"

// Marker ends a cut: an editorial omission mark the agent's own prose never
// produces, so a reader can tell a cut from a sentence left hanging. Inside
// a text it stands for a dropped parenthetical.
const Marker = " […]"

// ClauseBoundaries are the joints an effect sentence is most often built
// around; cutting at the last one before the limit keeps a line readable.
var ClauseBoundaries = []string{", so ", "; ", " because ", ", which ", ", and ", ", causing ", ", leaving ", ", "}

// Clamp cuts s to max runes. A text within the limit is returned unchanged.
// A longer one first loses its parentheticals, left to right, when that
// alone brings it within the limit, so "X (aside) does Y, so Z" keeps its
// consequence. Otherwise it is cut before the limit at the end of a
// sentence, else a clause joint, else a word, never inside an open pair of
// parentheses, brackets, code span or double quotes, and ends with Marker.
// When no balanced cut exists in the window, the cut lands on the last
// word and the open pairs are closed after the marker.
func Clamp(s string, max int) string {
	runes := []rune(s)
	if len(runes) <= max {
		return s
	}
	if reduced, ok := dropParentheticals(s, max); ok {
		return reduced
	}
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
		if sentenceEndAt(window, i) && balanced[i+1] {
			cut = i + 1
			break
		}
	}
	if cut < 0 {
		for _, b := range ClauseBoundaries {
			if i := strings.LastIndex(window, b); i > cut && i >= floor && balanced[i] {
				cut = i
			}
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
		if !functionWords[strings.ToLower(window[strings.LastIndex(window[:i], " ")+1:i])] {
			return i
		}
	}
	return -1
}

var functionWords = map[string]bool{
	"a": true, "an": true, "the": true, "of": true, "to": true, "in": true, "on": true, "at": true, "by": true,
	"for": true, "from": true, "via": true, "with": true, "without": true, "into": true, "onto": true, "over": true,
	"under": true, "per": true, "as": true, "than": true, "and": true, "or": true, "but": true, "nor": true,
	"so": true, "yet": true, "is": true, "are": true, "was": true, "were": true, "be": true, "been": true,
	"that": true, "which": true, "who": true, "whose": true, "when": true, "where": true, "while": true,
	"not": true, "no": true, "its": true, "their": true, "this": true, "these": true, "those": true,
}

// FirstSentence returns the leading sentence of line: up to the first
// period, exclamation or question mark that ends a word and sits outside
// every open pair; failing that, the first one anywhere; failing that, the
// whole line.
func FirstSentence(line string) string {
	balanced := balancedPrefixes(line)
	first := -1
	for i := 0; i < len(line); i++ {
		if !sentenceEndAt(line, i) {
			continue
		}
		if balanced[i+1] {
			return line[:i+1]
		}
		if first < 0 {
			first = i + 1
		}
	}
	if first > 0 {
		return line[:first]
	}
	return line
}

func sentenceEndAt(s string, i int) bool {
	switch s[i] {
	case '.', '!', '?':
		return i+1 == len(s) || s[i+1] == ' '
	}
	return false
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
