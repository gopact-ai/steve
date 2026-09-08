package httpapi

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"

	"github.com/gopact-ai/steve/internal/consoleapi"
)

func (s *Server) SetDesktop(service consoleapi.DesktopService) { s.desktop = service }

func (s *Server) consoleDesktop(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	if s.desktop == nil {
		writeJSON(w, consoleapi.DesktopStatus{})
		return
	}
	status, err := s.desktop.DesktopStatus(r.Context())
	if err != nil {
		writeDesktopError(w, err, http.StatusInternalServerError)
		return
	}
	writeJSON(w, status)
}

func (s *Server) consoleDesktopAgents(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	if s.desktop == nil {
		writeDesktopError(w, errors.New("desktop setup is unavailable"), http.StatusNotImplemented)
		return
	}
	if r.Method == http.MethodGet {
		result, err := s.desktop.DesktopDiscover(r.Context())
		if err != nil {
			writeDesktopError(w, err, http.StatusInternalServerError)
			return
		}
		if result.Agents == nil {
			result.Agents = []consoleapi.DesktopAgentCandidate{}
		}
		writeJSON(w, result)
		return
	}
	var request consoleapi.DesktopEnrollRequest
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16<<10))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&request); err != nil {
		writeDesktopError(w, err, http.StatusBadRequest)
		return
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		writeDesktopError(w, errors.New("expected one enrollment object"), http.StatusBadRequest)
		return
	}
	result, err := s.desktop.DesktopEnroll(r.Context(), request)
	if err != nil {
		writeDesktopError(w, err, http.StatusBadRequest)
		return
	}
	writeJSON(w, result)
}

func writeDesktopError(w http.ResponseWriter, err error, status int) {
	w.WriteHeader(status)
	writeJSON(w, map[string]string{"error": err.Error()})
}
