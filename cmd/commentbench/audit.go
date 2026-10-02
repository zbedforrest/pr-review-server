package main

import (
	"encoding/json"
	"fmt"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// audit is the subset of the comment-system audit the builder reads.
type audit struct {
	PerPR    []auditPR      `json:"perPr"`
	Verified []auditPattern `json:"verified"`
}

type auditPR struct {
	Key            string          `json:"key"`
	ReadOK         bool            `json:"read_ok"`
	Comments       []auditComment  `json:"comments"`
	HumanResponses []auditResponse `json:"human_responses"`
	Patterns       []string        `json:"patterns"`
	IdealBehavior  string          `json:"ideal_behavior"`
}

type auditComment struct {
	CommentID           int64  `json:"comment_id"`
	Path                string `json:"path"`
	Line                int    `json:"line"`
	Created             string `json:"created"`
	Headline            string `json:"headline"`
	Correctness         string `json:"correctness"`
	FirstRaisedBy       string `json:"first_raised_by"`
	RedundantRepost     bool   `json:"redundant_repost"`
	RepostOfCommentID   int64  `json:"repost_of_comment_id"`
	AddressedBeforePost bool   `json:"addressed_before_post"`
	SeverityShift       string `json:"severity_shift"`
	AddsContext         bool   `json:"adds_context"`
}

type auditResponse struct {
	CommentID         int64     `json:"comment_id"`
	Login             string    `json:"login"`
	Created           time.Time `json:"created"`
	Class             string    `json:"class"`
	Quote             string    `json:"quote"`
	PrismReplied      bool      `json:"prism_replied"`
	PrismReplyQuality string    `json:"prism_reply_quality"`
}

type auditPattern struct {
	Pattern string `json:"pattern"`
	Votes   []struct {
		Real              bool     `json:"real"`
		ExamplesConfirmed []string `json:"examples_confirmed"`
		ExamplesRefuted   []string `json:"examples_refuted"`
	} `json:"votes"`
}

func loadAudit(path string) (*audit, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var a audit
	if err := json.Unmarshal(raw, &a); err != nil {
		return nil, fmt.Errorf("audit: %w", err)
	}
	norm := func(s string) string { return strings.ToLower(strings.TrimSpace(s)) }
	for i := range a.PerPR {
		for j := range a.PerPR[i].Comments {
			c := &a.PerPR[i].Comments[j]
			c.Correctness, c.FirstRaisedBy = norm(c.Correctness), norm(c.FirstRaisedBy)
		}
		for j := range a.PerPR[i].HumanResponses {
			h := &a.PerPR[i].HumanResponses[j]
			h.Class, h.PrismReplyQuality = norm(h.Class), norm(h.PrismReplyQuality)
		}
	}
	return &a, nil
}

// examples indexes the verifiers' verdicts by PR key. An example that names
// comment ids covers those comments only; one that names none covers the PR.
type examples struct {
	confirmed map[string][]example
	refuted   map[string][]example
}

type example struct {
	Pattern string
	IDs     map[int64]bool
}

var (
	exampleKeyRe = regexp.MustCompile(`^\s*([\w.-]+/[\w.-]+)#(\d+)`)
	commentIDRe  = regexp.MustCompile(`\b\d{10,}\b`)
)

func indexExamples(patterns []auditPattern) examples {
	ex := examples{confirmed: map[string][]example{}, refuted: map[string][]example{}}
	for _, p := range patterns {
		for _, v := range p.Votes {
			add := func(into map[string][]example, list []string) {
				for _, s := range list {
					m := exampleKeyRe.FindStringSubmatch(s)
					if m == nil {
						continue
					}
					key := m[1] + "#" + m[2]
					e := example{Pattern: p.Pattern, IDs: map[int64]bool{}}
					for _, id := range commentIDRe.FindAllString(s, -1) {
						n, _ := strconv.ParseInt(id, 10, 64)
						e.IDs[n] = true
					}
					into[key] = append(into[key], e)
				}
			}
			add(ex.confirmed, v.ExamplesConfirmed)
			add(ex.refuted, v.ExamplesRefuted)
		}
	}
	return ex
}

// covers returns the patterns of the examples that cover comment id on key
// (id 0 asks for PR-wide examples only).
func covers(list []example, id int64) []string {
	var out []string
	for _, e := range list {
		if len(e.IDs) == 0 || (id != 0 && e.IDs[id]) {
			out = append(out, e.Pattern)
		}
	}
	return out
}

func wholePR(list []example) bool {
	for _, e := range list {
		if len(e.IDs) == 0 {
			return true
		}
	}
	return false
}
