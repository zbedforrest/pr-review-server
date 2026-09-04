package reconcile

import "testing"

func TestSimilarityIdentical(t *testing.T) {
	s := "the synthetic privateMessage has no originatingRoom"
	if got := Similarity(s, s); got != 1.0 {
		t.Errorf("Similarity = %v, want 1.0", got)
	}
}

func TestSimilarityDisjoint(t *testing.T) {
	if got := Similarity("database connection leaks", "frontend button colour"); got != 0 {
		t.Errorf("Similarity = %v, want 0", got)
	}
}

func TestSimilarityCaseInsensitiveAndPunctuationAgnostic(t *testing.T) {
	if got := Similarity("Missing `originatingRoom` field!", "missing originatingroom field"); got != 1.0 {
		t.Errorf("Similarity = %v, want 1.0", got)
	}
}

func TestSimilarityIgnoresStopwords(t *testing.T) {
	if got := Similarity("the connection leaks", "this connection leaks with that and those"); got != 1.0 {
		t.Errorf("Similarity = %v, want 1.0 after dropping stopwords", got)
	}
}

func TestSimilarityIgnoresShortTokens(t *testing.T) {
	if got := Similarity("connection leaks", "connection leaks if x != 0 or y"); got != 1.0 {
		t.Errorf("Similarity = %v, want 1.0 after dropping short tokens", got)
	}
}

func TestSimilarityPartialOverlap(t *testing.T) {
	got := Similarity("alpha beta gamma delta", "alpha beta epsilon zeta")
	if got < 0.33 || got > 0.34 {
		t.Errorf("Similarity = %v, want 2/6", got)
	}
}

func TestSimilarityEmpty(t *testing.T) {
	if got := Similarity("", "connection leaks"); got != 0 {
		t.Errorf("Similarity = %v, want 0", got)
	}
	if got := Similarity("the a", "of"); got != 0 {
		t.Errorf("Similarity of two empty token sets = %v, want 0", got)
	}
}
