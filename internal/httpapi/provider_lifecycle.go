package httpapi

import (
	"net/http"

	"github.com/gopact-ai/steve/internal/identity"
	"github.com/gopact-ai/steve/internal/provider"
)

func (s *Server) connectionLifecycleRoutes(mux *http.ServeMux) {
	mux.HandleFunc("PUT /console/connections/{id}/scope", s.guard(s.connectionScope))
	mux.HandleFunc("POST /console/connections/{id}/credential", s.guard(s.connectionCredential))
	mux.HandleFunc("GET /console/connection-audit", s.guard(s.connectionAudit))
	mux.HandleFunc("GET /console/event-providers", s.guard(s.providerDeclarations))
	mux.HandleFunc("POST /services/provider/register", s.connectionGuard(s.providerRegister))
	mux.HandleFunc("POST /services/provider/heartbeat", s.connectionGuard(s.providerHeartbeat))
	mux.HandleFunc("GET /services/provider", s.connectionGuard(s.providerDeclaration))
}

func (s *Server) connectionScope(w http.ResponseWriter, r *http.Request) {
	if !s.assistantEnabled(w) {
		return
	}
	var input identity.ScopeUpdate
	if err := readAssistantBody(w, r, &input); err != nil {
		assistantResponse(w, nil, err)
		return
	}
	out, err := s.assistant.Identity.UpdateScope(r.Context(), r.PathValue("id"), input)
	assistantResponse(w, out, err)
}

func (s *Server) connectionCredential(w http.ResponseWriter, r *http.Request) {
	if !s.assistantEnabled(w) {
		return
	}
	var input identity.RotateRequest
	if err := readAssistantBody(w, r, &input); err != nil {
		assistantResponse(w, nil, err)
		return
	}
	out, err := s.assistant.Identity.RotateCredential(r.Context(), r.PathValue("id"), input)
	assistantResponse(w, out, err)
}

func (s *Server) connectionAudit(w http.ResponseWriter, r *http.Request) {
	if !s.assistantEnabled(w) {
		return
	}
	out, err := s.assistant.Identity.Audit(r.Context(), r.URL.Query().Get("cursor"))
	assistantResponse(w, out, err)
}

func (s *Server) providersEnabled(w http.ResponseWriter) bool {
	if !s.assistantEnabled(w) {
		return false
	}
	if s.assistant.Providers == nil {
		http.Error(w, "provider discovery is unavailable", http.StatusNotImplemented)
		return false
	}
	return true
}

func (s *Server) providerDeclarations(w http.ResponseWriter, r *http.Request) {
	if !s.providersEnabled(w) {
		return
	}
	out, err := s.assistant.Providers.Registrations(r.Context(), r.URL.Query().Get("cursor"))
	assistantResponse(w, out, err)
}

func (s *Server) providerRegister(w http.ResponseWriter, r *http.Request, p identity.Principal) {
	if !s.providersEnabled(w) {
		return
	}
	var input provider.Register
	if err := readAssistantBody(w, r, &input); err != nil {
		assistantResponse(w, nil, err)
		return
	}
	out, err := s.assistant.Providers.Register(r.Context(), p, input)
	assistantResponse(w, out, err)
}

func (s *Server) providerHeartbeat(w http.ResponseWriter, r *http.Request, p identity.Principal) {
	if !s.providersEnabled(w) {
		return
	}
	var input struct {
		Generation uint64 `json:"generation"`
	}
	if err := readAssistantBody(w, r, &input); err != nil {
		assistantResponse(w, nil, err)
		return
	}
	assistantResponse(w, map[string]bool{"ok": true}, s.assistant.Providers.Heartbeat(r.Context(), p, input.Generation))
}

func (s *Server) providerDeclaration(w http.ResponseWriter, r *http.Request, p identity.Principal) {
	if !s.providersEnabled(w) {
		return
	}
	out, err := s.assistant.Providers.SelfRegistration(r.Context(), p)
	assistantResponse(w, out, err)
}
