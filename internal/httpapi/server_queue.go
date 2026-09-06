package httpapi

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"github.com/gopact-ai/steve/internal/consoleapi"
)

func (s *Server) queueEnabled(w http.ResponseWriter) bool {
	if s.console == nil {
		http.Error(w, "the console is not enabled on this gateway", http.StatusNotImplemented)
		return false
	}
	return true
}

func queueResponse(w http.ResponseWriter, value any, err error) {
	w.Header().Set("Content-Type", "application/json")
	if err != nil {
		status := http.StatusBadRequest
		switch {
		case errors.Is(err, consoleapi.ErrConsoleClosing):
			status = http.StatusServiceUnavailable
		case errors.Is(err, consoleapi.ErrExchangeNotFound):
			status = http.StatusNotFound
		case errors.Is(err, consoleapi.ErrExchangeNotQueued), errors.Is(err, consoleapi.ErrCommandConflict):
			status = http.StatusConflict
		}
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}
	_ = json.NewEncoder(w).Encode(value)
}

func (s *Server) consoleEnqueue(w http.ResponseWriter, r *http.Request) {
	if !s.queueEnabled(w) {
		return
	}
	var req consoleapi.Submission
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
		queueResponse(w, nil, err)
		return
	}
	if strings.TrimSpace(req.Input) == "" && len(req.Refs) == 0 {
		queueResponse(w, nil, errors.New("input is required"))
		return
	}
	if req.Conversation == "" {
		req.Conversation = "console:main"
	}
	var exchange consoleapi.Exchange
	var err error
	if extended, ok := s.console.(consoleapi.Submissions); ok {
		exchange, err = extended.Submit(r.Context(), req)
	} else if len(req.Refs) > 0 {
		http.Error(w, "material submission is not supported", http.StatusNotImplemented)
		return
	} else {
		exchange, err = s.console.EnqueueCommand(r.Context(), req.Conversation, req.Input, req.CommandID, req.Quotes)
	}
	queueResponse(w, exchange, err)
}

func (s *Server) consoleQueue(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if !s.queueEnabled(w) {
		return
	}
	conversation := r.URL.Query().Get("conversation")
	if conversation == "" {
		conversation = "console:main"
	}
	list := s.console.Queue(conversation)
	if list == nil {
		list = []consoleapi.Exchange{}
	}
	// Clients must confirm support before submitting or retrying a command ID;
	// older hubs accepted the field but did not preserve its identity.
	response := map[string]any{"queue": list, "submission_keys": true}
	if capabilities, ok := s.console.(interface{ SubmissionCapabilities() (bool, bool) }); ok {
		materialRefs, interactiveRequests := capabilities.SubmissionCapabilities()
		response["material_refs"], response["interactive_requests"] = materialRefs, interactiveRequests
	}
	queueResponse(w, response, nil)
}

func (s *Server) consoleDeleteQueued(w http.ResponseWriter, r *http.Request) {
	if !s.queueEnabled(w) {
		return
	}
	queueResponse(w, map[string]bool{"ok": true}, s.console.DeleteQueued(r.PathValue("id")))
}

func (s *Server) consoleEditQueued(w http.ResponseWriter, r *http.Request) {
	if !s.queueEnabled(w) {
		return
	}
	var req struct {
		Input string `json:"input"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
		queueResponse(w, nil, err)
		return
	}
	if strings.TrimSpace(req.Input) == "" {
		queueResponse(w, nil, errors.New("input is required"))
		return
	}
	exchange, err := s.console.EditQueued(r.PathValue("id"), req.Input)
	queueResponse(w, exchange, err)
}

func (s *Server) consoleSteer(w http.ResponseWriter, r *http.Request) {
	if !s.queueEnabled(w) {
		return
	}
	exchange, err := s.console.Steer(r.Context(), r.PathValue("id"))
	queueResponse(w, exchange, err)
}
