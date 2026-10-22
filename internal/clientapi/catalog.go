package clientapi

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"github.com/gopact-ai/steve/internal/task"
)

func (h *Handler) attemptDiff(w http.ResponseWriter, r *http.Request) {
	attempts := h.backend.Attempts()
	if attempts == nil {
		writeError(w, errNotWired)
		return
	}
	id, path := r.PathValue("id"), r.URL.Query().Get("path")
	index, err := attempts.AttemptChanges(r.Context(), id)
	if err != nil {
		writeError(w, err)
		return
	}
	out := AttemptDiff{Attempt: id, Project: index.Project, Truncated: index.Truncated, Note: index.Note, Files: []ChangedFile{}}
	for _, change := range index.Changes {
		out.Files = append(out.Files, ChangedFile{Path: change.Path, Status: change.Status, Added: change.Added, Deleted: change.Deleted, Binary: change.Binary})
	}
	if path != "" {
		diff, err := attempts.AttemptDiff(r.Context(), id, path)
		if err != nil {
			writeError(w, err)
			return
		}
		out.Path, out.Diff, out.Truncated = diff.Path, diff.Diff, diff.Truncated
	}
	writeJSON(w, out)
}

func (h *Handler) listProjects(w http.ResponseWriter, r *http.Request) {
	model := h.backend.Model()
	if model == nil {
		writeError(w, errNotWired)
		return
	}
	out := ProjectList{Projects: []Project{}}
	for _, p := range model.Snapshot(r.Context()).Projects {
		out.Projects = append(out.Projects, toProject(p))
	}
	writeJSON(w, out)
}

func (h *Handler) listAgents(w http.ResponseWriter, r *http.Request) {
	model := h.backend.Model()
	if model == nil {
		writeError(w, errNotWired)
		return
	}
	out := AgentList{Agents: []Agent{}}
	for _, a := range model.Snapshot(r.Context()).Agents {
		out.Agents = append(out.Agents, toAgent(a))
	}
	writeJSON(w, out)
}

// listTasks pages task history. thread or project scope it; status is all,
// live or closed.
func (h *Handler) listTasks(w http.ResponseWriter, r *http.Request) {
	model := h.backend.Model()
	if model == nil {
		writeError(w, errNotWired)
		return
	}
	q, err := url.ParseQuery(r.URL.RawQuery)
	if err != nil {
		writeError(w, badRequest("malformed query"))
		return
	}
	for _, values := range q {
		if len(values) != 1 {
			writeError(w, badRequest("each parameter may be given once"))
			return
		}
	}
	query := task.Query{Status: q.Get("status"), Cursor: q.Get("cursor")}
	switch {
	case q.Has("thread") && q.Has("project"):
		writeError(w, badRequest("thread and project cannot both scope tasks"))
		return
	case q.Has("thread"):
		query.Scope = task.Scope{Kind: "conversation", ID: q.Get("thread")}
	case q.Has("project"):
		query.Scope = task.Scope{Kind: "project", ID: q.Get("project")}
	}
	if q.Has("limit") {
		n, err := strconv.Atoi(q.Get("limit"))
		if err != nil || n < 1 || n > 100 {
			writeError(w, badRequest("limit must be between 1 and 100"))
			return
		}
		query.Limit = n
	}
	page, err := model.TaskHistory(r.Context(), query)
	if err != nil {
		writeError(w, err)
		return
	}
	out := TaskPage{Tasks: []Task{}, Total: page.Total, NextCursor: page.NextCursor}
	for _, t := range page.Items {
		out.Tasks = append(out.Tasks, toTask(t))
	}
	writeJSON(w, out)
}

// keepalive is how often an idle stream sends a comment, so proxies keep
// the connection open.
var keepalive = 20 * time.Second

// events streams changes as server-sent events. A client resumes after the
// last cursor it received, by Last-Event-ID or after; one whose cursor can
// no longer be continued is first sent a reset event and should re-read
// what it shows. thread keeps only that thread's events; a reset is always
// sent.
func (h *Handler) events(w http.ResponseWriter, r *http.Request) {
	model := h.backend.Model()
	if model == nil {
		writeError(w, errNotWired)
		return
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeError(w, refuse(http.StatusInternalServerError, CodeInternal, fmt.Errorf("streaming unsupported")))
		return
	}
	thread := r.URL.Query().Get("thread")
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	resume, stream, stop := model.SubscribeAfter(r.Context(), r.Header.Get("Last-Event-ID"), r.URL.Query().Get("after"))
	defer stop()
	if resume.Reset {
		fmt.Fprint(w, "event: reset\ndata: {}\n\n")
	}
	send := func(ev Event) {
		if thread != "" && ev.Thread != thread {
			return
		}
		payload, err := json.Marshal(ev)
		if err != nil {
			return
		}
		fmt.Fprintf(w, "id: %s\nevent: %s\ndata: %s\n\n", ev.Cursor, ev.Type, payload)
	}
	for _, ev := range resume.Replay {
		send(toEvent(model.EventID(ev), ev))
	}
	flusher.Flush()
	tick := time.NewTicker(keepalive)
	defer tick.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case ev, open := <-stream:
			if !open {
				return
			}
			send(toEvent(model.EventID(ev), ev))
			flusher.Flush()
		case <-tick.C:
			fmt.Fprint(w, ": keepalive\n\n")
			flusher.Flush()
		}
	}
}
