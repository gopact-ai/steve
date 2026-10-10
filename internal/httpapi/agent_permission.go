package httpapi

import (
	"net/http"
)

func (s *Server) agentPermissionRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /console/agents/permission", s.guard(s.consoleAgentPermission))
}

func (s *Server) consoleAgentPermission(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Type", "application/json")
	if s.admin == nil {
		w.WriteHeader(http.StatusNotImplemented)
		writeJSON(w, map[string]string{"error": "agent permission discovery is not enabled"})
		return
	}
	fact, err := s.admin.AgentPermission(r.Context(), r.URL.Query().Get("node"), r.URL.Query().Get("harness"))
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	writeJSON(w, fact)
}
