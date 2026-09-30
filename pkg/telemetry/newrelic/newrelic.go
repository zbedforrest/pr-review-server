// Package newrelic sends custom events to the New Relic Event API.
//
// Record never blocks and never fails the caller: events queue in memory,
// flush in batches from one goroutine, and are dropped (with a log line) when
// the queue is full or New Relic keeps rejecting them. A nil *Sink is valid
// and records nothing, so telemetry stays optional.
package newrelic

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"
)

// Config selects the account and key. Region is "US" (default) or "EU".
type Config struct {
	AccountID string
	// InsertKey is an Insights insert key; LicenseKey is an ingest license
	// key. One is required; the license key wins when both are set.
	InsertKey  string
	LicenseKey string
	Region     string
	// Endpoint overrides the collector URL (a proxy, or a test server).
	Endpoint string
}

const (
	queueSize     = 5000
	batchSize     = 500
	flushInterval = 10 * time.Second
	sendAttempts  = 3
)

var sendBackoff = []time.Duration{time.Second, 5 * time.Second}

// Sink batches events for one account.
type Sink struct {
	url    string
	header string
	key    string
	client *http.Client
	events chan map[string]any
	done   chan struct{}
	once   sync.Once
	// dropped counts events lost to a full queue or a failed send.
	mu      sync.Mutex
	dropped int
}

// New returns a running Sink, or nil when cfg names no account or key.
func New(cfg Config) *Sink {
	if cfg.AccountID == "" || (cfg.InsertKey == "" && cfg.LicenseKey == "") {
		return nil
	}
	host := "insights-collector.newrelic.com"
	if strings.EqualFold(cfg.Region, "EU") {
		host = "insights-collector.eu01.nr-data.net"
	}
	url := fmt.Sprintf("https://%s/v1/accounts/%s/events", host, cfg.AccountID)
	if cfg.Endpoint != "" {
		url = cfg.Endpoint
	}
	s := &Sink{
		url:    url,
		client: &http.Client{Timeout: 30 * time.Second},
		events: make(chan map[string]any, queueSize),
		done:   make(chan struct{}),
	}
	if cfg.LicenseKey != "" {
		s.header, s.key = "Api-Key", cfg.LicenseKey
	} else {
		s.header, s.key = "X-Insert-Key", cfg.InsertKey
	}
	go s.loop()
	return s
}

// Record queues one event of eventType with attrs (flat string, number or
// bool values; nested values are JSON-encoded as strings).
func (s *Sink) Record(eventType string, attrs map[string]any) {
	if s == nil {
		return
	}
	ev := make(map[string]any, len(attrs)+2)
	for k, v := range attrs {
		ev[k] = flatten(v)
	}
	ev["eventType"] = eventType
	if _, ok := ev["timestamp"]; !ok {
		ev["timestamp"] = time.Now().UnixMilli()
	}
	select {
	case s.events <- ev:
	default:
		s.drop(1, "queue full")
	}
}

// Close flushes what is queued and stops the sender.
func (s *Sink) Close() {
	if s == nil {
		return
	}
	s.once.Do(func() {
		close(s.events)
		<-s.done
	})
}

func (s *Sink) loop() {
	defer close(s.done)
	ticker := time.NewTicker(flushInterval)
	defer ticker.Stop()
	var batch []map[string]any
	for {
		select {
		case ev, ok := <-s.events:
			if !ok {
				s.send(batch)
				return
			}
			batch = append(batch, ev)
			if len(batch) >= batchSize {
				s.send(batch)
				batch = nil
			}
		case <-ticker.C:
			s.send(batch)
			batch = nil
		}
	}
}

func (s *Sink) send(batch []map[string]any) {
	if len(batch) == 0 {
		return
	}
	body, err := gzipJSON(batch)
	if err != nil {
		s.drop(len(batch), err.Error())
		return
	}
	for attempt := 1; attempt <= sendAttempts; attempt++ {
		status, err := s.post(body)
		if err == nil && status < 300 {
			return
		}
		retry := err != nil || status == http.StatusTooManyRequests || status >= 500
		if !retry || attempt == sendAttempts {
			s.drop(len(batch), fmt.Sprintf("status %d: %v", status, err))
			return
		}
		time.Sleep(sendBackoff[min(attempt-1, len(sendBackoff)-1)])
	}
}

func (s *Sink) post(body []byte) (int, error) {
	req, err := http.NewRequestWithContext(context.Background(), http.MethodPost, s.url, bytes.NewReader(body))
	if err != nil {
		return 0, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Content-Encoding", "gzip")
	req.Header.Set(s.header, s.key)
	resp, err := s.client.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)
	return resp.StatusCode, nil
}

func (s *Sink) drop(n int, reason string) {
	s.mu.Lock()
	s.dropped += n
	total := s.dropped
	s.mu.Unlock()
	log.Printf("[NEWRELIC] dropped %d events (%s); %d dropped since start", n, reason, total)
}

// Dropped reports how many events were lost.
func (s *Sink) Dropped() int {
	if s == nil {
		return 0
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.dropped
}

func gzipJSON(v any) ([]byte, error) {
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	if err := json.NewEncoder(zw).Encode(v); err != nil {
		return nil, err
	}
	if err := zw.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// flatten keeps New Relic attribute values scalar.
func flatten(v any) any {
	switch t := v.(type) {
	case nil, string, bool, int, int32, int64, float32, float64:
		return t
	case time.Duration:
		return t.Milliseconds()
	case time.Time:
		return t.UnixMilli()
	default:
		b, err := json.Marshal(t)
		if err != nil {
			return fmt.Sprint(t)
		}
		return string(b)
	}
}
