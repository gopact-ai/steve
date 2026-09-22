package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gopact-ai/steve/internal/consoleapi"
)

type channelHistoryStub struct {
	err   error
	extra string
}

type capabilityProbeConsole struct {
	fakeConsole
	queueReads int
}

func (*capabilityProbeConsole) Conversations() []string { return nil }
func (f *capabilityProbeConsole) Queue(conversation string) []consoleapi.Exchange {
	f.queueReads++
	return []consoleapi.Exchange{{ID: "private-exchange", Conversation: conversation}}
}
func (*capabilityProbeConsole) SubmissionCapabilities() (bool, bool) { return true, false }

type capabilityProbeHistory struct {
	channelHistoryStub
	containsReads int
}

func (f *capabilityProbeHistory) Contains(ctx context.Context, id string) (bool, error) {
	f.containsReads++
	return f.channelHistoryStub.Contains(ctx, id)
}

func TestConsoleQueueCapabilitiesDoNotReadConversations(t *testing.T) {
	for _, channel := range []string{"console:main", "main"} {
		for _, unavailable := range []bool{false, true} {
			f := &capabilityProbeConsole{}
			history := &capabilityProbeHistory{channelHistoryStub: channelHistoryStub{extra: channel}}
			if unavailable {
				history.err = errors.New("private database failure")
			}
			s := &Server{token: "owner", console: f}
			s.SetChannelHistory(history)
			read := func(query, token string) *httptest.ResponseRecorder {
				req := httptest.NewRequest("GET", "/console/queue"+query, nil)
				req.Header.Set("Authorization", "Bearer "+token)
				rec := httptest.NewRecorder()
				s.guard(s.consoleQueue)(rec, req)
				return rec
			}
			for _, query := range []string{"?capabilities=1", "?capabilities=1&conversation=native%2Ftopic"} {
				rec := read(query, "owner")
				var got struct {
					Queue               []consoleapi.Exchange `json:"queue"`
					SubmissionKeys      bool                  `json:"submission_keys"`
					MaterialRefs        bool                  `json:"material_refs"`
					InteractiveRequests *bool                 `json:"interactive_requests"`
				}
				if rec.Code != 200 || json.Unmarshal(rec.Body.Bytes(), &got) != nil ||
					got.Queue == nil || len(got.Queue) != 0 || !got.SubmissionKeys || !got.MaterialRefs ||
					got.InteractiveRequests == nil || *got.InteractiveRequests ||
					rec.Header().Get("Cache-Control") != "no-store" {
					t.Fatalf("channel=%q unavailable=%v query=%q: %d %s", channel, unavailable, query, rec.Code, rec.Body.String())
				}
				if f.queueReads != 0 || history.containsReads != 0 {
					t.Fatal("capability probe read conversation state", f.queueReads, history.containsReads)
				}
			}
			if rec := read("?capabilities=1", ""); rec.Code != 401 {
				t.Fatal("capability probe bypassed authentication", rec.Code)
			}
			want := 403
			if unavailable {
				want = 503
			}
			for _, query := range []string{"", "?capabilities=0", "?conversation=console%3Amain"} {
				if rec := read(query, "owner"); rec.Code != want {
					t.Fatal("ordinary queue read bypassed identity guard", query, rec.Code, rec.Body.String())
				}
			}
			if f.queueReads != 0 {
				t.Fatal("guarded queue was read", f.queueReads)
			}
			rec := httptest.NewRecorder()
			req := httptest.NewRequest("POST", "/console/queue?capabilities=1", strings.NewReader(`{"input":"must not send","command_id":"probe"}`))
			req.Header.Set("Authorization", "Bearer owner")
			s.guard(s.consoleEnqueue)(rec, req)
			if rec.Code != want || len(f.exchanges) != 0 {
				t.Fatal("capability query bypassed mutation guard", rec.Code, rec.Body.String())
			}
			if !unavailable {
				if rec := read("?conversation=console%3Aother", "owner"); rec.Code != 200 || f.queueReads != 1 {
					t.Fatal("unrelated console queue was blocked", rec.Code, rec.Body.String())
				}
			}
			s.console = nil
			if rec := read("?capabilities=1", "owner"); rec.Code != 501 {
				t.Fatal("disabled console advertised capabilities", rec.Code)
			}
		}
	}
}

func TestChannelIdentityCannotReachConsoleMetadataOrReadAliases(t *testing.T) {
	for _, test := range []struct {
		method, path, body string
		handle             func(*Server) http.HandlerFunc
	}{
		{"PUT", "/console/conversations/native%2Ftopic", `{"title":"changed"}`, func(s *Server) http.HandlerFunc { return s.consoleUpdateConversation }},
		{"DELETE", "/console/conversations/native%2Ftopic", "", func(s *Server) http.HandlerFunc { return s.consoleDeleteConversation }},
		{"PUT", "/console/conversations/native%2Ftopic/initialize", `{"project":"workspace"}`, func(s *Server) http.HandlerFunc { return s.consoleInitializeConversation }},
		{"PUT", "/console/preferences", `{"conversation":"native/topic","agent":"agent","patch":{"model":"other"}}`, func(s *Server) http.HandlerFunc { return s.consoleSetPreferences }},
		{"GET", "/console/replies?conversation=native%2Ftopic", "", func(s *Server) http.HandlerFunc { return s.consoleReplies }},
		{"GET", "/console/context?conversation=native%2Ftopic", "", func(s *Server) http.HandlerFunc { return s.consoleContext }},
		{"GET", "/console/setup?conversation=native%2Ftopic", "", func(s *Server) http.HandlerFunc { return s.consoleSetup }},
		{"GET", "/console/queue?conversation=native%2Ftopic", "", func(s *Server) http.HandlerFunc { return s.consoleQueue }},
	} {
		t.Run(test.method+test.path, func(t *testing.T) {
			admin := &fakeDeleteAdmin{}
			s := &Server{console: &fakeConsole{}, admin: admin}
			s.SetChannelHistory(channelHistoryStub{})
			req := httptest.NewRequest(test.method, test.path, strings.NewReader(test.body))
			req.SetPathValue("id", "native/topic")
			rec := httptest.NewRecorder()
			test.handle(s)(rec, req)
			if rec.Code != 403 || len(admin.deleted) != 0 {
				t.Fatal(rec.Code, rec.Body.String())
			}
		})
	}
}

func TestExplicitChannelTransportRejectsConsoleWriteEvenForExistingID(t *testing.T) {
	s := &Server{console: &fakeConsole{}}
	s.SetChannelHistory(channelHistoryStub{})
	rec := httptest.NewRecorder()
	s.consoleSend(rec, httptest.NewRequest("POST", "/console/send?transport=feishu", strings.NewReader(`{"conversation":"console:main","input":"must not send"}`)))
	if rec.Code != 403 {
		t.Fatal(rec.Code, rec.Body.String())
	}
}

func (f channelHistoryStub) List(context.Context, string, int) (consoleapi.ChannelConversationPage, error) {
	return consoleapi.ChannelConversationPage{Conversations: []consoleapi.Conversation{{
		ID: "native/topic", Transport: "feishu", ReadOnly: true, Project: "workspace", Title: "Channel history",
	}}}, f.err
}
func (f channelHistoryStub) Contains(_ context.Context, id string) (bool, error) {
	return id == "native/topic" || id == "console:opaque-channel" || f.extra != "" && id == f.extra, f.err
}

func (f *fakeConsole) ExchangeConversation(id string) (string, bool) {
	for _, e := range f.exchanges {
		if e.ID == id {
			return e.Conversation, true
		}
	}
	return "", false
}

func TestChannelQueueControlsResolveAuthoritativeConversation(t *testing.T) {
	for _, control := range []string{"edit", "delete", "steer"} {
		f := &fakeConsole{exchanges: []consoleapi.Exchange{{ID: "existing-exchange", Conversation: "console:opaque-channel", State: "queued", Input: "original"}}}
		s := &Server{console: f}
		s.SetChannelHistory(channelHistoryStub{})
		rec := httptest.NewRecorder()
		req := httptest.NewRequest("POST", "/console/queue/existing-exchange", strings.NewReader(`{"input":"edited"}`))
		req.SetPathValue("id", "existing-exchange")
		switch control {
		case "edit":
			s.consoleEditQueued(rec, req)
		case "delete":
			s.consoleDeleteQueued(rec, req)
		case "steer":
			s.consoleSteer(rec, req)
		}
		if rec.Code != 403 || len(f.exchanges) != 1 || f.exchanges[0].Input != "original" || f.exchanges[0].State != "queued" {
			t.Fatal("queue control bypassed channel identity", control, rec.Code, f.exchanges)
		}
	}
}

func TestConsoleSelectorsCannotOpenDiscoveryForChannelCollision(t *testing.T) {
	s := &Server{console: &fakeConsole{}, admin: &fakeDeleteAdmin{}}
	s.SetChannelHistory(channelHistoryStub{extra: "console:main"})
	rec := httptest.NewRecorder()
	s.consoleSelectors(rec, httptest.NewRequest("GET", "/console/selectors?conversation=console:main&agent=agent", nil))
	if rec.Code != 403 {
		t.Fatal("selector discovery reached channel runtime", rec.Code)
	}
}

func TestConsoleMutationChecksBothLegacyNormalizations(t *testing.T) {
	for _, test := range []struct{ channel, request string }{
		{"native/topic", "console:native/topic"},
		{"console: opaque ", " opaque "},
		{"console:opaque", " opaque "},
	} {
		s := &Server{console: &fakeConsole{}}
		s.SetChannelHistory(channelHistoryStub{extra: test.channel})
		rec := httptest.NewRecorder()
		if s.consoleIdentity(rec, httptest.NewRequest("POST", "/console/send", nil), test.request) || rec.Code != 403 {
			t.Fatal("legacy alias bypassed channel identity", test, rec.Code)
		}
	}
}

func TestConsoleDefaultCannotMutateCollidingChannel(t *testing.T) {
	s := &Server{console: &fakeConsole{}}
	s.SetChannelHistory(channelHistoryStub{extra: "console:main"})
	for _, id := range []string{"", "main", "console:main", "  "} {
		req := httptest.NewRequest("PUT", "/console/preferences", nil)
		rec := httptest.NewRecorder()
		if s.consoleIdentity(rec, req, id) || rec.Code != 403 {
			t.Fatal("default alias bypassed read-only boundary", id, rec.Code)
		}
	}
}
func (f channelHistoryStub) Read(_ context.Context, id, cursor string, limit int) (consoleapi.ChannelConversationHistory, error) {
	if f.err != nil {
		return consoleapi.ChannelConversationHistory{}, f.err
	}
	if id != "native/topic" && id != "console:opaque-channel" {
		return consoleapi.ChannelConversationHistory{}, consoleapi.ErrChannelConversationNotFound
	}
	if cursor == "bad" {
		return consoleapi.ChannelConversationHistory{}, consoleapi.ErrChannelHistoryCursor
	}
	return consoleapi.ChannelConversationHistory{
		Conversation: consoleapi.Conversation{ID: id, Transport: "feishu", ReadOnly: true},
		Replies:      []consoleapi.Reply{{ID: "original-input", Conversation: id, Kind: "sent", Input: "Original channel message"}},
		NextCursor:   "older",
	}, nil
}

func TestChannelConversationsJoinDirectoryWithoutChangingIdentity(t *testing.T) {
	s := &Server{console: &fakeConsole{}}
	s.SetChannelHistory(channelHistoryStub{})
	rec := httptest.NewRecorder()
	s.consoleConversations(rec, httptest.NewRequest("GET", "/console/conversations", nil))
	if rec.Code != 200 {
		t.Fatal(rec.Code, rec.Body.String())
	}
	var got struct{ Conversations []consoleapi.Conversation }
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if len(got.Conversations) != 2 {
		t.Fatalf("channel conversation missing: %+v", got)
	}
	for _, c := range got.Conversations {
		if c.ID == "native/topic" && c.Transport == "feishu" && c.ReadOnly && c.Project == "workspace" {
			return
		}
	}
	t.Fatal("channel identity or source lost", rec.Body.String())
}

func TestChannelHistoryReadIsGuardedAndPreservesOpaqueID(t *testing.T) {
	s := &Server{token: "owner", console: &fakeConsole{}}
	s.SetChannelHistory(channelHistoryStub{})
	for _, test := range []struct {
		id, token, query string
		want             int
	}{
		{"native/topic", "", "", 401},
		{"native/topic", "owner", "", 200},
		{"console:opaque-channel", "owner", "", 200},
		{"missing", "owner", "", 404},
		{"native/topic", "owner", "?cursor=bad", 400},
		{"native/topic", "owner", "?limit=0", 400},
		{"native/topic", "owner", "?limit=201", 400},
		{"native/topic", "owner", "?limit=2&limit=3", 400},
	} {
		t.Run(test.id+test.query+test.token, func(t *testing.T) {
			req := httptest.NewRequest("GET", "/console/channel-conversations/id"+test.query, nil)
			req.SetPathValue("id", test.id)
			req.Header.Set("Authorization", "Bearer "+test.token)
			rec := httptest.NewRecorder()
			s.guard(s.consoleChannelConversation)(rec, req)
			if rec.Code != test.want {
				t.Fatal(rec.Code, rec.Body.String())
			}
			if test.want == 200 {
				var got consoleapi.ChannelConversationHistory
				if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
					t.Fatal(err)
				}
				if got.Conversation.ID != test.id || got.Replies[0].Conversation != test.id || got.NextCursor != "older" {
					t.Fatal("read rewrote channel identity", rec.Body.String())
				}
			}
		})
	}
}

func TestConsoleCommandsRejectKnownChannelBeforeEnqueue(t *testing.T) {
	for _, endpoint := range []string{"/console/send", "/console/queue"} {
		for _, id := range []string{"native/topic", "console:opaque-channel", "opaque-channel", " native/topic "} {
			f := &fakeConsole{}
			s := &Server{console: f}
			s.SetChannelHistory(channelHistoryStub{})
			body, _ := json.Marshal(map[string]string{"conversation": id, "input": "must not execute", "command_id": "test"})
			req := httptest.NewRequest("POST", endpoint, strings.NewReader(string(body)))
			rec := httptest.NewRecorder()
			if endpoint == "/console/send" {
				s.consoleSend(rec, req)
			} else {
				s.consoleEnqueue(rec, req)
			}
			if rec.Code != http.StatusForbidden || len(f.exchanges) != 0 || len(f.replies) != 0 {
				t.Fatalf("%s %s reached console: status=%d exchanges=%+v replies=%+v", endpoint, id, rec.Code, f.exchanges, f.replies)
			}
		}
	}
}

func TestChannelHistoryFailureDoesNotMasqueradeAsEmptySuccess(t *testing.T) {
	s := &Server{console: &fakeConsole{}}
	s.SetChannelHistory(channelHistoryStub{err: errors.New("private database path")})
	rec := httptest.NewRecorder()
	s.consoleConversations(rec, httptest.NewRequest("GET", "/console/conversations", nil))
	if rec.Code != 503 || strings.Contains(rec.Body.String(), "private database path") {
		t.Fatal(rec.Code, rec.Body.String())
	}
	f := &fakeConsole{}
	s.console = f
	rec = httptest.NewRecorder()
	s.consoleSend(rec, httptest.NewRequest("POST", "/console/send", strings.NewReader(`{"conversation":"native/topic","input":"must not execute"}`)))
	if rec.Code != 503 || len(f.replies) != 0 {
		t.Fatal("unknown source was allowed to execute", rec.Code, rec.Body.String())
	}
}
