package app

import (
	"testing"
	"time"

	"github.com/gopact-ai/steve/internal/channel"
	"github.com/gopact-ai/steve/internal/task"
)

// The hub is down between the two processes. Recovery settles the row the
// dead process left open; the outage is not work the task did.
const recoveryDowntime = time.Second

// The bound execution ran this long before the process died: its session
// settled, then the hub stopped before binding the result. That time is
// work, and recovery must keep charging it.
const recoveryWorked = 100 * time.Millisecond

func TestStartupRecoveryDoesNotChargeDowntimeToInterruptedRow(t *testing.T) {
	for name, bound := range map[string]bool{"row bound to an attempt expired at startup": true, "unbound row": false} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			began := time.Now()
			first := openCrashProbe(t, dir)
			first.worked = recoveryWorked
			var taskID string
			if bound {
				taskID = first.seed(t, false, "").TaskID
			} else {
				tracked, err := first.tasks.Create(task.Task{Transport: "console", Channel: crashConversation, Member: "worker", Requester: "owner", Goal: "original goal"})
				if err != nil {
					t.Fatal(err)
				}
				if _, err := first.tasks.BeginTurn(tracked.ID, "worker", "node-a", task.TurnInput{Address: channel.Address{Channel: "console", Conversation: crashConversation, Message: "web-original"}, ChatID: "console", ChatType: "p2p"}); err != nil {
					t.Fatal(err)
				}
				taskID = tracked.ID
			}
			first.close(t)
			downFrom := time.Now()
			time.Sleep(recoveryDowntime)
			downUntil := time.Now()

			recovered := openCrashProbe(t, dir)
			defer recovered.close(t)
			if err := recovered.assemble(t); err != nil {
				t.Fatal(err)
			}
			recovered.waitTerminal(t)
			finished := time.Now()
			tracked, _ := recovered.tasks.Get(taskID)
			row := tracked.Attempts[0]
			if row.Open() || row.Outcome != task.OutcomeInterrupted {
				t.Fatalf("interrupted row = %+v", row)
			}
			charged := row.EndedAt.Sub(row.StartedAt)
			if bound {
				// The row ends at the last activity the dead process
				// persisted — its session settling — not when it began, and
				// not when this process noticed it after the outage.
				if charged < recoveryWorked || row.EndedAt.After(downFrom) {
					t.Errorf("interrupted row charged %s from %s to %s; want at least the %s it ran, ending before the outage began at %s", charged, row.StartedAt, row.EndedAt, recoveryWorked, downFrom)
				}
			} else if charged != 0 {
				// A row that never reached an admitted execution ran nothing.
				t.Errorf("unstarted row charged %s from %s to %s", charged, row.StartedAt, row.EndedAt)
			}
			// Only the work before the crash and the continuation's own turn
			// ran; neither overlaps the outage.
			if running := finished.Sub(began) - downUntil.Sub(downFrom); tracked.Budget.Elapsed > running {
				t.Errorf("task elapsed %s exceeds the %s both processes ran; it includes the outage", tracked.Budget.Elapsed, running)
			}
		})
	}
}
