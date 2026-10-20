package service

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"math"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

// openRouterListPrices is USD per million tokens: input, cached input, output.
// It prices a model only until a live listing of its root has been read;
// values as listed on 2026-10-20.
var openRouterListPrices = map[string][3]float64{
	"deepseek/deepseek-v4.1-flash": {0.02832, 0.000566, 0.12},
	"anthropic/claude-fable-5.1":   {10, 0.25, 50},
	"anthropic/claude-opus-5.5":    {4, 0.20, 20},
}

const (
	openRouterPriceTTL   = 24 * time.Hour
	openRouterPriceRetry = time.Hour
)

// openRouterPriceBook prices Codex usage, which reports tokens but no cost,
// from the model listing of each OpenRouter root the runtime talks to. A
// listing is read in the background the first time a root is priced and
// refreshed daily, because a hard-coded price drifted 10x from the list
// price within weeks; until it arrives the table prices the usage.
//
// The listing's price is the model-level one, which OpenRouter takes from
// its cheapest endpoint. With no provider order pinned, a call can be
// served by a dearer endpoint (the spread is about 20x for some models),
// so this is an estimate on the low side, not the bill. Pinning
// providers.order, or pricing each generation by its id, would make it
// exact.
type openRouterPriceBook struct {
	client *http.Client
	now    func() time.Time

	mu       sync.Mutex
	roots    map[string]*openRouterListing
	warned   map[string]bool
	inflight sync.WaitGroup
}

type openRouterListing struct {
	prices      map[string][3]float64
	nextAttempt time.Time
	refreshing  bool
}

var openRouterPrices = &openRouterPriceBook{client: &http.Client{Timeout: 10 * time.Second}, now: time.Now}

func (b *openRouterPriceBook) price(baseURL, model string) ([3]float64, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.roots == nil {
		b.roots, b.warned = map[string]*openRouterListing{}, map[string]bool{}
	}
	l := b.roots[baseURL]
	if l == nil {
		l = &openRouterListing{}
		b.roots[baseURL] = l
	}
	if !l.refreshing && !b.now().Before(l.nextAttempt) {
		l.refreshing = true
		b.inflight.Add(1)
		go b.refresh(baseURL, l)
	}
	source, prices := "listing", l.prices
	if prices == nil {
		source, prices = "built-in table", openRouterListPrices
	}
	p, ok := prices[model]
	if !ok && !b.warned[baseURL+" "+model] {
		b.warned[baseURL+" "+model] = true
		log.Printf("[AGENT] openrouter %s for %s has no price for %s; its cost goes unrecorded", source, baseURL, model)
	}
	return p, ok
}

func (b *openRouterPriceBook) refresh(baseURL string, l *openRouterListing) {
	defer b.inflight.Done()
	live, err := b.fetch(strings.TrimRight(baseURL, "/") + "/models")
	b.mu.Lock()
	defer b.mu.Unlock()
	l.refreshing = false
	if err != nil {
		log.Printf("[AGENT] openrouter model listing at %s: %v; pricing from the last listing or the built-in table", baseURL, err)
		l.nextAttempt = b.now().Add(openRouterPriceRetry)
		return
	}
	l.prices, l.nextAttempt = live, b.now().Add(openRouterPriceTTL)
}

func (b *openRouterPriceBook) fetch(url string) (map[string][3]float64, error) {
	resp, err := b.client.Get(url)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	var body struct {
		Data []struct {
			ID      string `json:"id"`
			Pricing struct {
				Prompt         string `json:"prompt"`
				Completion     string `json:"completion"`
				InputCacheRead string `json:"input_cache_read"`
			} `json:"pricing"`
		} `json:"data"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 32<<20)).Decode(&body); err != nil {
		return nil, err
	}
	if len(body.Data) == 0 {
		return nil, errors.New("empty listing")
	}
	perMillion := func(s string) (float64, bool) {
		v, err := strconv.ParseFloat(s, 64)
		v *= 1e6
		return v, err == nil && v >= 0 && !math.IsInf(v, 0) && !math.IsNaN(v)
	}
	prices := make(map[string][3]float64, len(body.Data))
	for _, m := range body.Data {
		in, okIn := perMillion(m.Pricing.Prompt)
		out, okOut := perMillion(m.Pricing.Completion)
		if m.ID == "" || !okIn || !okOut {
			continue
		}
		cached, okCached := perMillion(m.Pricing.InputCacheRead)
		if !okCached {
			cached = in
		}
		prices[m.ID] = [3]float64{in, cached, out}
	}
	if len(prices) == 0 {
		return nil, errors.New("listing has no priced models")
	}
	return prices, nil
}
