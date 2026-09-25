package app

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gopact-ai/steve/internal/console"
	"github.com/gopact-ai/steve/internal/consoleapi"
	"github.com/gopact-ai/steve/internal/gateway"
	"github.com/gopact-ai/steve/internal/httpapi"
	"github.com/gopact-ai/steve/internal/ledger"
	"github.com/gopact-ai/steve/internal/readmodel"
	"github.com/gopact-ai/steve/internal/task"
	"github.com/gopact-ai/steve/internal/turn"
	"github.com/gopact-ai/steve/internal/turn/turntest"
)

type historyContextProbe struct{ seen []string }

type historyActivityProbe struct{ busy atomic.Bool }

func (p *historyActivityProbe) ConversationBusy(string) bool { return p.busy.Load() }

func (p *historyContextProbe) Context(_ context.Context, id string) (turn.Context, error) {
	p.seen = append(p.seen, id)
	return turn.Context{
		Project: &turn.ContextProject{ID: "workspace", Bound: true},
		Agent:   &turn.AgentChoice{ID: "agent", Place: &turn.Placement{Node: "local", Workspace: "home"}},
	}, nil
}

// This test uses real ledger receipts, task accounting, the Console service and
// authenticated HTTP, but no channel or harness exists to receive side effects.
func TestChannelHistoryHTTPRetainedMessagesAndExecutionLifecycle(t *testing.T) {
	book, err := ledger.Open(t.TempDir(), ledger.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer book.Close()
	tasks, err := task.OpenLedger(testLedger(t))
	if err != nil {
		t.Fatal(err)
	}
	const identity = "original/topic with space"
	work, err := tasks.Create(task.Task{Transport: "feishu", Channel: identity, ProjectID: "workspace", Member: "agent"})
	if err != nil {
		t.Fatal(err)
	}
	token, err := tasks.ExecutionToken(work.ID)
	if err != nil {
		t.Fatal(err)
	}
	started := time.Now().UTC()
	if _, err := tasks.ReserveAttempt(token, "attempt", "message", "agent", "local", started); err != nil {
		t.Fatal(err)
	}
	record := func(id, kind string, value any) {
		t.Helper()
		raw, err := json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		if err := book.RecordCommand(t.Context(), id, kind, "owner", raw); err != nil {
			t.Fatal(err)
		}
	}
	record("channel-message", "gateway-input", map[string]any{"message": map[string]any{
		"ConversationID": identity, "ChatID": "chat", "MessageID": "message", "SenderOpenID": "owner", "Text": "retained original input", "Mentioned": true,
	}})
	model := readmodel.New(readmodel.Sources{})
	cons := console.New(turntest.IdleCoordinator{}, "owner", model)
	server, err := httpapi.NewServer(model, httpapi.ServerConfig{Addr: "127.0.0.1:0", Token: "isolated-test"})
	if err != nil {
		t.Fatal(err)
	}
	defer server.Close()
	server.SetConsole(cons)
	activity := &historyActivityProbe{}
	activity.busy.Store(true)
	server.SetChannelHistory(&channelConversations{ChannelHistory: gateway.NewChannelHistory(book), contexts: &historyContextProbe{}, tasks: tasks, activity: activity})
	go func() { _ = server.Serve() }()
	get := func(path string, target any) {
		t.Helper()
		req, _ := http.NewRequest("GET", server.URL()+path, nil)
		req.Header.Set("Authorization", "Bearer isolated-test")
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		if res.StatusCode != 200 {
			raw, _ := io.ReadAll(res.Body)
			t.Fatal(res.StatusCode, string(raw))
		}
		if err := json.NewDecoder(res.Body).Decode(target); err != nil {
			t.Fatal(err)
		}
	}
	var directory struct{ Conversations []consoleapi.Conversation }
	get("/console/conversations", &directory)
	if len(directory.Conversations) != 1 || directory.Conversations[0].ID != identity || directory.Conversations[0].Project != "workspace" || !directory.Conversations[0].Running {
		t.Fatalf("channel missing or misbound: %+v", directory)
	}
	var history consoleapi.ChannelConversationHistory
	path := "/console/channel-conversations/" + url.PathEscape(identity)
	get(path, &history)
	if len(history.Replies) != 1 || history.Replies[0].Input != "retained original input" {
		t.Fatal(history)
	}
	record("channel-message/dispatch", "gateway-input-dispatch", map[string]any{"result": turn.Result{
		Text: "retained final answer", AgentID: "agent", Attempt: "attempt",
	}})
	record("channel-message/reply", "gateway-input-reply", ledger.CommandProof{CommandID: "channel-message", Receipt: "lark-receipt"})
	if err := tasks.SettleAttempt(t.Context(), work.ID, "attempt", "message", started.Add(time.Second), task.OutcomeOK, task.RecoveryUsage{}); err != nil {
		t.Fatal(err)
	}
	activity.busy.Store(false)
	get(path, &history)
	if history.Conversation.Running || len(history.Replies) != 2 || history.Replies[1].Text != "retained final answer" || history.Replies[1].Delivery != "confirmed" {
		t.Fatalf("poll did not show settled result: %+v", history)
	}
	var before int
	_ = book.DB().QueryRow("SELECT count(*) FROM commands").Scan(&before)
	for _, endpoint := range []string{"/console/send", "/console/queue"} {
		body, _ := json.Marshal(map[string]string{"conversation": identity, "input": "forbidden"})
		req, _ := http.NewRequest("POST", server.URL()+endpoint, strings.NewReader(string(body)))
		req.Header.Set("Authorization", "Bearer isolated-test")
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		res.Body.Close()
		if res.StatusCode != http.StatusForbidden {
			t.Fatal("channel mutation reached Console", endpoint, res.StatusCode)
		}
	}
	var after int
	_ = book.DB().QueryRow("SELECT count(*) FROM commands").Scan(&after)
	if before != after || len(cons.Conversations()) != 0 {
		t.Fatal("reading or rejected mutation created a mirror session", before, after, cons.Conversations())
	}
}

type historyTaskProbe struct{ open int }

func (p historyTaskProbe) Query(q task.Query) (task.Page, error) {
	return task.Page{Items: []task.Header{
		{Task: task.Task{Transport: "feishu", Channel: q.Scope.ID, State: task.StateRunning}, Summary: task.ReadSummary{OpenExecutions: p.open}},
		// A Console task sharing an opaque ID cannot mark this channel busy.
		{Task: task.Task{Transport: "console", Channel: q.Scope.ID}, Summary: task.ReadSummary{OpenExecutions: 1}},
	}}, nil
}

func TestChannelHistoryContextUsesOriginalIdentityAndActualExecution(t *testing.T) {
	for _, open := range []int{0, 1} {
		contexts := &historyContextProbe{}
		h := &channelConversations{contexts: contexts, tasks: historyTaskProbe{open: open}}
		c := consoleapi.Conversation{ID: "original/channel", Transport: "feishu", ReadOnly: true}
		if err := h.enrich(t.Context(), &c); err != nil {
			t.Fatal(err)
		}
		if len(contexts.seen) != 1 || contexts.seen[0] != c.ID {
			t.Fatal("channel context normalized into another identity", contexts.seen)
		}
		if c.Project != "workspace" || c.Agent != "agent" || c.Place == nil || c.Place.Node != "local" {
			t.Fatalf("channel lost its project/agent: %+v", c)
		}
		want := "idle"
		if open > 0 {
			want = "unknown"
		}
		if c.Running || c.Execution != want {
			t.Fatal("unfinished accounting was confused with an executing turn", c.Execution, open)
		}
	}
}
