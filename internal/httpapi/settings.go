package httpapi

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"

	"github.com/gopact-ai/steve/internal/consoleapi"
)

func (s *Server) SetSettings(settings consoleapi.SettingsService) { s.settings = settings }
func (s *Server) consoleSettings(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	if s.settings == nil {
		http.Error(w, "settings are not configured", http.StatusNotImplemented)
		return
	}
	var view consoleapi.SettingsView
	var err error
	if r.Method == http.MethodGet {
		view, err = s.settings.Settings(r.Context())
	} else {
		var request consoleapi.SettingsUpdate
		decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20))
		decoder.DisallowUnknownFields()
		if err = decoder.Decode(&request); err == nil {
			if end := decoder.Decode(&struct{}{}); end != io.EOF {
				err = errors.New("expected one settings object")
			}
		}
		if err == nil {
			view, err = s.settings.UpdateSettings(r.Context(), request)
		}
	}
	if err != nil {
		status := http.StatusBadRequest
		if errors.Is(err, consoleapi.ErrSettingsConflict) {
			status = http.StatusConflict
		}
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}
	_ = json.NewEncoder(w).Encode(view)
}
