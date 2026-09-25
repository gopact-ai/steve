package gateway

import (
	"errors"
	"io"
	"testing"

	"github.com/gopact-ai/steve/internal/channel"
	"github.com/gopact-ai/steve/internal/ledger"
	"github.com/gopact-ai/steve/internal/task"
)

func TestTaskResumeReportsRefusalWithoutDispatch(t *testing.T) {
	for _, scenario := range []string{"unavailable", "missing-anchor", "revive-failed", "notice-failed", "notice-uncertain", "missing-receipt"} {
		t.Run(scenario, func(t *testing.T) {
			book, err := ledger.Open(t.TempDir(), ledger.Options{})
			if err != nil {
				t.Fatal(err)
			}
			defer book.Close()
			tasks, err := task.OpenLedger(book)
			if err != nil {
				t.Fatal(err)
			}
			row, err := tasks.Create(task.Task{Transport: "feishu", Channel: "opaque", Member: "worker"})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := tasks.SetAside(row.ID, task.StatePaused); err != nil {
				t.Fatal(err)
			}
			row, _ = tasks.Get(row.ID)
			a := task.ResumeAdmission{ID: "control", TaskID: row.ID, Epoch: row.ExecutionEpoch + 1}
			processor := &countingProcessor{}
			g := New(processor)
			notice := &scheduledNotice{id: "resume-notice"}
			if scenario != "unavailable" {
				g.BindChannel(notice)
			}
			r := Revival{TaskID: row.ID, Member: "worker", ConversationID: "opaque", MessageID: "anchor", Requester: "owner", Manual: true}
			failure := errors.New("refused")
			switch scenario {
			case "missing-anchor":
				r.MessageID = ""
			case "notice-failed":
				notice.err = failure
			case "notice-uncertain":
				notice.err = io.EOF
			case "missing-receipt":
				notice.id = ""
			}
			err = g.QueueTaskResume(t.Context(), book, a.ID, r, a)
			if scenario == "unavailable" || scenario == "missing-anchor" {
				if err == nil || processor.calls.Load() != 0 {
					t.Fatalf("refused acceptance dispatched: calls=%d err=%v", processor.calls.Load(), err)
				}
				return
			}
			if err != nil || processor.calls.Load() != 0 {
				t.Fatalf("dormant acceptance: %v", err)
			}
			if _, err := tasks.Resume(row.ID, row.ExecutionEpoch, row.State, a); err != nil {
				t.Fatal(err)
			}
			err = g.recoverQueuedFixture(t.Context(), book, &recoveryProbe{}, func(string, string) error {
				if scenario == "revive-failed" {
					return failure
				}
				return nil
			})
			if err == nil || processor.calls.Load() != 0 {
				t.Fatalf("refused resume dispatched or lost its error: calls=%d err=%v", processor.calls.Load(), err)
			}
			if (scenario == "revive-failed" || scenario == "notice-failed") && !errors.Is(err, failure) {
				t.Fatalf("lost refusal cause: %v", err)
			}
			// Once an external command has been reserved, an error is retained
			// as unknown, not permission to blindly send the notice again.
			unknown := scenario == "notice-failed" || scenario == "notice-uncertain" || scenario == "missing-receipt"
			if errors.Is(err, channel.ErrOutcomeUnknown) != unknown {
				t.Fatalf("lost notice uncertainty: %v", err)
			}
		})
	}
}
