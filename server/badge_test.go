package server

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestBadgeRouteServesSeveritySVGsWithoutAuth(t *testing.T) {
	server, _ := newTestServer(t, "tester")
	for _, sev := range []string{"critical", "medium", "low"} {
		w := httptest.NewRecorder()
		server.handleBadge(w, httptest.NewRequest(http.MethodGet, "/badge/"+sev+".svg", nil))
		if w.Code != http.StatusOK || w.Header().Get("Content-Type") != "image/svg+xml" {
			t.Fatalf("%s: code=%d type=%q", sev, w.Code, w.Header().Get("Content-Type"))
		}
		if !strings.Contains(w.Body.String(), "<title>"+strings.ToUpper(sev)+"</title>") {
			t.Errorf("%s badge must carry its label as the SVG title", sev)
		}
		if cc := w.Header().Get("Cache-Control"); !strings.Contains(cc, "max-age") {
			t.Errorf("%s: badges are immutable assets and should be cacheable, got %q", sev, cc)
		}
	}
	w := httptest.NewRecorder()
	server.handleBadge(w, httptest.NewRequest(http.MethodGet, "/badge/urgent.svg", nil))
	if w.Code != http.StatusNotFound {
		t.Errorf("unknown severity must 404, got %d", w.Code)
	}
}
