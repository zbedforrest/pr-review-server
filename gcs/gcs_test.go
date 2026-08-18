package gcs

import "testing"

func TestReviewRunFileNames(t *testing.T) {
	runID := "run-0123456789abcdef0123456789abcdef"
	html := ReviewRunFileName("acme", "widgets", 42, "abcdef0123456789", runID)
	wantHTML := "runs/acme/widgets/42/abcdef0/" + runID + ".html"
	if html != wantHTML {
		t.Fatalf("ReviewRunFileName() = %q, want %q", html, wantHTML)
	}
	if got, want := ReviewRunJSONFileName("acme", "widgets", 42, "abcdef0123456789", runID),
		"runs/acme/widgets/42/abcdef0/"+runID+".json"; got != want {
		t.Fatalf("ReviewRunJSONFileName() = %q, want %q", got, want)
	}
}
