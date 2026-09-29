package auth

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestSafeReturnPathKeepsOnlySameOriginPaths(t *testing.T) {
	for in, want := range map[string]string{
		"/pages/quarterly-review/":   "/pages/quarterly-review/",
		"/prs?filter=mine":           "/prs?filter=mine",
		"":                           "",
		"pages/x":                    "",
		"//evil.example.com/x":       "",
		"/\\evil.example.com":        "",
		"https://evil.example.com/x": "",
		"/x\r\nSet-Cookie: a=b":      "",
		"/\t/evil.example.com":       "",
		"/\n/evil.example.com":       "",
		"/a/..\\..\\evil":            "",
	} {
		if got := SafeReturnPath(in); got != want {
			t.Errorf("SafeReturnPath(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestHandleLoginRemembersASafeNextPathOnly(t *testing.T) {
	a := newTestAuth(nil)
	cookieFor := func(next string) *http.Cookie {
		w := httptest.NewRecorder()
		a.HandleLogin(w, httptest.NewRequest(http.MethodGet, "/login?next="+next, nil))
		for _, c := range w.Result().Cookies() {
			if c.Name == ReturnToCookieName {
				return c
			}
		}
		return nil
	}
	if c := cookieFor("/pages/quarterly-review/"); c == nil || c.Value != "/pages/quarterly-review/" || !c.HttpOnly {
		t.Fatalf("safe next path not remembered: %+v", c)
	}
	if c := cookieFor("//evil.example.com"); c != nil {
		t.Fatalf("unsafe next path was remembered: %+v", c)
	}
}

func TestConsumeReturnToLandsOnTheRememberedPageAndClearsIt(t *testing.T) {
	r := httptest.NewRequest(http.MethodGet, "/auth/github/callback", nil)
	r.AddCookie(&http.Cookie{Name: ReturnToCookieName, Value: "/pages/quarterly-review/"})
	w := httptest.NewRecorder()
	if got := consumeReturnTo(w, r); got != "/pages/quarterly-review/" {
		t.Fatalf("got %q", got)
	}
	if c := w.Result().Cookies(); len(c) != 1 || c[0].MaxAge >= 0 {
		t.Fatalf("return-to cookie not cleared: %+v", c)
	}
	tampered := httptest.NewRequest(http.MethodGet, "/auth/github/callback", nil)
	tampered.AddCookie(&http.Cookie{Name: ReturnToCookieName, Value: "https://evil.example.com"})
	if got := consumeReturnTo(httptest.NewRecorder(), tampered); got != "/" {
		t.Fatalf("tampered cookie redirected to %q", got)
	}
	if got := consumeReturnTo(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/", nil)); got != "/" {
		t.Fatalf("no cookie should land on the dashboard, got %q", got)
	}
}
