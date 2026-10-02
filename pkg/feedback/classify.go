// Package feedback scans what authors say about PRism's comments (replies,
// conversation comments naming the bot, reactions), labels each item by mood
// and keeps the result so the daily health report can quote the unhappy ones.
package feedback

import (
	"context"
	"fmt"
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"

	"pr-review-server/pkg/health"
)

// Label is an author's mood about a PRism comment.
type Label string

const (
	Happy          Label = health.FeedbackHappy
	Neutral        Label = health.FeedbackNeutral
	Frustrated     Label = health.FeedbackFrustrated
	VeryFrustrated Label = health.FeedbackVeryFrustrated
)

// Labels in report order.
var Labels = []Label{Happy, Neutral, Frustrated, VeryFrustrated}

// IsFrustrated reports a label that earns a quote in the daily line.
func (l Label) IsFrustrated() bool { return l == Frustrated || l == VeryFrustrated }

const (
	SourceReply    = "reply"    // an author reply under a PRism inline comment
	SourceComment  = "comment"  // a PR conversation comment that names the bot
	SourceReaction = "reaction" // a reaction on a PRism comment
)

var (
	veryFrustratedRe = regexp.MustCompile(`(?i)\b(wtf|f+u+c*k\w*|shit\w*|bullshit|crap|please stop|stop (commenting|posting|spamming|reviewing|doing this)|(second|third|fourth|fifth|nth) time (it|you|prism|the bot)|shut up|go away|useless|garbage|spam(my|ming)?|unsubscribe|turn (this|it) off|disable (this|it|the bot)|waste of (my |our )?time|stop it|make it stop|so annoying|incredibly annoying|extremely annoying|infuriating|ridiculous)\b`)
	frustratedRe     = regexp.MustCompile(`(?i)\b(wrong|incorrect|not true|untrue|false positive|false alarm|irrelevant|nitpick\w*|pointless|unnecessary|noise|noisy|annoying|misleading|not helpful|unhelpful|hallucinat\w*|not a bug|doesn'?t make sense|makes no sense|already (handled|covered|done|the case|does)|too many comments|nope|not (an|the) issue)\b|^no[,.!]|again\?`)
	happyRe          = regexp.MustCompile(`(?i)\b(thanks?|thank you|thx|good catch|nice catch|great catch|helpful|love (it|this)|awesome|great (point|catch|find|call)|nice (one|find|point)|appreciate\w*|spot on|good point|fair point|you'?re right|well spotted|legit|valid point|good bot|nice bot|perfect)\b|(\+1|:\+1:|:thumbsup:|:heart:|:pray:|:tada:|👍|🙏|❤️|🎉|💯)`)
	shoutRe          = regexp.MustCompile(`!{2,}`)
)

// reactionLabels maps GitHub reaction content to a mood; anything else is
// neutral.
var reactionLabels = map[string]Label{
	"+1": Happy, "heart": Happy, "hooray": Happy,
	"-1": Frustrated, "confused": Frustrated,
}

// Lexicon labels an item without a model. Reactions are read by content; text
// by anchored vocabulary, strongest signal first.
func Lexicon(item Item) Label {
	if item.Source == SourceReaction {
		if l, ok := reactionLabels[item.Reaction]; ok {
			return l
		}
		return Neutral
	}
	text := strings.TrimSpace(item.Body)
	very := len(veryFrustratedRe.FindAllString(text, -1))
	frustrated := len(frustratedRe.FindAllString(text, -1))
	happy := len(happyRe.FindAllString(text, -1))
	switch {
	case very > 0, frustrated >= 3, frustrated >= 1 && shoutRe.MatchString(text):
		return VeryFrustrated
	case frustrated > 0 && happy == 0:
		return Frustrated
	case happy > 0 && frustrated == 0:
		return Happy
	}
	return Neutral
}

// Classifier labels one item and names what did it.
type Classifier interface {
	Classify(ctx context.Context, item Item) (Label, string)
}

// LexiconClassifier is the deterministic fallback.
type LexiconClassifier struct{}

func (LexiconClassifier) Classify(_ context.Context, item Item) (Label, string) {
	return Lexicon(item), "lexicon"
}

// ModelClassifier spends one cheap model call per text item and falls back to
// the lexicon when the call fails or answers with anything but a label.
// Reactions never need the model.
type ModelClassifier struct {
	Ask  func(ctx context.Context, prompt string) (string, error)
	Name string
}

func (m ModelClassifier) Classify(ctx context.Context, item Item) (Label, string) {
	if item.Source == SourceReaction || m.Ask == nil {
		return Lexicon(item), "lexicon"
	}
	answer, err := m.Ask(ctx, Prompt(item))
	if err != nil {
		return Lexicon(item), "lexicon"
	}
	if label, ok := ParseLabel(answer); ok {
		return label, m.Name
	}
	return Lexicon(item), "lexicon"
}

const maxPromptBody = 1500

// Prompt is the classification question for one text item.
func Prompt(item Item) string {
	// A comment must not be able to close the fence and address the model.
	body := strings.ReplaceAll(truncateBytes(item.Body, maxPromptBody), `"""`, "'''")
	hint := item.ReplyClass
	if hint == "" {
		hint = "none"
	}
	return fmt.Sprintf(`You label how a software engineer feels about an automated code review bot, judging only from their comment below.
Answer with exactly one word: happy, neutral, frustrated, or very_frustrated.
happy: thanks, agreement, finds the review useful.
neutral: factual discussion, questions, fix notes, mild or polite disagreement.
frustrated: annoyed; calls the comment wrong, noisy, repetitive or unhelpful.
very_frustrated: angry or insulting, asks the bot to stop or be turned off, or exasperated at repeated behaviour.
Reply classifier hint: %s

Comment:
"""
%s
"""`, hint, body)
}

var labelRe = regexp.MustCompile(`^(very[ _-]?frustrated|frustrated|happy|neutral)\b`)

// ParseLabel accepts an answer that starts with a label; a sentence ("not
// happy") does not count, the lexicon decides instead.
func ParseLabel(answer string) (Label, bool) {
	text := strings.TrimLeft(strings.ToLower(answer), " \t\r\n`*\"'.:")
	m := labelRe.FindString(text)
	if strings.HasPrefix(m, "very") {
		m = string(VeryFrustrated)
	}
	return Label(m), m != ""
}

const quoteRunes = 140

var htmlTagRe = regexp.MustCompile(`<[^>]*>`)

// neutralizeMarkup keeps a quote from rendering as a link, image or HTML in
// the report; the words stay.
func neutralizeMarkup(text string) string {
	text = htmlTagRe.ReplaceAllString(text, "")
	return strings.ReplaceAll(text, "](", "] (")
}

// Quote flattens a comment to one line of at most 140 characters, dropping
// quoted lines and code fences first.
func Quote(body string) string {
	var kept []string
	inFence := false
	for _, line := range strings.Split(body, "\n") {
		t := strings.TrimSpace(line)
		if strings.HasPrefix(t, "```") {
			inFence = !inFence
			continue
		}
		if inFence || strings.HasPrefix(t, ">") || t == "" {
			continue
		}
		kept = append(kept, t)
	}
	text := strings.Join(strings.Fields(neutralizeMarkup(strings.Join(kept, " "))), " ")
	if utf8.RuneCountInString(text) <= quoteRunes {
		return text
	}
	runes := []rune(text)[:quoteRunes]
	cut := len(runes)
	for cut > quoteRunes/2 && !unicode.IsSpace(runes[cut-1]) {
		cut--
	}
	return strings.TrimRightFunc(string(runes[:cut]), unicode.IsSpace) + "..."
}
