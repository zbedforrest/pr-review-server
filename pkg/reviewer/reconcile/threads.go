package reconcile

import (
	"regexp"
	"strings"
	"time"
)

// ExternalThread is another reviewer's root thread with what the PR
// author said in it and whether the reviewer then took the point back.
type ExternalThread struct {
	Root ExternalFinding
	// AuthorReplies are the PR author's replies in the thread, in order.
	AuthorReplies []string
	// Withdrawn is set when the root's author replied after the PR author
	// and took the point back (a withdrawal, "no change is needed").
	Withdrawn bool
	// SettledAt is when the thread was settled: the withdrawal's time, else
	// the author's last reply; zero when the comments carry no times.
	SettledAt time.Time
}

var withdrawalRe = regexp.MustCompile(`(?i)\bwithdraw(?:ing|n|s|al)?\b|\bno (?:further |code )?changes? (?:is |are )?(?:needed|necessary|required)\b|\bnot necessary\b|\bnothing to change\b`)

// IsWithdrawal reports whether a reviewer's reply takes its own point back.
func IsWithdrawal(body string) bool {
	return withdrawalRe.MatchString(body)
}

// BuildExternalThreads pairs each parsed root with the PR author's replies
// under it and the reviewer's withdrawal, if any, from the PR's review
// comments. A root nobody answered is a thread with nothing in it.
func BuildExternalThreads(cs []ExternalComment, roots []ExternalFinding, author string) []ExternalThread {
	author = strings.ToLower(strings.TrimSpace(author))
	byRoot := make(map[int64]int, len(roots))
	out := make([]ExternalThread, len(roots))
	rootAuthor := make(map[int64]string, len(roots))
	for i, g := range roots {
		byRoot[g.CommentID] = i
		out[i].Root = g
	}
	for _, c := range cs {
		if c.InReplyToID == 0 {
			if _, ok := byRoot[c.ID]; ok {
				rootAuthor[c.ID] = strings.ToLower(c.Author)
			}
		}
	}
	sorted := append([]ExternalComment(nil), cs...)
	stableSortByTime(sorted)
	for _, c := range sorted {
		i, ok := byRoot[c.InReplyToID]
		if !ok {
			continue
		}
		t := &out[i]
		login := strings.ToLower(c.Author)
		switch {
		case author != "" && login == author:
			t.AuthorReplies = append(t.AuthorReplies, c.Body)
			if !t.Withdrawn {
				t.SettledAt = c.CreatedAt
			}
		case len(t.AuthorReplies) > 0 && login == rootAuthor[c.InReplyToID] && IsWithdrawal(c.Body):
			t.Withdrawn = true
			t.SettledAt = c.CreatedAt
		}
	}
	return out
}

func stableSortByTime(cs []ExternalComment) {
	for i := 1; i < len(cs); i++ {
		for j := i; j > 0 && cs[j].CreatedAt.Before(cs[j-1].CreatedAt); j-- {
			cs[j], cs[j-1] = cs[j-1], cs[j]
		}
	}
}
