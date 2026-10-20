package service

import (
	"bytes"
	"math"
	"strings"
	"testing"
	"time"
)

func codexProc(stream string) *fakeProcess {
	return &fakeProcess{stdout: bytes.NewBufferString(stream), stderr: &bytes.Buffer{}, killCh: make(chan struct{})}
}

func TestParseCodexStreamRecordsUsageAndPricesKnownModels(t *testing.T) {
	stream := `{"type":"item.completed","item":{"type":"agent_message","text":"[]"}}
{"type":"turn.completed","usage":{"input_tokens":1000000,"cached_input_tokens":900000,"output_tokens":10000}}
`
	srv := newPriceServer(t, flashListing)
	prev := openRouterPrices
	clock := time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)
	openRouterPrices = priceBookAt(srv, &clock)
	t.Cleanup(func() { openRouterPrices = prev })
	res, err := parseCodexStreamModel(codexProc(stream), &bytes.Buffer{}, 10, srv.URL, "deepseek/deepseek-v4.1-flash")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if res.inputTokens != 1000000 || res.outputTokens != 10000 {
		t.Fatalf("tokens in=%d out=%d", res.inputTokens, res.outputTokens)
	}
	want := (100000*0.02832 + 900000*0.000566 + 10000*0.12) / 1e6
	if math.Abs(res.costUSD-want) > 1e-9 {
		t.Fatalf("cost = %v, want %v", res.costUSD, want)
	}
	unpriced, _ := parseCodexStreamModel(codexProc(stream), &bytes.Buffer{}, 10, srv.URL, "vendor/unlisted")
	if unpriced.costUSD != 0 || unpriced.inputTokens != 1000000 {
		t.Fatalf("an unlisted model must record tokens without inventing a price: %+v", unpriced)
	}
}

func TestParseCodexStreamIgnoresErrorsAfterACompletedTurn(t *testing.T) {
	after := `{"type":"error","message":"Model metadata for vendor/model not found. Defaulting to fallback metadata"}
{"type":"item.completed","item":{"type":"agent_message","text":"[]"}}
{"type":"turn.completed","usage":{"input_tokens":10,"output_tokens":2}}
{"type":"error","message":"stream closed after completion"}
`
	res, err := parseCodexStreamModel(codexProc(after), &bytes.Buffer{}, 10, "http://127.0.0.1:9", "")
	if err != nil || res.streamErr != "" {
		t.Fatalf("a completed turn must clear earlier transient errors and ignore later ones: err=%v streamErr=%q", err, res.streamErr)
	}
	failed := `{"type":"error","message":"provider returned 500"}
`
	res, _ = parseCodexStreamModel(codexProc(failed), &bytes.Buffer{}, 10, "http://127.0.0.1:9", "")
	if !strings.Contains(res.streamErr, "provider returned 500") {
		t.Fatalf("an error with no completed turn must fail the run, got %q", res.streamErr)
	}
}
