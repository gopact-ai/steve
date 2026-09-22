package httpapi

import (
	"errors"
	"log/slog"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"

	"github.com/gopact-ai/steve/internal/consoleapi"
)

// SetChannelHistory wires reads only. There is deliberately no channel command
// port here; reading retained messages cannot re-enter an execution.
func (s *Server) SetChannelHistory(history consoleapi.ChannelHistory) {
	s.channelHistory = history
}

func channelHistoryError(w http.ResponseWriter, err error) {
	status, message := http.StatusServiceUnavailable, "Channel history is unavailable; retry the read"
	switch {
	case errors.Is(err, consoleapi.ErrChannelConversationNotFound):
		status, message = http.StatusNotFound, "Channel conversation not found"
	case errors.Is(err, consoleapi.ErrChannelHistoryCursor):
		status, message = http.StatusBadRequest, "Invalid channel history cursor or limit"
	default:
		slog.Error("channel history read failed", "error", err)
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	writeJSON(w, map[string]string{"error": message})
}

func (s *Server) consoleChannelConversation(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if s.channelHistory == nil {
		http.Error(w, "Channel history is not enabled", http.StatusNotImplemented)
		return
	}
	q, err := url.ParseQuery(r.URL.RawQuery)
	limit := 50
	if err != nil || len(q["cursor"]) > 1 || len(q["limit"]) > 1 {
		err = consoleapi.ErrChannelHistoryCursor
	} else if q.Has("limit") {
		limit, err = strconv.Atoi(q.Get("limit"))
	}
	if err != nil || limit < 1 || limit > 200 {
		channelHistoryError(w, consoleapi.ErrChannelHistoryCursor)
		return
	}
	page, err := s.channelHistory.Read(r.Context(), r.PathValue("id"), q.Get("cursor"), limit)
	if err != nil {
		channelHistoryError(w, err)
		return
	}
	if page.Replies == nil {
		page.Replies = []consoleapi.Reply{}
	}
	w.Header().Set("Content-Type", "application/json")
	writeJSON(w, page)
}

// consoleIdentity prevents the Console's legacy short-name normalization from
// turning an existing channel identity into a new, unrelated Console session.
// An existing Console transcript can still be read when IDs collide. Mutations
// fail closed in that case: downstream task/session stores also use the raw ID.
func (s *Server) consoleIdentity(w http.ResponseWriter, r *http.Request, id string) bool {
	return s.checkConsoleIdentity(w, r, id, r.Method == http.MethodGet)
}

func (s *Server) consoleMutationIdentity(w http.ResponseWriter, r *http.Request, id string) bool {
	return s.checkConsoleIdentity(w, r, id, false)
}

func (s *Server) checkConsoleIdentity(w http.ResponseWriter, r *http.Request, id string, readOnly bool) bool {
	if transport := r.URL.Query().Get("transport"); transport != "" && transport != "console" {
		http.Error(w, "Channel conversations are read-only in Console", http.StatusForbidden)
		return false
	}
	if s.channelHistory == nil {
		return true
	}
	// Console retains short-name APIs and initialization trims whitespace.
	// Check both submitted and normalized addresses before either can mutate
	// the shared raw-ID state. A channel may itself start with "console:".
	candidates := map[string]bool{}
	for _, raw := range []string{id, strings.TrimSpace(id)} {
		candidates[raw] = true
		switch {
		case raw == "":
			candidates["console:main"] = true
		case strings.HasPrefix(raw, "console:"):
			candidates[strings.TrimPrefix(raw, "console:")] = true
		default:
			candidates["console:"+raw] = true
		}
	}
	found := false
	for candidate := range candidates {
		channel, err := s.channelHistory.Contains(r.Context(), candidate)
		if err != nil {
			channelHistoryError(w, err)
			return false
		}
		if channel {
			found = true
			break
		}
	}
	if !found {
		return true
	}
	if readOnly && s.console != nil {
		for _, existing := range s.console.Conversations() {
			if existing == id {
				return true
			}
		}
	}
	http.Error(w, "Channel conversations are read-only in Console; use the channel history endpoint", http.StatusForbidden)
	return false
}

func (s *Server) consoleExchangeIdentity(w http.ResponseWriter, r *http.Request) bool {
	if s.channelHistory == nil {
		return true
	}
	lookup, ok := s.console.(consoleapi.ExchangeIdentity)
	if !ok {
		http.Error(w, "Console exchange identity lookup is unavailable", http.StatusServiceUnavailable)
		return false
	}
	conversation, found := lookup.ExchangeConversation(r.PathValue("id"))
	if !found {
		queueResponse(w, nil, consoleapi.ErrExchangeNotFound)
		return false
	}
	return s.consoleMutationIdentity(w, r, conversation)
}

func (s *Server) consoleSubmissionIdentity(w http.ResponseWriter, r *http.Request, req consoleapi.Submission) bool {
	if !s.consoleIdentity(w, r, req.Conversation) {
		return false
	}
	for _, quote := range req.Quotes {
		if !s.consoleIdentity(w, r, quote.Conversation) {
			return false
		}
	}
	return true
}

func sortConversations(list []consoleapi.Conversation) {
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
}
