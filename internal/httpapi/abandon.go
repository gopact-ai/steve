package httpapi

import (
	"encoding/json"
	"io"
	"net/http"

	"github.com/gopact-ai/steve/internal/consoleapi"
)

func (s *Server) SetAbandons(control consoleapi.Abandons) { s.abandons = control }
func (s *Server) consoleAbandon(w http.ResponseWriter, r *http.Request) {
	if s.abandons == nil {
		http.Error(w, "abandonment is not enabled", http.StatusNotImplemented)
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
	result, err := s.abandons.Abandon(r.Context(), r.PathValue("attempt"), *request.ExpectedRevision)
	if err != nil {
		http.Error(w, err.Error(), http.StatusConflict)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusAccepted)
	writeJSON(w, result)
}
