package agentmcp

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/gopact-ai/steve/internal/agent"
	"github.com/gopact-ai/steve/internal/capability"
	"github.com/gopact-ai/steve/internal/home"
	"github.com/gopact-ai/steve/internal/schedule"
)

type scheduleRecorder struct {
	mu       sync.Mutex
	creates  int
	binding  Binding
	request  ScheduleRequest
	onCreate func()
	authErr  error
}

func (r *scheduleRecorder) AuthorizeSchedules(context.Context, Binding, bool) error {
	return r.authErr
}
func (r *scheduleRecorder) Schedule(_ context.Context, b Binding, req ScheduleRequest) (schedule.Job, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.creates++
	r.binding, r.request = b, req
	if r.onCreate != nil {
		r.onCreate()
	}
	return schedule.Job{ID: "42", Prompt: req.Prompt}, nil
}
func (*scheduleRecorder) Schedules(context.Context, Binding) ([]schedule.Job, error) {
	return []schedule.Job{{ID: "42", State: schedule.FiringUnknown, Error: "response lost", PendingKey: "firing-42"}}, nil
}
func (*scheduleRecorder) CancelSchedule(_ context.Context, _ Binding, id string) (schedule.Job, error) {
	return schedule.Job{ID: id}, nil
}

func scheduleGate(t *testing.T, store *grantStore, recorder *scheduleRecorder) *Server {
	t.Helper()
	s, _ := grantFixture(t, store)
	s.SetScheduler(recorder)
	s.Extras("chat", "agent", "schedule-token", "")
	if err := s.BindExecution(t.Context(), Binding{ConversationID: "chat", AgentID: "agent"}, testScope()); err != nil {
		t.Fatal(err)
	}
	return s
}

func scheduleArguments() map[string]any {
	return map[string]any{"mode": "every", "when": "30m", "prompt": "check CI\n- report failures", "idempotency_key": "ci-request"}
}

func TestScheduleToolsAreOfferedAndRefreshOnlyPlatformFingerprint(t *testing.T) {
	s, _ := startServer(t)
	assembler := capability.NewAssembler(nil)
	before, err := assembler.AssembleExtra(agent.Agent{ID: "agent"}, home.ModeNone, s.DescribeExtras("token", ""))
	if err != nil {
		t.Fatal(err)
	}
	for _, tool := range s.toolList() {
		if strings.HasPrefix(tool["name"].(string), "steve_schedule") {
			t.Fatal("unwired scheduling was offered")
		}
	}
	s.SetScheduler(&scheduleRecorder{})
	after, err := assembler.AssembleExtra(agent.Agent{ID: "agent"}, home.ModeNone, s.DescribeExtras("token", ""))
	if err != nil {
		t.Fatal(err)
	}
	if before.Fingerprint == after.Fingerprint || before.SessionFingerprint != after.SessionFingerprint {
		t.Fatal("scheduling must refresh guidance without replacing the native session")
	}
	for _, name := range []string{"steve_schedule", "steve_schedules", "steve_schedule_cancel"} {
		found := false
		for _, tool := range s.toolList() {
			found = found || tool["name"] == name
		}
		if !found || !strings.Contains(after.Instructions, name) || ToolTitles()[name] == "" {
			t.Fatalf("%s missing from tools, instructions, or platform catalogue", name)
		}
	}
}

func TestScheduleCreationReplaysReceiptAcrossGateRestart(t *testing.T) {
	store := newGrantStore()
	store.allowed[testScope().AttemptID] = true
	recorder := &scheduleRecorder{}
	first := scheduleGate(t, store, recorder)
	text, bad := callTool(t, first.URL(), "schedule-token", "steve_schedule", scheduleArguments())
	if bad || !strings.Contains(text, `"42"`) {
		t.Fatalf("create: %s error=%v", text, bad)
	}
	fresh := scheduleGate(t, store, recorder)
	replayed, bad := callTool(t, fresh.URL(), "schedule-token", "steve_schedule", scheduleArguments())
	if bad || text != replayed || recorder.creates != 1 {
		t.Fatalf("retry created another job: %s, calls=%d", replayed, recorder.creates)
	}
	if recorder.binding.ConversationID != "chat" || recorder.binding.AgentID != "agent" {
		t.Fatalf("lost authenticated binding: %+v", recorder.binding)
	}
	recorder.authErr = errors.New("current caller is no longer authorized")
	if text, bad := callTool(t, fresh.URL(), "schedule-token", "steve_schedule", scheduleArguments()); !bad {
		t.Fatalf("receipt replay bypassed current authorization: %s", text)
	}
	recorder.authErr = nil
	changed := scheduleArguments()
	changed["prompt"] = "different operation"
	if text, bad := callTool(t, fresh.URL(), "schedule-token", "steve_schedule", changed); !bad || !strings.Contains(text, "different") {
		t.Fatalf("key reuse changed intent: %s", text)
	}
}

func TestScheduleCrashGapNeverAutomaticallyRecreates(t *testing.T) {
	store := newGrantStore()
	store.allowed[testScope().AttemptID] = true
	recorder := &scheduleRecorder{onCreate: func() {
		store.mu.Lock()
		store.failure = errors.New("receipt write failed")
		store.mu.Unlock()
	}}
	first := scheduleGate(t, store, recorder)
	if text, bad := callTool(t, first.URL(), "schedule-token", "steve_schedule", scheduleArguments()); !bad {
		t.Fatalf("lost receipt reported success: %s", text)
	}
	store.mu.Lock()
	store.failure = nil
	store.mu.Unlock()
	recorder.onCreate = nil
	fresh := scheduleGate(t, store, recorder)
	text, bad := callTool(t, fresh.URL(), "schedule-token", "steve_schedule", scheduleArguments())
	if !bad || !strings.Contains(text, "steve_schedules") || recorder.creates != 1 {
		t.Fatalf("unknown create was replayed: %s calls=%d", text, recorder.creates)
	}
}

func TestScheduleRejectsForgedIdentityDelegationAndInvalidInput(t *testing.T) {
	store := newGrantStore()
	store.allowed[testScope().AttemptID] = true
	recorder := &scheduleRecorder{}
	s := scheduleGate(t, store, recorder)
	for _, field := range []string{"channel", "project_id", "requester", "anchor_message", "agent", "conversation_id", "task_id"} {
		args := scheduleArguments()
		args[field] = "forged"
		if text, bad := callTool(t, s.URL(), "schedule-token", "steve_schedule", args); !bad {
			t.Fatalf("accepted forged %s: %s", field, text)
		}
	}
	for _, field := range []string{"mode", "when", "prompt", "idempotency_key"} {
		args := scheduleArguments()
		args[field] = ""
		if text, bad := callTool(t, s.URL(), "schedule-token", "steve_schedule", args); !bad {
			t.Fatalf("accepted empty %s: %s", field, text)
		}
	}
	child := Binding{ConversationID: "chat", AgentID: "child", TaskID: testScope().TaskID, DelegatedBy: "parent"}
	s.Delegated(child.ConversationID, child.AgentID, child.TaskID, child.DelegatedBy, "child-token", "")
	if err := s.BindExecution(t.Context(), child, testScope()); err != nil {
		t.Fatal(err)
	}
	for _, tool := range []string{"steve_schedule", "steve_schedules", "steve_schedule_cancel"} {
		if text, bad := callTool(t, s.URL(), "child-token", tool, scheduleArguments()); !bad {
			t.Fatalf("delegated child used %s: %s", tool, text)
		}
	}
	if recorder.creates != 0 {
		t.Fatal("refused requests reached the scheduler")
	}
}

func TestScheduleListIncludesUncertainFiringAndCancelUsesReturnedID(t *testing.T) {
	store := newGrantStore()
	store.allowed[testScope().AttemptID] = true
	s := scheduleGate(t, store, &scheduleRecorder{})
	text, bad := callTool(t, s.URL(), "schedule-token", "steve_schedules", map[string]any{})
	for _, want := range []string{"42", schedule.FiringUnknown, "response lost", "firing-42"} {
		if bad || !strings.Contains(text, want) {
			t.Fatalf("listing hid %s: %s", want, text)
		}
	}
	if text, bad := callTool(t, s.URL(), "schedule-token", "steve_schedule_cancel", map[string]any{"id": "42"}); bad || !strings.Contains(text, "42") {
		t.Fatalf("cancel: %s", text)
	}
}
