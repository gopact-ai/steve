package httpapi

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"

	"github.com/gopact-ai/steve/internal/consoleapi"
	"github.com/gopact-ai/steve/internal/desktop"
	"github.com/gopact-ai/steve/internal/i18n"
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
		writeDesktopError(w, r, err, http.StatusInternalServerError)
		return
	}
	writeJSON(w, status)
}

func (s *Server) consoleDesktopAgents(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	if s.desktop == nil {
		writeDesktopError(w, r, errors.New("desktop setup is unavailable"), http.StatusNotImplemented)
		return
	}
	if r.Method == http.MethodGet {
		result, err := s.desktop.DesktopDiscover(r.Context())
		if err != nil {
			writeDesktopError(w, r, err, http.StatusInternalServerError)
			return
		}
		if result.Agents == nil {
			result.Agents = []consoleapi.DesktopAgentCandidate{}
		}
		writeJSON(w, result)
		return
	}
	var request consoleapi.DesktopEnrollRequest
	if !decodeDesktop(w, r, &request) {
		return
	}
	result, err := s.desktop.DesktopEnroll(r.Context(), request)
	if err != nil {
		writeDesktopError(w, r, err, http.StatusBadRequest)
		return
	}
	writeJSON(w, result)
}

// writeDesktopError answers err in the language of the request r.
func writeDesktopError(w http.ResponseWriter, r *http.Request, err error, status int) {
	w.WriteHeader(status)
	writeJSON(w, map[string]string{"error": i18n.FromContext(r.Context()).Explain(err)})
}

// consoleDesktopSetup records where the first-run guide should open next.
func (s *Server) consoleDesktopSetup(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	if s.desktop == nil {
		writeDesktopError(w, r, errors.New("desktop setup is unavailable"), http.StatusNotImplemented)
		return
	}
	var request consoleapi.DesktopSetupRequest
	if !decodeDesktop(w, r, &request) {
		return
	}
	result, err := s.desktop.DesktopSetup(r.Context(), request)
	if err != nil {
		writeDesktopError(w, r, err, desktopStatusCode(err))
		return
	}
	writeJSON(w, result)
}

// consoleDesktopWorkspace moves the default project's directory on this computer.
func (s *Server) consoleDesktopWorkspace(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	if s.desktop == nil {
		writeDesktopError(w, r, errors.New("desktop setup is unavailable"), http.StatusNotImplemented)
		return
	}
	var request consoleapi.DesktopWorkspaceRequest
	if !decodeDesktop(w, r, &request) {
		return
	}
	result, err := s.desktop.DesktopWorkspace(r.Context(), request)
	if err != nil {
		writeDesktopError(w, r, err, desktopStatusCode(err))
		return
	}
	writeJSON(w, result)
}

// desktopStatusCode tells a refusal of the owner's input from a failure of
// this machine.
func desktopStatusCode(err error) int {
	if desktop.IsInputError(err) {
		return http.StatusBadRequest
	}
	return http.StatusInternalServerError
}

// decodeDesktop reads exactly one small JSON object of the expected shape.
func decodeDesktop(w http.ResponseWriter, r *http.Request, into any) bool {
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16<<10))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(into); err != nil {
		writeDesktopError(w, r, err, http.StatusBadRequest)
		return false
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		writeDesktopError(w, r, errors.New("expected one request object"), http.StatusBadRequest)
		return false
	}
	return true
}
