package node

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/gopact-ai/steve/internal/nodewire"
	"github.com/gopact-ai/steve/internal/procgroup"
)

func terminalOwnerRecord(t *testing.T, phase string) sessionRecord {
	t.Helper()
	req := nodeSessionRequest(nodewire.SessionActionOpen)
	one := &ownedSession{record: sessionRecord{
		Format: 1, ClusterID: req.Authority.ClusterID, Authority: req.Authority, Generation: 1, UpstreamID: "original-agent",
		Commands: map[string]nodewire.SessionCommand{}, CommandHashes: map[string]string{},
		State: nodewire.SessionState{ID: "ns_" + strings.Repeat("a", 64), Binding: req.Binding, State: nodewire.SessionIdle},
	}}
	id := "nt_" + strings.Repeat("b", 64)
	one.record.State.InputAccepted = 1
	one.record.CurrentCommand = "original/input"
	one.record.Process = sessionProcess{Identity: procgroup.Identity{Group: 77, Leader: 77, Start: 100, Mark: "original-agent-mark"}, Place: procgroup.Place{Machine: "test-machine", Boot: "test-boot", Namespace: "test-namespace"}}
	one.record.Terminals = map[string]sessionTerminal{id: {ID: id, UpstreamID: one.record.UpstreamID, Generation: 1, CommandID: "original/input", InputSequence: 1, Binding: req.Binding, Authority: req.Authority, Phase: phase}}
	return one.record
}

func TestTerminalOwnerMetadataMustNotEnterLegacyFormat(t *testing.T) {
	record := terminalOwnerRecord(t, "reserved")
	if _, err := sessionRecordJSON(sessionHeader(record)); err == nil {
		t.Fatal("legacy record silently accepted new terminal cleanup ownership")
	}
}

func TestTerminalOwnerRecordRejectsIdentityAndPhaseCorruption(t *testing.T) {
	for _, test := range []string{"unknown-phase", "wrong-key", "missing-command", "wrong-generation", "active-no-group", "preparing-no-transition", "claimed-stop"} {
		t.Run(test, func(t *testing.T) {
			record := terminalOwnerRecord(t, "reserved")
			record.Format = 2
			for id, owner := range record.Terminals {
				switch test {
				case "unknown-phase":
					owner.Phase = "forgotten"
				case "wrong-key":
					owner.ID = "nt_" + strings.Repeat("c", 64)
				case "missing-command":
					owner.CommandID = ""
				case "wrong-generation":
					owner.Generation = 0
				case "active-no-group":
					owner.Phase = "active"
				case "preparing-no-transition":
					owner.Phase = "preparing"
				case "claimed-stop":
					record.State.ProcessStopped = true
				}
				record.Terminals[id] = owner
			}
			if _, err := sessionRecordJSON(sessionHeader(record)); err == nil {
				t.Fatal("invalid terminal owner was saved as trustworthy cleanup evidence")
			}
		})
	}
}

func TestTerminalOwnerValidFactsPersistWithoutPayload(t *testing.T) {
	record := terminalOwnerRecord(t, "active")
	record.Format = 2
	for id, owner := range record.Terminals {
		owner.Process = sessionProcess{Identity: procgroup.Identity{Group: 123, Leader: 123, Start: 456, Mark: "new-group-mark"}, Place: procgroup.Place{Machine: "test-machine", Boot: "test-boot", Namespace: "test-namespace"}}
		record.Terminals[id] = owner
	}
	raw, err := sessionRecordJSON(sessionHeader(record))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), "new-group-mark") {
		t.Fatal("durable terminal group identity was omitted")
	}
	for _, name := range []string{"output", "command_env", "payload", "credential"} {
		if strings.Contains(string(raw), "\""+name+"\"") {
			t.Fatalf("terminal cleanup record unexpectedly included %s", name)
		}
	}
}

func TestTerminalOwnerCopyDoesNotAliasCleanupFacts(t *testing.T) {
	one := &ownedSession{record: terminalOwnerRecord(t, "reserved")}
	next := one.copyLocked()
	for id, owner := range next.Terminals {
		owner.Phase = "stopped"
		next.Terminals[id] = owner
	}
	for _, owner := range one.record.Terminals {
		if owner.Phase != "reserved" {
			t.Fatal("mutation snapshot changed original terminal ownership")
		}
	}
}

func TestTerminalOwnerColdRecordsKeepVersionedCleanupFacts(t *testing.T) {
	dir := t.TempDir()
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "sessions.db")
	store, err := openSessionRecords(path)
	if err != nil {
		t.Fatal(err)
	}
	record := terminalOwnerRecord(t, "active")
	record.Format = 2
	record.State.Sequence = 1
	for id, owner := range record.Terminals {
		owner.Process = sessionProcess{Identity: procgroup.Identity{Group: 123, Leader: 123, Start: 456, Mark: "new-group-mark"}, Place: procgroup.Place{Machine: "test-machine", Boot: "test-boot", Namespace: "test-namespace"}}
		record.Terminals[id] = owner
	}
	if err := store.save(sessionRecord{}, record); err != nil {
		store.close()
		t.Fatal(err)
	}
	if err := store.close(); err != nil {
		t.Fatal(err)
	}
	cold, err := openSessionRecords(path)
	if err != nil {
		t.Fatal(err)
	}
	defer cold.close()
	got, found, err := cold.read(record.State.ID, "")
	if err != nil || !found || got.Format != 2 || !reflect.DeepEqual(got.Terminals, record.Terminals) {
		t.Fatalf("cold owner facts changed or lost: found=%v err=%v got=%+v", found, err, got.Terminals)
	}
	if got.State.ProcessStopped || sessionTerminalsStopped(got) {
		t.Fatal("cold reopen inferred terminal writer-stop")
	}
	legacy := record
	legacy.Format = 1
	if validateSessionTerminals(legacy) == nil {
		t.Fatal("old record contract silently ignored terminal groups")
	}
}

func TestTerminalOwnerRejectsWrongAgentGroupPlaceAndInput(t *testing.T) {
	for _, kind := range []string{"missing-agent", "parent-group", "self-leader", "agent-group", "other-boot", "other-namespace", "before-agent", "future-input"} {
		t.Run(kind, func(t *testing.T) {
			record := terminalOwnerRecord(t, "active")
			record.Format = 2
			for id, owner := range record.Terminals {
				owner.Process = sessionProcess{Identity: procgroup.Identity{Group: 123, Leader: 123, Start: 456, Mark: "new-group-mark"}, Place: record.Process.Place}
				switch kind {
				case "missing-agent":
					record.Process = sessionProcess{}
				case "parent-group":
					owner.Phase = "preparing"
					owner.Process = sessionProcess{}
					owner.Preparation = terminalPreparation{PID: 123, Start: 456, ParentGroup: 88, Mark: "new-group-mark"}
				case "self-leader":
					owner.Phase = "preparing"
					owner.Process = sessionProcess{}
					owner.Preparation = terminalPreparation{PID: 77, Start: 100, ParentGroup: 77, Mark: "new-group-mark"}
				case "agent-group":
					owner.Process.Identity = record.Process.Identity
				case "other-boot":
					owner.Process.Boot = "other-boot"
				case "other-namespace":
					owner.Process.Namespace = "other-namespace"
				case "before-agent":
					owner.Process.Start = 99
				case "future-input":
					owner.InputSequence = 2
				}
				record.Terminals[id] = owner
			}
			if _, err := sessionRecordJSON(sessionHeader(record)); err == nil {
				t.Fatal("terminal cleanup identity was not bound to original Agent/input/place")
			}
		})
	}
}

func TestTerminalOwnerSaveCannotForgetOrRewriteLiveFacts(t *testing.T) {
	for _, kind := range []string{"drop-and-downgrade", "drop", "rewrite-command", "rewrite-generation", "rewrite-authority", "wrong-split"} {
		t.Run(kind, func(t *testing.T) {
			dir := t.TempDir()
			if err := os.Chmod(dir, 0700); err != nil {
				t.Fatal(err)
			}
			store, err := openSessionRecords(filepath.Join(dir, "sessions.db"))
			if err != nil {
				t.Fatal(err)
			}
			defer store.close()
			original := terminalOwnerRecord(t, "preparing")
			original.Format = 2
			original.State.Sequence = 1
			for id, owner := range original.Terminals {
				owner.Preparation = terminalPreparation{PID: 123, Start: 456, ParentGroup: 77, Mark: "new-group-mark"}
				original.Terminals[id] = owner
			}
			if err := store.save(sessionRecord{}, original); err != nil {
				t.Fatal(err)
			}
			one := &ownedSession{record: original}
			next := one.copyLocked()
			next.State.Sequence = 2
			switch kind {
			case "drop-and-downgrade":
				next.Terminals = nil
				next.Format = 1
			case "drop":
				next.Terminals = nil
			default:
				for id, owner := range next.Terminals {
					switch kind {
					case "rewrite-command":
						owner.CommandID = "other/input"
					case "rewrite-generation":
						owner.Generation = 2
						next.Generation = 2
					case "rewrite-authority":
						owner.Authority.WriterGeneration = 2
					case "wrong-split":
						owner.Phase = "active"
						owner.Preparation = terminalPreparation{}
						owner.Process = sessionProcess{Identity: procgroup.Identity{Group: 124, Leader: 124, Start: 457, Mark: "changed-mark"}, Place: original.Process.Place}
					}
					next.Terminals[id] = owner
				}
			}
			if err := store.save(original, next); err == nil {
				t.Fatal("durable transition erased or rewrote an unstopped original terminal")
			}
			got, found, err := store.read(original.State.ID, "")
			if err != nil || !found || !reflect.DeepEqual(got.Terminals, original.Terminals) {
				t.Fatal("refused transition altered original cleanup archive")
			}
		})
	}
}

func TestTerminalOwnerRealSaveRejectsCommandStoppedEarly(t *testing.T) {
	dir := t.TempDir()
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	store, err := openSessionRecords(filepath.Join(dir, "sessions.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.close()
	record := terminalOwnerRecord(t, "active")
	record.Format = 2
	record.State.Sequence = 1
	for id, owner := range record.Terminals {
		owner.Process = sessionProcess{Identity: procgroup.Identity{Group: 123, Leader: 123, Start: 456, Mark: "new-group-mark"}, Place: record.Process.Place}
		record.Terminals[id] = owner
	}
	record.Commands[record.CurrentCommand] = nodewire.SessionCommand{ID: record.CurrentCommand, InputSequence: 1, State: nodewire.SessionCommandRunning, ProcessStopped: true}
	record.CommandHashes[record.CurrentCommand] = "original-input-hash"
	if err := store.save(sessionRecord{}, record); err == nil {
		t.Fatal("header projection hid a command stop that omitted active terminal")
	}
}
