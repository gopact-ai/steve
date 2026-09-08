package httpapi

import (
	"context"
	"net/http"

	"github.com/gopact-ai/steve/internal/agenttools"
)

type nodeAgentService interface {
	NodeAgents(context.Context, string) (agenttools.Discovery, error)
	EnrollNodeAgent(context.Context, string, agenttools.EnrollRequest) (agenttools.Enrollment, error)
}

func (s *Server) consoleNodeAgents(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	service, ok := s.admin.(nodeAgentService)
	if !ok {
		w.WriteHeader(http.StatusNotImplemented)
		writeJSON(w, map[string]string{"error": "当前服务不支持节点工具登记"})
		return
	}
	if r.Method == http.MethodGet {
		result, err := service.NodeAgents(r.Context(), r.PathValue("name"))
		if err != nil {
			writeDesktopError(w, err, http.StatusServiceUnavailable)
			return
		}
		writeJSON(w, result)
		return
	}
	var request agenttools.EnrollRequest
	if !decodeSSH(w, r, &request) {
		return
	}
	result, err := service.EnrollNodeAgent(r.Context(), r.PathValue("name"), request)
	if err != nil {
		if result.Registered {
			writeJSON(w, result)
			return
		}
		writeDesktopError(w, err, http.StatusBadRequest)
		return
	}
	writeJSON(w, result)
}
