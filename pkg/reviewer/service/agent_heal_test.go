package service

import "testing"

func TestParseAgentJSONHealsBrokenStructureInsteadOfCollapsingToSummary(t *testing.T) {
	raw := `{"findings":[{"id":"A-1","file_path":"app/views.py","line_number":40,"importance":"CRITICAL",` +
		`"comment_body":"Anonymous users crash because request.user.id is None."},],` +
		`"summary":{"verdict":"request_changes","upshot":"Fix the anonymous crash first."}}`
	comments, err := parseAgentJSON(raw)
	if err != nil {
		t.Fatalf("parseAgentJSON: %v", err)
	}
	if len(comments) != 2 || comments[0].FilePath != "app/views.py" || comments[0].LineNumber != 40 {
		t.Fatalf("got %+v", comments)
	}
	if comments[1].FilePath != "SUMMARY" || comments[1].Summary == nil || comments[1].Summary.Verdict != "request_changes" {
		t.Fatalf("summary block beside the findings was not kept: %+v", comments[1])
	}
}

func TestParseAgentJSONStillFailsWhenNothingCanBeHealed(t *testing.T) {
	if _, err := parseAgentJSON("Verdict: approve.\n\nNothing to flag."); err == nil {
		t.Fatal("a prose answer must still fall back to the SUMMARY wrapper")
	}
}
