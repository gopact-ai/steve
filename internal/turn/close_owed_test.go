package turn

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/gopact-ai/acp"
	"github.com/gopact-ai/steve/internal/agent"
	"github.com/gopact-ai/steve/internal/attempt"
	"github.com/gopact-ai/steve/internal/capability"
	"github.com/gopact-ai/steve/internal/harness"
	"github.com/gopact-ai/steve/internal/i18n"
	"github.com/gopact-ai/steve/internal/ledger"
	"github.com/gopact-ai/steve/internal/node"
	"github.com/gopact-ai/steve/internal/nodewire"
	"github.com/gopact-ai/steve/internal/protocol"
	"github.com/gopact-ai/steve/internal/state"
	"github.com/gopact-ai/steve/internal/task"
)

type sessionClose struct {
	place harness.Placement
	id    string
}

// nodeSessions opens sessions under ids a node gives them and answers each
// close the way that node would.
type nodeSessions struct {
	*fakeManager
	closes []sessionClose
	err    error
}

func (m *nodeSessions) OpenSession(ctx context.Context, at harness.Placement, upstreamID, workdir string, servers []acp.MCPServer) (harness.Runner, error) {
	if upstreamID == "" {
		upstreamID = "ns_owed"
	}
	return m.fakeManager.OpenSession(ctx, at, upstreamID, workdir, servers)
}

func (m *nodeSessions) CloseSession(_ context.Context, at harness.Placement, id string) error {
	m.closes = append(m.closes, sessionClose{place: at, id: id})
	return m.err
}

// owedCloseCoordinator runs its one agent on node-b, answering through
// sessions, for a console owner and a Feishu owner both named owner.
func owedCloseCoordinator(t *testing.T, sessions *nodeSessions) (*Coordinator, *task.Store, *ledger.Ledger) {
	t.Helper()
	catalog, err := agent.NewCatalog(map[string]agent.Config{"codex": {Harness: "codex", Node: "node-b", Default: true}})
	if err != nil {
		t.Fatal(err)
	}
	book := testLedger(t)
	store, err := state.OpenLedger(book)
	if err != nil {
		t.Fatal(err)
	}
	tasks, err := task.OpenLedger(book)
	if err != nil {
		t.Fatal(err)
	}
	c := newCoordinator(t, catalog, store, capability.NewAssembler(nil), sessions, time.Minute,
		onLedger(book), withTasks(tasks, "laptop"), withOwner("owner"), withChannelOwner("feishu", "owner"))
	c.artifacts.SetExecution(c.executions)
	return c, tasks, book
}

// ownerChannels are the two ways the owner reaches a conversation.
var ownerChannels = []struct {
	name string
	req  Request
}{
	{"console", Request{Channel: "console", ConversationID: "console:owed", SenderOpenID: "owner", ChatType: protocol.ChatP2P, ChatID: "console", Mentioned: true}},
	{"feishu", Request{Channel: "feishu", ConversationID: "oc_owed", SenderOpenID: "owner", ChatType: protocol.ChatP2P, ChatID: "oc_owed", Mentioned: true}},
}

// startOwedSession runs one turn in req's conversation, so it holds a
// session on node-b, and returns that session and the execution that ran.
func startOwedSession(t *testing.T, c *Coordinator, req Request) (state.Session, Result) {
	t.Helper()
	req.Input, req.MessageID = "hello", "m1"
	first, err := c.Handle(t.Context(), req)
	if err != nil {
		t.Fatalf("first turn: %v", err)
	}
	session, ok := c.store.Conversation(req.ConversationID).Sessions["codex"]
	if !ok || session.UpstreamID != "ns_owed" {
		t.Fatalf("the first turn left the session %+v", c.store.Conversation(req.ConversationID))
	}
	return session, first
}

// /new lets go of a session whose node cannot be reached: the request to
// close it certainly never got there, so the conversation starts over now
// and the close is owed to the node, naming the execution it ends.
func TestResetLetsGoOfASessionItsNodeCannotBeReachedFor(t *testing.T) {
	for _, tc := range ownerChannels {
		t.Run(tc.name, func(t *testing.T) {
			sessions := &nodeSessions{fakeManager: &fakeManager{runners: map[string]*fakeRunner{"codex": {reply: "ok"}}}}
			c, tasks, _ := owedCloseCoordinator(t, sessions)
			session, first := startOwedSession(t, c, tc.req)
			record, err := c.attempts.Get(t.Context(), first.Attempt)
			if err != nil {
				t.Fatal(err)
			}
			if record.Session != "ns_owed" || record.Node != session.NodeID || record.Harness != session.HarnessID {
				t.Fatalf("the execution %+v does not name the session %+v", record, session)
			}
			sessions.err = &nodewire.SessionNotDispatched{Cause: errors.New("dial node-b: connection refused")}
			req := tc.req
			req.Input, req.MessageID = "/new", "m2"
			result, err := c.Handle(t.Context(), req)
			if err != nil {
				t.Fatalf("/new with its node unreachable: %v", err)
			}
			if want := c.text.T(i18n.Reset, "codex"); result.Text != want {
				t.Fatalf("/new answered %q, want %q", result.Text, want)
			}
			if want := []sessionClose{{place: harness.Placement{Node: session.NodeID, Harness: session.HarnessID}, id: "ns_owed"}}; !reflect.DeepEqual(sessions.closes, want) {
				t.Fatalf("closes %+v, want %+v", sessions.closes, want)
			}
			if live, ok := c.store.Conversation(req.ConversationID).Sessions["codex"]; ok {
				t.Fatalf("the session is still live: %+v", live)
			}
			archived := c.store.ArchivedSessions(req.ConversationID, "codex")
			if len(archived) != 1 || archived[0].UpstreamID != "ns_owed" {
				t.Fatalf("archived %+v", archived)
			}
			want := []state.OwedClose{{
				NodeID: session.NodeID, HarnessID: session.HarnessID, UpstreamID: "ns_owed", NativeContext: record.NativeContext,
				TaskID: record.TaskID, AttemptID: record.ID, OwedAt: archived[0].ArchivedAt,
			}}
			if got := c.store.OwedCloses(); !reflect.DeepEqual(got, want) {
				t.Fatalf("closes owed %+v, want %+v", got, want)
			}
			if tracked, ok := tasks.Get(record.TaskID); !ok || tracked.State != task.StateDone {
				t.Fatalf("the conversation's task is %+v", tracked)
			}
			// The conversation starts over in a session of its own.
			sessions.err = nil
			req.Input, req.MessageID = "again", "m3"
			if _, err := c.Handle(t.Context(), req); err != nil {
				t.Fatalf("the turn after /new: %v", err)
			}
		})
	}
}

// Only a close that certainly never reached the node is owed, and only
// for a session the cluster opened for an execution it can still name. A
// node that answered has settled the question its own way, and a close
// whose fate is unknown may have landed; either keeps the session and says
// so.
func TestResetKeepsTheSessionWhenTheCloseMayHaveLanded(t *testing.T) {
	notSent := &nodewire.SessionNotDispatched{Cause: errors.New("dial node-b: connection refused")}
	for name, tc := range map[string]struct {
		failure error
		change  func(*testing.T, *Coordinator, string)
	}{
		"the node refused":           {failure: &node.SessionError{Code: "conflict", Message: "session is bound to another execution"}},
		"the node could not stop it": {failure: &node.SessionError{Code: "uncertain", Message: "native process stop is not confirmed"}},
		"the answer was lost":        {failure: errors.New("read node response: connection reset by peer")},
		"the request timed out":      {failure: context.DeadlineExceeded},
		"never sent, not a cluster session": {failure: notSent, change: func(t *testing.T, c *Coordinator, conversation string) {
			resumeAs(t, c, conversation, "codex-session")
		}},
		"never sent, no execution ran there": {failure: notSent, change: func(t *testing.T, c *Coordinator, conversation string) {
			resumeAs(t, c, conversation, "ns_elsewhere")
		}},
		"never sent, history unreadable": {failure: notSent, change: func(t *testing.T, c *Coordinator, _ string) {
			unreadable, err := ledger.Open(t.TempDir(), ledger.Options{})
			if err != nil {
				t.Fatal(err)
			}
			if err := unreadable.Close(); err != nil {
				t.Fatal(err)
			}
			c.attempts = attempt.New(unreadable)
		}},
	} {
		t.Run(name, func(t *testing.T) {
			sessions := &nodeSessions{fakeManager: &fakeManager{runners: map[string]*fakeRunner{"codex": {reply: "ok"}}}}
			c, _, _ := owedCloseCoordinator(t, sessions)
			req := ownerChannels[0].req
			startOwedSession(t, c, req)
			if tc.change != nil {
				tc.change(t, c, req.ConversationID)
			}
			sessions.err = tc.failure
			before := c.store.Conversation(req.ConversationID).Sessions["codex"]
			req.Input, req.MessageID = "/new", "m2"
			if _, err := c.Handle(t.Context(), req); err == nil {
				t.Fatal("/new reported success for a close that may have landed")
			}
			if len(sessions.closes) != 1 || sessions.closes[0].id != before.UpstreamID {
				t.Fatalf("closes %+v, want one of %s", sessions.closes, before.UpstreamID)
			}
			if live, ok := c.store.Conversation(req.ConversationID).Sessions["codex"]; !ok || live.UpstreamID != before.UpstreamID {
				t.Fatalf("the session went: %+v", c.store.Conversation(req.ConversationID))
			}
			if got := c.store.ArchivedSessions(req.ConversationID, "codex"); len(got) != 0 {
				t.Fatalf("archived %+v", got)
			}
			if got := c.store.OwedCloses(); len(got) != 0 {
				t.Fatalf("owes %+v", got)
			}
		})
	}
}

// resumeAs has the conversation's live session carry id instead.
func resumeAs(t *testing.T, c *Coordinator, conversation, id string) {
	t.Helper()
	session := c.store.Conversation(conversation).Sessions["codex"]
	session.UpstreamID = id
	if err := c.store.SaveSession(session); err != nil {
		t.Fatal(err)
	}
}
