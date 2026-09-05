package readmodel

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"
)

// Exchange names one submission throughout its queue, sent line and answer.
// Reply IDs still name individual transcript lines, including quoted lines.
type Exchange struct {
	ID           string `json:"id"`
	Conversation string `json:"conversation"`
	Input        string `json:"input"`
	// Prompt is what the agent is given when it differs from Input: a
	// continuation after a restart shows the notice and says "go on".
	Prompt     string     `json:"prompt,omitempty"`
	Quotes     []QuoteRef `json:"quotes,omitempty"`
	State      string     `json:"state"` // queued | running | done | failed
	EnqueuedAt time.Time  `json:"enqueued_at"`
	StartedAt  time.Time  `json:"started_at,omitzero"`
	ReplyID    string     `json:"reply_id,omitempty"`
}

var (
	ErrExchangeNotFound  = errors.New("exchange not found")
	ErrExchangeNotQueued = errors.New("exchange is no longer queued")
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
		case errors.Is(err, ErrExchangeNotFound):
			status = http.StatusNotFound
		case errors.Is(err, ErrExchangeNotQueued):
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
	var req struct {
		Conversation string     `json:"conversation"`
		Input        string     `json:"input"`
		Quotes       []QuoteRef `json:"quotes,omitempty"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
		queueResponse(w, nil, err)
		return
	}
	if strings.TrimSpace(req.Input) == "" {
		queueResponse(w, nil, errors.New("input is required"))
		return
	}
	if req.Conversation == "" {
		req.Conversation = "console:main"
	}
	exchange, err := s.console.Enqueue(r.Context(), req.Conversation, req.Input, req.Quotes)
	queueResponse(w, exchange, err)
}

func (s *Server) consoleQueue(w http.ResponseWriter, r *http.Request) {
	if !s.queueEnabled(w) {
		return
	}
	conversation := r.URL.Query().Get("conversation")
	if conversation == "" {
		conversation = "console:main"
	}
	list := s.console.Queue(conversation)
	if list == nil {
		list = []Exchange{}
	}
	queueResponse(w, map[string]any{"queue": list}, nil)
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
