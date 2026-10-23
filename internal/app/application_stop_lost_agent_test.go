//go:build linux || darwin

package app

import (
	"context"
	"database/sql"
	"errors"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/gopact-ai/steve/internal/execution"
	"github.com/gopact-ai/steve/internal/harness"
	"github.com/gopact-ai/steve/internal/nodewire"
	"github.com/gopact-ai/steve/internal/view"
)

// An agent that dies while its node's session journal refuses a write
// leaves that session latched failed: every later request, a stop
// included, used to be answered with the persistence failure, so the
// cancelled attempt stayed running with an unconfirmed stop and every
// restart waited on it. The node can still prove the exit from the process
// group it recorded, so cancel and force-stop both settle the execution.
func TestApplicationStopConfirmsAgentLostBehindFailedJournal(t *testing.T) {
	bin := filepath.Join(t.TempDir(), "mockagent")
	if output, err := exec.Command("go", "build", "-o", bin, "github.com/gopact-ai/steve/cmd/mockagent").CombinedOutput(); err != nil {
		t.Fatalf("build isolated ACP peer: %v %s", err, output)
	}
	// In the state-refused modes the journal also refuses the state change
	// after the recorded stop, as one at its byte bound does: the node's
	// receipt then reports the original command still running, with its
	// process stopped. The exit is what the stop needs proved, so that
	// receipt confirms it too.
	for _, mode := range []string{"cancel", "force-stop", "cancel-state-refused", "force-stop-state-refused"} {
		t.Run(mode, func(t *testing.T) {
			refused := strings.HasSuffix(mode, "-state-refused")
			f := newStopRegistryFixtureBefore(t, bin, false, func(f *stopRegistryFixture) {
				loseAgentBehindFailedJournal(t, f, refused)
			})
			f.owner.Finish(&execution.RetainedObserverDetached{AttemptID: f.record.ID, NodeID: f.record.Node, SessionID: f.record.Session, Cause: harness.ErrStopUnconfirmed})
			f.requireBusy(t)
			if strings.HasPrefix(mode, "force-stop") {
				if _, err := f.attempts.RequestForceStop(f.ctx, f.record.ID, "owner"); err != nil {
					t.Fatal(err)
				}
			}
			if err := f.stops.Reconcile(f.ctx); err != nil {
				t.Fatal(err)
			}
			stopped, err := f.attempts.Get(f.ctx, f.record.ID)
			if err != nil {
				t.Fatal(err)
			}
			if !stopped.State.Terminal() || stopped.Unsettled || stopped.StopEvidence != "task-stop/"+stopped.ID {
				t.Fatalf("stop of a lost agent stayed unconfirmed: state=%s unsettled=%v error=%q", stopped.State, stopped.Unsettled, stopped.Error)
			}
			if strings.HasPrefix(mode, "force-stop") && (stopped.ForceStop == nil || stopped.ForceStop.Level != "confirmed") {
				t.Fatalf("force stop of a lost agent did not complete: %+v", stopped.ForceStop)
			}
			receipt, ok, err := f.attempts.TaskStopReceipt(f.ctx, stopped.ID)
			if err != nil || !ok || !receipt.Evidence.Session.ProcessStopped {
				t.Fatalf("stop was confirmed without the node's exit evidence: %+v %v", receipt, err)
			}
			if command := receipt.Evidence.Session.Command; refused && (command == nil || command.State != nodewire.SessionCommandRunning || command.Settled) {
				t.Fatalf("fixture did not leave the original command running in the receipt: %+v", command)
			}
			// SealIdle is the gate a service restart waits on.
			f.requireResolved(t)
		})
	}
}

// loseAgentBehindFailedJournal starts the original prompt, makes the node's
// journal refuse question writes as a full journal does, and kills the
// agent's process group while its question is pending.
func loseAgentBehindFailedJournal(t *testing.T, f *stopRegistryFixture, refuseState bool) {
	t.Helper()
	observer, detach := context.WithCancel(harness.WithNodeSession(f.ctx, f.binding))
	defer detach()
	asked, ended := make(chan struct{}), make(chan error, 1)
	go func() {
		_, _, err := f.runner.(harness.TurnRunner).PromptTurn(observer, "askme", nil, nil, func(ctx context.Context, _ view.Question) (view.Answer, error) {
			close(asked)
			<-ctx.Done()
			return view.Answer{}, ctx.Err()
		}, nil)
		ended <- err
	}()
	select {
	case <-asked:
	case <-f.ctx.Done():
		t.Fatal("original prompt did not reach its question")
	}
	journal, err := sql.Open("sqlite", "file:"+filepath.Join(f.nodeState, "node-sessions", "sessions.db")+"?_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatal(err)
	}
	defer journal.Close()
	var group int
	if err := journal.QueryRow(`SELECT json_extract(header,'$.process.group') FROM sessions WHERE id=?`, f.record.Session).Scan(&group); err != nil || group <= 0 {
		t.Fatalf("agent process group was not recorded: %d %v", group, err)
	}
	if _, err := journal.Exec(`CREATE TRIGGER journal_full BEFORE UPDATE ON session_questions BEGIN SELECT RAISE(ABORT, 'question journal is full'); END`); err != nil {
		t.Fatal(err)
	}
	if refuseState {
		if _, err := journal.Exec(`CREATE TRIGGER journal_bound BEFORE UPDATE OF command ON session_commands WHEN json_extract(NEW.command,'$.state') = 'uncertain' BEGIN SELECT RAISE(ABORT, 'CHECK constraint failed: retained_bytes'); END`); err != nil {
			t.Fatal(err)
		}
	}
	if err := syscall.Kill(-group, syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	for deadline := time.Now().Add(10 * time.Second); syscall.Kill(-group, 0) == nil; time.Sleep(10 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("killed agent group did not exit")
		}
	}
	select {
	case err := <-ended:
		if err == nil {
			t.Fatal("prompt of a killed agent completed")
		}
	case <-time.After(5 * time.Second):
		detach()
		if err := <-ended; !errors.Is(err, harness.ErrStopUnconfirmed) {
			t.Fatalf("detached observer fabricated an outcome: %v", err)
		}
	}
	var stopped bool
	if err := journal.QueryRow(`SELECT coalesce(json_extract(header,'$.state.process_stopped'),0) FROM sessions WHERE id=?`, f.record.Session).Scan(&stopped); err != nil {
		t.Fatal(err)
	}
	if stopped {
		t.Fatal("fixture did not reproduce an unrecorded exit")
	}
}
