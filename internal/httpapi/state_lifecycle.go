package httpapi

import (
	"github.com/gopact-ai/steve/internal/clientcap"
	"github.com/gopact-ai/steve/internal/events"
	"net/http"
)

func (s *Server) stateLifecycleRoutes(mux *http.ServeMux) {
	mux.HandleFunc("POST /console/event-deliveries/{id}/reconcile", s.guard(s.eventReconcile))
	mux.HandleFunc("POST /console/client-invocations/{id}/cancel", s.guard(s.clientCancel))
	mux.HandleFunc("POST /console/client-invocations/{id}/reconcile", s.guard(s.clientReconcile))
	mux.HandleFunc("GET /console/service-backlog", s.guard(s.serviceBacklog))
	mux.HandleFunc("POST /console/service-history/compact", s.guard(s.serviceCompact))
}
func (s *Server) eventReconcile(w http.ResponseWriter, r *http.Request) {
	if !s.assistantEnabled(w) {
		return
	}
	var input events.Reconciliation
	if err := readAssistantBody(w, r, &input); err != nil {
		assistantResponse(w, nil, err)
		return
	}
	assistantResponse(w, map[string]bool{"ok": true}, s.assistant.Events.Reconcile(r.Context(), r.PathValue("id"), input))
}
func (s *Server) clientCancel(w http.ResponseWriter, r *http.Request) {
	if !s.assistantEnabled(w) {
		return
	}
	var input clientcap.Decision
	if err := readAssistantBody(w, r, &input); err != nil {
		assistantResponse(w, nil, err)
		return
	}
	assistantResponse(w, map[string]bool{"ok": true}, s.assistant.Clients.Cancel(r.Context(), r.PathValue("id"), input))
}
func (s *Server) clientReconcile(w http.ResponseWriter, r *http.Request) {
	if !s.assistantEnabled(w) {
		return
	}
	var input clientcap.Reconciliation
	if err := readAssistantBody(w, r, &input); err != nil {
		assistantResponse(w, nil, err)
		return
	}
	assistantResponse(w, map[string]bool{"ok": true}, s.assistant.Clients.Reconcile(r.Context(), r.PathValue("id"), input))
}
func (s *Server) maintenanceEnabled(w http.ResponseWriter) bool {
	if !s.assistantEnabled(w) {
		return false
	}
	if s.assistant.Maintenance == nil {
		http.Error(w, "service maintenance is unavailable", http.StatusNotImplemented)
		return false
	}
	return true
}
func (s *Server) serviceBacklog(w http.ResponseWriter, r *http.Request) {
	if !s.maintenanceEnabled(w) {
		return
	}
	out, err := s.assistant.Maintenance.Stats(r.Context())
	assistantResponse(w, out, err)
}
func (s *Server) serviceCompact(w http.ResponseWriter, r *http.Request) {
	if !s.maintenanceEnabled(w) {
		return
	}
	out, err := s.assistant.Maintenance.Compact(r.Context())
	assistantResponse(w, out, err)
}
