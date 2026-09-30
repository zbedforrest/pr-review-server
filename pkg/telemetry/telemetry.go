// Package telemetry records structured operational events.
//
// The default Recorder writes each event as one JSON line to stdout, which
// log platforms that parse JSON (for example Google Cloud Logging) index as
// structured fields. Other sinks (see package newrelic) implement the same
// interface.
package telemetry

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"sync"
	"time"
)

// Recorder records one event of eventType with flat attributes.
type Recorder interface {
	Record(eventType string, attrs map[string]any)
}

// Multi fans an event out to every non-nil recorder.
type Multi []Recorder

// Record implements Recorder.
func (m Multi) Record(eventType string, attrs map[string]any) {
	for _, r := range m {
		if r != nil {
			r.Record(eventType, attrs)
		}
	}
}

// JSONLog writes events as single-line JSON objects with a severity and a
// message, the shape Cloud Logging turns into a structured jsonPayload.
type JSONLog struct {
	mu sync.Mutex
	w  io.Writer
}

// NewJSONLog writes to w, or to stdout when w is nil.
func NewJSONLog(w io.Writer) *JSONLog {
	if w == nil {
		w = os.Stdout
	}
	return &JSONLog{w: w}
}

// Record implements Recorder.
func (l *JSONLog) Record(eventType string, attrs map[string]any) {
	if l == nil {
		return
	}
	line := make(map[string]any, len(attrs)+4)
	for k, v := range attrs {
		line[k] = v
	}
	line["severity"] = "INFO"
	line["message"] = eventType
	line["event_type"] = eventType
	line["time"] = time.Now().UTC().Format(time.RFC3339Nano)
	b, err := json.Marshal(line)
	if err != nil {
		b = []byte(fmt.Sprintf(`{"severity":"WARNING","message":"telemetry encode failed","event_type":%q}`, eventType))
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	_, _ = l.w.Write(append(b, '\n'))
}
