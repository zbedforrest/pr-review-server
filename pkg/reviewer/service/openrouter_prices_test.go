package service

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func priceBookAt(t *testing.T, handler http.HandlerFunc) (*openRouterPriceBook, *int) {
	t.Helper()
	hits := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		handler(w, r)
	}))
	t.Cleanup(srv.Close)
	return &openRouterPriceBook{client: srv.Client(), url: srv.URL}, &hits
}

func TestPriceBookReadsTheLiveListingOnce(t *testing.T) {
	book, hits := priceBookAt(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"data":[{"id":"deepseek/deepseek-v4.1-flash","pricing":{"prompt":"0.00000002832","completion":"0.00000012","input_cache_read":"0.000000000566"}},{"id":"acme/no-cache","pricing":{"prompt":"0.000001","completion":"0.000002"}}]}`))
	})
	p, ok := book.price("deepseek/deepseek-v4.1-flash")
	if !ok || p != [3]float64{0.02832, 0.000566, 0.12} {
		t.Fatalf("price = %v, %v", p, ok)
	}
	if p, ok := book.price("acme/no-cache"); !ok || p != [3]float64{1, 1, 2} {
		t.Fatalf("uncached price = %v, %v", p, ok)
	}
	if _, ok := book.price("acme/unlisted"); ok {
		t.Fatal("an unlisted model has no price")
	}
	if *hits != 1 {
		t.Fatalf("listing fetched %d times", *hits)
	}
}

func TestPriceBookFallsBackToTheTable(t *testing.T) {
	book, hits := priceBookAt(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
	})
	p, ok := book.price("deepseek/deepseek-v4.1-flash")
	if !ok || p != openRouterListPrices["deepseek/deepseek-v4.1-flash"] {
		t.Fatalf("price = %v, %v", p, ok)
	}
	if _, ok := book.price("anthropic/claude-fable-5.1"); ok {
		t.Fatal("the table has no Fable entry")
	}
	if *hits != 1 {
		t.Fatalf("a failed listing was refetched %d times within the retry window", *hits)
	}
}

func TestCodexUsageIsPricedFromTheBook(t *testing.T) {
	prev := openRouterPrices
	openRouterPrices, _ = priceBookAt(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"data":[{"id":"acme/m","pricing":{"prompt":"0.000001","completion":"0.000002","input_cache_read":"0.0000005"}}]}`))
	})
	t.Cleanup(func() { openRouterPrices = prev })
	r := &agentParseResult{}
	noteCodexUsage(r, map[string]any{"usage": map[string]any{"input_tokens": 1000000.0, "cached_input_tokens": 500000.0, "output_tokens": 100000.0}}, "acme/m")
	if want := 0.5 + 0.25 + 0.2; r.costUSD < want-1e-9 || r.costUSD > want+1e-9 {
		t.Fatalf("cost = %v, want %v", r.costUSD, want)
	}
	if r.inputTokens != 1000000 || r.outputTokens != 100000 {
		t.Fatalf("tokens = %d/%d", r.inputTokens, r.outputTokens)
	}
}
