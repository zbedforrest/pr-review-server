package service

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

const flashListing = `{"data":[{"id":"deepseek/deepseek-v4.1-flash","pricing":{"prompt":"0.00000002832","completion":"0.00000012","input_cache_read":"0.000000000566"}},{"id":"acme/no-cache","pricing":{"prompt":"0.000001","completion":"0.000002"}},{"id":"acme/unpriced","pricing":{"prompt":"","completion":"0.000002"}}]}`

type priceServer struct {
	*httptest.Server
	hits int
	body string
	code int
}

func newPriceServer(t *testing.T, body string) *priceServer {
	t.Helper()
	s := &priceServer{body: body, code: http.StatusOK}
	s.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		s.hits++
		w.WriteHeader(s.code)
		_, _ = w.Write([]byte(s.body))
	}))
	t.Cleanup(s.Close)
	return s
}

func priceBookAt(s *priceServer, clock *time.Time) *openRouterPriceBook {
	return &openRouterPriceBook{client: s.Client(), now: func() time.Time { return *clock }}
}

func TestPriceBookReadsTheLiveListingOnce(t *testing.T) {
	srv := newPriceServer(t, flashListing)
	clock := time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)
	book := priceBookAt(srv, &clock)
	p, ok := book.price(srv.URL, "deepseek/deepseek-v4.1-flash")
	if !ok || p != [3]float64{0.02832, 0.000566, 0.12} {
		t.Fatalf("price = %v, %v", p, ok)
	}
	if p, ok := book.price(srv.URL, "acme/no-cache"); !ok || p != [3]float64{1, 1, 2} {
		t.Fatalf("uncached price = %v, %v", p, ok)
	}
	if _, ok := book.price(srv.URL, "acme/unpriced"); ok {
		t.Fatal("an entry without a prompt price is not priced at zero")
	}
	if _, ok := book.price(srv.URL, "acme/unlisted"); ok {
		t.Fatal("an unlisted model has no price")
	}
	if srv.hits != 1 {
		t.Fatalf("listing fetched %d times", srv.hits)
	}
	clock = clock.Add(openRouterPriceTTL)
	book.price(srv.URL, "acme/no-cache")
	if srv.hits != 2 {
		t.Fatalf("listing not refreshed after the TTL: %d fetches", srv.hits)
	}
}

func TestPriceBookFallsBackAndRetriesHourly(t *testing.T) {
	srv := newPriceServer(t, "")
	srv.code = http.StatusBadGateway
	clock := time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)
	book := priceBookAt(srv, &clock)
	p, ok := book.price(srv.URL, "deepseek/deepseek-v4.1-flash")
	if !ok || p != openRouterListPrices["deepseek/deepseek-v4.1-flash"] {
		t.Fatalf("price = %v, %v", p, ok)
	}
	if _, ok := book.price(srv.URL, "anthropic/claude-fable-5.1"); ok {
		t.Fatal("the table has no Fable entry")
	}
	if srv.hits != 1 {
		t.Fatalf("a failed listing was refetched %d times within the retry window", srv.hits)
	}
	clock = clock.Add(openRouterPriceRetry)
	srv.code, srv.body = http.StatusOK, flashListing
	if p, ok := book.price(srv.URL, "acme/no-cache"); !ok || p != [3]float64{1, 1, 2} {
		t.Fatalf("after the retry window price = %v, %v", p, ok)
	}
	if srv.hits != 2 {
		t.Fatalf("listing fetched %d times", srv.hits)
	}
}

func TestPriceBookFollowsTheConfiguredRoot(t *testing.T) {
	public := newPriceServer(t, flashListing)
	gateway := newPriceServer(t, `{"data":[{"id":"acme/no-cache","pricing":{"prompt":"0.000003","completion":"0.000004"}}]}`)
	clock := time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)
	book := priceBookAt(public, &clock)
	book.price(public.URL, "acme/no-cache")
	if p, ok := book.price(gateway.URL, "acme/no-cache"); !ok || p != [3]float64{3, 3, 4} {
		t.Fatalf("gateway price = %v, %v", p, ok)
	}
	if public.hits != 1 || gateway.hits != 1 {
		t.Fatalf("fetches public=%d gateway=%d", public.hits, gateway.hits)
	}
}

func TestPriceBookKeepsTheLastListingWhileRefreshing(t *testing.T) {
	srv := newPriceServer(t, flashListing)
	clock := time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)
	book := priceBookAt(srv, &clock)
	book.price(srv.URL, "acme/no-cache")
	clock = clock.Add(openRouterPriceTTL)
	srv.code, srv.body = http.StatusBadGateway, ""
	if p, ok := book.price(srv.URL, "acme/no-cache"); !ok || p != [3]float64{1, 1, 2} {
		t.Fatalf("a failed refresh lost the last listing: %v, %v", p, ok)
	}
}

func TestCodexUsageIsPricedFromTheBook(t *testing.T) {
	srv := newPriceServer(t, `{"data":[{"id":"acme/m","pricing":{"prompt":"0.000001","completion":"0.000002","input_cache_read":"0.0000005"}}]}`)
	prev := openRouterPrices
	clock := time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)
	openRouterPrices = priceBookAt(srv, &clock)
	t.Cleanup(func() { openRouterPrices = prev })
	r := &agentParseResult{}
	noteCodexUsage(r, map[string]any{"usage": map[string]any{"input_tokens": 1000000.0, "cached_input_tokens": 500000.0, "output_tokens": 100000.0}}, srv.URL, "acme/m")
	if want := 0.5 + 0.25 + 0.2; r.costUSD < want-1e-9 || r.costUSD > want+1e-9 {
		t.Fatalf("cost = %v, want %v", r.costUSD, want)
	}
	if r.inputTokens != 1000000 || r.outputTokens != 100000 {
		t.Fatalf("tokens = %d/%d", r.inputTokens, r.outputTokens)
	}
}
