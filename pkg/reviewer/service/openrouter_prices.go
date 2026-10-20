package service

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"strconv"
	"sync"
	"time"
)

// openRouterListPrices is USD per million tokens: input, cached input, output.
// It is the fallback until a live listing has been read; values as listed
// on 2026-10-20.
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
// from the model listing of the OpenRouter root the runtime talks to. The
// listing is read once per process and refreshed daily, because a
// hard-coded price drifted 10x from the list price within weeks. A refresh
// runs outside the lock, so callers price from the last listing or the
// table while it is in flight.
type openRouterPriceBook struct {
	client *http.Client
	now    func() time.Time

	mu          sync.Mutex
	url         string
	nextAttempt time.Time
	refreshing  bool
	prices      map[string][3]float64
}

var openRouterPrices = &openRouterPriceBook{client: &http.Client{Timeout: 10 * time.Second}, now: time.Now}

func (b *openRouterPriceBook) price(baseURL, model string) ([3]float64, bool) {
	url := baseURL + "/models"
	b.mu.Lock()
	if url != b.url {
		b.url, b.prices, b.nextAttempt = url, nil, time.Time{}
	}
	if !b.refreshing && !b.now().Before(b.nextAttempt) {
		b.refreshing = true
		b.mu.Unlock()
		live, err := b.fetch(url)
		b.mu.Lock()
		b.refreshing = false
		if b.url == url {
			if err != nil {
				log.Printf("[AGENT] openrouter model listing: %v; pricing from the last listing or the built-in table", err)
				b.nextAttempt = b.now().Add(openRouterPriceRetry)
			} else {
				b.prices, b.nextAttempt = live, b.now().Add(openRouterPriceTTL)
			}
		}
	}
	defer b.mu.Unlock()
	if p, ok := b.prices[model]; ok {
		return p, true
	}
	p, ok := openRouterListPrices[model]
	return p, ok
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
		return v * 1e6, err == nil && v >= 0
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
