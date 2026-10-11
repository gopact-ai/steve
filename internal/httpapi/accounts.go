package httpapi

import (
	"github.com/gopact-ai/steve/internal/identity"
	"net/http"
)

func (s *Server) assistantAccount(w http.ResponseWriter, r *http.Request) {
	if !s.assistantEnabled(w) {
		return
	}
	if s.assistant.Accounts == nil {
		http.Error(w, "account service unavailable", http.StatusNotImplemented)
		return
	}
	var input struct {
		Account   identity.Account `json:"account"`
		Principal string           `json:"principal"`
		Revision  uint64           `json:"revision"`
		Enabled   bool             `json:"enabled"`
	}
	if err := readAssistantBody(w, r, &input); err != nil {
		assistantResponse(w, nil, err)
		return
	}
	value, err := s.assistant.Accounts.Bind(r.Context(), input.Account, input.Principal, input.Revision, input.Enabled)
	assistantResponse(w, value, err)
}
