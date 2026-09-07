package attempt

import (
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/gopact-ai/steve/internal/ledger"
	"github.com/gopact-ai/steve/internal/nodewire"
	"github.com/gopact-ai/steve/internal/task"
)

func retainedFixture(t *testing.T) (*Service, *clock, Record, RetainedEvidence, *task.Store) {
	t.Helper()
	s, now := newService(t)
	tasks, err := task.OpenLedger(s.l, "")
	if err != nil {
		t.Fatal(err)
	}
	tracked, err := tasks.Create(task.Task{Goal: "continue", Channel: "console:main", Member: "worker"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tasks.Begin(tracked.ID, "worker", "node-a", ""); err != nil {
		t.Fatal(err)
	}
	token, err := tasks.ExecutionToken(tracked.ID)
	if err != nil {
		t.Fatal(err)
	}
	r, err := s.Open(t.Context(), Spec{ID: "att-live", TaskID: tracked.ID, TurnID: "web-exchange", Kind: KindChat, Project: "project", Node: "node-a", Harness: "test", Agent: "worker", Execution: &token, Scope: ScopePathSet, Workspace: worktree("workspace", "project")})
	if err != nil {
		t.Fatal(err)
	}
	for _, phase := range []State{Prepared, Running} {
		r, err = s.Advance(t.Context(), r.ID, phase, "test", func(record *Record) { record.Session = "ns_existing" })
		if err != nil {
			t.Fatal(err)
		}
	}
	proof := RetainedEvidence{ObservedAt: now.t, Session: nodewire.SessionState{ID: r.Session, Harness: r.Harness, State: "running", InputAccepted: 1, Binding: nodewire.SessionBinding{ProjectID: r.Project, SessionID: RetainedSessionID(tracked.Channel, tracked.ID, r.Agent), TaskID: r.TaskID, AttemptID: r.ID, NodeID: r.Node, ExecutionEpoch: r.Leases[0].Epoch, TaskEpoch: token.Epoch}, Command: &nodewire.SessionCommand{ID: r.TurnID, InputSequence: 1, State: "running"}}}
	return s, now, r, proof, tasks
}

func TestRetainedExecutionRecoversExpiredSameHolderWithoutNewAttempt(t *testing.T) {
	s, now, old, proof, _ := retainedFixture(t)
	if err := s.MarkUnsettled(t.Context(), old.ID, "startup", errors.New("observer restarted"), nil); err != nil {
		t.Fatal(err)
	}
	now.t = now.t.Add(3 * time.Minute)
	proof.ObservedAt = now.t
	r, err := s.RecoverRetained(t.Context(), old.ID, proof)
	if err != nil {
		t.Fatal(err)
	}
	if r.ID != old.ID || r.State != Running || r.Unsettled || r.Session != old.Session {
		t.Fatalf("original execution not retained: %+v", r)
	}
	if err := s.Renew(t.Context(), r.ID); err != nil {
		t.Fatalf("recovered lease cannot heartbeat: %v", err)
	}
	for i, lease := range r.Leases {
		if lease.Epoch != old.Leases[i].Epoch || lease.Holder != old.ID {
			t.Fatal("recovery minted replacement authority")
		}
	}
}

func TestRetainedRecoveryRejectsDifferentHolderOrTaskEpochAtomically(t *testing.T) {
	for _, changed := range []string{"holder", "task", "command", "node", "uncertain"} {
		t.Run(changed, func(t *testing.T) {
			s, now, old, proof, tasks := retainedFixture(t)
			now.t = now.t.Add(3 * time.Minute)
			proof.ObservedAt = now.t
			first := old.Leases[0]
			switch changed {
			case "holder":
				if _, err := s.l.Acquire(t.Context(), old.Leases[len(old.Leases)-1].Key, "replacement", time.Minute); err != nil {
					t.Fatal(err)
				}
			case "task":
				if _, err := tasks.SetAside(old.TaskID, task.StatePaused); err != nil {
					t.Fatal(err)
				}
			case "command":
				proof.Session.Command.ID = "different-input"
			case "node":
				proof.Session.Binding.NodeID = "other-node"
			case "uncertain":
				proof.Session.State = "interrupted"
				proof.Session.Command.State = "uncertain"
			}
			if _, err := s.RecoverRetained(t.Context(), old.ID, proof); err == nil {
				t.Fatal("unverified execution accepted")
			}
			current, _, err := s.l.LeaseOf(t.Context(), first.Key)
			if err != nil {
				t.Fatal(err)
			}
			if current.Holder == first.Holder && current.ExpiresAt.After(now.t) {
				t.Fatal("failed recovery partially renewed leases")
			}
		})
	}
}

func TestRetainedRecoveryAcceptsDurableSettledResultButNotMissingReceipt(t *testing.T) {
	s, now, r, proof, _ := retainedFixture(t)
	now.t = now.t.Add(3 * time.Minute)
	proof.ObservedAt = now.t
	proof.Session.State = "interrupted"
	proof.Session.Command.State = "completed"
	proof.Session.Command.Settled = true
	got, err := s.RecoverRetained(t.Context(), r.ID, proof)
	if err != nil || got.SessionSettled == nil || !*got.SessionSettled {
		t.Fatalf("settled result cannot finish original attempt: %+v %v", got, err)
	}
	proof.Session.Command = nil
	if _, err := s.RecoverRetained(t.Context(), r.ID, proof); err == nil || errors.Is(err, ledger.ErrStale) {
		t.Fatalf("missing input receipt was not rejected: %v", err)
	}
}

func TestStartupKeepsManagedSettledRunningAttemptForResultRecovery(t *testing.T) {
	s, _, r, _, _ := retainedFixture(t)
	if err := s.MarkSessionSettled(t.Context(), r.ID, "node response received"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.PrepareRecovery(t.Context(), "startup"); err != nil {
		t.Fatal(err)
	}
	got, err := s.Get(t.Context(), r.ID)
	if err != nil || got.State != Running {
		t.Fatalf("settled managed result expired before recovery: %+v %v", got, err)
	}
}

func TestRetainedRecoveryOnlyAcceptsRegisteredKinds(t *testing.T) {
	for _, kind := range []Kind{KindChat, KindDelegate, KindStep, KindPlan, KindVerify, Kind("arbitrary")} {
		t.Run(string(kind), func(t *testing.T) {
			s, _, r, proof, _ := retainedFixture(t)
			_, err := s.l.Transition(t.Context(), r.ID, string(Running), string(Running), "test", nil, nil, func(tx *ledger.Tx, op *ledger.Operation) error {
				var updated Record
				if err := json.Unmarshal(op.Data, &updated); err != nil {
					return err
				}
				updated.Kind = kind
				return tx.SetData(op, updated)
			})
			if err != nil {
				t.Fatal(err)
			}
			_, err = s.RecoverRetained(t.Context(), r.ID, proof)
			if (kind != Kind("arbitrary")) != (err == nil) {
				t.Fatalf("kind %s: %v", kind, err)
			}
		})
	}
}

func TestRetainedPostPromptPhaseRequiresSettlementAndNeverRewinds(t *testing.T) {
	s, _, r, proof, _ := retainedFixture(t)
	if _, err := s.Advance(t.Context(), r.ID, Snapshotted, "exec", nil); err != nil {
		t.Fatal(err)
	}
	if _, err := s.RecoverRetained(t.Context(), r.ID, proof); err == nil {
		t.Fatal("unfinished native prompt admitted into post-prompt recovery")
	}
	proof.Session.Command.State = "completed"
	proof.Session.Command.Settled = true
	proof.Session.State = "idle"
	saved, err := s.RecoverRetained(t.Context(), r.ID, proof)
	if err != nil || saved.State != Snapshotted {
		t.Fatalf("recovery rewound durable phase: %+v %v", saved, err)
	}
}

func TestRecordSessionCannotReplaceNativeIdentity(t *testing.T) {
	s, _, r, _, _ := retainedFixture(t)
	if _, err := s.RecordSession(t.Context(), r.ID, "exec", r.Session); err != nil {
		t.Fatal(err)
	}
	if _, err := s.RecordSession(t.Context(), r.ID, "exec", "ns_other"); err == nil {
		t.Fatal("running session identity changed")
	}
}
