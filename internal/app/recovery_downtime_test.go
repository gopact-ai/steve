package app

import (
	"testing"
	"time"

	"github.com/gopact-ai/steve/internal/channel"
	"github.com/gopact-ai/steve/internal/task"
)

// The hub is down between the two processes. Recovery settles the row the
// dead process left open; the outage is not work the task did.
const recoveryDowntime = 200 * time.Millisecond

func TestStartupRecoveryDoesNotChargeDowntimeToInterruptedRow(t *testing.T) {
	for name, bound := range map[string]bool{"row bound to an attempt expired at startup": true, "unbound row": false} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			first := openCrashProbe(t, dir)
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
			if !row.EndedAt.Equal(row.StartedAt) {
				t.Errorf("interrupted row charged %s from %s to %s", row.EndedAt.Sub(row.StartedAt), row.StartedAt, row.EndedAt)
			}
			// Only the continuation's own turn ran; it took far less than the outage.
			if tracked.Budget.Elapsed >= recoveryDowntime {
				t.Errorf("task elapsed %s includes the %s outage", tracked.Budget.Elapsed, recoveryDowntime)
			}
		})
	}
}
