// Package reconcile matches PRism findings against review comments another
// bot already posted on the same pull request, so PRism never duplicates one
// and every finding carries a source tag.
package reconcile

// Source names the review producer a finding came from.
type Source string

const (
	SourcePrism    Source = "prism"
	SourceGreptile Source = "greptile"
)

const (
	SourceTagPrismOnly    = "prism-only"
	SourceTagGreptileOnly = "greptile-only"
	SourceTagBoth         = "both"
)

// ExternalComment is the subset of a GitHub review comment the reconciler
// needs. StartLine is zero for single-line comments.
type ExternalComment struct {
	ID          int64
	Author      string
	Body        string
	Path        string
	Line        int
	StartLine   int
	InReplyToID int64
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
