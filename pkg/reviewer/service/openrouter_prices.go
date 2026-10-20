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
// It is the fallback when the live listing is unreachable; values as listed
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
// from OpenRouter's public model listing. The listing is fetched once per
// process and refreshed daily, because a hard-coded price drifted 10x from
// the list price within weeks.
type openRouterPriceBook struct {
	client *http.Client
	url    string

	mu      sync.Mutex
	fetched time.Time
	prices  map[string][3]float64
}

var openRouterPrices = &openRouterPriceBook{client: &http.Client{Timeout: 10 * time.Second}, url: DefaultOpenRouterBaseURL + "/models"}

func (b *openRouterPriceBook) price(model string) ([3]float64, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if time.Since(b.fetched) >= openRouterPriceTTL {
		live, err := b.fetch()
		if err != nil {
			log.Printf("[AGENT] openrouter model listing: %v; pricing from the built-in table", err)
			b.fetched = time.Now().Add(openRouterPriceRetry - openRouterPriceTTL)
		} else {
			b.prices, b.fetched = live, time.Now()
		}
	}
	if p, ok := b.prices[model]; ok {
		return p, true
	}
	p, ok := openRouterListPrices[model]
	return p, ok
}

func (b *openRouterPriceBook) fetch() (map[string][3]float64, error) {
	resp, err := b.client.Get(b.url)
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
	perMillion := func(s string) (float64, error) {
		if s == "" {
			return 0, nil
		}
		v, err := strconv.ParseFloat(s, 64)
		return v * 1e6, err
	}
	prices := make(map[string][3]float64, len(body.Data))
	for _, m := range body.Data {
		in, err1 := perMillion(m.Pricing.Prompt)
		cached, err2 := perMillion(m.Pricing.InputCacheRead)
		out, err3 := perMillion(m.Pricing.Completion)
		if m.ID == "" || err1 != nil || err2 != nil || err3 != nil {
			continue
		}
		if m.Pricing.InputCacheRead == "" {
			cached = in
		}
		prices[m.ID] = [3]float64{in, cached, out}
	}
	return prices, nil
}
