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
		case errors.Is(err, consoleapi.ErrExchangeNotFound), errors.Is(err, consoleapi.ErrRewindTargetGone):
			status = http.StatusNotFound
		case errors.Is(err, consoleapi.ErrExchangeNotQueued), errors.Is(err, consoleapi.ErrCommandConflict):
			status = http.StatusConflict
		}
		w.WriteHeader(status)
		writeJSON(w, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, value)
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
	if !s.consoleSubmissionIdentity(w, r, req) {
		return
	}
	exchange, err := s.console.Submit(r.Context(), req)
	// The history a rewound submission carries is for the agent; the
	// acknowledgement the page reads has no use for it.
	exchange.History = ""
	queueResponse(w, exchange, err)
}

func (s *Server) consoleQueue(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if !s.queueEnabled(w) {
		return
	}
	list := []consoleapi.Exchange{}
	// Capabilities describe the Console service, not a conversation. A probe
	// must not read a queue or depend on the channel identity directory.
	if r.URL.Query().Get("capabilities") != "1" {
		conversation := r.URL.Query().Get("conversation")
		if conversation == "" {
			conversation = "console:main"
		}
		if !s.consoleIdentity(w, r, conversation) {
			return
		}
		if queue := s.console.Queue(conversation); queue != nil {
			list = queue
		}
	}
	materialRefs, interactiveRequests := s.console.SubmissionCapabilities()
	// Clients must confirm support before submitting or retrying a command ID;
	// older hubs accepted the field but did not preserve its identity.
	queueResponse(w, map[string]any{"queue": list, "submission_keys": true, "material_refs": materialRefs, "interactive_requests": interactiveRequests}, nil)
}

func (s *Server) consoleDeleteQueued(w http.ResponseWriter, r *http.Request) {
	if !s.queueEnabled(w) {
		return
	}
	if !s.consoleExchangeIdentity(w, r) {
		return
	}
	queueResponse(w, map[string]bool{"ok": true}, s.console.DeleteQueued(r.PathValue("id")))
}

func (s *Server) consoleEditQueued(w http.ResponseWriter, r *http.Request) {
	if !s.queueEnabled(w) {
		return
	}
	if !s.consoleExchangeIdentity(w, r) {
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
	if !s.consoleExchangeIdentity(w, r) {
		return
	}
	exchange, err := s.console.Steer(r.Context(), r.PathValue("id"))
	queueResponse(w, exchange, err)
}
