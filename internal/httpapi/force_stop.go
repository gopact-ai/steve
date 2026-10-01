package httpapi

import (
	"net/http"

	"github.com/gopact-ai/steve/internal/consoleapi"
)

func (s *Server) SetForceStops(control consoleapi.ForceStops) { s.forceStops = control }
func (s *Server) consoleForceStop(w http.ResponseWriter, r *http.Request) {
	if s.forceStops == nil {
		http.Error(w, "force stop is not enabled", http.StatusNotImplemented)
		return
	}
	if err := s.forceStops.ForceStop(r.Context(), r.PathValue("attempt")); err != nil {
		http.Error(w, err.Error(), http.StatusConflict)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusAccepted)
	writeJSON(w, map[string]bool{"accepted": true})
}
