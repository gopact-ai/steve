package node

import (
	"fmt"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/gopact-ai/steve/internal/nodewire"
	"github.com/gopact-ai/steve/internal/view"
)

func recordsFixture(t *testing.T) (*sessionRecords, sessionRecord) {
	t.Helper()
	store, err := openSessionRecords(filepath.Join(t.TempDir(), "records", "sessions.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.close() })
	record := sessionRecord{
		Format: 1, ClusterID: "cluster-1", Authority: nodeSessionRequest("open").Authority,
		UpstreamID: "native-upstream", CurrentCommand: "first",
		State: nodewire.SessionState{
			ID: "ns_" + strings.Repeat("a", 64), ContextID: "native-context",
			Binding: nodeSessionRequest("open").Binding, State: nodewire.SessionIdle,
			Sequence: 1, InputAccepted: 1, Progress: view.Progress{Answer: "first answer"},
		},
		Commands: map[string]nodewire.SessionCommand{
			"first": {ID: "first", InputSequence: 1, State: nodewire.SessionCommandCompleted, Output: "first answer", Settled: true},
		},
		CommandHashes: map[string]string{"first": "first-hash"},
	}
	if err := store.save(sessionRecord{}, record); err != nil {
		t.Fatal(err)
	}
	return store, record
}

func TestSessionRecordsProgressDoesNotRewriteReceiptHistory(t *testing.T) {
	store, before := recordsFixture(t)
	for _, table := range []string{"session_commands", "session_questions"} {
		_, err := store.db.Exec(`CREATE TRIGGER refuse_` + table + ` BEFORE UPDATE ON ` + table + ` BEGIN SELECT RAISE(ABORT, 'history rewritten'); END`)
		if err != nil {
			t.Fatal(err)
		}
	}
	next := before
	next.State.Sequence++
	next.State.Progress.Answer = "latest progress"
	if err := store.save(before, next); err != nil {
		t.Fatalf("progress rewrote historical records: %v", err)
	}
	got, exists, err := store.read(next.State.ID, "")
	if err != nil || !exists || got.State.Progress.Answer != "latest progress" || got.Commands["first"].Output != "first answer" {
		t.Fatalf("progress and receipt differ: %+v %v", got, err)
	}
	var header string
	if err := store.db.QueryRow(`SELECT header FROM sessions WHERE id=?`, next.State.ID).Scan(&header); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(header, "first answer") || strings.Contains(header, "first-hash") || strings.Contains(header, "latest progress") {
		t.Fatal("header retained command history or progress")
	}
}

func TestSessionRecordsRollbackHeaderCommandAndQuestionTogether(t *testing.T) {
	store, before := recordsFixture(t)
	if _, err := store.db.Exec(`CREATE TRIGGER refuse_question BEFORE INSERT ON session_questions BEGIN SELECT RAISE(ABORT, 'question write failed'); END`); err != nil {
		t.Fatal(err)
	}
	one := &ownedSession{record: before}
	next := one.copyLocked()
	next.State.Sequence++
	next.State.Progress.Answer = "not committed"
	command := next.Commands["first"]
	command.Error = "not committed"
	next.Commands["first"] = command
	next.State.Questions = []nodewire.SessionQuestion{{ID: "question", CommandID: "first", State: nodewire.SessionQuestionPending}}
	if err := store.save(before, next); err == nil {
		t.Fatal("injected SQLite failure did not reject transition")
	}
	got, _, err := store.read(before.State.ID, "")
	if err != nil || got.State.Sequence != before.State.Sequence || got.Commands["first"].Error != "" || got.State.Progress.Answer != before.State.Progress.Answer || len(got.State.Questions) != 0 {
		t.Fatalf("failed transition partially committed: %+v %v", got, err)
	}
}

func TestSessionRecordsColdReceiptKeepsOriginalBindingAndNativeIdentity(t *testing.T) {
	store, before := recordsFixture(t)
	next := (&ownedSession{record: before}).copyLocked()
	next.State.Sequence++
	next.State.Binding.AttemptID = "second-attempt"
	next.State.InputAccepted = 2
	next.CurrentCommand = "second"
	next.Commands = map[string]nodewire.SessionCommand{"second": {ID: "second", InputSequence: 2, State: nodewire.SessionCommandAccepted}}
	next.CommandHashes = map[string]string{"second": "second-hash"}
	next.State.Progress = view.Progress{}
	if err := store.save(before, next); err != nil {
		t.Fatal(err)
	}
	old, exists, err := store.read(next.State.ID, "first")
	if err != nil || !exists || len(old.Commands) != 1 || old.Commands["first"].Output != "first answer" || old.State.Progress.Answer != "first answer" || old.State.Binding != before.State.Binding {
		t.Fatalf("cold original receipt overwritten by rebind: %+v %v", old, err)
	}
	current, exists, err := store.read(next.State.ID, "")
	if err != nil || !exists || len(current.Commands) != 1 || current.CurrentCommand != "second" || current.UpstreamID != before.UpstreamID || current.State.ContextID != before.State.ContextID || current.State.InputAccepted != 2 {
		t.Fatalf("hot header lost native identity or retained history: %+v %v", current, err)
	}
	if err := store.save(before, before); err == nil {
		t.Fatal("stale owner overwrote a later transition")
	}
}

func TestSessionRecordsQuotaIncludesColdAndUnknown(t *testing.T) {
	for _, kind := range []string{"commands", "questions", "bytes"} {
		t.Run(kind, func(t *testing.T) {
			store, first := recordsFixture(t)
			full := (&ownedSession{record: first}).copyLocked()
			full.State.Sequence++
			switch kind {
			case "commands":
				full.State.InputAccepted = 512
				for i := 2; i <= 512; i++ {
					id := fmt.Sprintf("unknown-%d", i)
					full.Commands[id] = nodewire.SessionCommand{ID: id, InputSequence: uint64(i), State: nodewire.SessionCommandUncertain}
					full.CommandHashes[id] = id
				}
			case "questions":
				for i := 0; i < 256; i++ {
					full.State.Questions = append(full.State.Questions, nodewire.SessionQuestion{
						ID: fmt.Sprint(i), CommandID: "first", State: nodewire.SessionQuestionInterrupted,
					})
				}
			case "bytes":
				command := full.Commands["first"]
				command.Output = strings.Repeat("x", 9<<20)
				command.State, command.Settled = nodewire.SessionCommandUncertain, false
				full.Commands["first"] = command
			}
			if err := store.save(first, full); err != nil {
				t.Fatal(err)
			}
			next := (&ownedSession{record: liveSessionRecord(full)}).copyLocked()
			next.State.Sequence++
			next.State.Binding.AttemptID = "another-attempt"
			next.CurrentCommand = "new-command"
			next.State.InputAccepted++
			next.Commands = map[string]nodewire.SessionCommand{"new-command": {
				ID: "new-command", InputSequence: next.State.InputAccepted, State: nodewire.SessionCommandAccepted,
			}}
			next.CommandHashes = map[string]string{"new-command": "new-hash"}
			next.State.Questions = nil
			if kind == "questions" {
				next.State.Questions = []nodewire.SessionQuestion{{ID: "another", CommandID: "new-command", State: nodewire.SessionQuestionPending}}
			}
			if kind == "bytes" {
				next.State.Progress.Answer = strings.Repeat("x", 8<<20)
			}
			if err := store.save(liveSessionRecord(full), next); err == nil {
				t.Fatal("cold/unknown rows were excluded from the session quota")
			}
			got, _, err := store.read(full.State.ID, "first")
			if err != nil || got.State.Sequence != full.State.Sequence || !reflect.DeepEqual(got.Commands["first"], full.Commands["first"]) || len(got.State.Questions) != len(full.State.Questions) {
				t.Fatalf("quota failure evicted or changed retained evidence: %v", err)
			}
			if kind == "commands" {
				count, err := store.pending(full.State.ID)
				if err != nil || count != 512 {
					t.Fatalf("quota did not retain all unknown commands: %d %v", count, err)
				}
			}
		})
	}
}

func TestSessionRecordsQuestionCannotChangeCommandOwner(t *testing.T) {
	store, before := recordsFixture(t)
	first := (&ownedSession{record: before}).copyLocked()
	first.State.Sequence++
	first.State.Questions = []nodewire.SessionQuestion{{ID: "same-question", CommandID: "first", State: nodewire.SessionQuestionAnswered}}
	if err := store.save(before, first); err != nil {
		t.Fatal(err)
	}
	next := (&ownedSession{record: first}).copyLocked()
	next.State.Sequence++
	next.CurrentCommand = "second"
	next.State.InputAccepted = 2
	next.Commands = map[string]nodewire.SessionCommand{"second": {ID: "second", InputSequence: 2, State: nodewire.SessionCommandAccepted}}
	next.CommandHashes = map[string]string{"second": "hash"}
	next.State.Questions[0].CommandID = "second"
	if err := store.save(first, next); err == nil {
		t.Fatal("question owner mismatch silently committed the header")
	}
	got, _, err := store.read(first.State.ID, "")
	if err != nil || got.State.Sequence != first.State.Sequence {
		t.Fatalf("question mismatch partially committed: %v", err)
	}
}
