package httpapi

import (
	"net/http"
	"path"
	"strings"

	"github.com/gopact-ai/steve/internal/readmodel/web"
)

func (s *Server) page(w http.ResponseWriter, r *http.Request) {
	root, err := web.Files()
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
