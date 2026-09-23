package app

import (
	"testing"
	"time"

	"github.com/gopact-ai/steve/internal/channel"
	"github.com/gopact-ai/steve/internal/task"
)

// The hub is down between the two processes. Recovery settles the row the
// dead process left open; the outage is not work the task did.
const recoveryDowntime = 300 * time.Millisecond

// The bound execution ran this long before the process died: its session
// settled, then the hub stopped before binding the result. That time is
// work, and recovery must keep charging it.
const recoveryWorked = 100 * time.Millisecond

func TestStartupRecoveryDoesNotChargeDowntimeToInterruptedRow(t *testing.T) {
	for name, bound := range map[string]bool{"row bound to an attempt expired at startup": true, "unbound row": false} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
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
			time.Sleep(recoveryDowntime)

			recovered := openCrashProbe(t, dir)
			defer recovered.close(t)
			if err := recovered.assemble(t); err != nil {
				t.Fatal(err)
			}
			recovered.waitTerminal(t)
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
				if charged < recoveryWorked || charged >= recoveryWorked+recoveryDowntime {
					t.Errorf("interrupted row charged %s from %s to %s; want the %s it ran, without the %s outage", charged, row.StartedAt, row.EndedAt, recoveryWorked, recoveryDowntime)
				}
			} else if charged != 0 {
				// A row that never reached an admitted execution ran nothing.
				t.Errorf("unstarted row charged %s from %s to %s", charged, row.StartedAt, row.EndedAt)
			}
			// Only the work before the crash and the continuation's own turn
			// ran; together they took far less than the outage.
			if tracked.Budget.Elapsed >= recoveryWorked+recoveryDowntime {
				t.Errorf("task elapsed %s includes the %s outage", tracked.Budget.Elapsed, recoveryDowntime)
			}
		})
	}
}
