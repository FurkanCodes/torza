package api

import (
	"embed"
	"net/http"
	"net/url"
)

// The test page: one HTML file plus its script and stylesheet, built into the
// binary. It only uses the public REST API, with the key the user pastes in.
//
//go:embed web/index.html web/app.js web/app.css
var webFiles embed.FS

// webHandler serves the page at / and its assets under /ui/.
func (s *Server) webHandler() http.Handler {
	csp := "default-src 'none'; script-src 'self'; style-src 'self'; connect-src 'self'; img-src 'self'; " +
		"media-src 'self'" + mediaOrigin(s.cfg.PublicURL) + "; base-uri 'none'; form-action 'self'; frame-ancestors 'none'"
	serve := func(name, contentType string) http.HandlerFunc {
		return func(w http.ResponseWriter, _ *http.Request) {
			b, err := webFiles.ReadFile("web/" + name)
			if err != nil {
				http.NotFound(w, nil)
				return
			}
			h := w.Header()
			h.Set("Content-Type", contentType)
			h.Set("Content-Security-Policy", csp)
			h.Set("X-Content-Type-Options", "nosniff")
			h.Set("Referrer-Policy", "no-referrer")
			h.Set("Cache-Control", "no-cache")
			_, _ = w.Write(b)
		}
	}
	mux := http.NewServeMux()
	mux.Handle("GET /{$}", serve("index.html", "text/html; charset=utf-8"))
	mux.Handle("GET /ui/app.js", serve("app.js", "text/javascript; charset=utf-8"))
	mux.Handle("GET /ui/app.css", serve("app.css", "text/css; charset=utf-8"))
	return mux
}

// mediaOrigin returns " <scheme>://<host>" for the stream service, so the
// page's video player may load from it; empty if PublicURL can't be parsed.
func mediaOrigin(publicURL string) string {
	u, err := url.Parse(publicURL)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return ""
	}
	return " " + u.Scheme + "://" + u.Host
}
