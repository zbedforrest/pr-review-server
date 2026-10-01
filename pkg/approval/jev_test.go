package approval

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func jevFixture(t *testing.T, answer func(question string, q map[string]any) map[string]any) (*httptest.Server, *[]map[string]any) {
	t.Helper()
	var requests []map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
		}
		requests = append(requests, body)
		answers := map[string]any{}
		for id, q := range body["questions"].(map[string]any) {
			answers[id] = answer(id, q.(map[string]any))
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"model": "typesafe/jev-test", "answers": answers})
	}))
	t.Cleanup(server.Close)
	return server, &requests
}

func impactAnswer(level string) map[string]any {
	probabilities := map[string]any{"0": 0.0, "1": 0.0, "2": 0.0, "3": 0.0}
	probabilities[level] = 1.0
	score := map[string]float64{"0": 0, "1": 1, "2": 2, "3": 3}[level]
	return map[string]any{"type": "score", "score": score, "probabilities": probabilities}
}

func scoringSnapshot() Snapshot {
	s, _ := validFixture()
	s.Concerns = []Concern{
		{ID: "nit", Claim: "Rename this variable for clarity.", OriginalSeverity: "low", OriginalRevision: s.Revision.Head},
		{ID: "bug", Claim: "The retry loop never stops when the provider is down.", OriginalSeverity: "medium", OriginalRevision: s.Revision.Head},
	}
	return s
}

func runScorer(t *testing.T, s Snapshot, answer func(string, map[string]any) map[string]any) (Assessment, []map[string]any) {
	t.Helper()
	server, requests := jevFixture(t, answer)
	scorer := JevScorer{Config: ModelConfig{Provider: ProviderJev, Model: "jev-latest", APIKey: "fixture", BaseURL: server.URL, Client: server.Client()}}
	a, err := scorer.Investigate(context.Background(), s, nil, &testBudget{})
	if err != nil {
		t.Fatal(err)
	}
	return a, *requests
}

func TestJevScoreWeightsUnresolvedConcernsBySeverity(t *testing.T) {
	answer := func(nitImpact, bugResolved float64, bugImpact string) func(string, map[string]any) map[string]any {
		return func(id string, _ map[string]any) map[string]any {
			switch {
			case strings.HasSuffix(id, "_resolved") && strings.HasPrefix(id, "C1"):
				return map[string]any{"type": "noul", "noul": 0.0}
			case strings.HasSuffix(id, "_resolved"):
				return map[string]any{"type": "noul", "noul": bugResolved}
			case strings.HasSuffix(id, "_impact") && strings.HasPrefix(id, "C1"):
				return impactAnswer("0")
			case strings.HasSuffix(id, "_impact"):
				return impactAnswer(bugImpact)
			case strings.HasSuffix(id, "_disposition"):
				return map[string]any{"type": "choice", "choice": "still_present"}
			}
			return map[string]any{"type": "score", "score": 3.0}
		}
	}
	clean, _ := runScorer(t, scoringSnapshot(), answer(0, 1, "2"))
	if clean.Score.Value != 100 || !clean.Score.Candidate || clean.Decision != "candidate" {
		t.Fatalf("an unresolved cosmetic nit must not lower the score: %+v", clean.Score)
	}
	open, _ := runScorer(t, scoringSnapshot(), answer(0, 0, "2"))
	if open.Score.Value != 65 || open.Score.Candidate || open.Decision != "needs_attention" || open.Score.Concerns[0].ID != "bug" {
		t.Fatalf("an unresolved real defect must land below the cutoff and lead the risks: %+v", open.Score)
	}
	severe, _ := runScorer(t, scoringSnapshot(), answer(0, 0, "3"))
	if severe.Score.Value != 20 {
		t.Fatalf("an unresolved severe concern must fall near 20: %+v", severe.Score)
	}
	s := scoringSnapshot()
	for i := range s.Concerns {
		s.Concerns[i].OriginalRevision = strings.Repeat("b", 40)
	}
	old, _ := runScorer(t, s, answer(0, 0, "3"))
	if old.Score.Value != 88 {
		t.Fatalf("a superseded severe concern counts at the superseded weight: %+v", old.Score)
	}
}

func TestJevScoreCapsHardBlockersAndDeductsDrafts(t *testing.T) {
	resolved := func(id string, _ map[string]any) map[string]any {
		switch {
		case strings.HasSuffix(id, "_resolved"):
			return map[string]any{"type": "noul", "noul": 1.0}
		case strings.HasSuffix(id, "_impact"):
			return impactAnswer("2")
		case strings.HasSuffix(id, "_disposition"):
			return map[string]any{"type": "choice", "choice": "fixed"}
		}
		return map[string]any{"type": "score", "score": 4.0}
	}
	s := scoringSnapshot()
	s.Checks = append(s.Checks, Check{Name: "unit", State: "failure", SHA: s.Revision.Head})
	failing, _ := runScorer(t, s, resolved)
	if failing.Score.Value != blockerCap || failing.Score.Candidate || len(failing.Score.Blockers) != 1 || failing.Score.Blockers[0] != "ci_failed" {
		t.Fatalf("red CI must cap the score and block: %+v", failing.Score)
	}
	s = scoringSnapshot()
	s.Draft = true
	draft, _ := runScorer(t, s, resolved)
	if draft.Score.Value != 100-draftDeduction || draft.Score.Candidate {
		t.Fatalf("a draft must lose %v points and never be a candidate: %+v", draftDeduction, draft.Score)
	}
}

func TestJevScoreAsksAboutEveryConcernAndReadinessInOneRequest(t *testing.T) {
	_, requests := runScorer(t, scoringSnapshot(), func(id string, q map[string]any) map[string]any {
		switch q["type"] {
		case "noul":
			return map[string]any{"type": "noul", "noul": 1.0}
		case "choice":
			return map[string]any{"type": "choice", "choice": "fixed"}
		}
		return impactAnswer("1")
	})
	if len(requests) != 1 {
		t.Fatalf("a small pull request needs one Jev request, got %d", len(requests))
	}
	questions := requests[0]["questions"].(map[string]any)
	for _, id := range []string{"C1_resolved", "C1_impact", "C1_disposition", "C2_resolved", "C2_impact", "C2_disposition", "readiness"} {
		if _, ok := questions[id]; !ok {
			t.Errorf("missing question %s", id)
		}
	}
}

func TestScrubSecretsRedactsCredentialsInEveryString(t *testing.T) {
	in := map[string]any{"code": "token = \"ghp_" + strings.Repeat("a", 30) + "\"\nkey: AKIA" + strings.Repeat("B", 16), "nested": []any{"password=supersecretvalue1"}}
	out, _ := json.Marshal(scrubSecrets(in))
	text := string(out)
	if strings.Contains(text, "ghp_") || strings.Contains(text, "AKIA") || strings.Contains(text, "supersecretvalue1") {
		t.Fatalf("credentials survived scrubbing: %s", text)
	}
	if !strings.Contains(text, "[redacted]") {
		t.Fatalf("expected redaction markers: %s", text)
	}
}
