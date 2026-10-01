package httpapi

import (
	"encoding/json"
	"io"
	"net/http"

	"github.com/gopact-ai/steve/internal/consoleapi"
)

func (s *Server) SetForceStops(control consoleapi.ForceStops) { s.forceStops = control }
func (s *Server) consoleForceStop(w http.ResponseWriter, r *http.Request) {
	if s.forceStops == nil {
		http.Error(w, "force stop is not enabled", http.StatusNotImplemented)
		return
	}
	var request struct {
		ExpectedRevision *uint64 `json:"expected_revision"`
	}
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&request); err != nil || request.ExpectedRevision == nil {
		http.Error(w, "expected force-stop revision is required", http.StatusBadRequest)
		return
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		http.Error(w, "invalid force-stop confirmation", http.StatusBadRequest)
		return
	}
	if err := s.forceStops.ForceStop(r.Context(), r.PathValue("attempt"), *request.ExpectedRevision); err != nil {
		http.Error(w, err.Error(), http.StatusConflict)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusAccepted)
	writeJSON(w, map[string]bool{"accepted": true})
}
