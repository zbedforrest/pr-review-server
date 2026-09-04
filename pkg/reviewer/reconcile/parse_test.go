package reconcile

import (
	"strings"
	"testing"
)

const greptileBadge = `<a href="#"><img alt="P1" src="https://greptile-static-assets.s3.amazonaws.com/badges/p1.svg?v=9" align="top"></a>`

const greptileBody = greptileBadge + ` **Purchase threads miss LIVE**

When a purchase event creates a new conversation during broadcasting or grace, the synthetic ` + "`privateMessage`" + ` has no ` + "`originatingRoom`" + `, so the thread is created without a status.

<details><summary>Prompt To Fix With AI</summary>
This is a comment left during a code review.
Path: app/chat.go
</details>

<a href="https://app.greptile.com/ide/claude-code?prompt=abc"><picture><source srcset="x.svg"><img src="y.svg"></picture></a>`

func greptileComment(body string) ExternalComment {
	return ExternalComment{ID: 42, Author: "greptile-apps[bot]", Body: body, Path: "app/chat.go", Line: 120, StartLine: 118}
}

func TestIsGreptileAuthor(t *testing.T) {
	for login, want := range map[string]bool{
		"greptile-apps[bot]": true,
		"greptile-apps":      true,
		"octocat":            false,
		"":                   false,
	} {
		if got := IsGreptileAuthor(login); got != want {
			t.Errorf("IsGreptileAuthor(%q) = %v, want %v", login, got, want)
		}
	}
}

func TestParseGreptileCommentFullBody(t *testing.T) {
	f, ok := ParseGreptileComment(greptileComment(greptileBody))
	if !ok {
		t.Fatal("expected ok")
	}
	if f.Source != SourceGreptile {
		t.Errorf("Source = %q", f.Source)
	}
	if f.CommentID != 42 {
		t.Errorf("CommentID = %d", f.CommentID)
	}
	if f.Severity != "medium" {
		t.Errorf("Severity = %q, want medium", f.Severity)
	}
	if f.Title != "Purchase threads miss LIVE" {
		t.Errorf("Title = %q", f.Title)
	}
	if !strings.HasPrefix(f.Body, "When a purchase event") || !strings.HasSuffix(f.Body, "without a status.") {
		t.Errorf("Body = %q", f.Body)
	}
	if strings.Contains(f.Body, "<details>") || strings.Contains(f.Body, "Prompt To Fix") || strings.Contains(f.Body, "<picture>") || strings.Contains(f.Body, "app.greptile.com") {
		t.Errorf("Body includes trailing blocks: %q", f.Body)
	}
	if f.File != "app/chat.go" || f.StartLine != 118 || f.EndLine != 120 {
		t.Errorf("location = %s:%d-%d", f.File, f.StartLine, f.EndLine)
	}
	if f.Suggestion != "" {
		t.Errorf("Suggestion = %q, want empty", f.Suggestion)
	}
}

func TestParseGreptileCommentSeverityMapping(t *testing.T) {
	for badge, want := range map[string]string{"P0": "critical", "P1": "medium", "P2": "low", "P9": "unknown"} {
		body := `<a href="#"><img alt="` + badge + `" src="https://example.invalid/b.svg" align="top"></a> **Title here**` + "\n\nSome body text."
		f, ok := ParseGreptileComment(greptileComment(body))
		if !ok {
			t.Fatalf("%s: expected ok", badge)
		}
		if f.Severity != want {
			t.Errorf("%s: Severity = %q, want %q", badge, f.Severity, want)
		}
	}
}

func TestParseGreptileCommentSuggestion(t *testing.T) {
	body := greptileBadge + " **Use the constant**\n\nReplace the literal.\n\n```suggestion\n\tstatus := StatusLive\n```\n\n<details><summary>Prompt To Fix With AI</summary>x</details>"
	f, ok := ParseGreptileComment(greptileComment(body))
	if !ok {
		t.Fatal("expected ok")
	}
	if f.Suggestion != "\tstatus := StatusLive" {
		t.Errorf("Suggestion = %q", f.Suggestion)
	}
	if strings.Contains(f.Body, "```") || strings.Contains(f.Body, "StatusLive") {
		t.Errorf("Body should exclude the suggestion fence: %q", f.Body)
	}
	if f.Body != "Replace the literal." {
		t.Errorf("Body = %q", f.Body)
	}
}

func TestParseGreptileCommentNoStartLineUsesLine(t *testing.T) {
	c := greptileComment(greptileBody)
	c.StartLine = 0
	f, ok := ParseGreptileComment(c)
	if !ok {
		t.Fatal("expected ok")
	}
	if f.StartLine != 120 || f.EndLine != 120 {
		t.Errorf("range = %d-%d, want 120-120", f.StartLine, f.EndLine)
	}
}

func TestParseGreptileCommentReplyIsNotFinding(t *testing.T) {
	c := greptileComment("Thanks, that makes sense. Resolving.")
	c.InReplyToID = 7
	if _, ok := ParseGreptileComment(c); ok {
		t.Error("reply should not parse as a finding")
	}
}

func TestParseGreptileCommentNoBadgeIsNotFinding(t *testing.T) {
	if _, ok := ParseGreptileComment(greptileComment("**Bold title** but no badge anywhere.")); ok {
		t.Error("body without a badge should not parse")
	}
}

func TestParseGreptileCommentMalformed(t *testing.T) {
	for name, body := range map[string]string{
		"empty":         "",
		"badge only":    greptileBadge,
		"badge no bold": greptileBadge + " just some text",
	} {
		if _, ok := ParseGreptileComment(greptileComment(body)); ok {
			t.Errorf("%s: expected ok=false", name)
		}
	}
}

func TestParseGreptileCommentsFiltersAuthorsAndReplies(t *testing.T) {
	human := greptileComment(greptileBody)
	human.ID = 1
	human.Author = "octocat"
	reply := greptileComment("Agreed.")
	reply.ID = 2
	reply.InReplyToID = 1
	good := greptileComment(greptileBody)
	good.ID = 3
	legacy := greptileComment(greptileBody)
	legacy.ID = 4
	legacy.Author = "greptile-apps"

	got := ParseGreptileComments([]ExternalComment{human, reply, good, legacy})
	if len(got) != 2 {
		t.Fatalf("got %d findings, want 2: %+v", len(got), got)
	}
	if got[0].CommentID != 3 || got[1].CommentID != 4 {
		t.Errorf("ids = %d,%d", got[0].CommentID, got[1].CommentID)
	}
}
