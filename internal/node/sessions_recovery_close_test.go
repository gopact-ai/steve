package node

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/gopact-ai/steve/internal/nodewire"
)

func TestRecoveryIdleCloseReplaysItsPersistedWholeProcessProofAfterRestart(t *testing.T) {
	authority := &sessionAuthorityTest{epoch: 1, writer: 1}
	cfg := ServerConfig{Name: "worker", StateDir: t.TempDir(), WorkspaceRoot: t.TempDir(), Harnesses: map[string]HarnessSpec{"mock": {Command: buildMockAgent(t)}}, SessionAuthorizer: authority}
	server := NewServer(cfg)
	if err := server.startSessions(t.Context()); err != nil {
		t.Fatal(err)
	}
	defer server.sessions.Close()
	req := nodeSessionRequest(nodewire.SessionActionOpen)
	req.Harness, req.Workdir, req.CommandID = "mock", t.TempDir(), "exact-open"
	opened, err := server.sessions.Do(t.Context(), "cluster-1", req)
	if err != nil {
		t.Fatal(err)
	}
	req.ID, req.Action = opened.ID, nodewire.SessionActionClose
	stopped, err := server.sessions.Do(t.Context(), "cluster-1", req)
	if err != nil || !stopped.ProcessStopped || stopped.State != nodewire.SessionClosed {
		t.Fatalf("idle close did not issue real whole-process proof: %+v %v", stopped, err)
	}
	server.sessions.Close()
	reopened := NewServer(cfg)
	if err := reopened.startSessions(t.Context()); err != nil {
		t.Fatal(err)
	}
	defer reopened.sessions.Close()
	for range 2 {
		replay, err := reopened.sessions.Do(t.Context(), "cluster-1", req)
		if err != nil || !replay.ProcessStopped || replay.ID != stopped.ID || replay.Binding != stopped.Binding || replay.ContextID != stopped.ContextID || replay.Sequence != stopped.Sequence {
			t.Fatalf("restarted close lost its persisted machine proof: %+v %v", replay, err)
		}
	}
	for _, mode := range []string{"binding", "cluster", "command"} {
		bad := req
		switch mode {
		case "binding":
			bad.Binding.TaskEpoch++
		case "cluster":
			bad.Authority.ClusterID = "another-cluster"
		case "command":
			bad.CommandID = "unknown-command"
		}
		if _, err := reopened.sessions.Do(t.Context(), "cluster-1", bad); err == nil {
			t.Fatalf("%s mismatch was promoted to exact idle-close proof", mode)
		}
	}
	record, found, err := reopened.sessions.readRecord(req.ID)
	if err != nil || !found || !reflect.DeepEqual(record.State.Binding, stopped.Binding) {
		t.Fatalf("bad retry changed original binding: %+v %v", record, err)
	}
}

type recoveryCloseAuthority struct {
	sessionAuthorityTest
	receipt nodewire.SessionReceipt
}

func (a *recoveryCloseAuthority) AuthorizeNodeReceipt(_ context.Context, principal string, _ nodewire.SessionAuthority, receipt nodewire.SessionReceipt) error {
	if principal != "cluster-1" || receipt != a.receipt {
		return errors.New("wrong fixture receipt")
	}
	return nil
}

func TestRecoveryCloseKeepsPrunedCommandIdentityWithoutRestoringItsBody(t *testing.T) {
	authority := &recoveryCloseAuthority{sessionAuthorityTest: sessionAuthorityTest{epoch: 1, writer: 1}}
	cfg := ServerConfig{Name: "worker", StateDir: t.TempDir(), WorkspaceRoot: t.TempDir(), Harnesses: map[string]HarnessSpec{"mock": {Command: buildMockAgent(t)}}, SessionAuthorizer: authority}
	server := NewServer(cfg)
	if err := server.startSessions(t.Context()); err != nil {
		t.Fatal(err)
	}
	defer server.sessions.Close()
	req := nodeSessionRequest(nodewire.SessionActionOpen)
	req.Harness, req.Workdir, req.CommandID = "mock", t.TempDir(), "first/open"
	st, err := server.sessions.Do(t.Context(), "cluster-1", req)
	if err != nil {
		t.Fatal(err)
	}
	req.ID, req.Action, req.CommandID, req.InputSequence, req.Text = st.ID, nodewire.SessionActionPrompt, "first", 1, "answer"
	st, err = server.sessions.Do(t.Context(), "cluster-1", req)
	if err != nil {
		t.Fatal(err)
	}
	for st.Command == nil || !st.Command.Settled {
		poll := req
		poll.Action = nodewire.SessionActionPoll
		poll.After, poll.WaitMS = st.Sequence, 50
		st, err = server.sessions.Do(t.Context(), "cluster-1", poll)
		if err != nil {
			t.Fatal(err)
		}
	}
	if st.Command.Receipt.Validate() != nil {
		t.Fatal("real native terminal input did not issue its immutable receipt")
	}
	authority.receipt = st.Command.Receipt
	ack := nodewire.SessionReceiptRequest{Authority: req.Authority, Receipt: authority.receipt}
	store, err := server.sessions.recordsStore()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.Exec(`CREATE TRIGGER refuse_consumed BEFORE UPDATE ON sessions WHEN json_extract(NEW.header,'$.consumed') IS NOT NULL BEGIN SELECT RAISE(ABORT,'consumed refused'); END`); err != nil {
		t.Fatal(err)
	}
	if err := server.sessions.AcknowledgeReceipt(t.Context(), "cluster-1", ack); err == nil {
		t.Fatal("ack refused its consumed identity but succeeded")
	}
	before, found, err := server.sessions.readRecord(req.ID)
	if err != nil || !found || before.Consumed != nil || len(before.Commands) == 0 {
		t.Fatalf("failed ack lost receipt or accepted identity: %+v %v", before, err)
	}
	if _, err := store.db.Exec("DROP TRIGGER refuse_consumed"); err != nil {
		t.Fatal(err)
	}
	if err := server.sessions.AcknowledgeReceipt(t.Context(), "cluster-1", ack); err != nil {
		t.Fatal(err)
	}
	if err := server.sessions.AcknowledgeReceipt(t.Context(), "cluster-1", ack); err != nil {
		t.Fatal(err)
	}
	current, found, err := server.sessions.readRecord(req.ID)
	if err != nil || !found || current.Consumed == nil || *current.Consumed != authority.receipt || len(current.Commands) != 0 {
		t.Fatalf("ack did not atomically preserve only exact identity: %+v %v", current, err)
	}
	req.Action = nodewire.SessionActionClose
	closed, err := server.sessions.Do(t.Context(), "cluster-1", req)
	if err != nil || !closed.ProcessStopped || closed.Command != nil {
		t.Fatalf("live idle close could not use exact consumed identity: %+v %v", closed, err)
	}
	server.sessions.Close()
	reopened := NewServer(cfg)
	if err := reopened.startSessions(t.Context()); err != nil {
		t.Fatal(err)
	}
	defer reopened.sessions.Close()
	replayed, err := reopened.sessions.Do(t.Context(), "cluster-1", req)
	if err != nil || !replayed.ProcessStopped || replayed.Command != nil || replayed.Binding != closed.Binding || replayed.Sequence != closed.Sequence {
		t.Fatalf("restart replaced process proof with a fabricated command: %+v %v", replayed, err)
	}
	for _, mode := range []string{"command", "sequence", "context", "binding", "unconfirmed"} {
		bad := req
		switch mode {
		case "command":
			bad.CommandID = "unknown"
		case "sequence":
			bad.InputSequence++
		case "binding":
			bad.Binding.AttemptID = "another"
		case "context", "unconfirmed":
			r, found, err := reopened.sessions.readRecord(req.ID)
			if err != nil || !found {
				t.Fatal(err)
			}
			if mode == "context" {
				r.Consumed.ContextID = "wrong-context"
			} else {
				r.State.ProcessStopped = false
				r.State.State = nodewire.SessionInterrupted
			}
			store, err := reopened.sessions.recordsStore()
			if err != nil {
				t.Fatal(err)
			}
			raw, err := sessionRecordJSON(sessionHeader(r))
			if err != nil {
				t.Fatal(err)
			}
			if _, err := store.db.Exec(`UPDATE sessions SET header=? WHERE id=?`, raw, r.State.ID); err != nil {
				t.Fatal(err)
			}
		}
		if got, err := reopened.sessions.Do(t.Context(), "cluster-1", bad); err == nil {
			t.Fatalf("%s mismatch was accepted as close proof: %+v", mode, got)
		}
	}
}
