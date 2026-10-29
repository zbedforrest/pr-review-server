// Package rewrite turns a finding the publisher is about to post inline into
// a short title and a direct body (trigger, consequence, one fix), the shape
// authors act on; the full explanation stays in the comment's details.
package rewrite

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"strings"
)

// Item is one finding to rewrite. ID is unique within a batch.
type Item struct {
	ID          string `json:"id"`
	Severity    string `json:"severity,omitempty"`
	Headline    string `json:"headline,omitempty"`
	Impact      string `json:"impact,omitempty"`
	Caveat      string `json:"caveat,omitempty"`
	Explanation string `json:"explanation"`
}

// Result is the rewrite of one item.
type Result struct {
	ID    string `json:"id"`
	Title string `json:"title"`
	Body  string `json:"body"`
}

// Limits a rewrite must meet to be used: the prompt's 6 and 80 words with a
// little slack, since a reply a word over is still the shape asked for.
const (
	MaxTitleWords = 8
	MaxBodyWords  = 100
)

// JSONSchema is the reply shape, handed to the CLI as its structured-output
// schema so a stray quote cannot lose a whole batch.
const JSONSchema = `{"type":"object","additionalProperties":false,"required":["items"],"properties":{"items":{"type":"array","items":{"type":"object","additionalProperties":false,"required":["id","title","body"],"properties":{"id":{"type":"string"},"title":{"type":"string"},"body":{"type":"string"}}}}}}`

//go:embed prompt.md
var promptTemplate string

// Prompt renders the rewrite prompt for one batch.
func Prompt(items []Item) (string, error) {
	data, err := json.MarshalIndent(items, "", " ")
	if err != nil {
		return "", err
	}
	return strings.Replace(promptTemplate, "{{ITEMS}}", string(data), 1), nil
}

// ParseReply reads the model's reply and keeps the rewrites that name a
// known item and meet the limits; items without a usable rewrite are absent.
func ParseReply(raw string, items []Item) ([]Result, error) {
	start, end := strings.Index(raw, "{"), strings.LastIndex(raw, "}")
	if start < 0 || end <= start {
		return nil, fmt.Errorf("rewrite: no JSON object in the reply")
	}
	var reply struct {
		Items []Result `json:"items"`
	}
	if err := json.Unmarshal([]byte(raw[start:end+1]), &reply); err != nil {
		return nil, fmt.Errorf("rewrite: %w", err)
	}
	known := make(map[string]bool, len(items))
	for _, it := range items {
		known[it.ID] = true
	}
	var out []Result
	for _, r := range reply.Items {
		r.Title = strings.TrimSuffix(strings.TrimSpace(r.Title), ".")
		r.Body = strings.TrimSpace(r.Body)
		if !known[r.ID] || !usable(r) {
			continue
		}
		known[r.ID] = false
		out = append(out, r)
	}
	return out, nil
}

func usable(r Result) bool {
	if r.Title == "" || r.Body == "" || strings.ContainsAny(r.Title, "\n`*") || strings.Contains(r.Body, "```") {
		return false
	}
	return len(strings.Fields(r.Title)) <= MaxTitleWords && len(strings.Fields(r.Body)) <= MaxBodyWords
}
