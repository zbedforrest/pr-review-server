// Package heal recovers an agent's findings JSON when the model emitted it
// with broken structure, without changing a word of its content.
//
// Repairs only move structure: brackets, quotes, commas, stray tails and
// split objects. A repaired document is accepted only if it passes three
// checks against the raw answer: every string value in it appears verbatim in
// the raw text (nothing changed or invented), it holds at least as many
// findings as the raw text has file_path keys, and every key name occurs in it
// at least as often as in the raw text (no finding or field was dropped).
package heal

import (
	"encoding/json"
	"errors"
	"regexp"
	"strings"

	"github.com/kaptinlin/jsonrepair"
)

// Method names the step that produced an accepted document.
const (
	MethodClean      = "clean"
	MethodRebalanced = "rebalanced"
	MethodRepaired   = "repaired"
	MethodRequoted   = "requoted"
	MethodTrimmed    = "trimmed"
)

// ErrNoJSON means the answer carries no JSON object at all, such as a prose
// verdict with no findings. That is a valid answer, not a failure to heal.
var ErrNoJSON = errors.New("heal: answer has no JSON object")

// ErrUnrecoverable means no structural repair passed both checks.
var ErrUnrecoverable = errors.New("heal: no repair preserved the answer's content")

// Result is a healed findings document, re-encoded as canonical JSON with a
// top-level "findings" array.
type Result struct {
	JSON     []byte
	Method   string
	Findings int
}

// minCheckedLen skips short strings (enums like "LOW", ids like "A-1") in the
// verbatim check: they carry no claim and may be re-quoted by a repair.
const minCheckedLen = 4

var anyKey = regexp.MustCompile(`["']([A-Za-z_][A-Za-z0-9_]*)["']\s*:`)

// Heal returns the findings document inside raw, repaired if needed.
func Heal(raw string) (Result, error) {
	start := strings.IndexAny(raw, "{[")
	if start < 0 {
		return Result{}, ErrNoJSON
	}
	body := raw[start:]
	want := keyCounts(body)
	haystack := normalizeRaw(raw)
	for _, a := range repairAttempts(body) {
		src, err := a.src()
		if err != nil {
			continue
		}
		if res, ok := accept(src, a.method, want, haystack); ok {
			return res, nil
		}
	}
	// Last resort: a garbage tail no repairer can parse through. Try each
	// prefix ending at a closing brace or quote, longest first; the checks
	// still decide.
	for i, tries := len(body)-1, 0; i > 0 && tries < maxTrimTries; i-- {
		if body[i] != '}' && body[i] != '"' {
			continue
		}
		tries++
		if res, ok := accept(Rebalance(body[:i+1]), MethodTrimmed, want, haystack); ok {
			return res, nil
		}
	}
	return Result{}, ErrUnrecoverable
}

// maxTrimTries bounds the prefix search; real tails are a few dozen bytes.
const maxTrimTries = 64

func accept(src, method string, want map[string]int, haystack string) (Result, bool) {
	var doc any
	if json.Unmarshal([]byte(src), &doc) != nil {
		return Result{}, false
	}
	findings, ok := findingsOf(doc)
	if !ok {
		return Result{}, false
	}
	findings = joinFragments(findings)
	// Keep every top-level key (a summary block beside the findings, say):
	// dropping one is losing content.
	top, isObject := doc.(map[string]any)
	if !isObject {
		top = map[string]any{}
	}
	top["findings"] = findings
	if !keysComplete(top, want) || !allVerbatim(top, haystack) {
		return Result{}, false
	}
	out, err := json.Marshal(top)
	if err != nil {
		return Result{}, false
	}
	return Result{JSON: out, Method: method, Findings: len(findings)}, true
}

var (
	quoteBeforeDelim = regexp.MustCompile(`\\?'(\s*[,:}\]])`)
	quoteAfterDelim  = regexp.MustCompile(`([,:{\[]\s*)\\?'`)
)

// requoteDelimiters turns ' (or \') into " where it stands next to a JSON
// delimiter, for answers where the model switched to single quotes around keys.
func requoteDelimiters(s string) string {
	s = quoteBeforeDelim.ReplaceAllString(s, `"$1`)
	return quoteAfterDelim.ReplaceAllString(s, `$1"`)
}

// trimTrailing drops prose after the last closing bracket, so a clean
// document followed by commentary still parses as-is.
func trimTrailing(s string) string {
	if i := strings.LastIndexAny(s, "}]"); i >= 0 {
		return s[:i+1]
	}
	return s
}

func findingsOf(doc any) ([]any, bool) {
	switch v := doc.(type) {
	case []any:
		return v, true
	case map[string]any:
		f, ok := v["findings"].([]any)
		return f, ok
	}
	return nil, false
}

// joinFragments folds an object back into the one before it when the model
// closed that object early and carried on writing its keys: the fragment has
// no file_path and shares no key with its predecessor.
func joinFragments(findings []any) []any {
	var out []any
	for _, f := range findings {
		obj, ok := f.(map[string]any)
		if !ok {
			continue
		}
		if len(out) > 0 {
			if prev, ok := out[len(out)-1].(map[string]any); ok && isFragmentOf(obj, prev) {
				for k, v := range obj {
					prev[k] = v
				}
				continue
			}
		}
		out = append(out, obj)
	}
	return out
}

func isFragmentOf(obj, prev map[string]any) bool {
	if _, has := obj["file_path"]; has {
		return false
	}
	for k := range obj {
		if _, dup := prev[k]; dup {
			return false
		}
	}
	return true
}

// keyCounts counts key names as they appear in the raw text, quoted with
// either quote style.
func keyCounts(raw string) map[string]int {
	counts := map[string]int{}
	for _, m := range anyKey.FindAllStringSubmatch(raw, -1) {
		counts[m[1]]++
	}
	return counts
}

// keysComplete is true when every key name counted in the raw text occurs at
// least as often in the decoded document: no finding and no field was lost.
func keysComplete(doc any, want map[string]int) bool {
	got := map[string]int{}
	var walk func(v any)
	walk = func(v any) {
		switch t := v.(type) {
		case []any:
			for _, e := range t {
				walk(e)
			}
		case map[string]any:
			for k, e := range t {
				got[k]++
				walk(e)
			}
		}
	}
	walk(doc)
	for k, n := range want {
		if got[k] < n {
			return false
		}
	}
	return true
}

func allVerbatim(v any, haystack string) bool {
	switch t := v.(type) {
	case string:
		return len(t) < minCheckedLen || strings.Contains(haystack, normalize(t))
	case []any:
		for _, e := range t {
			if !allVerbatim(e, haystack) {
				return false
			}
		}
	case map[string]any:
		for _, e := range t {
			if !allVerbatim(e, haystack) {
				return false
			}
		}
	}
	return true
}

var rawEscapes = strings.NewReplacer(`\"`, `"`, `\'`, `'`, `\n`, " ", `\t`, " ", `\r`, " ", `\/`, "/", `\\`, `\`)

// normalizeRaw decodes the escapes a model writes inside JSON strings so a
// decoded value can be found in the raw text it came from.
func normalizeRaw(raw string) string {
	return normalize(rawEscapes.Replace(raw))
}

func normalize(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

type repairAttempt struct {
	method string
	src    func() (string, error)
}

func repairAttempts(body string) []repairAttempt {
	return []repairAttempt{
		{MethodClean, func() (string, error) { return trimTrailing(body), nil }},
		{MethodRebalanced, func() (string, error) { return Rebalance(body), nil }},
		{MethodRequoted, func() (string, error) { return jsonrepair.Repair(requoteDelimiters(body)) }},
		{MethodRepaired, func() (string, error) { return jsonrepair.Repair(body) }},
	}
}

// HealObject returns the JSON object inside raw, repaired if needed, under the
// same guarantees as Heal: every key the raw text names survives and every
// string value appears verbatim in raw, so a repair never invents content.
func HealObject(raw string) ([]byte, string, error) {
	start := strings.Index(raw, "{")
	if start < 0 {
		return nil, "", ErrNoJSON
	}
	body := raw[start:]
	want := keyCounts(body)
	haystack := normalizeRaw(raw)
	for _, a := range repairAttempts(body) {
		src, err := a.src()
		if err != nil {
			continue
		}
		var doc map[string]any
		if json.Unmarshal([]byte(src), &doc) != nil || !keysComplete(doc, want) || !allVerbatim(doc, haystack) {
			continue
		}
		out, err := json.Marshal(doc)
		if err != nil {
			continue
		}
		return out, a.method, nil
	}
	return nil, "", ErrUnrecoverable
}
