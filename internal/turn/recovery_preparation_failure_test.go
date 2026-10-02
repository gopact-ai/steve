package turn

import (
	"errors"
	"testing"

	"github.com/gopact-ai/steve/internal/agent"
	"github.com/gopact-ai/steve/internal/attempt"
	"github.com/gopact-ai/steve/internal/project"
)

func TestRecoveryReservationBeforeNativePreparationCanBeReleasedWithoutChangingFiles(t *testing.T) {
	c, p, source, ws := sharedCopy(t)
	r, err := c.attempts.Open(t.Context(), recoveryCopySpec(t, c, p, ws, "never-prepared"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.attempts.FailWith(t.Context(), r.ID, "fixture", "admission refused", nil); err != nil {
		t.Fatal(err)
	}
	_, err = c.workspaceFor(t.Context(), Request{ConversationID: "console:retry", SenderOpenID: "owner"}, agent.Agent{ID: "worker", Node: "node", Harness: "mock"}, project.Binding{ProjectID: p.ID})
	if err != nil {
		t.Fatalf("an unprepared failed input permanently held the copy: %v", err)
	}
	episode, err := c.attempts.WorkspaceRecovery(t.Context(), source.Abandoned.WorkspaceRecoveryID)
	if err != nil || episode.Producer != nil || episode.Head.Version != 1 {
		t.Fatalf("releasing unused reservation changed accepted work: %+v %v", episode, err)
	}
}

func TestRecoveryPreparationAndLostOpenKeepTheirMonotonicWriteObligation(t *testing.T) {
	for _, arm := range []bool{false, true} {
		t.Run(map[bool]string{false: "prepared", true: "open may have started"}[arm], func(t *testing.T) {
			c, p, source, ws := sharedCopy(t)
			r, err := c.attempts.Open(t.Context(), recoveryCopySpec(t, c, p, ws, "prepared-input"))
			if err != nil {
				t.Fatal(err)
			}
			if _, err := c.attempts.Advance(t.Context(), r.ID, attempt.Prepared, "fixture", nil); err != nil {
				t.Fatal(err)
			}
			if arm {
				if err := c.attempts.ArmSession(t.Context(), r.ID, "fixture"); err != nil {
					t.Fatal(err)
				}
				if err := c.attempts.MarkSessionSettled(t.Context(), r.ID, "fixture"); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := c.attempts.FailWith(t.Context(), r.ID, "fixture", "preparation failed after authorization", nil); err != nil {
				t.Fatal(err)
			}
			_, err = c.workspaceFor(t.Context(), Request{ConversationID: "console:retry", SenderOpenID: "owner"}, agent.Agent{ID: "worker", Node: "node", Harness: "mock"}, project.Binding{ProjectID: p.ID})
			if !errors.Is(err, attempt.ErrWorkspaceRecovery) {
				t.Fatalf("possible native write without output lost its fence: %v", err)
			}
			episode, err := c.attempts.WorkspaceRecovery(t.Context(), source.Abandoned.WorkspaceRecoveryID)
			if err != nil || episode.Producer == nil || episode.Producer.NativeMayWrite == nil || !*episode.Producer.NativeMayWrite {
				t.Fatalf("preparation was not durably marked before external work: %+v %v", episode, err)
			}
		})
	}
}
