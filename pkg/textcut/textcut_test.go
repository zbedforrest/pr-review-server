package textcut

import (
	"strings"
	"testing"
)

func TestClamp_WordCutDoesNotEndOnAFunctionWord(t *testing.T) {
	s := "Registering the collections carousel means the featured-carousel path can now select it via ?c=collections, yet only list-based carousel classes carry a roomlist contract"
	got := Clamp(s, 110)
	if !strings.HasSuffix(got, "can now select it […]") {
		t.Fatalf("got %q", got)
	}
}

func TestClamp_DroppedTrailingParentheticalLeavesNoStrayPeriod(t *testing.T) {
	s := "The helper returns true whenever the permission cannot be queried (missing the permissions API, or the query throwing on an older browser)."
	if got := Clamp(s, 100); got != "The helper returns true whenever the permission cannot be queried […]" {
		t.Fatalf("got %q", got)
	}
}

func TestClamp_ShortTextAndTinyLimit(t *testing.T) {
	if got := Clamp("short", 10); got != "short" {
		t.Fatalf("got %q", got)
	}
	if got := Clamp("a text longer than the limit", 3); got != "[…]" {
		t.Fatalf("got %q", got)
	}
}

func TestFirstSentence_FallsBackToTheFirstEndWhenNoBalancedOneExists(t *testing.T) {
	if got := FirstSentence("A stray (parenthesis never closes. Second sentence."); got != "A stray (parenthesis never closes." {
		t.Fatalf("got %q", got)
	}
	if got := FirstSentence("No sentence end at all"); got != "No sentence end at all" {
		t.Fatalf("got %q", got)
	}
}
