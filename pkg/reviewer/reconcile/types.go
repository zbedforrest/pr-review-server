// Package reconcile matches PRism findings against review comments another
// bot or a human already posted on the same pull request, so PRism never
// duplicates one and every finding carries a source tag.
package reconcile

// Source names the review producer a finding came from.
type Source string

const (
	SourcePrism       Source = "prism"
	SourceGreptile    Source = "greptile"
	SourceCopilot     Source = "copilot"
	SourceCodeRabbit  Source = "coderabbit"
	SourceGemini      Source = "gemini"
	SourceCodeScan    Source = "code-scanning"
	SourceHuman       Source = "human"
	SourceOtherBot    Source = "bot"
	SourceTagPrefixEx        = "external:"
)

const (
	SourceTagPrismOnly    = "prism-only"
	SourceTagGreptileOnly = "greptile-only"
	SourceTagBoth         = "both"
)

// ExternalComment is the subset of a GitHub review comment the reconciler
// needs. StartLine is zero for single-line comments.
type ExternalComment struct {
	ID        int64
	Author    string
	Body      string
	Path      string
	Line      int
	StartLine int
	// OriginalLine and OriginalStartLine stand in when the comment's diff
	// is outdated and GitHub reports no current line.
	OriginalLine      int
	OriginalStartLine int
	InReplyToID       int64
}

// ExternalFinding is a review comment parsed into a finding.
type ExternalFinding struct {
	Source     Source
	CommentID  int64
	Severity   string
	Title      string
	Body       string
	File       string
	StartLine  int
	EndLine    int
	Suggestion string
}

// Label is the reviewer name a source is shown under in a summary.
func (s Source) Label() string {
	switch s {
	case SourceGreptile:
		return "Greptile"
	case SourceCopilot:
		return "Copilot"
	case SourceCodeRabbit:
		return "CodeRabbit"
	case SourceGemini:
		return "Gemini"
	case SourceCodeScan:
		return "code scanning"
	case SourceHuman:
		return "a reviewer"
	case SourcePrism:
		return "PRism"
	}
	return "another bot"
}

// SourceTag is the ledger source tag of a row matched to this source.
func (s Source) SourceTag() string { return SourceTagPrefixEx + string(s) }

// SourceFromTag reads the source back out of a ledger source tag; ok is
// false for the tags that predate external rows.
func SourceFromTag(tag string) (Source, bool) {
	if len(tag) <= len(SourceTagPrefixEx) || tag[:len(SourceTagPrefixEx)] != SourceTagPrefixEx {
		return "", false
	}
	return Source(tag[len(SourceTagPrefixEx):]), true
}
