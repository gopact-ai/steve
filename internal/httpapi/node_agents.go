package httpapi

import (
	"net/http"

	"github.com/gopact-ai/steve/internal/agenttools"
)

func (s *Server) consoleNodeAgents(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	if s.admin == nil {
		w.WriteHeader(http.StatusNotImplemented)
		writeJSON(w, map[string]string{"error": "当前服务不支持节点工具登记"})
		return
	}
	if r.Method == http.MethodGet {
		result, err := s.admin.NodeAgents(r.Context(), r.PathValue("name"))
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
	result, err := s.admin.EnrollNodeAgent(r.Context(), r.PathValue("name"), request)
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
