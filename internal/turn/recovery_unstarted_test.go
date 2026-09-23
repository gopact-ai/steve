package turn

import (
	"errors"
	"testing"

	"github.com/gopact-ai/steve/internal/attempt"
	"github.com/gopact-ai/steve/internal/project"
	"github.com/gopact-ai/steve/internal/task"
)

// An attempt that failed after admission but before it opened a native
// session has nothing retained on any node. Recovering it delivers the
// failure it recorded and settles its accounting row at the attempt's end,
// instead of asking forever for a native session identity it never had.
func TestRetainedChatDeliversTheFailureOfAnAttemptThatNeverOpenedASession(t *testing.T) {
	coordinator, tasks, _ := taskCoordinatorBook(t, &fakeRunner{reply: "ok"})
	tracked, err := tasks.Create(task.Task{Goal: "original work", Channel: "chat", Member: "codex", Requester: "owner"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tasks.Begin(tracked.ID, "codex", "laptop", ""); err != nil {
		t.Fatal(err)
	}
	token, err := tasks.ExecutionToken(tracked.ID)
	if err != nil {
		t.Fatal(err)
	}
	record, err := coordinator.attempts.Open(t.Context(), attempt.Spec{ID: "att-unstarted", TaskID: tracked.ID, TurnID: "om_1", Kind: attempt.KindChat,
		Project: "p", Node: "laptop", Harness: "codex", Agent: "codex", Execution: &token, Scope: attempt.ScopeUnrestricted,
		Workspace: project.Workspace{ID: "workspace", Project: "p", Node: "laptop", Path: t.TempDir(), Kind: project.KindCanonical}})
	if err != nil {
		t.Fatal(err)
	}
	if err := tasks.BindAttempt(token, record.ID, record.TurnID); err != nil {
		t.Fatal(err)
	}
	const cause = "before-snapshot: content has insufficient durable replicas"
	failed, err := coordinator.attempts.Fail(t.Context(), record.ID, "test", cause)
	if err != nil {
		t.Fatal(err)
	}
	if failed.Session != "" || failed.Unsettled {
		t.Fatalf("fixture = %+v; want a settled failure without a session", failed)
	}
	result, err := coordinator.ResumeRetainedChat(t.Context(), record.ID, Request{ConversationID: "chat", MessageID: "om_1", SenderOpenID: "owner"})
	var blocked *RecoveryBlocked
	if errors.As(err, &blocked) || err == nil || err.Error() != cause || result.Attempt != record.ID {
		t.Fatalf("recovery = %+v, %v; want the recorded failure delivered", result, err)
	}
	got, _ := tasks.Get(tracked.ID)
	if len(got.Attempts) != 1 || got.Attempts[0].Open() || got.Attempts[0].Outcome != task.OutcomeError || !got.Attempts[0].EndedAt.Equal(failed.EndedAt) {
		t.Fatalf("accounting = %+v; want the row settled as an error at %s", got.Attempts, failed.EndedAt)
	}
}
