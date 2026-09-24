package turn

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gopact-ai/steve/internal/agentmcp"
	"github.com/gopact-ai/steve/internal/attempt"
	"github.com/gopact-ai/steve/internal/channel"
	"github.com/gopact-ai/steve/internal/ledger"
	"github.com/gopact-ai/steve/internal/project"
	"github.com/gopact-ai/steve/internal/protocol"
	"github.com/gopact-ai/steve/internal/schedule"
	"github.com/gopact-ai/steve/internal/task"
)

// Only the MCP grant adapter is replaced. Schedules, task/attempt/project
// state, schedule persistence and HTTP tool dispatch are the real implementations.
type scheduleGrantStore struct {
	mu   sync.Mutex
	data map[string]json.RawMessage
}
type scheduleGrantTx struct{ data map[string]json.RawMessage }

func (s *scheduleGrantStore) Update(ctx context.Context, fn func(agentmcp.StoreTx) error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	next := make(map[string]json.RawMessage, len(s.data))
	for k, v := range s.data {
		next[k] = v
	}
	if err := fn(scheduleGrantTx{next}); err != nil {
		return err
	}
	s.data = next
	return nil
}
func (s scheduleGrantTx) Get(kind, id string, v any) (bool, error) {
	raw, ok := s.data[kind+"/"+id]
	if !ok {
		return false, nil
	}
	return true, json.Unmarshal(raw, v)
}
func (s scheduleGrantTx) Put(kind, id string, v any) error {
	raw, err := json.Marshal(v)
	if err == nil {
		s.data[kind+"/"+id] = raw
	}
	return err
}
func (s scheduleGrantTx) Delete(kind, id string) error                        { delete(s.data, kind+"/"+id); return nil }
func (scheduleGrantTx) Authorize(agentmcp.Binding, agentmcp.GrantScope) error { return nil }
func (scheduleGrantTx) Bind(agentmcp.Binding, *agentmcp.GrantScope, agentmcp.GrantScope) error {
	return nil
}

type scheduleMCPFixture struct {
	c       *Coordinator
	jobs    *schedule.Store
	gate    *agentmcp.Server
	book    *ledger.Ledger
	tracked task.Task
	scope   agentmcp.GrantScope
}

func newScheduleMCPFixture(t *testing.T, change func(*task.Task, *attempt.Record)) scheduleMCPFixture {
	t.Helper()
	c, tasks, book := taskCoordinatorBook(t, &fakeRunner{reply: "ok"}, withOwner("console-owner"), withChannelOwner("feishu", "feishu-owner"))
	work := task.Task{Channel: "chat", Transport: "feishu", Member: "codex", Requester: "feishu-owner", ProjectID: "codex", ChatType: string(protocol.ChatP2P), ChatID: "chat-native", AnchorMessage: "message-1"}
	r := attempt.Record{Spec: attempt.Spec{ID: "schedule-attempt", Kind: attempt.KindChat, Project: "codex", Node: "laptop", Agent: "codex", TurnID: "message-1", By: "feishu-owner"}, State: attempt.Running, Session: "ns_schedule"}
	settled := false
	r.SessionSettled = &settled
	if change != nil {
		change(&work, &r)
	}
	if work.Parent != "" {
		parent, err := tasks.Create(task.Task{Channel: "chat", Member: "parent"})
		if err != nil {
			t.Fatal(err)
		}
		work.Parent = parent.ID
	}
	tracked, err := tasks.Create(work)
	if err != nil {
		t.Fatal(err)
	}
	tracked, err = tasks.BeginTurn(tracked.ID, tracked.Member, r.Node, task.TurnInput{Address: channel.Address{Channel: tracked.Transport, Conversation: tracked.Channel, Message: tracked.AnchorMessage}, ChatID: tracked.ChatID, ChatType: tracked.ChatType})
	if err != nil {
		t.Fatal(err)
	}
	r.TaskID = tracked.ID
	r.Execution = &task.ExecutionToken{TaskID: tracked.ID, Epoch: tracked.ExecutionEpoch}
	if _, err := book.Begin(t.Context(), r.ID, "attempt", string(r.State), "", r); err != nil {
		t.Fatal(err)
	}
	if _, err := c.projects.Bind(t.Context(), tracked.Channel, tracked.ProjectID, tracked.Requester); err != nil {
		t.Fatal(err)
	}
	jobs := c.schedules
	gate, err := agentmcp.New(0)
	if err != nil {
		t.Fatal(err)
	}
	if err := gate.SetStore(&scheduleGrantStore{data: map[string]json.RawMessage{}}, nil); err != nil {
		t.Fatal(err)
	}
	gate.SetScheduler(schedulesFor(t, c))
	gate.Extras("chat", "codex", "schedule-token", "")
	scope := agentmcp.GrantScope{TaskID: tracked.ID, TaskEpoch: tracked.ExecutionEpoch, AttemptID: r.ID, ExecutionGeneration: attempt.SessionExecutionEpoch(r), NodeID: r.Node, SessionID: r.Session}
	if err := gate.BindExecution(t.Context(), agentmcp.Binding{ConversationID: "chat", AgentID: "codex"}, scope); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- gate.Start(ctx) }()
	t.Cleanup(func() {
		cancel()
		if err := <-done; err != nil {
			t.Error(err)
		}
	})
	return scheduleMCPFixture{c: c, jobs: jobs, gate: gate, book: book, tracked: tracked, scope: scope}
}

// schedulesFor builds Schedules over c's stores and owners.
func schedulesFor(t *testing.T, c *Coordinator) *Schedules {
	t.Helper()
	s, err := NewSchedules(ScheduleDeps{
		Attempts: c.attempts, Tasks: c.tasks, Projects: c.projects, Schedules: c.schedules, Text: c.text,
		Owner: c.owners.baseline, ChannelOwners: c.owners.byChannel,
	})
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func scheduleToolCall(t *testing.T, gate *agentmcp.Server, tool string, args any) (string, bool) {
	t.Helper()
	raw, err := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": "tools/call", "params": map[string]any{"name": tool, "arguments": args}})
	if err != nil {
		t.Fatal(err)
	}
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, gate.URL(), strings.NewReader(string(raw)))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer schedule-token")
	response, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	var out struct {
		Result struct {
			IsError bool `json:"isError"`
			Content []struct {
				Text string `json:"text"`
			} `json:"content"`
		} `json:"result"`
	}
	if err := json.Unmarshal(body, &out); err != nil || response.StatusCode != http.StatusOK || len(out.Result.Content) != 1 {
		t.Fatalf("MCP response: %s (%v)", body, err)
	}
	return out.Result.Content[0].Text, out.Result.IsError
}

func createScheduleArgs() agentmcp.ScheduleRequest {
	return agentmcp.ScheduleRequest{Mode: "every", When: "1h", Prompt: "check build\n\n- preserve indentation\n  - details", IdempotencyKey: "user-request"}
}

func TestScheduleMCPCreatesDurableJobFromExecutionIdentity(t *testing.T) {
	f := newScheduleMCPFixture(t, nil)
	text, bad := scheduleToolCall(t, f.gate, "steve_schedule", createScheduleArgs())
	if bad {
		t.Fatalf("owner's native session was refused: %s", text)
	}
	jobs := f.jobs.List("chat")
	if len(jobs) != 1 {
		t.Fatalf("jobs: %+v", jobs)
	}
	job := jobs[0]
	if job.Channel != "feishu" || job.Requester != "feishu-owner" || job.Member != "codex" || job.ProjectID != "codex" || job.AnchorMessage != "message-1" || job.ChatID != "chat-native" || job.ChatType != string(protocol.ChatP2P) || job.Prompt != createScheduleArgs().Prompt || job.Spec.Every != time.Hour {
		t.Fatalf("trusted identity or instruction changed: %+v", job)
	}
	if _, bad := scheduleToolCall(t, f.gate, "steve_schedule", createScheduleArgs()); bad || len(f.jobs.List("chat")) != 1 {
		t.Fatal("create retry duplicated a schedule")
	}
	reopened, err := schedule.OpenLedger(f.book)
	if err != nil || len(reopened.List("chat")) != 1 {
		t.Fatalf("schedule was not durable: %v", err)
	}
}

func TestScheduleMCPRefusesGuestGroupChildAndMismatchedExecution(t *testing.T) {
	cases := map[string]func(*task.Task, *attempt.Record){
		"guest":               func(work *task.Task, r *attempt.Record) { work.Requester, r.By = "guest", "guest" },
		"guest-on-owner-task": func(_ *task.Task, r *attempt.Record) { r.By = "guest" },
		"group":               func(work *task.Task, _ *attempt.Record) { work.ChatType = string(protocol.ChatGroup) },
		"child":               func(work *task.Task, _ *attempt.Record) { work.Parent = "parent-task" },
		"wrong-project":       func(_ *task.Task, r *attempt.Record) { r.Project = "elsewhere" },
		"wrong-anchor":        func(_ *task.Task, r *attempt.Record) { r.TurnID = "different-message" },
		"unsettled":           func(_ *task.Task, r *attempt.Record) { r.Unsettled = true },
		"settled":             func(_ *task.Task, r *attempt.Record) { settled := true; r.SessionSettled = &settled },
		"not-running":         func(_ *task.Task, r *attempt.Record) { r.State = attempt.Bound },
	}
	for name, change := range cases {
		t.Run(name, func(t *testing.T) {
			f := newScheduleMCPFixture(t, change)
			if text, bad := scheduleToolCall(t, f.gate, "steve_schedule", createScheduleArgs()); !bad {
				t.Fatalf("accepted %s: %s", name, text)
			}
			if len(f.jobs.List("")) != 0 {
				t.Fatal("refused request mutated schedules")
			}
		})
	}
}

// A stored receipt is replayed only after the current caller is authorized
// again, so a retry after the conversation moved to another project is refused.
func TestScheduleMCPReplayRefusedAfterProjectRebinding(t *testing.T) {
	f := newScheduleMCPFixture(t, nil)
	if text, bad := scheduleToolCall(t, f.gate, "steve_schedule", createScheduleArgs()); bad {
		t.Fatalf("owner's native session was refused: %s", text)
	}
	if err := f.c.projects.Declare(t.Context(), []project.Project{{ID: "elsewhere", Home: project.Home{Node: "laptop", Path: t.TempDir()}}}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.c.projects.Bind(t.Context(), f.tracked.Channel, "elsewhere", f.tracked.Requester); err != nil {
		t.Fatal(err)
	}
	for tool, args := range map[string]any{"steve_schedule": createScheduleArgs(), "steve_schedules": map[string]any{}} {
		if text, bad := scheduleToolCall(t, f.gate, tool, args); !bad || !strings.Contains(text, "project binding changed") {
			t.Fatalf("%s after rebinding: %s", tool, text)
		}
	}
	if len(f.jobs.List("")) != 1 {
		t.Fatal("refused replay mutated schedules")
	}
}

func TestScheduleMCPScopesListCancelAndPreservesUnknownFiring(t *testing.T) {
	f := newScheduleMCPFixture(t, nil)
	var owned schedule.Job
	for _, route := range []struct{ conversation, project, transport string }{
		{"chat", "codex", "feishu"}, {"other-chat", "codex", "feishu"}, {"chat", "other-project", "feishu"}, {"chat", "codex", "console"},
	} {
		job, err := f.jobs.Create(schedule.Job{ConversationID: route.conversation, ProjectID: route.project, Channel: route.transport, Requester: "feishu-owner", Prompt: route.project, Spec: schedule.Spec{Kind: schedule.KindEvery, Every: time.Hour}})
		if err != nil {
			t.Fatal(err)
		}
		if owned.ID == "" {
			owned = job
			continue
		}
		if text, bad := scheduleToolCall(t, f.gate, "steve_schedule_cancel", map[string]string{"id": job.ID}); !bad {
			t.Fatalf("cancelled out-of-scope job: %s", text)
		}
	}
	text, bad := scheduleToolCall(t, f.gate, "steve_schedules", map[string]any{})
	var listing struct {
		Schedules []schedule.Job `json:"schedules"`
	}
	if err := json.Unmarshal([]byte(text), &listing); err != nil || bad || len(listing.Schedules) != 1 || listing.Schedules[0].ID != owned.ID {
		t.Fatalf("listing crossed identity: %s", text)
	}
	due, err := f.jobs.Due(owned.NextAt)
	if err != nil || len(due) != 1 {
		t.Fatalf("due: %+v %v", due, err)
	}
	if err := f.jobs.BeginFiring(due[0].Key); err != nil {
		t.Fatal(err)
	}
	if err := f.jobs.FailFiring(due[0].Key, fmt.Errorf("response lost"), true); err != nil {
		t.Fatal(err)
	}
	if text, bad := scheduleToolCall(t, f.gate, "steve_schedule_cancel", map[string]string{"id": owned.ID}); !bad || !strings.Contains(text, "unresolved") {
		t.Fatalf("cancel hid unknown firing: %s", text)
	}
}

func TestScheduledMCPWorkCanInspectButCannotCreate(t *testing.T) {
	f := newScheduleMCPFixture(t, func(work *task.Task, _ *attempt.Record) { work.Origin = "schedule:original" })
	if text, bad := scheduleToolCall(t, f.gate, "steve_schedules", map[string]any{}); bad {
		t.Fatalf("scheduled inspection refused: %s", text)
	}
	for _, mode := range []string{"at", "every"} {
		args := createScheduleArgs()
		args.Mode, args.IdempotencyKey = mode, mode
		if text, bad := scheduleToolCall(t, f.gate, "steve_schedule", args); !bad || !strings.Contains(text, "unattended") {
			t.Fatalf("scheduled recursion not refused: %s", text)
		}
	}
}

func TestScheduleGuidanceRefreshContinuesExistingNativeSession(t *testing.T) {
	gate, err := agentmcp.New(0)
	if err != nil {
		t.Fatal(err)
	}
	c, _ := taskCoordinator(t, &fakeRunner{reply: "ok"}, withCallbacks(func(cb *Callbacks) { cb.AgentGate = gate }))
	manager := c.runtime.(*fakeManager)
	manager.mcpHTTP = true
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- gate.Start(ctx) }()
	t.Cleanup(func() { cancel(); <-done })
	first, err := c.Handle(t.Context(), Request{ConversationID: "chat", Input: "hello"})
	if err != nil {
		t.Fatal(err)
	}
	before := c.store.Conversation("chat").Sessions["codex"]
	// Adding platform tools changes guidance, not the MCP connection or token.
	gate.SetScheduler(schedulesFor(t, c))
	second, err := c.Handle(t.Context(), Request{ConversationID: "chat", Input: "continue"})
	if err != nil {
		t.Fatalf("schedule upgrade broke the existing conversation: %v", err)
	}
	after := c.store.Conversation("chat").Sessions["codex"]
	if second.Injected.Session != first.Injected.Session || second.Injected.NewSession || !second.Injected.InstructionsSent || !strings.Contains(second.Injected.Instructions, "steve_schedule") || before.SessionConfigHash != after.SessionConfigHash || before.AgentToken != after.AgentToken || len(c.store.Conversation("chat").Archived) != 0 {
		t.Fatal("schedule guidance did not refresh within the existing session")
	}
	third, err := c.Handle(t.Context(), Request{ConversationID: "chat", Input: "continue again"})
	if err != nil || third.Injected.InstructionsSent {
		t.Fatalf("guidance was not stable: %+v %v", third, err)
	}
}

func TestNewSchedulesRefusesMissingDependenciesAndInvalidOwners(t *testing.T) {
	_, err := NewSchedules(ScheduleDeps{})
	if want := "turn: missing schedule dependencies: Attempts, Tasks, Projects, Schedules, Text"; err == nil || err.Error() != want {
		t.Fatalf("NewSchedules with nothing: %v, want %q", err, want)
	}
	var deps Deps
	fillDeps(t, testLedger(t), &deps)
	full := ScheduleDeps{Attempts: deps.Attempts, Tasks: deps.Tasks, Projects: deps.Projects, Schedules: deps.Schedules, Text: deps.Text, Owner: "owner", ChannelOwners: map[string]string{"feishu": "ou_owner"}}
	if _, err := NewSchedules(full); err != nil {
		t.Fatal(err)
	}
	full.ChannelOwners = map[string]string{"console": "owner"}
	if _, err := NewSchedules(full); err == nil {
		t.Fatal("NewSchedules accepted an owner for the console channel")
	}
}
