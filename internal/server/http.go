package server

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gopact-ai/steve/internal/approvals"
	"github.com/gopact-ai/steve/internal/feed"
	"github.com/gopact-ai/steve/internal/work"
)

// Heartbeat is how often an idle stream proves it is alive.
var Heartbeat = 15 * time.Second

// Handler is the /v2 API. Every route but /healthz needs the bearer token.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) { writeJSON(w, http.StatusOK, map[string]bool{"ok": true}) })
	mux.HandleFunc("GET /v2/snapshot", s.snapshot)
	mux.HandleFunc("GET /v2/stream", s.stream)
	mux.HandleFunc("GET /v2/tasks", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{"tasks": nonNil(s.Work.List())})
	})
	mux.HandleFunc("GET /v2/tasks/{id}", s.getTask)
	mux.HandleFunc("POST /v2/tasks", s.submitTask)
	mux.HandleFunc("POST /v2/tasks/{id}/stop", s.stopTask)
	mux.HandleFunc("GET /v2/approvals", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{"approvals": nonNil(s.Approvals.Open())})
	})
	mux.HandleFunc("POST /v2/approvals/{id}", s.resolveApproval)
	return s.authenticate(mux)
}

func (s *Server) authenticate(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/healthz" {
			next.ServeHTTP(w, r)
			return
		}
		got, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		if !ok {
			// EventSource cannot set headers; the stream also takes ?token=.
			got = r.URL.Query().Get("token")
		}
		if s.Token == "" || subtle.ConstantTimeCompare([]byte(strings.TrimSpace(got)), []byte(s.Token)) != 1 {
			writeError(w, http.StatusUnauthorized, "unauthorized", "a valid bearer token is required")
			return
		}
		next.ServeHTTP(w, r)
	})
}

// Snapshot is everything a client draws, with the cursor to follow from.
type Snapshot struct {
	Cursor    string            `json:"cursor"`
	Tasks     []work.Card       `json:"tasks"`
	Approvals []approvals.Card  `json:"approvals"`
	Server    map[string]string `json:"server"`
}

func (s *Server) snapshot(w http.ResponseWriter, r *http.Request) {
	// The cursor is taken first: anything that changes while the state is
	// read is replayed after it, so nothing falls between the two.
	cursor := s.Feed.Head()
	writeJSON(w, http.StatusOK, Snapshot{Cursor: encodeCursor(cursor), Tasks: nonNil(s.Work.List()), Approvals: nonNil(s.Approvals.Open()), Server: map[string]string{"node": s.nodeName}})
}

func (s *Server) stream(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeError(w, http.StatusInternalServerError, "streaming", "streaming is not supported")
		return
	}
	raw := r.Header.Get("Last-Event-ID")
	if raw == "" {
		raw = r.URL.Query().Get("after")
	}
	cursor := s.Feed.Head()
	if raw != "" {
		var err error
		if cursor, err = decodeCursor(raw); err != nil {
			writeError(w, http.StatusBadRequest, "cursor", err.Error())
			return
		}
	}
	wake, stop := s.Feed.Subscribe()
	defer stop()
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	fmt.Fprintf(w, "retry: 1500\n\n")
	flusher.Flush()
	beat := time.NewTicker(Heartbeat)
	defer beat.Stop()
	for {
		events, err := s.Feed.After(cursor)
		if errors.Is(err, feed.ErrCursorTooOld) {
			fmt.Fprintf(w, "event: cursor_too_old\ndata: {}\n\n")
			flusher.Flush()
			return
		}
		for _, e := range events {
			cursor.Seq = e.Seq
			fmt.Fprintf(w, "id: %s\nevent: %s\ndata: %s\n\n", encodeCursor(cursor), e.Type, e.Data)
		}
		if len(events) > 0 {
			flusher.Flush()
		}
		select {
		case <-r.Context().Done():
			return
		case <-s.ctx.Done():
			return
		case <-wake:
		case <-beat.C:
			fmt.Fprintf(w, "event: heartbeat\ndata: {\"cursor\":%q}\n\n", encodeCursor(cursor))
			flusher.Flush()
		}
	}
}

func (s *Server) getTask(w http.ResponseWriter, r *http.Request) {
	card, err := s.Work.Get(r.PathValue("id"))
	if err != nil {
		writeWorkError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, card)
}

type submitRequest struct {
	ClientNonce string `json:"client_nonce"`
	Goal        string `json:"goal"`
	Agent       string `json:"agent,omitempty"`
	Project     string `json:"project,omitempty"`
	Mode        string `json:"mode,omitempty"`
	Thread      string `json:"thread,omitempty"`
}

func (s *Server) submitTask(w http.ResponseWriter, r *http.Request) {
	var in submitRequest
	if !readJSON(w, r, &in) {
		return
	}
	if strings.TrimSpace(in.ClientNonce) == "" {
		writeWorkError(w, fmt.Errorf("%w: client_nonce is required", work.ErrInvalid))
		return
	}
	card, err := s.Work.Submit(r.Context(), work.Request{Key: "client:" + in.ClientNonce, Goal: in.Goal, Agent: in.Agent, Project: in.Project, Mode: in.Mode, Thread: in.Thread, By: "owner"})
	if err != nil {
		writeWorkError(w, err)
		return
	}
	// Accepted, not done: the card follows on the stream.
	writeJSON(w, http.StatusAccepted, card)
}

func (s *Server) stopTask(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	card, err := s.Work.Stop(ctx, r.PathValue("id"))
	if err != nil {
		writeWorkError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, card)
}

type decisionRequest struct {
	Decision string `json:"decision"` // approve | reject | answer
	Choice   string `json:"choice,omitempty"`
	Text     string `json:"text,omitempty"`
}

func (s *Server) resolveApproval(w http.ResponseWriter, r *http.Request) {
	var in decisionRequest
	if !readJSON(w, r, &in) {
		return
	}
	var d approvals.Decision
	switch in.Decision {
	case "approve":
		d = approvals.Decision{Approve: true, Choice: in.Choice}
	case "reject":
		d = approvals.Decision{}
	case "answer":
		d = approvals.Decision{Choice: in.Choice, Text: in.Text}
	default:
		writeError(w, http.StatusBadRequest, "invalid", "decision must be approve, reject or answer")
		return
	}
	card, err := s.Approvals.Resolve(r.PathValue("id"), d)
	switch {
	case errors.Is(err, approvals.ErrNotFound):
		writeError(w, http.StatusNotFound, "not_found", err.Error())
	case errors.Is(err, approvals.ErrClosed):
		// Answered already (a retry, another device) or expired: the card
		// says which.
		writeJSON(w, http.StatusConflict, card)
	case errors.Is(err, approvals.ErrInvalid):
		writeError(w, http.StatusBadRequest, "invalid", err.Error())
	case err != nil:
		writeError(w, http.StatusInternalServerError, "internal", err.Error())
	default:
		writeJSON(w, http.StatusOK, card)
	}
}

func writeWorkError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, work.ErrInvalid):
		writeError(w, http.StatusBadRequest, "invalid", err.Error())
	case errors.Is(err, work.ErrNotFound):
		writeError(w, http.StatusNotFound, "not_found", err.Error())
	default:
		writeError(w, http.StatusInternalServerError, "internal", err.Error())
	}
}

func readJSON(w http.ResponseWriter, r *http.Request, into any) bool {
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20))
	dec.DisallowUnknownFields()
	if err := dec.Decode(into); err != nil {
		writeError(w, http.StatusBadRequest, "invalid", "body: "+err.Error())
		return false
	}
	return true
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, code, message string) {
	writeJSON(w, status, map[string]any{"error": map[string]string{"code": code, "message": message}})
}

func nonNil[T any](items []T) []T {
	if items == nil {
		return []T{}
	}
	return items
}

func encodeCursor(c feed.Cursor) string { return c.Epoch + ":" + strconv.FormatUint(c.Seq, 10) }

func decodeCursor(raw string) (feed.Cursor, error) {
	epoch, seq, ok := strings.Cut(raw, ":")
	n, err := strconv.ParseUint(seq, 10, 64)
	if !ok || epoch == "" || err != nil {
		return feed.Cursor{}, errors.New("cursor must be epoch:seq")
	}
	return feed.Cursor{Epoch: epoch, Seq: n}, nil
}
