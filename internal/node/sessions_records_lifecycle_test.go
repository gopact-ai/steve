package node

import (
	"errors"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/gopact-ai/steve/internal/nodewire"
	"github.com/gopact-ai/steve/internal/view"
)

func TestSessionRecordsMissingConsumedInputIsNotFresh(t *testing.T) {
	for _, cold := range []bool{false, true} {
		for _, action := range []nodewire.SessionAction{nodewire.SessionActionAttach, nodewire.SessionActionPrompt} {
			t.Run(string(action)+map[bool]string{false: "/live", true: "/cold"}[cold], func(t *testing.T) {
				one := progressSession(t)
				req := nodeSessionRequest(action)
				req.ID, req.CommandID = one.record.State.ID, one.record.CurrentCommand
				next := one.copyLocked()
				next.ClusterID, next.Authority = req.Authority.ClusterID, req.Authority
				next.State.State = nodewire.SessionClosed
				next.State.ProcessStopped = true
				if err := one.commitLocked(next); err != nil {
					t.Fatal(err)
				}
				store, err := one.service.recordsStore()
				if err != nil {
					t.Fatal(err)
				}
				// Fault injection only. Checkpoint A has no deletion endpoint.
				if _, err := store.db.Exec(`DELETE FROM session_commands WHERE session_id=?`, req.ID); err != nil {
					t.Fatal(err)
				}
				if action == nodewire.SessionActionPrompt {
					req.InputSequence = 1
				}
				if cold {
					_, err = one.service.closedState(req)
				} else if action == nodewire.SessionActionPrompt {
					_, err = one.prompt(req)
				} else {
					_, err = one.attach(req)
				}
				var refusal *SessionError
				if !errors.As(err, &refusal) || refusal.Code != "receipt_expired" {
					t.Fatalf("consumed input can be mistaken for a new sequence: %v", err)
				}
			})
		}
	}
}

func TestSessionRecordsCloseDrainsTimerAndCannotReopen(t *testing.T) {
	cfg := ServerConfig{Name: "worker", StateDir: t.TempDir(), SessionAuthorizer: &sessionAuthorityTest{epoch: 1, writer: 1}}
	server := NewServer(cfg)
	if err := server.startSessions(t.Context()); err != nil {
		t.Fatal(err)
	}
	store, err := server.sessions.recordsStore()
	if err != nil {
		t.Fatal(err)
	}
	one := progressSession(t)
	one.service = server.sessions
	if err := store.save(sessionRecord{}, one.record); err != nil {
		t.Fatal(err)
	}
	server.sessions.sessions[one.record.State.ID] = one
	if err := one.updateProgress("input", view.Progress{Answer: "first"}); err != nil {
		t.Fatal(err)
	}
	if err := one.updateProgress("input", view.Progress{Answer: "last pending chunk"}); err != nil {
		t.Fatal(err)
	}
	server.sessions.Close()
	server.sessions.Close()
	if _, err := server.sessions.recordsStore(); err == nil {
		t.Fatal("shutdown lazily reopened the database")
	}
	if err := store.db.Ping(); err == nil {
		t.Fatal("database remained open after service shutdown")
	}
	reopened, err := openSessionRecords(filepath.Join(cfg.StateDir, "node-sessions", "sessions.db"))
	if err != nil {
		t.Fatalf("service leaked its exclusive lock: %v", err)
	}
	defer reopened.close()
	saved, _, err := reopened.read(one.record.State.ID, "")
	if err != nil || saved.State.Progress.Answer != "last pending chunk" {
		t.Fatalf("close dropped coalesced progress: %v", err)
	}
	time.Sleep(2 * sessionProgressEvery)
	after, _, err := reopened.read(one.record.State.ID, "")
	if err != nil || after.State.Sequence != saved.State.Sequence {
		t.Fatal("late timer wrote after shutdown")
	}
}

func TestSessionRecordsFailureKeepsOwnerAndDurableSequence(t *testing.T) {
	one := progressSession(t)
	before := one.copyLocked()
	store, err := one.service.recordsStore()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.Exec(`CREATE TRIGGER fail_after_progress AFTER UPDATE ON session_progress BEGIN SELECT RAISE(ABORT,'disk failed'); END`); err != nil {
		t.Fatal(err)
	}
	next := one.copyLocked()
	next.State.Progress.Answer = "not published"
	if err := one.commitLocked(next); err == nil {
		t.Fatal("faulted commit reported success")
	}
	if one.failure == nil || !reflect.DeepEqual(one.record, before) {
		t.Fatal("faulted commit published new owner state or did not latch failure")
	}
	saved := savedProgress(t, one)
	if saved.State.Sequence != before.State.Sequence || saved.State.Progress.Answer != before.State.Progress.Answer {
		t.Fatal("faulted commit leaked a partial durable state")
	}
	req := nodeSessionRequest(nodewire.SessionActionPrompt)
	req.ID, req.CommandID, req.InputSequence = one.record.State.ID, "another", 2
	if _, err := one.prompt(req); err != one.failure {
		t.Fatalf("quarantined owner could continue execution: %v", err)
	}
}
