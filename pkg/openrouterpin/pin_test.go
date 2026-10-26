package openrouterpin

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
)

type seen struct {
	mu     sync.Mutex
	bodies []map[string]any
	paths  []string
}

func (s *seen) at(i int) (map[string]any, string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.bodies[i], s.paths[i]
}

func echoUpstream(t *testing.T) (*url.URL, *seen) {
	t.Helper()
	s := &seen{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var m map[string]any
		_ = json.NewDecoder(r.Body).Decode(&m)
		s.mu.Lock()
		s.paths = append(s.paths, r.URL.Path)
		s.bodies = append(s.bodies, m)
		s.mu.Unlock()
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: ok\n\n")
	}))
	t.Cleanup(srv.Close)
	up, _ := url.Parse(srv.URL + "/api/v1")
	return up, s
}

func post(t *testing.T, h http.Handler, path, body string) string {
	t.Helper()
	front := httptest.NewServer(h)
	defer front.Close()
	resp, err := http.Post(front.URL+path, "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	out, _ := io.ReadAll(resp.Body)
	return string(out)
}

func TestMatchingModelGetsProviderPreferences(t *testing.T) {
	up, s := echoUpstream(t)
	h := Handler(up, Prefs{Match: "deepseek/", Order: []string{"digitalocean", "fireworks"}, Ignore: []string{"google-vertex"}})

	if got := post(t, h, "/responses", `{"model":"deepseek/deepseek-v4.1-flash","input":"x","stream":true}`); got != "data: ok\n\n" {
		t.Fatalf("response not streamed through: %q", got)
	}
	body, path := s.at(0)
	if path != "/api/v1/responses" {
		t.Errorf("path %q, want /api/v1/responses", path)
	}
	provider, _ := body["provider"].(map[string]any)
	if provider == nil {
		t.Fatal("provider preferences missing")
	}
	if got, _ := json.Marshal(provider); string(got) != `{"allow_fallbacks":true,"ignore":["google-vertex"],"order":["digitalocean","fireworks"]}` {
		t.Errorf("provider = %s", got)
	}
	if body["input"] != "x" {
		t.Error("other fields were not kept")
	}
}

func TestOtherModelsAndExplicitProvidersPassUnchanged(t *testing.T) {
	up, s := echoUpstream(t)
	h := Handler(up, Prefs{Match: "deepseek/", Order: []string{"digitalocean"}})

	post(t, h, "/chat/completions", `{"model":"anthropic/claude-haiku-4.5","messages":[]}`)
	post(t, h, "/chat/completions", `{"model":"deepseek/deepseek-v4.1-flash","provider":{"only":["together"]}}`)

	other, _ := s.at(0)
	if _, set := other["provider"]; set {
		t.Error("a non-matching model got provider preferences")
	}
	explicit, _ := s.at(1)
	if got, _ := json.Marshal(explicit["provider"]); string(got) != `{"only":["together"]}` {
		t.Errorf("an explicit provider was overwritten: %s", got)
	}
}

func TestStartRejectsABadUpstream(t *testing.T) {
	if _, _, err := Start("not a url", Prefs{Order: []string{"x"}}); err == nil {
		t.Fatal("want an error for a bad upstream")
	}
}

func TestEmptyMatchPinsNothing(t *testing.T) {
	body := []byte(`{"model":"deepseek/deepseek-v4.1-flash"}`)
	if got := withProvider(body, Prefs{Order: []string{"digitalocean"}}); string(got) != string(body) {
		t.Fatalf("empty match changed the body: %s", got)
	}
}

func TestNullProviderStillGetsPreferences(t *testing.T) {
	got := withProvider([]byte(`{"model":"deepseek/x","provider":null}`), Prefs{Match: "deepseek/", Order: []string{"digitalocean"}})
	if !strings.Contains(string(got), `"order":["digitalocean"]`) {
		t.Fatalf("null provider was not replaced: %s", got)
	}
	if (Prefs{Order: []string{"digitalocean"}}).Enabled() {
		t.Error("preferences without a match prefix count as enabled")
	}
}
