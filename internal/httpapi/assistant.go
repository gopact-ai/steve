package httpapi

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"

	"github.com/gopact-ai/steve/internal/clientcap"
	"github.com/gopact-ai/steve/internal/durable"
	"github.com/gopact-ai/steve/internal/events"
	"github.com/gopact-ai/steve/internal/identity"
	"github.com/gopact-ai/steve/internal/ingress"
	"github.com/gopact-ai/steve/internal/ledger"
	"github.com/gopact-ai/steve/internal/serviceapi"
	"github.com/gopact-ai/steve/internal/work"
)

func (s *Server) SetAssistant(ports *serviceapi.Services) { s.assistant = ports }

func (s *Server) assistantRoutes(mux *http.ServeMux) {
	s.connectionLifecycleRoutes(mux)
	s.stateLifecycleRoutes(mux)
	for _, path := range []string{"connections", "subscriptions", "inbox", "event-deliveries", "client-capabilities", "client-invocations", "accounts", "grant-migrations", "work-interactions"} {
		mux.HandleFunc("GET /console/"+path, s.guard(s.assistantList))
	}
	mux.HandleFunc("POST /console/connections", s.guard(s.assistantEnroll))
	mux.HandleFunc("PUT /console/accounts", s.guard(s.assistantAccount))
	mux.HandleFunc("DELETE /console/connections/{id}", s.guard(s.assistantRevoke))
	mux.HandleFunc("PUT /console/subscriptions/{id}", s.guard(s.assistantSubscription))
	mux.HandleFunc("GET /console/business-events/{id}", s.guard(s.assistantEvent))
	mux.HandleFunc("POST /console/business-events/{id}/reprocess", s.guard(s.assistantReprocess))
	mux.HandleFunc("POST /console/inbox/{id}/resolve", s.guard(s.assistantResolve))
	mux.HandleFunc("POST /console/event-deliveries/{id}/retry", s.guard(s.assistantRetry))
	mux.HandleFunc("POST /console/client-invocations/{id}/approval", s.guard(s.assistantApproval))
	mux.HandleFunc("POST /services/events", s.connectionGuard(s.providerPublish))
	mux.HandleFunc("POST /services/events/claim", s.connectionGuard(s.providerClaim))
	mux.HandleFunc("POST /services/events/deliveries/{id}/ack", s.connectionGuard(s.providerAck))
	mux.HandleFunc("POST /services/work", s.connectionGuard(s.providerWork))
	mux.HandleFunc("GET /services/work/{id}", s.connectionGuard(s.providerWorkStatus))
	mux.HandleFunc("POST /services/client/register", s.connectionGuard(s.clientRegister))
	mux.HandleFunc("POST /services/client/heartbeat", s.connectionGuard(s.clientHeartbeat))
	mux.HandleFunc("POST /services/client/claim", s.connectionGuard(s.clientClaim))
	mux.HandleFunc("POST /services/client/invocations/{id}/complete", s.connectionGuard(s.clientComplete))
	mux.HandleFunc("POST /services/client/invoke", s.connectionGuard(s.clientInvoke))
	mux.HandleFunc("GET /services/client/invocations/{id}", s.connectionGuard(s.clientStatus))
	mux.HandleFunc("GET /services/client/capabilities", s.connectionGuard(s.clientAvailable))
}

func assistantResponse(w http.ResponseWriter, value any, err error) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	if err != nil {
		status := http.StatusBadRequest
		if errors.Is(err, identity.ErrDenied) {
			status = http.StatusForbidden
		}
		if errors.Is(err, ledger.ErrConflict) || errors.Is(err, ledger.ErrStale) || errors.Is(err, ingress.ErrConflict) {
			status = http.StatusConflict
		}
		if errors.Is(err, durable.ErrCapacity) {
			status = http.StatusTooManyRequests
			w.Header().Set("Retry-After", "5")
		}
		w.WriteHeader(status)
		writeJSON(w, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, value)
}
func readAssistantBody(w http.ResponseWriter, r *http.Request, value any) error {
	raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 512<<10))
	if err != nil {
		return err
	}
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 || raw[0] != '{' {
		return errors.New("request must be a JSON object")
	}
	d := json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	if err := d.Decode(value); err != nil {
		return err
	}
	if err := d.Decode(new(any)); err != io.EOF {
		return errors.New("expected one JSON object")
	}
	return nil
}
func (s *Server) assistantEnabled(w http.ResponseWriter) bool {
	if s.assistant == nil {
		http.Error(w, "assistant services are unavailable", http.StatusNotImplemented)
		return false
	}
	return true
}

// A connection token grants ONLY these scoped APIs. It never passes the owner
// console guard, and the owner token is never handed to a provider/client.
func (s *Server) connectionGuard(next func(http.ResponseWriter, *http.Request, identity.Principal)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !s.assistantEnabled(w) {
			return
		}
		token, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		if !ok || token == "" {
			http.Error(w, "connection bearer token required", http.StatusUnauthorized)
			return
		}
		p, err := s.assistant.Identity.Authenticate(r.Context(), token)
		if err != nil {
			http.Error(w, "connection is unauthorized or revoked", http.StatusUnauthorized)
			return
		}
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			s.mutationMu.RLock()
			defer s.mutationMu.RUnlock()
			if s.maintenance {
				http.Error(w, "Hub is preparing a service restart", http.StatusServiceUnavailable)
				return
			}
		}
		next(w, r, p)
	}
}

func (s *Server) assistantList(w http.ResponseWriter, r *http.Request) {
	if !s.assistantEnabled(w) {
		return
	}
	var value any
	var err error
	after := r.URL.Query().Get("cursor")
	switch strings.TrimPrefix(r.URL.Path, "/console/") {
	case "accounts":
		if s.assistant.Accounts == nil {
			http.Error(w, "account service unavailable", http.StatusNotImplemented)
			return
		}
		value, err = s.assistant.Accounts.Bindings(r.Context(), after)
	case "grant-migrations":
		if s.assistant.Policy == nil {
			http.Error(w, "policy service unavailable", http.StatusNotImplemented)
			return
		}
		value, err = s.assistant.Policy.GrantMigrations(r.Context(), after)
	case "work-interactions":
		if s.assistant.Interactions == nil {
			http.Error(w, "interaction service unavailable", http.StatusNotImplemented)
			return
		}
		value, err = s.assistant.Interactions.Records(r.Context(), after)
	case "connections":
		value, err = s.assistant.Identity.Connections(r.Context(), after)
	case "subscriptions":
		value, err = s.assistant.Events.Subscriptions(r.Context(), after)
	case "inbox":
		value, err = s.assistant.Events.InboxEntries(r.Context(), after)
	case "event-deliveries":
		value, err = s.assistant.Events.Deliveries(r.Context(), after)
	case "client-capabilities":
		value, err = s.assistant.Clients.Registrations(r.Context(), after)
	case "client-invocations":
		if r.URL.Query().Get("attention") == "1" {
			value, err = s.assistant.Clients.Attention(r.Context(), after)
		} else {
			value, err = s.assistant.Clients.Invocations(r.Context(), after)
		}
	}
	assistantResponse(w, value, err)
}
func (s *Server) assistantEnroll(w http.ResponseWriter, r *http.Request) {
	if !s.assistantEnabled(w) {
		return
	}
	var input identity.Grant
	if err := readAssistantBody(w, r, &input); err != nil {
		assistantResponse(w, nil, err)
		return
	}
	out, err := s.assistant.Identity.Enroll(r.Context(), input)
	assistantResponse(w, out, err)
}
func (s *Server) assistantRevoke(w http.ResponseWriter, r *http.Request) {
	if !s.assistantEnabled(w) {
		return
	}
	assistantResponse(w, map[string]bool{"ok": true}, s.assistant.Identity.Revoke(r.Context(), r.PathValue("id")))
}
func (s *Server) assistantSubscription(w http.ResponseWriter, r *http.Request) {
	if !s.assistantEnabled(w) {
		return
	}
	var input events.Subscription
	if err := readAssistantBody(w, r, &input); err != nil {
		assistantResponse(w, nil, err)
		return
	}
	if input.ID != "" && input.ID != r.PathValue("id") {
		assistantResponse(w, nil, errors.New("subscription identity differs from its URL"))
		return
	}
	input.ID = r.PathValue("id")
	out, err := s.assistant.Events.SaveSubscription(r.Context(), input)
	assistantResponse(w, out, err)
}
func (s *Server) assistantEvent(w http.ResponseWriter, r *http.Request) {
	if !s.assistantEnabled(w) {
		return
	}
	out, err := s.assistant.Events.Event(r.Context(), r.PathValue("id"))
	assistantResponse(w, out, err)
}
func (s *Server) assistantResolve(w http.ResponseWriter, r *http.Request) {
	if !s.assistantEnabled(w) {
		return
	}
	assistantResponse(w, map[string]bool{"ok": true}, s.assistant.Events.ResolveInbox(r.Context(), r.PathValue("id")))
}
func (s *Server) assistantReprocess(w http.ResponseWriter, r *http.Request) {
	if !s.assistantEnabled(w) {
		return
	}
	assistantResponse(w, map[string]bool{"ok": true}, s.assistant.Events.Reprocess(r.Context(), r.PathValue("id")))
}
func (s *Server) assistantRetry(w http.ResponseWriter, r *http.Request) {
	if !s.assistantEnabled(w) {
		return
	}
	assistantResponse(w, map[string]bool{"ok": true}, s.assistant.Events.Retry(r.Context(), r.PathValue("id")))
}
func (s *Server) assistantApproval(w http.ResponseWriter, r *http.Request) {
	if !s.assistantEnabled(w) {
		return
	}
	var input struct {
		Allow bool `json:"allow"`
	}
	if err := readAssistantBody(w, r, &input); err != nil {
		assistantResponse(w, nil, err)
		return
	}
	assistantResponse(w, map[string]bool{"ok": true}, s.assistant.Clients.Approve(r.Context(), r.PathValue("id"), input.Allow))
}
func (s *Server) providerPublish(w http.ResponseWriter, r *http.Request, p identity.Principal) {
	var input events.Publish
	if err := readAssistantBody(w, r, &input); err != nil {
		assistantResponse(w, nil, err)
		return
	}
	out, err := s.assistant.Events.Publish(r.Context(), p, input)
	assistantResponse(w, out, err)
}
func (s *Server) providerClaim(w http.ResponseWriter, r *http.Request, p identity.Principal) {
	out, found, err := s.assistant.Events.Poll(r.Context(), p)
	assistantResponse(w, map[string]any{"found": found, "delivery": out.Delivery, "event": out.Event}, err)
}
func (s *Server) providerAck(w http.ResponseWriter, r *http.Request, p identity.Principal) {
	var input events.Acknowledgement
	if err := readAssistantBody(w, r, &input); err != nil {
		assistantResponse(w, nil, err)
		return
	}
	assistantResponse(w, map[string]bool{"ok": true}, s.assistant.Events.Ack(r.Context(), p, r.PathValue("id"), input))
}
func (s *Server) providerWork(w http.ResponseWriter, r *http.Request, p identity.Principal) {
	var input work.Request
	if err := readAssistantBody(w, r, &input); err != nil {
		assistantResponse(w, nil, err)
		return
	}
	out, err := s.assistant.Work.Submit(r.Context(), p, input)
	assistantResponse(w, out, err)
}
func (s *Server) providerWorkStatus(w http.ResponseWriter, r *http.Request, p identity.Principal) {
	record, result, err := s.assistant.Work.Status(r.Context(), p, r.PathValue("id"))
	assistantResponse(w, map[string]any{"work": record, "result": result}, err)
}
func (s *Server) clientRegister(w http.ResponseWriter, r *http.Request, p identity.Principal) {
	var input clientcap.Register
	if err := readAssistantBody(w, r, &input); err != nil {
		assistantResponse(w, nil, err)
		return
	}
	out, err := s.assistant.Clients.Register(r.Context(), p, input)
	assistantResponse(w, out, err)
}
func (s *Server) clientHeartbeat(w http.ResponseWriter, r *http.Request, p identity.Principal) {
	var input struct {
		Generation uint64 `json:"generation"`
	}
	if err := readAssistantBody(w, r, &input); err != nil {
		assistantResponse(w, nil, err)
		return
	}
	assistantResponse(w, map[string]bool{"ok": true}, s.assistant.Clients.Heartbeat(r.Context(), p, input.Generation))
}
func (s *Server) clientClaim(w http.ResponseWriter, r *http.Request, p identity.Principal) {
	var input struct {
		Generation uint64 `json:"generation"`
	}
	if err := readAssistantBody(w, r, &input); err != nil {
		assistantResponse(w, nil, err)
		return
	}
	out, found, err := s.assistant.Clients.Poll(r.Context(), p, input.Generation)
	assistantResponse(w, map[string]any{"found": found, "invocation": out}, err)
}
func (s *Server) clientComplete(w http.ResponseWriter, r *http.Request, p identity.Principal) {
	var input clientcap.Completion
	if err := readAssistantBody(w, r, &input); err != nil {
		assistantResponse(w, nil, err)
		return
	}
	assistantResponse(w, map[string]bool{"ok": true}, s.assistant.Clients.Complete(r.Context(), p, r.PathValue("id"), input))
}
func (s *Server) clientInvoke(w http.ResponseWriter, r *http.Request, p identity.Principal) {
	var input clientcap.Request
	if err := readAssistantBody(w, r, &input); err != nil {
		assistantResponse(w, nil, err)
		return
	}
	out, err := s.assistant.Clients.Invoke(r.Context(), p, input)
	assistantResponse(w, out, err)
}
func (s *Server) clientStatus(w http.ResponseWriter, r *http.Request, p identity.Principal) {
	out, err := s.assistant.Clients.Status(r.Context(), p, r.PathValue("id"))
	assistantResponse(w, out, err)
}
func (s *Server) clientAvailable(w http.ResponseWriter, r *http.Request, p identity.Principal) {
	out, err := s.assistant.Clients.Available(r.Context(), p, r.URL.Query().Get("conversation"))
	assistantResponse(w, out, err)
}
