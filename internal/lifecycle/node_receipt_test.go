package lifecycle

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/gopact-ai/steve/internal/attempt"
	"github.com/gopact-ai/steve/internal/harness"
	"github.com/gopact-ai/steve/internal/ledger"
	"github.com/gopact-ai/steve/internal/nodewire"
	"github.com/gopact-ai/steve/internal/task"
)

type receiptRunner struct {
	harness.Runner
	receipt nodewire.SessionReceipt
}

func (r receiptRunner) NodeReceipt(context.Context) (nodewire.SessionReceipt, bool) {
	return r.receipt, true
}

type receiptAttempts struct {
	*fakeAttempts
	completion attempt.Completion
}

func (r *receiptAttempts) Complete(_ context.Context, _ string, _ string, c attempt.Completion) (attempt.Record, error) {
	r.completion = c
	return attempt.Record{}, nil
}

func (r *receiptAttempts) FinishCompletion(ctx context.Context, id, actor string, c attempt.Completion) (attempt.Record, error) {
	return r.Complete(ctx, id, actor, c)
}

func (r *receiptAttempts) RejectCompletion(_ context.Context, _, _ string, c attempt.Completion, cause error) error {
	r.completion = c
	return cause
}

func TestLifecycleReceiptOnlyComesFromManagedSession(t *testing.T) {
	original := nodewire.SessionReceipt{Version: 1, SessionID: "native", ContextID: "context", CommandID: "command", InputSequence: 1,
		Binding: nodewire.SessionBinding{NodeID: "node", TaskID: "task", SessionID: "logical", AttemptID: "attempt", TaskEpoch: 1, ExecutionEpoch: 1},
		Digest:  strings.Repeat("a", 64)}
	for _, mode := range []string{"complete", "finish", "reject", "failure"} {
		for _, trusted := range []bool{false, true} {
			t.Run(mode+map[bool]string{false: "/no-source", true: "/authenticated-source"}[trusted], func(t *testing.T) {
				store := &receiptAttempts{fakeAttempts: &fakeAttempts{record: attempt.Record{Spec: attempt.Spec{ID: "attempt", Kind: attempt.KindChat}}}}
				e := &Execution{o: Options{Attempts: store, Settlement: Settlement{CommitAsGiven: mode == "complete"}},
					Record: store.record, stopBeat: func() {}, Managed: trusted, driven: true, Outcome: Outcome{PromptSettled: true}}
				if trusted {
					e.Session = receiptRunner{receipt: original}
				}
				forged := original
				forged.Digest = strings.Repeat("b", 64)
				completion := attempt.Completion{NodeReceipt: &forged}
				switch mode {
				case "complete", "finish":
					if _, err := e.commit(t.Context(), completion); err != nil {
						t.Fatal(err)
					}
				case "reject":
					_ = e.reject(t.Context(), completion, errors.New("refused"))
				case "failure":
					failed, err := e.failure(t.Context(), errors.New("native failed"))
					if err != nil {
						t.Fatal(err)
					}
					store.completion.NodeReceipt = failed.NodeReceipt
				}
				got := store.completion.NodeReceipt
				if trusted && (got == nil || *got != original) {
					t.Fatalf("terminal transition omitted or replaced authenticated source: %+v", got)
				}
				if !trusted && got != nil {
					t.Fatalf("caller supplied receipt became durable authority: %+v", got)
				}
			})
		}
	}
}

type receiptLifecycleRunner struct {
	*runner
	receipt nodewire.SessionReceipt
	reads   int
}

func (*receiptLifecycleRunner) NativeContextID() string { return "original-native-context" }
func (r *receiptLifecycleRunner) NodeReceipt(context.Context) (nodewire.SessionReceipt, bool) {
	r.reads++
	return r.receipt, true
}

func TestLifecycleReceiptDomainIsTheSameForRunAndReattach(t *testing.T) {
	for _, kind := range []attempt.Kind{attempt.KindChat, attempt.KindStep, attempt.KindPlan, attempt.KindDelegate, attempt.KindVerify} {
		for _, resume := range []bool{false, true} {
			t.Run(string(kind)+map[bool]string{false: "/run", true: "/reattach"}[resume], func(t *testing.T) {
				book, err := ledger.Open(t.TempDir(), ledger.Options{})
				if err != nil {
					t.Fatal(err)
				}
				defer book.Close()
				tasks, err := task.OpenLedger(book, "")
				if err != nil {
					t.Fatal(err)
				}
				tracked, err := tasks.Create(task.Task{Channel: "original-conversation", Transport: "console", Member: "worker"})
				if err != nil {
					t.Fatal(err)
				}
				if _, err := tasks.Begin(tracked.ID, "worker", "node", ""); err != nil {
					t.Fatal(err)
				}
				token, err := tasks.ExecutionToken(tracked.ID)
				if err != nil {
					t.Fatal(err)
				}
				attempts := attempt.New(book)
				session := &receiptLifecycleRunner{runner: &runner{id: "ns_" + strings.Repeat("a", 64)}}
				spec := attempt.Spec{ID: "original-attempt", TaskID: tracked.ID, TurnID: "original-input", Kind: kind,
					Agent: "worker", Node: "node", Execution: &token, Scope: attempt.ScopePathSet}
				seal := func(record attempt.Record) error {
					var err error
					session.receipt, err = nodewire.NewSessionReceipt(nodewire.SessionState{
						ID: session.ID(), ContextID: session.NativeContextID(),
						Binding: nodewire.SessionBinding{NodeID: spec.Node, TaskID: spec.TaskID, AttemptID: spec.ID,
							SessionID: attempt.RetainedSessionID(tracked.Channel, tracked.ID, spec.Agent),
							TaskEpoch: token.Epoch, ExecutionEpoch: attempt.SessionExecutionEpoch(record)},
						Command: &nodewire.SessionCommand{ID: spec.TurnID, InputSequence: 1,
							State: nodewire.SessionCommandCompleted, Settled: true},
					})
					return err
				}
				options := Options{Attempts: attempts, Spec: spec, Resume: resume,
					Open:       func(context.Context, *Execution) (harness.Runner, error) { return session, nil },
					Started:    func(_ context.Context, e *Execution) error { return seal(e.Record) },
					Settlement: Settlement{KeepSession: true, DetachManaged: true},
				}
				var result Result
				if !resume {
					result, err = Run(t.Context(), options)
				} else {
					record, openErr := attempts.Open(t.Context(), spec)
					if openErr != nil {
						t.Fatal(openErr)
					}
					for _, phase := range []attempt.State{attempt.Prepared, attempt.Running} {
						record, err = attempts.Advance(t.Context(), spec.ID, phase, "test", func(r *attempt.Record) {
							r.Session = session.ID()
							// The step wrapper records only the session before
							// handover; recovery must not invent its context.
							if kind != attempt.KindStep {
								r.NativeContext = session.NativeContextID()
							}
						})
						if err != nil {
							t.Fatal(err)
						}
					}
					if err := seal(record); err != nil {
						t.Fatal(err)
					}
					result, err = Reattach(t.Context(), options, record, session)
				}
				if err != nil || result.Record.State != attempt.Bound {
					t.Fatalf("receipt collection broke original completion: %s %v", result.Record.State, err)
				}
				pending, err := attempts.PendingNodeReceipts(t.Context(), "", 128)
				if err != nil {
					t.Fatal(err)
				}
				if kind == attempt.KindChat {
					if session.reads == 0 || len(pending) != 1 || result.Record.NodeReceipt == nil {
						t.Fatal("supported chat lost its exact receipt/pending")
					}
				} else if session.reads != 0 || len(pending) != 0 || result.Record.NodeReceipt != nil {
					t.Fatalf("unsupported %s collected an unconsumable receipt: reads=%d pending=%d", kind, session.reads, len(pending))
				}
			})
		}
	}
}
