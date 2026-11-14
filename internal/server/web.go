package server

import (
	"embed"
	"io/fs"
	"net/http"
)

// web holds the browser UI: plain HTML, CSS and JavaScript with no build
// step, served by the Go binary itself.
//
//go:embed web
var web embed.FS

// contentSecurityPolicy only allows Mosso's own scripts, styles and API.
// Claude's output is untrusted (it can echo prompt-injected text), so the UI
// never renders it as HTML, and this policy blocks inline and third-party
// scripts as a second line of defence.
const contentSecurityPolicy = "default-src 'none'; script-src 'self'; style-src 'self'; img-src 'self' data: blob:; " +
	"connect-src 'self'; base-uri 'none'; form-action 'none'; frame-ancestors 'none'"

func webUI() http.Handler {
	sub, err := fs.Sub(web, "web")
	if err != nil {
		panic(err)
	}
	files := http.FileServerFS(sub)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Content-Security-Policy", contentSecurityPolicy)
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Referrer-Policy", "no-referrer")
		h.Set("Cache-Control", "no-cache")
		files.ServeHTTP(w, r)
	})
}
