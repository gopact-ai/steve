package readmodel

import (
	"embed"
	"io/fs"
	"net/http"
	"path"
	"strings"
)

// The console is a built React app embedded whole, so the binary is the
// whole deployment: no asset directory to lose, nothing served from disk
// at runtime. The source lives in web/console; `make console` (npm run
// build there) refreshes web/dist before `go build`.
//
//go:embed all:web/dist
var dist embed.FS

func (s *Server) page(w http.ResponseWriter, r *http.Request) {
	root, err := fs.Sub(dist, "web/dist")
	if err != nil {
		http.Error(w, "console not built", http.StatusInternalServerError)
		return
	}
	name := strings.TrimPrefix(path.Clean(r.URL.Path), "/")
	if name == "" || name == "." {
		name = "index.html"
	}
	file, err := root.Open(name)
	if err != nil {
		// Unknown paths are the app's own routes: hand them the shell.
		name = "index.html"
		if file, err = root.Open(name); err != nil {
			http.NotFound(w, r)
			return
		}
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil || info.IsDir() {
		http.NotFound(w, r)
		return
	}
	if name == "index.html" {
		// The shell reads live state; a cached copy would show a stale
		// fleet. Hashed assets under assets/ may be cached for good.
		w.Header().Set("Cache-Control", "no-store")
	} else {
		w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
	}
	http.ServeFileFS(w, r, root, name)
}
