package turn

import (
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gopact-ai/steve/internal/schedule"
	"github.com/gopact-ai/steve/internal/task"
)

func scheduleCoordinator(t *testing.T) (*Coordinator, *schedule.Store) {
	t.Helper()
	coordinator, _ := taskCoordinator(t, &fakeRunner{reply: "ok"})
	store, err := schedule.Open(filepath.Join(t.TempDir(), "schedules.json"))
	if err != nil {
		t.Fatalf("open schedules: %v", err)
	}
	coordinator.SetSchedules(store)
	return coordinator, store
}

func schedRequest(input string) Request {
	return Request{
		ConversationID: "chat", Input: input,
		MessageID: "om_anchor", ChatID: "oc_chat", SenderOpenID: "ou_asker",
	}
}

// The stored prompt has to be exactly what the user would have typed, because
// that is what gets replayed: the schedule is a message, not a special mode.
func TestEveryStoresTheInstructionAndItsAnchor(t *testing.T) {
	coordinator, store := scheduleCoordinator(t)

	result, err := coordinator.Handle(t.Context(), schedRequest("/every 30m 看一眼 CI"))
	if err != nil {
		t.Fatalf("/every: %v", err)
	}
	if !strings.Contains(result.Text, "#1") || !strings.Contains(result.Text, "看一眼 CI") {
		t.Fatalf("card = %q; want the id and the instruction", result.Text)
	}
	jobs := store.List("chat")
	if len(jobs) != 1 {
		t.Fatalf("jobs = %d; want one", len(jobs))
	}
	job := jobs[0]
	if job.Prompt != "看一眼 CI" || job.Member != "codex" {
		t.Fatalf("job = %+v; want the bare instruction bound to codex", job)
	}
	if job.AnchorMessage != "om_anchor" || job.ChatID != "oc_chat" || job.Requester != "ou_asker" {
		t.Fatalf("job lost its anchor: %+v", job)
	}
	if job.Spec.Every != 30*time.Minute || job.NextAt.IsZero() {
		t.Fatalf("spec = %+v next=%s", job.Spec, job.NextAt)
	}
}

func TestScheduleListingAndCancel(t *testing.T) {
	coordinator, store := scheduleCoordinator(t)
	if _, err := coordinator.Handle(t.Context(), schedRequest("/every 09:00 汇报昨天进展")); err != nil {
		t.Fatalf("/every: %v", err)
	}
	if _, err := coordinator.Handle(t.Context(), schedRequest("/at 30m 回来看结果")); err != nil {
		t.Fatalf("/at: %v", err)
	}

	listing, err := coordinator.Handle(t.Context(), schedRequest("/schedules"))
	if err != nil {
		t.Fatalf("/schedules: %v", err)
	}
	for _, want := range []string{"#1", "#2", "汇报昨天进展", "回来看结果"} {
		if !strings.Contains(listing.Text, want) {
			t.Fatalf("listing = %q; want %q", listing.Text, want)
		}
	}

	// A schedule belongs to its conversation, like a task does.
	elsewhere, err := coordinator.Handle(t.Context(), Request{ConversationID: "other", Input: "/schedules cancel 1"})
	if err != nil {
		t.Fatalf("cross-conversation cancel: %v", err)
	}
	if len(store.List("chat")) != 2 {
		t.Fatalf("another conversation cancelled a schedule it cannot see: %q", elsewhere.Text)
	}

	cancelled, err := coordinator.Handle(t.Context(), schedRequest("/schedules cancel 1"))
	if err != nil {
		t.Fatalf("cancel: %v", err)
	}
	if !strings.Contains(cancelled.Text, "#1") {
		t.Fatalf("cancel card = %q; want it to name the schedule", cancelled.Text)
	}
	if jobs := store.List("chat"); len(jobs) != 1 || jobs[0].ID != "2" {
		t.Fatalf("remaining = %+v; want only #2", jobs)
	}
}

func TestUnreadableScheduleIsRefusedNotGuessed(t *testing.T) {
	coordinator, store := scheduleCoordinator(t)
	result, err := coordinator.Handle(t.Context(), schedRequest("/every 昨天 看一眼 CI"))
	if err != nil {
		t.Fatalf("/every: %v", err)
	}
	if !strings.Contains(result.Text, "昨天") {
		t.Fatalf("card = %q; want it to quote what it could not read", result.Text)
	}
	if len(store.List("chat")) != 0 {
		t.Fatal("an unreadable schedule was stored anyway")
	}
}

// A schedule that fires for weeks must not spend one task's budget a turn at
// a time — and must not touch work the user has since started themselves.
func TestRotateTaskOnlyClosesUnattendedWork(t *testing.T) {
	coordinator, tasks := taskCoordinator(t, &fakeRunner{reply: "ok"})

	if _, err := coordinator.Handle(t.Context(), Request{
		ConversationID: "chat", Input: "nightly report", Origin: "schedule:1",
	}); err != nil {
		t.Fatalf("scheduled turn: %v", err)
	}
	if tracked := tasks.List("chat")[0]; tracked.Origin != "schedule:1" {
		t.Fatalf("origin = %q; want the schedule that opened it", tracked.Origin)
	}

	// A different schedule's rotation leaves this one alone.
	coordinator.RotateTask("chat", "codex", "schedule:2")
	if tracked, _ := tasks.Get("1"); tracked.State.Terminal() {
		t.Fatal("another schedule's rotation closed this task")
	}
	coordinator.RotateTask("chat", "codex", "schedule:1")
	if tracked, _ := tasks.Get("1"); tracked.State != task.StateDone {
		t.Fatalf("state = %q; want the previous run closed", tracked.State)
	}

	// The next firing opens a fresh task with a fresh budget.
	if _, err := coordinator.Handle(t.Context(), Request{
		ConversationID: "chat", Input: "nightly report", Origin: "schedule:1",
	}); err != nil {
		t.Fatalf("second scheduled turn: %v", err)
	}
	all := tasks.List("chat")
	if len(all) != 2 {
		t.Fatalf("tasks = %d; want a new one per run", len(all))
	}
	for _, tracked := range all {
		if tracked.Budget.Turns != 1 {
			t.Fatalf("task #%s spent %d turns; want one per run", tracked.ID, tracked.Budget.Turns)
		}
	}

	// Work the user started themselves is a separate lineage: it neither
	// joins the schedule's task nor gets closed when that one rotates.
	if _, err := handle(coordinator, t.Context(), "my own thing"); err != nil {
		t.Fatalf("user turn: %v", err)
	}
	mine, ok := tasks.Active("chat", "codex", "")
	if !ok {
		t.Fatal("the user's own message joined the schedule's task instead of opening one")
	}
	if mine.Goal != "my own thing" || mine.Budget.Turns != 1 {
		t.Fatalf("user task = %+v; want a fresh task charged one turn", mine)
	}
	coordinator.RotateTask("chat", "codex", "schedule:1")
	if refreshed, _ := tasks.Get(mine.ID); refreshed.State.Terminal() {
		t.Fatalf("rotation closed the user's own task: %+v", refreshed)
	}
}
