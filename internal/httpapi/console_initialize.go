package httpapi

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"

	"github.com/gopact-ai/steve/internal/consoleapi"
)

func (s *Server) consoleInitializeConversation(w http.ResponseWriter, r *http.Request) {
	if !s.consoleIdentity(w, r, r.PathValue("id")) {
		return
	}
	initializer, ok := s.console.(consoleapi.ConversationInitializer)
	if !ok {
		http.Error(w, "conversation initialization is not supported", http.StatusNotImplemented)
		return
	}
	var req struct {
		Project string `json:"project"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<14)).Decode(&req); err != nil {
		http.Error(w, "bad request: "+err.Error(), http.StatusBadRequest)
		return
	}
	if err := initializer.InitializeConversation(r.Context(), r.PathValue("id"), req.Project); err != nil {
		status := http.StatusBadRequest
		if errors.Is(err, consoleapi.ErrConsoleClosing) {
			status = http.StatusServiceUnavailable
		}
		http.Error(w, err.Error(), status)
		return
	}
	writeJSON(w, map[string]any{"ok": true})
}
