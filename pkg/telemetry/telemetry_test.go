package telemetry

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

func TestJSONLogWritesOneStructuredLinePerEvent(t *testing.T) {
	var buf bytes.Buffer
	l := NewJSONLog(&buf)
	l.Record("PrismReviewRun", map[string]any{"profile": "lite", "cost_usd": 0.15})
	l.Record("PrismEnsembleRun", map[string]any{"invocation": 2})
	lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
	if len(lines) != 2 {
		t.Fatalf("lines = %q", lines)
	}
	var ev map[string]any
	if err := json.Unmarshal([]byte(lines[0]), &ev); err != nil {
		t.Fatal(err)
	}
	if ev["severity"] != "INFO" || ev["message"] != "PrismReviewRun" || ev["event_type"] != "PrismReviewRun" || ev["profile"] != "lite" || ev["time"] == nil {
		t.Fatalf("event = %v", ev)
	}
}

type countRecorder struct{ n int }

func (c *countRecorder) Record(string, map[string]any) { c.n++ }

func TestMultiSkipsNilRecorders(t *testing.T) {
	c := &countRecorder{}
	Multi{nil, c, c}.Record("x", nil)
	if c.n != 2 {
		t.Fatalf("n = %d", c.n)
	}
}
