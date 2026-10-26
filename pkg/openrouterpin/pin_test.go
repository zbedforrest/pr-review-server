package openrouterpin

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

func echoUpstream(t *testing.T) (*url.URL, *[]map[string]any, *[]string) {
	t.Helper()
	var bodies []map[string]any
	var paths []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.Path)
		var m map[string]any
		_ = json.NewDecoder(r.Body).Decode(&m)
		bodies = append(bodies, m)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: ok\n\n")
	}))
	t.Cleanup(srv.Close)
	up, _ := url.Parse(srv.URL + "/api/v1")
	return up, &bodies, &paths
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
	up, bodies, paths := echoUpstream(t)
	h := Handler(up, Prefs{Match: "deepseek/", Order: []string{"digitalocean", "fireworks"}, Ignore: []string{"google-vertex"}})

	if got := post(t, h, "/responses", `{"model":"deepseek/deepseek-v4.1-flash","input":"x","stream":true}`); got != "data: ok\n\n" {
		t.Fatalf("response not streamed through: %q", got)
	}
	if (*paths)[0] != "/api/v1/responses" {
		t.Errorf("path %q, want /api/v1/responses", (*paths)[0])
	}
	provider, _ := (*bodies)[0]["provider"].(map[string]any)
	if provider == nil {
		t.Fatal("provider preferences missing")
	}
	if got, _ := json.Marshal(provider); string(got) != `{"allow_fallbacks":true,"ignore":["google-vertex"],"order":["digitalocean","fireworks"]}` {
		t.Errorf("provider = %s", got)
	}
	if (*bodies)[0]["input"] != "x" {
		t.Error("other fields were not kept")
	}
}

func TestOtherModelsAndExplicitProvidersPassUnchanged(t *testing.T) {
	up, bodies, _ := echoUpstream(t)
	h := Handler(up, Prefs{Match: "deepseek/", Order: []string{"digitalocean"}})

	post(t, h, "/chat/completions", `{"model":"anthropic/claude-haiku-4.5","messages":[]}`)
	post(t, h, "/chat/completions", `{"model":"deepseek/deepseek-v4.1-flash","provider":{"only":["together"]}}`)

	if _, set := (*bodies)[0]["provider"]; set {
		t.Error("a non-matching model got provider preferences")
	}
	if got, _ := json.Marshal((*bodies)[1]["provider"]); string(got) != `{"only":["together"]}` {
		t.Errorf("an explicit provider was overwritten: %s", got)
	}
}

func TestStartRejectsABadUpstream(t *testing.T) {
	if _, err := Start("not a url", Prefs{Order: []string{"x"}}); err == nil {
		t.Fatal("want an error for a bad upstream")
	}
}
