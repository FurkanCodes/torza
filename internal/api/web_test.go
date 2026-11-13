package api

import (
	"net/http"
	"strings"
	"testing"
)

func TestWebPageServed(t *testing.T) {
	h := webServer()
	for _, tc := range []struct{ path, contentType, contains string }{
		{"/", "text/html", `<script src="/ui/app.js"`},
		{"/ui/app.js", "text/javascript", "textContent"},
		{"/ui/app.css", "text/css", "--accent"},
	} {
		rec := call(h, http.MethodGet, tc.path, "", "")
		if rec.Code != http.StatusOK {
			t.Fatalf("%s: status %d", tc.path, rec.Code)
		}
		if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, tc.contentType) {
			t.Errorf("%s: Content-Type %q", tc.path, ct)
		}
		if !strings.Contains(rec.Body.String(), tc.contains) {
			t.Errorf("%s: body lacks %q", tc.path, tc.contains)
		}
		csp := rec.Header().Get("Content-Security-Policy")
		if !strings.Contains(csp, "script-src 'self'") || strings.Contains(csp, "unsafe-inline") {
			t.Errorf("%s: weak CSP %q", tc.path, csp)
		}
	}
}

func TestWebPageNeedsNoKeyButAPIStillDoes(t *testing.T) {
	h := webServer()
	if rec := call(h, http.MethodGet, "/", "", ""); rec.Code != http.StatusOK {
		t.Fatalf("page: status %d", rec.Code)
	}
	if rec := call(h, http.MethodGet, "/v1/items", "", ""); rec.Code != http.StatusUnauthorized {
		t.Fatalf("API without key: status %d, want 401", rec.Code)
	}
	if rec := call(h, http.MethodGet, "/ui/nothing.js", "", ""); rec.Code != http.StatusNotFound {
		t.Fatalf("unknown asset: status %d, want 404", rec.Code)
	}
}

func TestMediaOrigin(t *testing.T) {
	for in, want := range map[string]string{
		"http://localhost:8081":       " http://localhost:8081",
		"https://torza.example.com/x": " https://torza.example.com",
		"":                            "",
		"not a url":                   "",
	} {
		if got := mediaOrigin(in); got != want {
			t.Errorf("mediaOrigin(%q) = %q, want %q", in, got, want)
		}
	}
}
