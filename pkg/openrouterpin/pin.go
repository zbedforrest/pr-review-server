// Package openrouterpin serves a loopback proxy in front of OpenRouter that
// adds provider routing preferences to matching requests. The Codex CLI that
// runs Lite's finders cannot send OpenRouter's provider field itself, so the
// preferences are applied on the way out instead.
package openrouterpin

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strconv"
	"strings"
)

// Prefs selects requests by model prefix and the providers to prefer or skip.
type Prefs struct {
	Match  string
	Order  []string
	Ignore []string
}

// Enabled reports whether the preferences would change any request.
func (p Prefs) Enabled() bool { return len(p.Order) > 0 || len(p.Ignore) > 0 }

// Start serves the proxy on a loopback port and returns the base URL to use in
// place of upstream (an OpenRouter API root such as https://openrouter.ai/api/v1).
func Start(upstream string, prefs Prefs) (string, error) {
	up, err := url.Parse(strings.TrimRight(upstream, "/"))
	if err != nil || up.Scheme == "" || up.Host == "" {
		return "", fmt.Errorf("openrouterpin: bad upstream %q", upstream)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return "", fmt.Errorf("openrouterpin: listen: %w", err)
	}
	go func() { _ = http.Serve(ln, Handler(up, prefs)) }()
	return "http://" + ln.Addr().String(), nil
}

// Handler forwards every request to upstream, with the request path appended
// to upstream's path, and streams responses through unbuffered.
func Handler(up *url.URL, prefs Prefs) http.Handler {
	return &httputil.ReverseProxy{
		FlushInterval: -1,
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.SetURL(up)
			pr.Out.Host = up.Host
			apply(pr.Out, prefs)
		},
	}
}

// apply adds the provider field to a JSON POST body whose model matches,
// unless the caller already set one.
func apply(r *http.Request, prefs Prefs) {
	if r.Method != http.MethodPost || r.Body == nil || !prefs.Enabled() {
		return
	}
	body, err := io.ReadAll(r.Body)
	_ = r.Body.Close()
	if err == nil {
		body = withProvider(body, prefs)
	}
	r.Body = io.NopCloser(bytes.NewReader(body))
	r.ContentLength = int64(len(body))
	r.Header.Set("Content-Length", strconv.Itoa(len(body)))
}

func withProvider(body []byte, prefs Prefs) []byte {
	var m map[string]json.RawMessage
	if json.Unmarshal(body, &m) != nil {
		return body
	}
	var model string
	if raw, ok := m["model"]; !ok || json.Unmarshal(raw, &model) != nil || !strings.HasPrefix(model, prefs.Match) {
		return body
	}
	if _, set := m["provider"]; set {
		return body
	}
	provider := map[string]any{"allow_fallbacks": true}
	if len(prefs.Order) > 0 {
		provider["order"] = prefs.Order
	}
	if len(prefs.Ignore) > 0 {
		provider["ignore"] = prefs.Ignore
	}
	raw, err := json.Marshal(provider)
	if err != nil {
		return body
	}
	m["provider"] = raw
	out, err := json.Marshal(m)
	if err != nil {
		return body
	}
	return out
}
