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

var (
	withdrawalRe   = regexp.MustCompile(`(?i)\b(?:i(?:'m| am| will|'ll| have|'ve|'d| would)?\s+(?:be\s+|now\s+|also\s+)?withdr(?:aw|awing|awn|aws|ew)|withdrawing (?:the|this|my|it)|withdrawn)\b|\bno (?:further |code )?changes? (?:is |are )?(?:needed|necessary|required)\b`)
	noWithdrawalRe = regexp.MustCompile(`(?i)\b(?:not|never|don't|won't|wouldn't|can't|cannot|shouldn't|doesn't|didn't|isn't|aren't|wasn't|weren't|haven't|hasn't|hadn't|shan't|no need to|no reason to)\s+(?:be\s+|been\s+|going to\s+|yet\s+|need to be\s+)?withdr`)
)

// courtesyReply is a reply too short to carry a stand, a fix or a
// deferral: a thanks, an emoji, a nod.
func courtesyReply(body string) bool {
	return len([]rune(strings.TrimSpace(body))) <= courtesyMaxRunes
}

const courtesyMaxRunes = 40

// conditionalWithdrawalRe is a withdrawal that hangs on a condition.
var conditionalWithdrawalRe = regexp.MustCompile(`(?i)withdr\w*\s+(?:this|it|the finding|the point)?\s*(?:if|once|when|after|unless|provided|as soon as)\b`)

// IsWithdrawal reports whether a reviewer's reply takes its own point
// back: an explicit withdrawal in the first person or aimed at the
// thread, or the "no change is needed" form, never a negated or a
// conditional one or a passing "not necessary".
func IsWithdrawal(body string) bool {
	return withdrawalRe.MatchString(body) && !noWithdrawalRe.MatchString(body) && !conditionalWithdrawalRe.MatchString(body)
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
			// A nod after the withdrawal (thanks) is not the stand the
			// reviewer took back; a longer reply may still defer or fix it.
			if t.Withdrawn && courtesyReply(c.Body) {
				continue
			}
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
