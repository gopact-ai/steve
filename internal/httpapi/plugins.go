package httpapi

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"

	"github.com/gopact-ai/steve/internal/consoleapi"
	"github.com/gopact-ai/steve/internal/plugins"
)

func (s *Server) SetPlugins(service consoleapi.PluginsService) { s.plugins = service }
func (s *Server) pluginRoutes(mux *http.ServeMux) {
	mux.HandleFunc("POST /console/plugins/installations/{id}/presets/preview", s.guard(s.consolePluginPreset))
	mux.HandleFunc("POST /console/plugins/installations/{id}/presets/apply", s.guard(s.consolePluginPreset))
	mux.HandleFunc("GET /console/plugins/installations/{id}/usage", s.guard(s.consolePluginRemoval))
	mux.HandleFunc("DELETE /console/plugins/installations/{id}", s.guard(s.consolePluginRemoval))
	mux.HandleFunc("POST /console/plugins/installations/{id}/runtimes/{runtime}/close", s.guard(s.consolePluginRuntimeClose))
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

func (s *Server) consolePluginPreset(w http.ResponseWriter, r *http.Request) {
	service, ok := s.plugins.(consoleapi.PluginPresetService)
	if !ok {
		http.Error(w, "plugin presets are unavailable", http.StatusNotImplemented)
		return
	}
	var req consoleapi.PluginPresetRequest
	err := readPluginBody(w, r, &req)
	var result consoleapi.PluginPresetPreview
	if err == nil {
		if strings.HasSuffix(r.URL.Path, "/preview") {
			result, err = service.PreviewPluginPreset(r.Context(), r.PathValue("id"), req)
		} else {
			result, err = service.ApplyPluginPreset(r.Context(), r.PathValue("id"), req)
		}
	}
	if err != nil {
		status := http.StatusBadRequest
		if errors.Is(err, consoleapi.ErrSettingsConflict) || errors.Is(err, plugins.ErrConflict) {
			status = http.StatusConflict
		}
		w.WriteHeader(status)
		writeJSON(w, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, result)
}

func (s *Server) consolePluginRemoval(w http.ResponseWriter, r *http.Request) {
	service, ok := s.plugins.(consoleapi.PluginRemovalService)
	if !ok {
		http.Error(w, "plugin removal is unavailable", http.StatusNotImplemented)
		return
	}
	var result any
	var err error
	if r.Method == http.MethodGet {
		result, err = service.PluginUsage(r.Context(), r.PathValue("id"))
	} else {
		var req consoleapi.PluginRemoveRequest
		if err = readPluginBody(w, r, &req); err == nil {
			result, err = service.RemovePlugin(r.Context(), r.PathValue("id"), req)
		}
	}
	if err != nil {
		status := http.StatusBadRequest
		if errors.Is(err, plugins.ErrRuntimeBusy) || errors.Is(err, consoleapi.ErrSettingsConflict) {
			status = http.StatusConflict
		}
		w.WriteHeader(status)
		writeJSON(w, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, result)
}

func (s *Server) consolePluginRuntimeClose(w http.ResponseWriter, r *http.Request) {
	service, ok := s.plugins.(consoleapi.PluginRuntimeCloseService)
	if !ok {
		http.Error(w, "plugin runtime close unavailable", http.StatusNotImplemented)
		return
	}
	result, err := service.ClosePluginRuntime(r.Context(), r.PathValue("id"), r.PathValue("runtime"))
	if err != nil {
		w.WriteHeader(http.StatusConflict)
		writeJSON(w, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, result)
}
