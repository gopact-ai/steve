package turn

import (
	"strings"
	"testing"

	"github.com/gopact-ai/steve/internal/task"
)

// The card offers a failed task two ways out that are not cancellation: the
// user dealt with it, or it does not matter. Both have to be reachable by
// hand, in either language, and both have to be reversible.
func TestSettlingAFailedTaskFromTheCardAndTakingItBack(t *testing.T) {
	t.Parallel()
	for _, spoken := range []struct {
		input string
		want  task.Settlement
	}{
		{"/tasks handled 1", task.SettlementHandled},
		{"/tasks 已处理 1", task.SettlementHandled},
		{"/tasks ignore 1", task.SettlementIgnored},
		{"/tasks 无需关注 1", task.SettlementIgnored},
	} {
		t.Run(spoken.input, func(t *testing.T) {
			coordinator, tasks := taskCoordinator(t, &fakeRunner{reply: "ok"})
			if _, err := handle(coordinator, t.Context(), "a job that will fail"); err != nil {
				t.Fatal(err)
			}
			if _, err := tasks.Advance("1", task.StateFailed); err != nil {
				t.Fatal(err)
			}
			if text := tasksCmdText(t, coordinator, spoken.input); !strings.Contains(text, "#1") {
				t.Fatalf("settle card = %q; want it to name the task", text)
			}
			settled, _ := tasks.Get("1")
			if settled.Settlement != spoken.want || settled.State != task.StateFailed {
				t.Fatalf("settled = %+v; want %s recorded beside the failure", settled, spoken.want)
			}
			if text := tasksCmdText(t, coordinator, "/tasks reopen 1"); !strings.Contains(text, "#1") {
				t.Fatalf("reopen card = %q", text)
			}
			if reopened, _ := tasks.Get("1"); reopened.Settled() {
				t.Fatalf("reopen left the decision in place: %+v", reopened)
			}
		})
	}
}

// Work that is still in play is paused or cancelled, never settled: the card
// says so rather than quietly closing something that is running.
func TestSettlingRefusesWorkThatHasNotFailed(t *testing.T) {
	coordinator, tasks := taskCoordinator(t, &fakeRunner{reply: "ok"})
	if _, err := handle(coordinator, t.Context(), "still going"); err != nil {
		t.Fatal(err)
	}
	text := tasksCmdText(t, coordinator, "/tasks handled 1")
	if !strings.Contains(text, "#1") {
		t.Fatalf("refusal = %q; want it to name the task", text)
	}
	if tracked, _ := tasks.Get("1"); tracked.Settled() {
		t.Fatal("running work was settled")
	}
}
