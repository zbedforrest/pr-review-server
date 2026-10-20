package service

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

const flashListing = `{"data":[{"id":"deepseek/deepseek-v4.1-flash","pricing":{"prompt":"0.00000002832","completion":"0.00000012","input_cache_read":"0.000000000566"}},{"id":"acme/no-cache","pricing":{"prompt":"0.000001","completion":"0.000002"}},{"id":"acme/unpriced","pricing":{"prompt":"","completion":"0.000002"}},{"id":"acme/infinite","pricing":{"prompt":"Inf","completion":"0.000002"}}]}`

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

// priced asks the book and waits for the listing read it may have started.
func priced(book *openRouterPriceBook, baseURL, model string) ([3]float64, bool) {
	book.price(baseURL, model)
	book.inflight.Wait()
	return book.price(baseURL, model)
}

func TestPriceBookReadsTheLiveListingInTheBackground(t *testing.T) {
	srv := newPriceServer(t, flashListing)
	clock := time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)
	book := priceBookAt(srv, &clock)
	if p, ok := book.price(srv.URL, "deepseek/deepseek-v4.1-flash"); !ok || p != openRouterListPrices["deepseek/deepseek-v4.1-flash"] {
		t.Fatalf("before the listing arrives the table prices the model: %v, %v", p, ok)
	}
	book.inflight.Wait()
	p, ok := book.price(srv.URL, "deepseek/deepseek-v4.1-flash")
	if !ok || p != [3]float64{0.02832, 0.000566, 0.12} {
		t.Fatalf("price = %v, %v", p, ok)
	}
	if p, ok := book.price(srv.URL, "acme/no-cache"); !ok || p != [3]float64{1, 1, 2} {
		t.Fatalf("uncached price = %v, %v", p, ok)
	}
	for _, m := range []string{"acme/unpriced", "acme/infinite", "acme/unlisted"} {
		if _, ok := book.price(srv.URL, m); ok {
			t.Fatalf("%s must not be priced", m)
		}
	}
	if srv.hits != 1 {
		t.Fatalf("listing fetched %d times", srv.hits)
	}
	clock = clock.Add(openRouterPriceTTL)
	priced(book, srv.URL, "acme/no-cache")
	if srv.hits != 2 {
		t.Fatalf("listing not refreshed after the TTL: %d fetches", srv.hits)
	}
}

func TestPriceBookFallsBackAndRetriesHourly(t *testing.T) {
	srv := newPriceServer(t, "")
	srv.code = http.StatusBadGateway
	clock := time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)
	book := priceBookAt(srv, &clock)
	p, ok := priced(book, srv.URL, "deepseek/deepseek-v4.1-flash")
	if !ok || p != openRouterListPrices["deepseek/deepseek-v4.1-flash"] {
		t.Fatalf("price = %v, %v", p, ok)
	}
	if _, ok := priced(book, srv.URL, "anthropic/claude-fable-5.1"); ok {
		t.Fatal("the table has no Fable entry")
	}
	if srv.hits != 1 {
		t.Fatalf("a failed listing was refetched %d times within the retry window", srv.hits)
	}
	clock = clock.Add(openRouterPriceRetry)
	srv.code, srv.body = http.StatusOK, flashListing
	if p, ok := priced(book, srv.URL, "acme/no-cache"); !ok || p != [3]float64{1, 1, 2} {
		t.Fatalf("after the retry window price = %v, %v", p, ok)
	}
	if srv.hits != 2 {
		t.Fatalf("listing fetched %d times", srv.hits)
	}
}

func TestPriceBookKeepsOneListingPerRoot(t *testing.T) {
	public := newPriceServer(t, flashListing)
	gateway := newPriceServer(t, `{"data":[{"id":"acme/no-cache","pricing":{"prompt":"0.000003","completion":"0.000004"}}]}`)
	clock := time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)
	book := priceBookAt(public, &clock)
	priced(book, public.URL, "acme/no-cache")
	if p, ok := priced(book, gateway.URL, "acme/no-cache"); !ok || p != [3]float64{3, 3, 4} {
		t.Fatalf("gateway price = %v, %v", p, ok)
	}
	if p, ok := book.price(public.URL, "acme/no-cache"); !ok || p != [3]float64{1, 1, 2} {
		t.Fatalf("the public listing was lost on a root switch: %v, %v", p, ok)
	}
	if public.hits != 1 || gateway.hits != 1 {
		t.Fatalf("fetches public=%d gateway=%d", public.hits, gateway.hits)
	}
}

func TestPriceBookKeepsTheLastListingWhenARefreshFails(t *testing.T) {
	srv := newPriceServer(t, flashListing)
	clock := time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)
	book := priceBookAt(srv, &clock)
	priced(book, srv.URL, "acme/no-cache")
	clock = clock.Add(openRouterPriceTTL)
	srv.code, srv.body = http.StatusBadGateway, ""
	if p, ok := priced(book, srv.URL, "acme/no-cache"); !ok || p != [3]float64{1, 1, 2} {
		t.Fatalf("a failed refresh lost the last listing: %v, %v", p, ok)
	}
	if _, ok := book.price(srv.URL, "deepseek/deepseek-v4.1-flash"); !ok {
		t.Fatal("a listed model is priced from the kept listing")
	}
}

func TestCodexUsageIsPricedFromTheBook(t *testing.T) {
	srv := newPriceServer(t, `{"data":[{"id":"acme/m","pricing":{"prompt":"0.000001","completion":"0.000002","input_cache_read":"0.0000005"}}]}`)
	prev := openRouterPrices
	clock := time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)
	openRouterPrices = priceBookAt(srv, &clock)
	t.Cleanup(func() { openRouterPrices = prev })
	priced(openRouterPrices, srv.URL, "acme/m")
	usage := map[string]any{"usage": map[string]any{"input_tokens": 1000000.0, "cached_input_tokens": 500000.0, "output_tokens": 100000.0}}
	r := &agentParseResult{}
	noteCodexUsage(r, usage, srv.URL, "acme/m")
	if want := 0.5 + 0.25 + 0.2; r.costUSD < want-1e-9 || r.costUSD > want+1e-9 {
		t.Fatalf("cost = %v, want %v", r.costUSD, want)
	}
	if r.inputTokens != 1000000 || r.outputTokens != 100000 {
		t.Fatalf("tokens = %d/%d", r.inputTokens, r.outputTokens)
	}
	noModel := &agentParseResult{}
	noteCodexUsage(noModel, usage, srv.URL, "")
	if noModel.costUSD != 0 || noModel.inputTokens != 1000000 || srv.hits != 1 {
		t.Fatalf("usage without a model records tokens, no cost and no listing read: %+v, %d fetches", noModel, srv.hits)
	}
}
