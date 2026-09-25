package readmodel

import (
	"encoding/json"
	"fmt"
	"math/rand"
	"reflect"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gopact-ai/steve/internal/consoleapi"
	"github.com/gopact-ai/steve/internal/task"
	"github.com/gopact-ai/steve/internal/view"
)

func TestDelegateProgressPreservesReasoningPlanAndModel(t *testing.T) {
	m := New(Sources{})
	events, stop := m.Subscribe(t.Context())
	defer stop()
	reasoning := "开头\n" + strings.Repeat("完整的思考摘要\n", 2000) + "\n[… 省略 123 字节 …]\n结尾"
	p := view.Progress{Reasoning: reasoning, Settings: view.Settings{Model: "reported-model"},
		Plan: []view.Step{{Text: "verify", Status: view.StepCompleted}}}
	m.DelegateProgress("59", "builder", "node-a", consoleapi.StepInfo{State: "done", Answer: "verified"}, p)
	ev := <-events
	if ev.Progress.Reasoning != reasoning || ev.Progress.Model != "reported-model" || ev.Progress.Agent != "builder" || ev.Progress.Node != "node-a" || len(ev.Progress.Plan) != 1 {
		t.Fatalf("delegate progress lost the agent's trace: %+v", ev.Progress)
	}
	if ev.Step == nil || ev.Step.ID != "#59" || ev.Step.Kind != "delegate" || ev.Step.Model != "reported-model" || ev.Step.Plan[0].Text != "verify" || ev.Step.Reasoning != reasoning || ev.Step.Answer != "verified" {
		t.Fatalf("step snapshot differs from progress: %+v", ev.Step)
	}
	data, err := json.Marshal(ev)
	if err != nil {
		t.Fatal(err)
	}
	var wire struct {
		Step struct{ ID, Kind, Model, Answer string }
	}
	if err := json.Unmarshal(data, &wire); err != nil || wire.Step.ID != "#59" || wire.Step.Kind != "delegate" || wire.Step.Model != "reported-model" || wire.Step.Answer != "verified" {
		t.Fatalf("step fields must stay flat on the wire: %s (%v)", data, err)
	}
}

// A stopped child's last word lands like a finished one's: the state it
// ends in is never held back by the throttle on its token stream.
func TestDelegateProgressPublishesAStoppedChildAtOnce(t *testing.T) {
	m := New(Sources{})
	events, stop := m.Subscribe(t.Context())
	defer stop()
	p := view.Progress{Tools: []view.Tool{{ID: "t1", Status: view.ToolRunning}}}
	m.DelegateProgress("60", "builder", "node-a", consoleapi.StepInfo{State: "running"}, p)
	<-events
	m.DelegateProgress("60", "builder", "node-a", consoleapi.StepInfo{State: "cancelled", Answer: "half done"}, p)
	select {
	case ev := <-events:
		if ev.Step == nil || ev.Step.State != "cancelled" || ev.Step.Answer != "half done" {
			t.Fatalf("stopped child published as %+v", ev.Step)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the stopped child's final state was throttled away")
	}
}

// A plan step's last update lands even when it follows the one before
// within the throttle window: nothing else publishes the step again.
func TestStepProgressPublishesTheLastUpdateTheThrottleHeldBack(t *testing.T) {
	m := New(Sources{})
	events, stop := m.Subscribe(t.Context())
	defer stop()
	for _, answer := range []string{"compiling", "compiling…", "compiling… done"} {
		m.StepProgress("task", "plan", "build", "builder", "node-a", view.Progress{Answer: answer})
	}
	deadline := time.After(10 * progressEvery)
	for {
		select {
		case ev := <-events:
			if ev.Kind != "step.progress" || ev.StepID != "build" {
				t.Fatalf("unexpected event %+v", ev)
			}
			if ev.Progress.Answer == "compiling… done" {
				return
			}
		case <-deadline:
			t.Fatal("the step's last update was throttled away")
		}
	}
}

// A step's conversation is read from the task store for the updates it
// publishes, not for each one the throttle merges into a later one.
func TestStepProgressReadsTheTaskOnlyForWhatItPublishes(t *testing.T) {
	m := New(Sources{})
	var reads atomic.Int64
	m.taskHeader = func(string) (task.Header, bool) {
		reads.Add(1)
		return task.Header{Task: task.Task{Channel: "console:test"}}, true
	}
	events, stop := m.Subscribe(t.Context())
	defer stop()
	const updates = 50
	for i := range updates {
		m.StepProgress("task", "plan", "build", "builder", "node-a", view.Progress{Answer: fmt.Sprint(i)})
	}
	published := 0
	deadline := time.After(10 * progressEvery)
	for last := false; !last; {
		select {
		case ev := <-events:
			if ev.Conversation != "console:test" {
				t.Fatalf("published %+v without its conversation", ev)
			}
			published++
			last = ev.Progress.Answer == fmt.Sprint(updates-1)
		case <-deadline:
			t.Fatal("the step's last update never landed")
		}
	}
	if n := reads.Load(); n > int64(published) {
		t.Fatalf("read the task %d times for %d updates published", n, published)
	}
}

// A held update released after its agent went on to another step still
// lands, but the agent's activity stays on the step it went on to.
func TestStepProgressReleaseLeavesAnAgentThatMovedOnWhereItIs(t *testing.T) {
	m := New(Sources{})
	events, stop := m.Subscribe(t.Context())
	defer stop()
	m.StepProgress("task", "plan", "first", "builder", "node-a", view.Progress{Answer: "working"})
	m.StepProgress("task", "plan", "first", "builder", "node-a", view.Progress{Answer: "done"})
	m.StepProgress("task", "plan", "second", "builder", "node-a", view.Progress{Answer: "starting"})
	deadline := time.After(10 * progressEvery)
	for landed := false; !landed; {
		select {
		case ev := <-events:
			landed = ev.StepID == "first" && ev.Progress.Answer == "done"
		case <-deadline:
			t.Fatal("the first step's last update never landed")
		}
	}
	m.mu.Lock()
	at := m.activity["builder"]
	m.mu.Unlock()
	if at.StepID != "second" {
		t.Fatalf("builder's activity is on step %q, want the step it went on to", at.StepID)
	}
}

// Several steps streaming at once, each now and then starting or
// finishing a tool call: each step's updates reach a subscriber in the
// order they were made, and each step's last update lands.
func TestStepProgressKeepsEachStepsUpdatesInOrder(t *testing.T) {
	m := New(Sources{})
	events, stop := m.Subscribe(t.Context())
	defer stop()
	const steps, updates = 8, 1500
	for k := range steps {
		go func() {
			r := rand.New(rand.NewSource(int64(k)))
			var tools []view.Tool
			for i := range updates {
				if r.Intn(300) == 0 {
					tools = make([]view.Tool, 1-len(tools))
				}
				m.StepProgress("task", "plan", fmt.Sprint(k), "builder", "node-a", view.Progress{Answer: strconv.Itoa(i), Tools: tools})
				time.Sleep(time.Duration(r.Intn(1000)) * time.Microsecond)
			}
		}()
	}
	next := map[string]int{}
	finished := 0
	deadline := time.After(time.Minute)
	for finished < steps {
		select {
		case ev, open := <-events:
			if !open {
				t.Fatal("the subscriber fell behind")
			}
			i, _ := strconv.Atoi(ev.Progress.Answer)
			if i < next[ev.StepID] {
				t.Fatalf("step %s published update %d after a later one", ev.StepID, i)
			}
			next[ev.StepID] = i + 1
			if i == updates-1 {
				finished++
			}
		case <-deadline:
			t.Fatalf("steps' last updates never all landed: %v", next)
		}
	}
}

func TestTimelineSurvivesProjectionWithEveryToolReference(t *testing.T) {
	at := time.Date(2026, 9, 5, 12, 0, 0, 0, time.UTC)
	p := view.Progress{Timeline: []view.Span{{Kind: "text", Text: "first", At: at}}}
	for i := range toolsKept + 5 {
		id := fmt.Sprintf("tool-%d", i)
		p.Tools = append(p.Tools, view.Tool{ID: id, Status: view.ToolCompleted})
		p.Timeline = append(p.Timeline, view.Span{Kind: "tool", Tool: id, At: at})
	}
	p.Timeline = append(p.Timeline, view.Span{Kind: "text", Text: "final", At: at})
	progress := FromProgress(p)
	step := FromStepProgress("#72", progress, consoleapi.StepInfo{Kind: "delegate", State: "done", Answer: "legacy answer"})
	raw, err := json.Marshal(step)
	if err != nil {
		t.Fatal(err)
	}
	var restored consoleapi.StepProcess
	if err := json.Unmarshal(raw, &restored); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(restored.Timeline, progress.Timeline) || len(restored.Tools) != len(p.Tools) || restored.Answer != "legacy answer" {
		t.Fatal("step snapshot lost timeline order, details, or legacy answer")
	}
	p.Timeline[0].Text = "mutated"
	if progress.Timeline[0].Text != "first" {
		t.Fatal("projection aliased the input timeline")
	}
	if restored.Timeline[len(restored.Timeline)-1].Text != "final" || !strings.Contains(string(raw), `"timeline":[{"kind":"text","text":"first","at":"2026-09-05T12:00:00Z"}`) {
		t.Fatalf("timeline wire shape = %s", raw)
	}
	p.Timeline = nil
	if legacy := FromProgress(p); legacy.Timeline != nil || len(legacy.Tools) != toolsKept {
		t.Fatal("legacy projection changed")
	}
}

func TestPlatformToolsAreRecognisedUnderAnyHarnessNaming(t *testing.T) {
	SetPlatformTools("steve", map[string]string{"steve_fleet": "查名册", "steve_delegate": "委派子任务", "channel_send": "发进度消息"})
	t.Cleanup(func() { SetPlatformTools("", nil) })
	p := FromProgress(view.Progress{Tools: []view.Tool{
		{ID: "1", Kind: "execute", Name: "mcp__steve__steve_fleet"},
		{ID: "2", Kind: "other", Name: "mcp.steve.steve_delegate"},
		{ID: "3", Kind: "execute", Name: "steve/channel_send"},
		{ID: "4", Kind: "execute", Name: "channel_send"},
		{ID: "5", Kind: "execute", Name: "mcp__other__steve_fleet"},
		{ID: "6", Kind: "read", Name: "Read file"},
	}})
	want := []struct{ kind, name, detail string }{
		{"platform", "steve_fleet", "查名册"}, {"platform", "steve_delegate", "委派子任务"}, {"platform", "channel_send", "发进度消息"},
		{"platform", "channel_send", "发进度消息"}, {"execute", "mcp__other__steve_fleet", ""}, {"read", "Read file", ""},
	}
	for i, w := range want {
		got := p.Tools[i]
		if got.Kind != w.kind || got.Name != w.name || got.Detail != w.detail {
			t.Fatalf("tool %d = %+v, want %+v", i, got, w)
		}
	}
}
