package httpapi

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/gopact-ai/steve/internal/consoleapi"
	"github.com/gopact-ai/steve/internal/coordination"
)

func (s *Server) SetCoordination(service consoleapi.CoordinationService) { s.coordination = service }

func (s *Server) coordinationRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /console/coordination", s.guard(s.consoleCoordination))
	mux.HandleFunc("POST /console/coordination/transfer", s.guard(s.consoleCoordinationTransfer))
	mux.HandleFunc("PUT /console/coordination/policy", s.guard(s.consoleCoordinationPolicy))
	mux.HandleFunc("PUT /console/coordination/eligibility", s.guard(s.consoleCoordinationEligibility))
}

func (s *Server) consoleCoordination(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	if s.coordination == nil {
		_ = json.NewEncoder(w).Encode(consoleapi.CoordinationView{Nodes: []consoleapi.CoordinatorNode{}, Events: []consoleapi.CoordinatorEvent{}})
		return
	}
	view, err := s.coordination.Coordination(r.Context())
	s.coordinationResult(w, view, err)
}

func (s *Server) consoleCoordinationTransfer(w http.ResponseWriter, r *http.Request) {
	if !s.coordinationAvailable(w) {
		return
	}
	var request consoleapi.CoordinatorTransfer
	if !decodeSSH(w, r, &request) {
		return
	}
	view, err := s.coordination.TransferCoordinator(r.Context(), request)
	s.coordinationResult(w, view, err)
}

func (s *Server) consoleCoordinationPolicy(w http.ResponseWriter, r *http.Request) {
	if !s.coordinationAvailable(w) {
		return
	}
	var request consoleapi.CoordinatorPolicy
	if !decodeSSH(w, r, &request) {
		return
	}
	view, err := s.coordination.SetAutoFailover(r.Context(), request)
	s.coordinationResult(w, view, err)
}

func (s *Server) consoleCoordinationEligibility(w http.ResponseWriter, r *http.Request) {
	if !s.coordinationAvailable(w) {
		return
	}
	var request consoleapi.CoordinatorEligibility
	if !decodeSSH(w, r, &request) {
		return
	}
	view, err := s.coordination.SetCoordinatorEligibility(r.Context(), request)
	s.coordinationResult(w, view, err)
}

func (s *Server) coordinationAvailable(w http.ResponseWriter) bool {
	w.Header().Set("Content-Type", "application/json")
	if s.coordination != nil {
		return true
	}
	w.WriteHeader(http.StatusNotImplemented)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": "当前服务尚未启用协调状态复制"})
	return false
}

func (s *Server) coordinationResult(w http.ResponseWriter, view consoleapi.CoordinationView, err error) {
	if err == nil {
		_ = json.NewEncoder(w).Encode(view)
		return
	}
	status := http.StatusServiceUnavailable
	switch {
	case errors.Is(err, coordination.ErrConflict), errors.Is(err, coordination.ErrStaleEpoch), errors.Is(err, coordination.ErrCommandConflict):
		status = http.StatusConflict
	case errors.Is(err, coordination.ErrInvalid):
		status = http.StatusBadRequest
	case errors.Is(err, coordination.ErrNotCoordinator):
		status = http.StatusForbidden
	}
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
}
