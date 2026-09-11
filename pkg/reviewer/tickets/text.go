package tickets

import (
	"encoding/json"
	"html"
	"regexp"
	"strings"
	"unicode"
)

var (
	htmlDropBlocks = regexp.MustCompile(`(?is)<(script|style)\b.*?</(script|style)\s*>`)
	htmlLineBreaks = regexp.MustCompile(`(?i)<br\s*/?>|</(p|div|li|tr|h[1-6]|blockquote|pre)\s*>|<li\b[^>]*>`)
	htmlTags       = regexp.MustCompile(`<[^>]*>`)
)

func htmlToText(s string) string {
	s = htmlDropBlocks.ReplaceAllString(s, "")
	s = htmlLineBreaks.ReplaceAllString(s, "\n")
	s = htmlTags.ReplaceAllString(s, "")
	return collapseWhitespace(html.UnescapeString(s))
}

// adfToText flattens an Atlassian Document Format JSON document to plain
// text: text nodes concatenate, block nodes (paragraphs, list items, headings)
// end with a newline, mentions render as their display text.
func adfToText(doc []byte) string {
	if len(doc) == 0 {
		return ""
	}
	var root adfNode
	if err := json.Unmarshal(doc, &root); err != nil {
		return ""
	}
	var b strings.Builder
	root.write(&b)
	return collapseWhitespace(b.String())
}

type adfNode struct {
	Type    string          `json:"type"`
	Text    string          `json:"text"`
	Attrs   json.RawMessage `json:"attrs"`
	Content []adfNode       `json:"content"`
}

func (n adfNode) write(b *strings.Builder) {
	switch n.Type {
	case "text":
		b.WriteString(n.Text)
		return
	case "hardBreak":
		b.WriteString("\n")
		return
	case "mention", "emoji", "status", "date", "inlineCard":
		b.WriteString(n.attrText())
		return
	}
	for _, child := range n.Content {
		child.write(b)
	}
	switch n.Type {
	case "paragraph", "heading", "listItem", "codeBlock", "blockquote", "tableRow", "rule", "mediaSingle":
		b.WriteString("\n")
	}
}

func (n adfNode) attrText() string {
	var attrs struct {
		Text      string `json:"text"`
		ShortName string `json:"shortName"`
		Timestamp string `json:"timestamp"`
		URL       string `json:"url"`
	}
	if err := json.Unmarshal(n.Attrs, &attrs); err != nil {
		return ""
	}
	for _, v := range []string{attrs.Text, attrs.ShortName, attrs.Timestamp, attrs.URL} {
		if v != "" {
			return " " + v
		}
	}
	return ""
}

// collapseWhitespace squeezes runs of horizontal whitespace to one space and
// runs of blank lines to one newline, trimming each line and the result.
func collapseWhitespace(s string) string {
	lines := strings.Split(s, "\n")
	out := make([]string, 0, len(lines))
	for _, line := range lines {
		line = strings.Join(strings.FieldsFunc(line, unicode.IsSpace), " ")
		if line != "" {
			out = append(out, line)
		}
	}
	return strings.Join(out, "\n")
}

func truncateRunes(s string, max int) string {
	if max <= 0 {
		return ""
	}
	runes := []rune(s)
	if len(runes) <= max {
		return s
	}
	if max <= 3 {
		return string(runes[:max])
	}
	return string(runes[:max-3]) + "..."
}
