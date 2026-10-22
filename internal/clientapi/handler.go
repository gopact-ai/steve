package clientapi

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"

	"github.com/gopact-ai/steve/internal/consoleapi"
	"github.com/gopact-ai/steve/internal/i18n"
	"github.com/gopact-ai/steve/internal/readmodel"
	"github.com/gopact-ai/steve/internal/task"
)

// Model is the read side the protocol projects: the snapshot, the event
// stream and task history.
type Model interface {
	Snapshot(context.Context) readmodel.Snapshot
	SubscribeAfter(context.Context, ...string) (readmodel.Resume, <-chan readmodel.Event, func())
	EventID(readmodel.Event) string
	TaskHistory(context.Context, task.Query) (readmodel.TaskPage, error)
}

// Attempts reads what an attempt changed.
type Attempts interface {
	AttemptChanges(ctx context.Context, attempt string) (consoleapi.ChangeIndex, error)
	AttemptDiff(ctx context.Context, attempt, path string) (consoleapi.FileDiff, error)
}

// Backend supplies the services the protocol adapts. Each accessor is read
// per request, so a service wired after the server started is served, and
// one that is nil answers 501.
type Backend interface {
	Model() Model
	Console() consoleapi.Console
	Attempts() Attempts
	ChannelHistory() consoleapi.ChannelHistory
	ServerVersion() string
}

// Handler serves steve.client.v1 over HTTP.
type Handler struct {
	backend Backend
}

func New(backend Backend) *Handler { return &Handler{backend: backend} }

// Routes mounts every v1 method on mux behind guard, which authenticates
// the request and sets its locale exactly as it does for the console.
func (h *Handler) Routes(mux *http.ServeMux, guard func(http.HandlerFunc) http.HandlerFunc) {
	mux.HandleFunc("GET /v1/initialize", guard(h.initialize))
	mux.HandleFunc("GET /v1/threads", guard(h.listThreads))
	mux.HandleFunc("GET /v1/threads/{id...}", guard(h.getThread))
	mux.HandleFunc("POST /v1/threads", guard(h.createThread))
	mux.HandleFunc("POST /v1/turns", guard(h.startTurn))
	mux.HandleFunc("POST /v1/turns/{id}/steer", guard(h.steerTurn))
	mux.HandleFunc("POST /v1/turns/{id}/interrupt", guard(h.interruptTurn))
	mux.HandleFunc("POST /v1/questions/{id}/answer", guard(h.answerQuestion))
	mux.HandleFunc("GET /v1/attempts/{id}/diff", guard(h.attemptDiff))
	mux.HandleFunc("GET /v1/projects", guard(h.listProjects))
	mux.HandleFunc("GET /v1/agents", guard(h.listAgents))
	mux.HandleFunc("GET /v1/tasks", guard(h.listTasks))
	mux.HandleFunc("GET /v1/events", guard(h.events))
}

// Error codes a client can branch on.
const (
	CodeBadRequest      = "bad_request"
	CodeNotFound        = "not_found"
	CodeForbidden       = "forbidden"
	CodeCommandConflict = "command_conflict"
	CodeProjectConflict = "project_conflict"
	CodeConflict        = "conflict"
	CodeStaleCursor     = "stale_cursor"
	CodeUnavailable     = "unavailable"
	CodeNotImplemented  = "not_implemented"
	CodeInternal        = "internal"
)

// apiError is a refusal with its status and code.
type apiError struct {
	status int
	code   string
	err    error
}

func (e *apiError) Error() string { return e.err.Error() }
func (e *apiError) Unwrap() error { return e.err }

func refuse(status int, code string, err error) error {
	return &apiError{status: status, code: code, err: err}
}

func badRequest(message string) error {
	return refuse(http.StatusBadRequest, CodeBadRequest, errors.New(message))
}

var errNotWired = refuse(http.StatusNotImplemented, CodeNotImplemented, errors.New("this server does not serve that method"))

// classify gives a console or conversation error its status and code, the
// same status the console routes answer it with.
func classify(err error) *apiError {
	var known *apiError
	if errors.As(err, &known) {
		return known
	}
	status, code := http.StatusBadRequest, CodeBadRequest
	switch {
	case errors.Is(err, consoleapi.ErrConsoleClosing):
		status, code = http.StatusServiceUnavailable, CodeUnavailable
	case errors.Is(err, consoleapi.ErrCommandConflict):
		status, code = http.StatusConflict, CodeCommandConflict
	case errors.Is(err, consoleapi.ErrExchangeNotQueued), errors.Is(err, consoleapi.ErrQuestionConflict), errors.Is(err, consoleapi.ErrBusy):
		status, code = http.StatusConflict, CodeConflict
	case errors.Is(err, consoleapi.ErrExchangeNotFound), errors.Is(err, consoleapi.ErrRewindTargetGone), errors.Is(err, consoleapi.ErrQuestionNotFound), errors.Is(err, consoleapi.ErrChannelConversationNotFound), errors.Is(err, task.ErrTaskNotFound):
		status, code = http.StatusNotFound, CodeNotFound
	case errors.Is(err, consoleapi.ErrQuestionForbidden):
		status, code = http.StatusForbidden, CodeForbidden
	case errors.Is(err, consoleapi.ErrInvalidAnswer), errors.Is(err, task.ErrInvalidQuery), errors.Is(err, consoleapi.ErrChannelHistoryCursor):
		status, code = http.StatusBadRequest, CodeBadRequest
	case errors.Is(err, task.ErrStaleCursor):
		status, code = http.StatusConflict, CodeStaleCursor
	}
	return &apiError{status: status, code: code, err: err}
}

func writeError(w http.ResponseWriter, err error) {
	e := classify(err)
	if e.status >= http.StatusInternalServerError && e.code != CodeUnavailable && e.code != CodeNotImplemented {
		slog.Error("clientapi: request failed", "error", err)
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(e.status)
	writeBody(w, Error{Error: ErrorDetail{Code: e.code, Message: e.err.Error()}})
}

func writeJSON(w http.ResponseWriter, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	writeBody(w, value)
}

func writeBody(w io.Writer, value any) {
	if err := json.NewEncoder(w).Encode(value); err != nil {
		slog.Warn("clientapi: encode response", "error", err)
	}
}

// respond writes value, or the error that stopped it.
func respond(w http.ResponseWriter, value any, err error) {
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, value)
}

// decode reads exactly one JSON value of at most limit bytes, refusing
// fields the method does not define.
func decode(w http.ResponseWriter, r *http.Request, limit int64, into any) error {
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, limit))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(into); err != nil {
		return badRequest("bad request: " + err.Error())
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		return badRequest("bad request: one JSON value is required")
	}
	return nil
}

func (h *Handler) console() (consoleapi.Console, error) {
	if c := h.backend.Console(); c != nil {
		return c, nil
	}
	return nil, errNotWired
}

func (h *Handler) initialize(w http.ResponseWriter, r *http.Request) {
	capabilities := []string{}
	if h.backend.Model() != nil {
		capabilities = append(capabilities, "projects", "agents", "tasks", "events")
	}
	if c := h.backend.Console(); c != nil {
		capabilities = append(capabilities, "threads", "turns", "steer", "interrupt", "questions")
		materials, interactive := c.SubmissionCapabilities()
		if materials {
			capabilities = append(capabilities, "material_refs")
		}
		if interactive {
			capabilities = append(capabilities, "interactive_requests")
		}
	}
	if h.backend.ChannelHistory() != nil {
		capabilities = append(capabilities, "channel_threads")
	}
	if h.backend.Attempts() != nil {
		capabilities = append(capabilities, "attempt_diff")
	}
	writeJSON(w, Initialize{Protocol: Protocol, ServerVersion: h.backend.ServerVersion(), Capabilities: capabilities, Locale: string(i18n.ContextLocale(r.Context()))})
}
