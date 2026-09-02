package readmodel

import (
	_ "embed"
	"net/http"
)

// dashboard is embedded so the binary is the whole deployment: no build step,
// no asset directory to lose, and nothing to serve from disk at runtime.
//
//go:embed web/index.html
var dashboard []byte

func (s *Server) page(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	// The page reads live state; a cached copy would show a stale fleet.
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write(dashboard)
}
