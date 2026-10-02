package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"pr-review-server/internal/replaykit"
	"pr-review-server/pkg/publisher"
	"pr-review-server/pkg/reviewer/payload"
)

const (
	TierGold     = "gold"
	TierAccepted = "accepted"
	TierExcluded = "excluded"

	ExpectPost     = "post"
	ExpectSuppress = "suppress"

	ActionNone     = "none"
	ActionReact    = "react_only"
	ActionConcede  = publisher.DecisionConcede
	ActionHold     = publisher.DecisionHold
	ActionAnswer   = publisher.DecisionAnswer
	ActionWithdraw = "withdraw"
)

// Suppression reasons, in the order the builder tests them.
const (
	ReasonRepost    = "repost"
	ReasonAddressed = "addressed_before_post"
	ReasonExternal  = "external"
	ReasonIncorrect = "incorrect"
	ReasonNoContext = "no_context"
)

// Case is one PR: its rounds as GitHub recorded them, the review sidecar of
// every round, the changed files between round heads, and what PRism should
// have done. A case file is self-contained so the runner needs no network.
type Case struct {
	ID      string   `json:"id"`
	Tier    string   `json:"tier"`
	TierWhy []string `json:"tier_why,omitempty"`
	Owner   string   `json:"owner"`
	Repo    string   `json:"repo"`
	Number  int      `json:"number"`
	Bot     string   `json:"bot"`

	Dump     replaykit.PRDump           `json:"dump"`
	Sidecars map[string]json.RawMessage `json:"sidecars"`           // sha7 -> review payload
	Compares map[string][]string        `json:"compares,omitempty"` // "base7...head7" -> changed files
	Rounds   []RoundInfo                `json:"rounds"`

	// Recorded is what the reply model decided for each author comment id, as
	// the reply ledger recorded it; the stub responder replays it.
	Recorded map[int64]RecordedDecision `json:"recorded_replies,omitempty"`

	Expect Expectations `json:"expect"`
}

// RoundInfo describes one round for readers of the case; the runner derives
// the same facts from the dump.
type RoundInfo struct {
	Index            int       `json:"index"`
	SHA              string    `json:"sha"`
	At               time.Time `json:"at"`
	HasSidecar       bool      `json:"has_sidecar"`
	CommentsPresent  []int64   `json:"comments_present,omitempty"`
	CommitsSincePrev []string  `json:"commits_since_prev,omitempty"`
	ChangedSincePrev []string  `json:"changed_since_prev,omitempty"`
}

type Expectations struct {
	Findings    []FindingExpect    `json:"findings,omitempty"`
	Summaries   []SummaryExpect    `json:"summaries,omitempty"`
	Resolutions []ResolutionExpect `json:"resolutions,omitempty"`
	Replies     []ReplyExpect      `json:"replies,omitempty"`
}

// FindingExpect is one historical PRism root comment and whether that round
// should have posted it.
type FindingExpect struct {
	CommentID int64    `json:"comment_id"`
	Round     int      `json:"round"`
	FindingID string   `json:"finding_id,omitempty"`
	File      string   `json:"file"`
	Line      int      `json:"line"`
	Text      string   `json:"text,omitempty"`
	Subjects  []string `json:"subjects,omitempty"`
	Headline  string   `json:"headline,omitempty"`
	MappedBy  string   `json:"mapped_by,omitempty"` // "alias" when the marker was gone from the sidecar
	Defect    int64    `json:"defect"`
	Expect    string   `json:"expect"`
	Reason    string   `json:"reason,omitempty"`
	Detail    string   `json:"detail,omitempty"`
	Gold      bool     `json:"gold"`
	Backed    []string `json:"backed,omitempty"`
}

// mapped reports whether the expectation names a finding of a replayable
// round; one that does not is reported as unmapped and never scored.
func (f FindingExpect) mapped() bool { return f.Round >= 0 && f.FindingID != "" }

// SummaryExpect is the "Since last review" line a round should have shown.
type SummaryExpect struct {
	Round     int  `json:"round"`
	New       int  `json:"new"`
	StillOpen int  `json:"still_open"`
	Fixed     int  `json:"fixed"`
	Gold      bool `json:"gold"`
}

// ResolutionExpect is a PRism thread that should have been resolved: on a fix
// that landed before round FixedBy (-1: after the last round), or on a
// settled verdict.
type ResolutionExpect struct {
	Defect    int64  `json:"defect"`
	CommentID int64  `json:"comment_id"`
	FixedBy   int    `json:"fixed_by"`
	Why       string `json:"why"`
	Gold      bool   `json:"gold"`
}

// ReplyExpect is one human reply under a PRism root and the actions that
// would have been right.
type ReplyExpect struct {
	CommentID     int64     `json:"comment_id"`
	RootCommentID int64     `json:"root_comment_id"`
	Author        string    `json:"author"`
	At            time.Time `json:"at"`
	Class         string    `json:"class"`
	Quality       string    `json:"quality"`
	Accept        []string  `json:"accept"`
	ClassAccept   []string  `json:"class_accept,omitempty"` // the class rule alone, also accepted in live mode
	WantDismissed bool      `json:"want_dismissed,omitempty"`
	Gold          bool      `json:"gold"`
	Backed        []string  `json:"backed,omitempty"`
}

// RecordedDecision is one reply model decision from the reply ledger.
type RecordedDecision struct {
	Decision        string                  `json:"decision"`
	Reply           string                  `json:"reply,omitempty"`
	Cited           []publisher.EvidenceRef `json:"cited,omitempty"`
	React           bool                    `json:"react"`
	BudgetExhausted bool                    `json:"budget_exhausted,omitempty"`
}

func (c *Case) prepare() error {
	if c.Dump.Owner == "" {
		c.Dump.Owner, c.Dump.Repo = c.Owner, c.Repo
	}
	if c.Dump.Number == 0 {
		c.Dump.Number = c.Number
	}
	if c.Bot == "" {
		for b := range c.Dump.BotLogins() {
			c.Bot = b
		}
	}
	if c.Bot == "" {
		return fmt.Errorf("%s: no bot login", c.ID)
	}
	return nil
}

// payload decodes round i's sidecar, or nil.
func (c *Case) payload(i int) *payload.Payload {
	if i < 0 || i >= len(c.Rounds) {
		return nil
	}
	raw, ok := c.Sidecars[replaykit.Short(c.Rounds[i].SHA)]
	if !ok {
		return nil
	}
	pl, err := payload.Decode(raw)
	if err != nil {
		return nil
	}
	return &pl
}

func (c *Case) bots() map[string]bool { return map[string]bool{replaykit.BareLogin(c.Bot): true} }

func compareKey(base, head string) string {
	return replaykit.Short(base) + "..." + replaykit.Short(head)
}

func loadCase(path string) (*Case, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var c Case
	if err := json.Unmarshal(raw, &c); err != nil {
		return nil, fmt.Errorf("%s: %w", filepath.Base(path), err)
	}
	if err := c.prepare(); err != nil {
		return nil, err
	}
	return &c, nil
}

// loadCases reads every case file under dir/cases (or dir itself when it has
// no cases folder), sorted by file name.
func loadCases(dir string) ([]*Case, error) {
	casesDir := filepath.Join(dir, "cases")
	if st, err := os.Stat(casesDir); err != nil || !st.IsDir() {
		casesDir = dir
	}
	paths, err := filepath.Glob(filepath.Join(casesDir, "*.json"))
	if err != nil {
		return nil, err
	}
	sort.Strings(paths)
	var out []*Case
	for _, p := range paths {
		if strings.HasSuffix(p, "manifest.json") {
			continue
		}
		c, err := loadCase(p)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, nil
}

func writeJSON(path string, v any) error {
	raw, err := json.MarshalIndent(v, "", " ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(raw, '\n'), 0o644)
}
