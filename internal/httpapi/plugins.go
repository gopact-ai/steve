package httpapi

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"

	"github.com/gopact-ai/steve/internal/consoleapi"
	"github.com/gopact-ai/steve/internal/plugins"
)

func (s *Server) SetPlugins(service consoleapi.PluginsService) { s.plugins = service }
func (s *Server) pluginRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /console/plugins", s.guard(s.consolePlugins))
	mux.HandleFunc("POST /console/plugins/preview", s.guard(s.consolePlugins))
	mux.HandleFunc("POST /console/plugins/import", s.guard(s.consolePlugins))
	mux.HandleFunc("PUT /console/plugins/installations/{id}", s.guard(s.consolePlugins))
	mux.HandleFunc("POST /console/plugins/installations/{id}/prepare", s.guard(s.consolePlugins))
	mux.HandleFunc("GET /console/plugins/nodes/{node}/secrets", s.guard(s.consolePlugins))
}

func (s *Server) consolePlugins(w http.ResponseWriter, r *http.Request) {
	if s.plugins == nil {
		http.Error(w, "plugins are not configured", http.StatusNotImplemented)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	var result any
	var err error
	switch r.Pattern {
	case "GET /console/plugins":
		result, err = s.plugins.Plugins(r.Context())
	case "POST /console/plugins/preview":
		var source plugins.Source
		if err = readPluginBody(w, r, &source); err == nil {
			result, err = s.plugins.PreviewPlugin(r.Context(), source)
		}
	case "POST /console/plugins/import":
		var request consoleapi.PluginImportRequest
		if err = readPluginBody(w, r, &request); err == nil {
			result, err = s.plugins.ImportPlugin(r.Context(), request)
		}
	case "PUT /console/plugins/installations/{id}":
		var request consoleapi.PluginUpdateRequest
		if err = readPluginBody(w, r, &request); err == nil {
			result, err = s.plugins.UpdatePlugin(r.Context(), r.PathValue("id"), request)
		}
	case "POST /console/plugins/installations/{id}/prepare":
		result, err = s.plugins.PreparePlugin(r.Context(), r.PathValue("id"))
	case "GET /console/plugins/nodes/{node}/secrets":
		result, err = s.plugins.PluginSecrets(r.Context(), r.PathValue("node"))
	}
	if err != nil {
		status := http.StatusBadRequest
		if errors.Is(err, plugins.ErrConflict) || errors.Is(err, consoleapi.ErrSettingsConflict) {
			status = http.StatusConflict
		}
		if errors.Is(err, plugins.ErrUnavailable) {
			status = http.StatusServiceUnavailable
		}
		w.WriteHeader(status)
		writeJSON(w, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, result)
}

func readPluginBody(w http.ResponseWriter, r *http.Request, value any) error {
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 256<<10))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(value); err != nil {
		return err
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return errors.New("expected one plugin request object")
	}
	return nil
}
