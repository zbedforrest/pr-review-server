package reconcile

import (
	"strings"
	"unicode"
)

const minTokenLen = 3

var stopwords = map[string]bool{
	"the": true, "and": true, "for": true, "are": true, "but": true, "not": true,
	"you": true, "all": true, "any": true, "can": true, "has": true, "have": true,
	"was": true, "were": true, "will": true, "with": true, "this": true, "that": true,
	"these": true, "those": true, "from": true, "into": true, "when": true, "then": true,
	"than": true, "which": true, "where": true, "there": true, "their": true, "they": true,
	"its": true, "also": true, "been": true, "being": true, "should": true, "would": true,
	"could": true, "does": true, "did": true, "here": true, "only": true, "same": true,
	"some": true, "such": true, "very": true, "more": true, "most": true, "other": true,
	"what": true, "while": true, "because": true, "about": true, "after": true, "before": true,
}

// Similarity is the Jaccard similarity of the word-token sets of a and b:
// lower-cased runs of letters and digits, dropping tokens shorter than three
// characters and common English stopwords. Two empty sets have similarity 0.
func Similarity(a, b string) float64 {
	ta, tb := tokens(a), tokens(b)
	if len(ta) == 0 || len(tb) == 0 {
		return 0
	}
	inter := 0
	for tok := range ta {
		if tb[tok] {
			inter++
		}
	}
	union := len(ta) + len(tb) - inter
	return float64(inter) / float64(union)
}

func tokens(s string) map[string]bool {
	out := map[string]bool{}
	for _, tok := range strings.FieldsFunc(strings.ToLower(s), func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r)
	}) {
		if len(tok) < minTokenLen || stopwords[tok] {
			continue
		}
		out[tok] = true
	}
	return out
}
