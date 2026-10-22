package clientapi

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/gopact-ai/steve/internal/consoleapi"
	"github.com/gopact-ai/steve/internal/conversationapi"
)

const (
	defaultItems = 100
	maxItems     = 500
)

func (h *Handler) listThreads(w http.ResponseWriter, r *http.Request) {
	c, err := h.console()
	if err != nil {
		writeError(w, err)
		return
	}
	list := c.Summaries(r.Context())
	for i := range list {
		list[i].Transport = "console"
	}
	if history := h.backend.ChannelHistory(); history != nil {
		cursor := ""
		for {
			page, err := history.List(r.Context(), cursor, 200)
			if err != nil {
				writeError(w, refuse(http.StatusServiceUnavailable, CodeUnavailable, errors.New("channel history is unavailable; retry the read")))
				return
			}
			list = append(list, page.Conversations...)
			if page.NextCursor == "" || page.NextCursor == cursor {
				break
			}
			cursor = page.NextCursor
		}
	}
	// Running first, then most recent, as the console's sidebar orders them.
	sort.SliceStable(list, func(i, j int) bool {
		if list[i].Running != list[j].Running {
			return list[i].Running
		}
		if !list[i].LastAt.Equal(list[j].LastAt) {
			return list[i].LastAt.After(list[j].LastAt)
		}
		if list[i].Transport != list[j].Transport {
			return list[i].Transport < list[j].Transport
		}
		return list[i].ID < list[j].ID
	})
	out := ThreadList{Threads: make([]Thread, 0, len(list))}
	for _, conversation := range list {
		out.Threads = append(out.Threads, toThread(conversation))
	}
	writeJSON(w, out)
}

func itemLimit(q url.Values) (int, error) {
	if len(q["limit"]) > 1 || len(q["cursor"]) > 1 {
		return 0, badRequest("limit and cursor may be given once")
	}
	if !q.Has("limit") {
		return defaultItems, nil
	}
	n, err := strconv.Atoi(q.Get("limit"))
	if err != nil || n < 1 || n > maxItems {
		return 0, badRequest("limit must be between 1 and " + strconv.Itoa(maxItems))
	}
	return n, nil
}

func (h *Handler) getThread(w http.ResponseWriter, r *http.Request) {
	c, err := h.console()
	if err != nil {
		writeError(w, err)
		return
	}
	q, err := url.ParseQuery(r.URL.RawQuery)
	if err != nil {
		writeError(w, badRequest("malformed query"))
		return
	}
	limit, err := itemLimit(q)
	if err != nil {
		writeError(w, err)
		return
	}
	id := r.PathValue("id")
	names := c.Conversations()
	// The exact console ID wins, then a channel holding the ID as given,
	// then the console's normalized form: a short name never turns a
	// channel's history into a console thread.
	history := h.backend.ChannelHistory()
	channel := false
	if history != nil && !slices.Contains(names, id) {
		if channel, err = history.Contains(r.Context(), id); err != nil {
			writeError(w, refuse(http.StatusServiceUnavailable, CodeUnavailable, errors.New("channel history is unavailable; retry the read")))
			return
		}
	}
	if !channel {
		name := id
		if !slices.Contains(names, name) {
			name = threadID(id)
		}
		if !slices.Contains(names, name) {
			writeError(w, refuse(http.StatusNotFound, CodeNotFound, errors.New("thread not found")))
			return
		}
		detail, err := h.consoleThreadDetail(r, c, name, q.Get("cursor"), limit)
		respond(w, detail, err)
		return
	}
	page, err := history.Read(r.Context(), id, q.Get("cursor"), limit)
	if err != nil {
		writeError(w, err)
		return
	}
	detail := ThreadDetail{Thread: toThread(page.Conversation), Turns: []Turn{}, Items: []Item{}, NextCursor: page.NextCursor}
	for _, reply := range page.Replies {
		detail.Items = append(detail.Items, toItems(reply)...)
	}
	writeJSON(w, detail)
}

// entry is a transcript line or a question, in thread order.
type entry struct {
	id    string
	at    time.Time
	items []Item
}

// consoleThreadDetail pages a console thread from its newest entry back.
// The cursor is the ID of the oldest entry of the page already read.
func (h *Handler) consoleThreadDetail(r *http.Request, c consoleapi.Console, name, cursor string, limit int) (ThreadDetail, error) {
	var thread Thread
	found := false
	for _, summary := range c.Summaries(r.Context()) {
		if summary.ID == name {
			summary.Transport = "console"
			thread, found = toThread(summary), true
			break
		}
	}
	if !found {
		thread = Thread{ID: name, Transport: "console"}
	}
	var entries []entry
	for _, reply := range c.Replies(name) {
		if items := toItems(reply); len(items) > 0 {
			entries = append(entries, entry{id: reply.ID, at: reply.At, items: items})
		}
	}
	for _, q := range c.Questions(name) {
		item := questionItem(q)
		entries = append(entries, entry{id: item.ID, at: q.CreatedAt, items: []Item{item}})
	}
	sort.SliceStable(entries, func(i, j int) bool { return entries[i].at.Before(entries[j].at) })
	end := len(entries)
	if cursor != "" {
		end = slices.IndexFunc(entries, func(e entry) bool { return e.id == cursor })
		if end < 0 {
			return ThreadDetail{}, refuse(http.StatusConflict, CodeStaleCursor, errors.New("the cursor's entry is no longer in this thread; read it again from the start"))
		}
	}
	start := max(0, end-limit)
	detail := ThreadDetail{Thread: thread, Turns: []Turn{}, Items: []Item{}}
	if start > 0 {
		detail.NextCursor = entries[start].id
	}
	for _, e := range entries[start:end] {
		detail.Items = append(detail.Items, e.items...)
	}
	for _, e := range c.Queue(name) {
		detail.Turns = append(detail.Turns, toTurn(e))
	}
	return detail, nil
}

func newThreadID() string {
	var raw [8]byte
	_, _ = rand.Read(raw[:])
	return threadPrefix + hex.EncodeToString(raw[:])
}

func (h *Handler) createThread(w http.ResponseWriter, r *http.Request) {
	c, err := h.console()
	if err != nil {
		writeError(w, err)
		return
	}
	var req CreateThread
	if err := decode(w, r, 1<<14, &req); err != nil {
		writeError(w, err)
		return
	}
	if strings.TrimSpace(req.Project) == "" {
		writeError(w, badRequest("project is required"))
		return
	}
	agent := strings.TrimSpace(req.Agent)
	id := newThreadID()
	if strings.TrimSpace(req.ID) != "" {
		id = threadID(req.ID)
	}
	if err := h.writable(r, id); err != nil {
		writeError(w, err)
		return
	}
	// Initializing the same thread with the same project again is a no-op,
	// so a retry carrying the same ID is safe.
	if agent != "" {
		if err := knownAgent(r.Context(), c, id, agent); err != nil {
			writeError(w, err)
			return
		}
	}
	if err := c.InitializeConversation(r.Context(), id, strings.TrimSpace(req.Project)); err != nil {
		writeError(w, err)
		return
	}
	if agent != "" {
		// Choosing the agent is the console's /use command, keyed by the
		// thread and agent so a retry is answered from its receipt.
		_, err := c.SendSubmission(r.Context(), conversationapi.Submission{Conversation: id, Input: "/use " + agent, CommandID: "v1-thread-agent:" + agent})
		if err != nil {
			writeError(w, err)
			return
		}
	}
	detail, err := h.consoleThreadDetail(r, c, id, "", defaultItems)
	respond(w, detail, err)
}

// knownAgent accepts only the exact ID of a registered agent, so choosing
// the agent never carries a prompt or matches an alias prefix.
func knownAgent(ctx context.Context, c consoleapi.Console, thread, agent string) error {
	if strings.IndexFunc(agent, unicode.IsSpace) >= 0 || strings.HasPrefix(agent, "@") || strings.HasPrefix(agent, "/") {
		return badRequest("agent must be a registered agent ID")
	}
	state, err := c.Context(ctx, thread)
	if err != nil {
		return err
	}
	for _, choice := range state.Agents {
		if choice.ID == agent {
			return nil
		}
	}
	return badRequest(fmt.Sprintf("agent %s is not registered", agent))
}

// writable refuses a mutation that names, in any form the console would
// normalize it to, a thread that belongs to a channel: channel histories
// are read-only and the console must not open a session under their ID.
func (h *Handler) writable(r *http.Request, id string) error {
	history := h.backend.ChannelHistory()
	if history == nil {
		return nil
	}
	candidates := map[string]bool{}
	for _, raw := range []string{id, strings.TrimSpace(id)} {
		candidates[raw] = true
		switch {
		case raw == "":
			candidates[threadPrefix+"main"] = true
		case strings.HasPrefix(raw, threadPrefix):
			candidates[strings.TrimPrefix(raw, threadPrefix)] = true
		default:
			candidates[threadPrefix+raw] = true
		}
	}
	for candidate := range candidates {
		channel, err := history.Contains(r.Context(), candidate)
		if err != nil {
			return refuse(http.StatusServiceUnavailable, CodeUnavailable, errors.New("channel history is unavailable; retry the request"))
		}
		if channel {
			return refuse(http.StatusForbidden, CodeForbidden, errors.New("channel threads are read-only"))
		}
	}
	return nil
}
