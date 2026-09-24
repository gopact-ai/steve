package httpapi

import (
	"errors"
	"net/http"
	"strconv"

	"github.com/gopact-ai/steve/internal/attempt"
	"github.com/gopact-ai/steve/internal/consoleapi"
	"github.com/gopact-ai/steve/internal/plan"
	"github.com/gopact-ai/steve/internal/task"
)

func workPageLimit(r *http.Request) (int, error) {
	values := r.URL.Query()
	for _, value := range values {
		if len(value) != 1 {
			return 0, task.ErrInvalidQuery
		}
	}
	if values.Get("limit") == "" {
		return 50, nil
	}
	n, err := strconv.Atoi(values.Get("limit"))
	if err != nil || n < 1 || n > 100 {
		return 0, task.ErrInvalidQuery
	}
	return n, nil
}

func writeWorkQueryError(w http.ResponseWriter, err error) {
	code := http.StatusInternalServerError
	switch {
	case errors.Is(err, task.ErrInvalidQuery), errors.Is(err, plan.ErrInvalidQuery), errors.Is(err, attempt.ErrInvalidHistoryQuery):
		code = http.StatusBadRequest
	case errors.Is(err, task.ErrStaleCursor), errors.Is(err, plan.ErrStaleCursor), errors.Is(err, attempt.ErrHistoryChanged):
		code = http.StatusConflict
	case errors.Is(err, task.ErrTaskNotFound):
		code = http.StatusNotFound
	}
	http.Error(w, err.Error(), code)
}

func (s *Server) consoleTasks(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	limit, err := workPageLimit(r)
	if err != nil {
		writeWorkQueryError(w, err)
		return
	}
	q := r.URL.Query()
	page, err := s.model.TaskHistory(r.Context(), task.Query{Scope: task.Scope{Kind: q.Get("scope"), ID: q.Get("scope_id")}, Status: q.Get("status"), Archived: q.Get("archived"), Cursor: q.Get("cursor"), Limit: limit})
	if err != nil {
		writeWorkQueryError(w, err)
		return
	}
	writeJSON(w, page)
}

func (s *Server) consolePlans(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	limit, err := workPageLimit(r)
	if err != nil {
		writeWorkQueryError(w, err)
		return
	}
	q := r.URL.Query()
	page, err := s.model.PlanHistory(plan.Query{TaskID: q.Get("task_id"), ProjectID: q.Get("project_id"), Cursor: q.Get("cursor"), Limit: limit})
	if err != nil {
		writeWorkQueryError(w, err)
		return
	}
	writeJSON(w, page)
}

func (s *Server) consoleTaskAccounting(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	limit, err := workPageLimit(r)
	if err != nil {
		writeWorkQueryError(w, err)
		return
	}
	page, err := s.model.TaskAccounting(r.PathValue("task"), r.URL.Query().Get("cursor"), limit)
	if err != nil {
		writeWorkQueryError(w, err)
		return
	}
	writeJSON(w, page)
}

func (s *Server) consoleNativeAttempts(w http.ResponseWriter, r *http.Request) {
	s.nativeAttempts(w, r, r.URL.Query().Get("task_id"))
}

func (s *Server) nativeAttempts(w http.ResponseWriter, r *http.Request, taskID string) {
	w.Header().Set("Cache-Control", "no-store")
	if s.admin == nil {
		http.Error(w, "native attempt queries are not wired", http.StatusNotImplemented)
		return
	}
	limit, err := workPageLimit(r)
	if err != nil {
		writeWorkQueryError(w, err)
		return
	}
	page, err := s.admin.QueryAttempts(r.Context(), consoleapi.AttemptHistoryQuery{TaskID: taskID, Conversation: r.URL.Query().Get("conversation"), Cursor: r.URL.Query().Get("cursor"), Limit: limit})
	if err != nil {
		writeWorkQueryError(w, err)
		return
	}
	writeJSON(w, page)
}
