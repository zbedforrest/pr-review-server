package newrelic

import (
	"compress/gzip"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

func testSink(t *testing.T, handler http.HandlerFunc) *Sink {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	s := New(Config{AccountID: "123", LicenseKey: "lic"})
	s.url = srv.URL + "/v1/accounts/123/events"
	return s
}

func TestSinkFlushesGzippedBatchesWithTheKeyHeader(t *testing.T) {
	var mu sync.Mutex
	var got []map[string]any
	var header, encoding string
	s := testSink(t, func(w http.ResponseWriter, r *http.Request) {
		zr, err := gzip.NewReader(r.Body)
		if err != nil {
			t.Error(err)
			return
		}
		var batch []map[string]any
		_ = json.NewDecoder(zr).Decode(&batch)
		mu.Lock()
		got = append(got, batch...)
		header, encoding = r.Header.Get("Api-Key"), r.Header.Get("Content-Encoding")
		mu.Unlock()
	})
	s.Record("PrismReviewRun", map[string]any{"profile": "lite", "cost_usd": 0.15, "runs": []int{1, 2}, "took": 2 * time.Second})
	s.Close()
	mu.Lock()
	defer mu.Unlock()
	if len(got) != 1 || header != "lic" || encoding != "gzip" {
		t.Fatalf("got=%v header=%q encoding=%q", got, header, encoding)
	}
	ev := got[0]
	if ev["eventType"] != "PrismReviewRun" || ev["profile"] != "lite" || ev["runs"] != "[1,2]" || ev["took"] != float64(2000) || ev["timestamp"] == nil {
		t.Fatalf("event = %v", ev)
	}
}

func TestSinkDropsAfterRetriesWithoutBlocking(t *testing.T) {
	sendBackoff = []time.Duration{time.Millisecond}
	calls := 0
	s := testSink(t, func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.WriteHeader(http.StatusServiceUnavailable)
	})
	s.Record("PrismReviewRun", map[string]any{"x": 1})
	s.Close()
	if calls != sendAttempts || s.Dropped() != 1 {
		t.Fatalf("calls=%d dropped=%d", calls, s.Dropped())
	}
}

func TestUnconfiguredSinkIsANoOp(t *testing.T) {
	var s *Sink = New(Config{AccountID: "123"})
	if s != nil {
		t.Fatal("a sink without a key must be nil")
	}
	s.Record("PrismReviewRun", map[string]any{"x": 1})
	s.Close()
	if !strings.Contains(New(Config{AccountID: "1", InsertKey: "k", Region: "EU"}).url, "eu01") {
		t.Fatal("EU region must use the EU collector")
	}
}
