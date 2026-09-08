package httpapi

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"

	"github.com/gopact-ai/steve/internal/consoleapi"
)

func (s *Server) SetServices(control consoleapi.ServiceControl) { s.services = control }

// SealWrites waits for mutations already admitted through HTTP and refuses
// subsequent ones while the service checks quiescence and schedules restart.
func (s *Server) SealWrites() (func(), error) {
	if !s.mutationMu.TryLock() {
		return nil, &consoleapi.ServiceError{Code: "busy", Message: "Wait for the current configuration or submission to finish"}
	}
	if s.maintenance {
		s.mutationMu.Unlock()
		return nil, &consoleapi.ServiceError{Code: "busy", Message: "A service restart is already in progress"}
	}
	s.maintenance = true
	s.mutationMu.Unlock()
	return func() { s.mutationMu.Lock(); defer s.mutationMu.Unlock(); s.maintenance = false }, nil
}

func serviceError(w http.ResponseWriter, err error) {
	status, code := http.StatusBadRequest, "invalid"
	var e *consoleapi.ServiceError
	if errors.As(err, &e) {
		code = e.Code
		switch code {
		case "busy", "conflict":
			status = http.StatusConflict
		case "unsupported":
			status = http.StatusNotImplemented
		case "not_found":
			status = http.StatusNotFound
		case "offline", "unavailable":
			status = http.StatusServiceUnavailable
		}
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	writeJSON(w, map[string]string{"code": code, "error": err.Error()})
}
func (s *Server) consoleServices(w http.ResponseWriter, r *http.Request) {
	if s.services == nil {
		serviceError(w, &consoleapi.ServiceError{Code: "unsupported", Message: "Service controls are not configured"})
		return
	}
	view, err := s.services.Services(r.Context())
	if err != nil {
		serviceError(w, err)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	writeJSON(w, view)
}
func (s *Server) consoleRestart(w http.ResponseWriter, r *http.Request) {
	if s.services == nil {
		serviceError(w, &consoleapi.ServiceError{Code: "unsupported", Message: "Service controls are not configured"})
		return
	}
	name := r.PathValue("name")
	var op consoleapi.RestartOperation
	var err error
	if r.Method == http.MethodGet {
		op, err = s.services.RestartStatus(r.Context(), name, r.URL.Query().Get("command_id"))
	} else {
		var req consoleapi.RestartRequest
		decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096))
		decoder.DisallowUnknownFields()
		if err = decoder.Decode(&req); err != nil {
			serviceError(w, err)
			return
		}
		if decoder.Decode(&struct{}{}) != io.EOF {
			serviceError(w, errors.New("Expected one restart request"))
			return
		}
		op, err = s.services.Restart(r.Context(), name, req)
	}
	if err != nil {
		serviceError(w, err)
		return
	}
	if r.Method == http.MethodPost {
		defer s.services.RestartAccepted(name, op.CommandID)
	}
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(op); err != nil {
		return
	}
	if flush, ok := w.(http.Flusher); ok {
		flush.Flush()
	}
}
