//go:build linux

package node

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"reflect"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/gopact-ai/steve/internal/acphost"
	"github.com/gopact-ai/steve/internal/nodewire"
	"github.com/gopact-ai/steve/internal/procgroup"
)

// Synthetic identities below exercise SQLite publication and scheduling only.
// They are passed exclusively to the in-process settler, never to the kernel.
// Preparing-phase positives below use real CapturePreparation and real groups.
func recoveryRecord(phases ...string) sessionRecord {
	req := nodeSessionRequest(nodewire.SessionActionOpen)
	record := sessionRecord{
		Format: 2, ClusterID: req.Authority.ClusterID, Authority: req.Authority,
		UpstreamID: "original-agent", Generation: 1, CurrentCommand: "input",
		State: nodewire.SessionState{
			ID: "ns_" + strings.Repeat("d", 64), ContextID: "original-context",
			Binding: req.Binding, InputAccepted: 1, State: nodewire.SessionInterrupted,
		},
		Process: sessionProcess{
			Identity: procgroup.Identity{Group: 600001, Leader: 600001, Start: 100, Mark: "agent-mark"},
			Place:    procgroup.Place{Machine: "synthetic-machine", Boot: "synthetic-boot", Namespace: "synthetic-namespace"},
		},
		Commands: map[string]nodewire.SessionCommand{
			"input": {ID: "input", InputSequence: 1, State: nodewire.SessionCommandUncertain},
		},
		CommandHashes: map[string]string{"input": "original-input-hash"},
		Terminals:     map[string]sessionTerminal{},
	}
	for i, phase := range phases {
		id := fmt.Sprintf("nt_%064x", i+1)
		owner := sessionTerminal{
			ID: id, UpstreamID: record.UpstreamID, Generation: record.Generation,
			CommandID: "input", InputSequence: 1, Binding: req.Binding, Authority: req.Authority,
			Phase: phase,
		}
		if phase == "active" || phase == "stopping" {
			owner.PayloadStarted = true
			pid := 600002 + i
			owner.Process = sessionProcess{
				Identity: procgroup.Identity{Group: pid, Leader: pid, Start: uint64(200 + i), Mark: fmt.Sprintf("terminal-mark-%d", i)},
				Place:    record.Process.Place,
			}
		}
		record.Terminals[id] = owner
	}
	return record
}

func recoveryOwner(t *testing.T, record sessionRecord) *ownedSession {
	t.Helper()
	server := NewServer(ServerConfig{Name: "worker", StateDir: t.TempDir()})
	service := &SessionService{
		server: server, ctx: t.Context(), sessions: map[string]*ownedSession{},
		unverifiedProcesses: map[string]bool{}, place: record.Process.Place, placeKnown: true,
	}
	store, err := service.recordsStore()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(service.closeRecords)
	record.State.Sequence = 1
	if err := store.save(sessionRecord{}, record); err != nil {
		t.Fatal(err)
	}
	cold, found, err := store.read(record.State.ID, "")
	if err != nil || !found {
		t.Fatalf("read isolated SQLite fixture: found=%v err=%v", found, err)
	}
	return &ownedSession{service: service, record: cold, changed: make(chan struct{})}
}

func recoverySaved(t *testing.T, one *ownedSession) sessionRecord {
	t.Helper()
	record, found, err := one.service.readRecord(one.record.State.ID)
	if err != nil || !found {
		t.Fatalf("read cleanup evidence: found=%v err=%v", found, err)
	}
	return record
}

func recoveryAssertStopped(t *testing.T, before, after sessionRecord) {
	t.Helper()
	if !after.State.ProcessStopped || !sessionTerminalsStopped(after) || before.Process != after.Process {
		t.Fatal("aggregate stop omitted an owner or replaced the original Agent group")
	}
	if after.State.Sequence != before.State.Sequence+1 {
		t.Fatal("cleanup aggregate was not one durable transition")
	}
	if len(after.Terminals) != len(before.Terminals) {
		t.Fatal("cleanup forgot a retained terminal identity")
	}
	for id, original := range before.Terminals {
		want := original
		want.Phase = "stopped"
		if after.Terminals[id] != want {
			t.Fatalf("cleanup rewrote terminal %s original identity or preparation", id)
		}
	}
	for _, command := range after.Commands {
		if !command.ProcessStopped {
			t.Fatal("command reported a partial native stop")
		}
	}
}

func TestTerminalRecoveryOriginalFirstAllOwnersBarrier(t *testing.T) {
	phases := make([]string, 32)
	for i := range phases {
		phases[i] = "active"
	}
	phases[0], phases[30], phases[31] = "stopping", "reserved", "stopped"
	one := recoveryOwner(t, recoveryRecord(phases...))
	before := one.copyLocked()
	deadline := time.Now().Add(recordedGroupWithin)
	arrived := make(chan procgroup.Identity, len(phases)+1)
	originalEnded, terminalsEnded := make(chan struct{}), make(chan struct{})
	var originalOnce, terminalsOnce sync.Once
	releaseOriginal := func() { originalOnce.Do(func() { close(originalEnded) }) }
	releaseTerminals := func() { terminalsOnce.Do(func() { close(terminalsEnded) }) }
	t.Cleanup(func() { releaseOriginal(); releaseTerminals() })
	one.service.settle = func(id procgroup.Identity, ran, here procgroup.Place, within time.Duration) error {
		if ran != before.Process.Place || here != before.Process.Place {
			return errors.New("cleanup substituted an original process place")
		}
		// Compare real deadlines, not an artificial clock or a fresh duration.
		if within <= 0 || time.Now().Add(within).After(deadline.Add(50*time.Millisecond)) {
			return errors.New("cleanup extended the original deadline")
		}
		arrived <- id
		gate := terminalsEnded
		if id == before.Process.Identity {
			gate = originalEnded
		} else {
			select {
			case <-originalEnded:
			default:
				return errors.New("terminal cleanup began before the Agent ended")
			}
		}
		select {
		case <-gate:
			return nil
		case <-time.After(within):
			return procgroup.ErrRunning
		}
	}
	done := make(chan error, 1)
	go func() { done <- one.service.endRecordedGroup(one, deadline) }()
	select {
	case id := <-arrived:
		if id != before.Process.Identity {
			t.Fatal("first cleanup was not the original Agent group")
		}
	case <-time.After(recordedGroupWithin):
		t.Fatal("original Agent cleanup did not start")
	}
	if got := recoverySaved(t, one); got.State.ProcessStopped || got.State.Sequence != before.State.Sequence {
		t.Fatal("stop was published before the original Agent group ended")
	}
	select {
	case <-arrived:
		t.Fatal("a terminal settler crossed the original Agent barrier")
	default:
	}
	// Real elapsed time makes a fresh per-terminal timeout observably wrong.
	time.Sleep(100 * time.Millisecond)
	releaseOriginal()
	wanted := map[procgroup.Identity]bool{}
	for _, owner := range before.Terminals {
		if owner.Phase == "active" || owner.Phase == "stopping" {
			wanted[owner.Process.Identity] = true
		}
	}
	for range len(wanted) {
		select {
		case id := <-arrived:
			if !wanted[id] {
				t.Fatal("cleanup used a substituted or duplicate terminal identity")
			}
			delete(wanted, id)
		case err := <-done:
			t.Fatalf("aggregate finished before all terminals arrived: %v", err)
		case <-time.After(recordedGroupWithin):
			t.Fatal("terminal groups were serialized behind their own completion")
		}
	}
	if got := recoverySaved(t, one); !reflect.DeepEqual(got.Terminals, before.Terminals) || got.State.ProcessStopped {
		t.Fatal("partial terminal success published cleanup facts")
	}
	releaseTerminals()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	recoveryAssertStopped(t, before, recoverySaved(t, one))
}

func TestTerminalRecoveryUnknownOwnerCommitsNoStop(t *testing.T) {
	for _, kind := range []string{"agent", "active", "stopping", "unsupported"} {
		t.Run(kind, func(t *testing.T) {
			one := recoveryOwner(t, recoveryRecord("active", "stopping", "reserved"))
			before, changed := one.copyLocked(), one.changed
			tried := make(chan procgroup.Identity, 3)
			one.service.settle = func(id procgroup.Identity, _, _ procgroup.Place, _ time.Duration) error {
				tried <- id
				if kind == "agent" && id == before.Process.Identity {
					return procgroup.ErrUnproven
				}
				if kind == "unsupported" {
					return procgroup.ErrUnsupported
				}
				for _, owner := range before.Terminals {
					if owner.Phase == kind && owner.Process.Identity == id {
						return procgroup.ErrUnproven
					}
				}
				return nil
			}
			if err := one.service.endRecordedGroup(one, time.Now().Add(recordedGroupWithin)); err == nil {
				t.Fatal("unknown cleanup owner became a confirmed aggregate stop")
			}
			if got := recoverySaved(t, one); !reflect.DeepEqual(got, before) ||
				!reflect.DeepEqual(one.record, before) || one.changed != changed || one.failure != nil {
				t.Fatal("unproven native cleanup committed or published partial stop evidence")
			}
			if kind == "agent" || kind == "unsupported" {
				if len(tried) != 1 {
					t.Fatal("terminal cleanup ran after an unconfirmed original Agent stop")
				}
			}
		})
	}
}

func TestTerminalRecoveryBoundAndAlreadyExpiredDeadline(t *testing.T) {
	for _, kind := range []string{"33-owners", "expired"} {
		t.Run(kind, func(t *testing.T) {
			one := recoveryOwner(t, recoveryRecord("reserved"))
			if kind == "33-owners" {
				// Corruption is intentionally not saved or passed to the kernel.
				phases := make([]string, 33)
				for i := range phases {
					phases[i] = "reserved"
				}
				one.record.Terminals = recoveryRecord(phases...).Terminals
			}
			calls := 0
			one.service.settle = func(procgroup.Identity, procgroup.Place, procgroup.Place, time.Duration) error {
				calls++
				return nil
			}
			deadline := time.Now().Add(recordedGroupWithin)
			if kind == "expired" {
				deadline = time.Now().Add(-time.Second)
			}
			if err := one.service.endRecordedGroup(one, deadline); err == nil || calls != 0 {
				t.Fatal("unbounded ownership or expired deadline reached the settler")
			}
		})
	}
}

func TestTerminalRecoveryUpdatesOlderCommandStopsAtomically(t *testing.T) {
	for _, reject := range []bool{false, true} {
		t.Run(fmt.Sprint(reject), func(t *testing.T) {
			record := recoveryRecord("active", "reserved")
			record.State.InputAccepted = 2
			record.CurrentCommand = "second"
			record.Commands["second"] = nodewire.SessionCommand{ID: "second", InputSequence: 2, State: nodewire.SessionCommandUncertain}
			record.CommandHashes["second"] = "second-input-hash"
			one := recoveryOwner(t, record)
			if len(one.record.Commands) != 1 {
				t.Fatal("fixture must use the cold current-only command projection")
			}
			before := one.copyLocked()
			store, err := one.service.recordsStore()
			if err != nil {
				t.Fatal(err)
			}
			if reject {
				if _, err := store.db.Exec(`CREATE TRIGGER deny_recovered_stop BEFORE UPDATE OF command ON session_commands
					WHEN OLD.id='input' BEGIN SELECT RAISE(ABORT,'isolated cleanup failure'); END`); err != nil {
					t.Fatal(err)
				}
			}
			one.service.settle = func(procgroup.Identity, procgroup.Place, procgroup.Place, time.Duration) error { return nil }
			err = one.service.endRecordedGroup(one, time.Now().Add(recordedGroupWithin))
			if reject && err == nil || !reject && err != nil {
				t.Fatalf("cleanup commit: reject=%v err=%v", reject, err)
			}
			got := recoverySaved(t, one)
			if reject {
				if !reflect.DeepEqual(got, before) || !reflect.DeepEqual(one.record, before) || one.failure == nil {
					t.Fatal("failed transaction published a header or partial terminal stop")
				}
			} else {
				recoveryAssertStopped(t, before, got)
			}
			for _, id := range []string{"input", "second"} {
				retained, found, err := store.read(record.State.ID, id)
				if err != nil || !found {
					t.Fatalf("older command missing: %s found=%v err=%v", id, found, err)
				}
				want := record.Commands[id]
				want.ProcessStopped = !reject
				if !reflect.DeepEqual(retained.Commands[id], want) || retained.CommandHashes[id] != record.CommandHashes[id] {
					t.Fatalf("cleanup rewrote command identity, result or settlement: %s", id)
				}
			}
		})
	}
}

func TestTerminalRecoveryLoadAndAdmittedRetryReachAggregate(t *testing.T) {
	one := recoveryOwner(t, recoveryRecord("active", "reserved"))
	service := one.service
	before := one.copyLocked()
	terminal := before.Terminals[fmt.Sprintf("nt_%064x", 1)].Process.Identity
	allowTerminal := false
	var calls []procgroup.Identity
	service.settle = func(id procgroup.Identity, _, _ procgroup.Place, _ time.Duration) error {
		calls = append(calls, id)
		if id == terminal && !allowTerminal {
			return procgroup.ErrUnproven
		}
		return nil
	}
	if err := service.load(); err != nil {
		t.Fatal(err)
	}
	if !service.unverifiedProcesses[before.State.ID] || len(service.sessions) != 0 {
		t.Fatal("restart resumed old callbacks or forgot unverified terminal cleanup")
	}
	if got := recoverySaved(t, one); got.State.ProcessStopped || !reflect.DeepEqual(got.Terminals, before.Terminals) {
		t.Fatal("restart load advertised a partial stop")
	}
	calls = nil
	req := nodeSessionRequest(nodewire.SessionActionKill)
	req.ID, req.CommandID, req.InputSequence = before.State.ID, "input", 1
	req.Binding.AttemptID = "other-attempt"
	if _, err := service.killRecorded(req); err == nil || len(calls) != 0 {
		t.Fatal("refused recovery request reached native cleanup")
	}
	req.Binding = before.State.Binding
	allowTerminal = true
	state, err := service.killRecorded(req)
	if err != nil || !state.ProcessStopped || state.Command == nil || !state.Command.ProcessStopped {
		t.Fatalf("admitted retry missed the aggregate: %+v err=%v", state, err)
	}
	if len(calls) != 2 || calls[0] != before.Process.Identity || calls[1] != terminal ||
		service.unverifiedProcesses[before.State.ID] {
		t.Fatal("retry omitted an original group or retained an obsolete unverified flag")
	}
}

func TestTerminalRecoveryFmt2AckIsNeverCleanup(t *testing.T) {
	for _, phase := range []string{"reserved", "preparing", "active", "stopping", "stopped"} {
		t.Run(phase, func(t *testing.T) {
			one, req, _ := ackFixture(t)
			next := one.copyLocked()
			next.Format, next.Generation = 2, 1
			next.Process = recoveryRecord().Process
			id := fmt.Sprintf("nt_%064x", 1)
			owner := recoveryRecord("reserved").Terminals[id]
			owner.UpstreamID, owner.Generation = next.UpstreamID, next.Generation
			owner.CommandID, owner.InputSequence = next.CurrentCommand, next.State.InputAccepted
			owner.Binding, owner.Authority = next.State.Binding, next.Authority
			// Advance through the actual reservation/transition validator,
			// retaining the same original immutable receipt.
			next.Terminals = map[string]sessionTerminal{id: owner}
			store, err := one.service.recordsStore()
			if err != nil {
				t.Fatal(err)
			}
			if err := one.commitLocked(next); err != nil {
				t.Fatal(err)
			}
			if phase == "stopped" {
				one.service.place, one.service.placeKnown = next.Process.Place, true
				one.service.settle = func(procgroup.Identity, procgroup.Place, procgroup.Place, time.Duration) error { return nil }
				if err := one.service.endRecordedGroup(one, time.Now().Add(recordedGroupWithin)); err != nil {
					t.Fatal(err)
				}
			} else if phase != "reserved" {
				step := one.copyLocked()
				slot := step.Terminals[id]
				slot.Phase = "preparing"
				slot.Preparation = terminalPreparation{PID: 600002, Start: 200, ParentGroup: next.Process.Group, Mark: "terminal-mark-0"}
				step.Terminals[id] = slot
				if err := one.commitLocked(step); err != nil {
					t.Fatal(err)
				}
				if phase == "active" || phase == "stopping" {
					step = one.copyLocked()
					slot = step.Terminals[id]
					slot.Phase, slot.Preparation, slot.Process = "active", terminalPreparation{}, recoveryRecord("active").Terminals[id].Process
					step.Terminals[id] = slot
					if err := one.commitLocked(step); err != nil {
						t.Fatal(err)
					}
					if phase == "stopping" {
						step = one.copyLocked()
						slot = step.Terminals[id]
						slot.Phase = "stopping"
						step.Terminals[id] = slot
						if err := one.commitLocked(step); err != nil {
							t.Fatal(err)
						}
					}
				}
			}
			before, changed := one.copyLocked(), one.changed
			calls := 0
			one.service.settle = func(procgroup.Identity, procgroup.Place, procgroup.Place, time.Duration) error {
				calls++
				return errors.New("ACK reached cleanup")
			}
			err = one.service.AcknowledgeReceipt(t.Context(), "cluster-1", req)
			if phase != "stopped" {
				if err == nil || !reflect.DeepEqual(one.copyLocked(), before) || one.changed != changed {
					t.Fatal("pending terminal ACK deleted evidence or published cleanup")
				}
				var count int
				if err := store.db.QueryRow(`SELECT count(*) FROM session_commands WHERE session_id=? AND id=?`,
					req.Receipt.SessionID, req.Receipt.CommandID).Scan(&count); err != nil || count != 1 {
					t.Fatal("pending terminal ACK removed the original command")
				}
			} else {
				if err != nil || !reflect.DeepEqual(one.record.Terminals, before.Terminals) || one.record.Process != before.Process {
					t.Fatalf("Fmt2 stopped-slot ACK changed cleanup identity: %v", err)
				}
				if err := one.service.AcknowledgeReceipt(t.Context(), "cluster-1", req); err != nil {
					t.Fatalf("exact Fmt2 ACK retry failed: %v", err)
				}
			}
			if calls != 0 {
				t.Fatal("receipt acknowledgement ran native cleanup")
			}
		})
	}
}

func TestTerminalRecoveryFmt2ResumeRequiresCompleteAggregate(t *testing.T) {
	req := nodeSessionRequest(nodewire.SessionActionOpen)
	req.Harness, req.CommandID = "mock", "next-open"
	record := recoveryRecord("active", "reserved")
	record.State.Harness, record.ConfigHash = req.Harness, sessionConfigHash(req)
	command := record.Commands["input"]
	command.State, command.Settled = nodewire.SessionCommandCompleted, true
	record.Commands["input"] = command
	one := recoveryOwner(t, record)
	req.ID, req.Binding.AttemptID = record.State.ID, "next-attempt"
	source, err := one.service.resumeSourceLocked(req, "next-native-owner")
	if err == nil || source != nil {
		t.Fatal("cold resume treated settled receipts as complete native cleanup")
	}
	one.service.settle = func(procgroup.Identity, procgroup.Place, procgroup.Place, time.Duration) error { return nil }
	if err := one.service.endRecordedGroup(one, time.Now().Add(recordedGroupWithin)); err != nil {
		t.Fatal(err)
	}
	source, err = one.service.resumeSourceLocked(req, "next-native-owner")
	if err != nil || source == nil || source.Format != 2 ||
		!source.State.ProcessStopped || !sessionTerminalsStopped(*source) {
		t.Fatalf("complete Fmt2 cleanup could not reach cold handoff: source=%+v err=%v", source, err)
	}
}

// This entrypoint reuses the product's existing inert helper. It neither
// duplicates the helper implementation nor sends its EXEC payload gate.
func TestTerminalRecoveryHelperProcess(t *testing.T) {
	if os.Getenv("STEVE_TERMINAL_RECOVERY_HELPER") != "1" {
		return
	}
	handled, err := acphost.RunTerminalChild([]string{acphost.TerminalChildVerb})
	if !handled || err != nil {
		os.Exit(2)
	}
	os.Exit(0)
}

func recoveryInertChild(t *testing.T, original procgroup.Identity) (*exec.Cmd, *os.File, *bufio.Reader, procgroup.Preparation, string) {
	t.Helper()
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	control, gate, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { control.Close(); gate.Close() })
	status, notice, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { status.Close(); notice.Close() })
	cwd, err := os.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cwd.Close() })
	executable, err := os.Open(self)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { executable.Close() })
	cmd := exec.Command(self, "-test.run=^TestTerminalRecoveryHelperProcess$")
	cmd.Env = append(os.Environ(), "STEVE_TERMINAL_RECOVERY_HELPER=1", procgroup.MarkVariable+"="+original.Mark)
	cmd.ExtraFiles = []*os.File{control, cwd, notice, executable}
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true, Pgid: original.Group}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	// Never reap before cleanup proof: this direct child pins its real PID.
	t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })
	control.Close()
	notice.Close()
	limit := time.Now().Add(5 * time.Second)
	if err := gate.SetWriteDeadline(limit); err != nil {
		t.Fatal(err)
	}
	if err := status.SetReadDeadline(limit); err != nil {
		t.Fatal(err)
	}
	mark := procgroup.NewMark()
	if err := json.NewEncoder(gate).Encode(map[string]any{
		"command": "/bin/true", "args": []string{}, "env": []string{},
		"parent_group": original.Group, "parent_mark": original.Mark, "mark": mark,
	}); err != nil {
		t.Fatal(err)
	}
	reader := bufio.NewReader(status)
	recoveryNotice(t, reader, "preparing", cmd.Process.Pid, original.Group)
	prep, err := procgroup.CapturePreparation(cmd.Process.Pid, original)
	if err != nil {
		t.Fatal(err)
	}
	return cmd, gate, reader, prep, mark
}

func recoveryNotice(t *testing.T, reader *bufio.Reader, phase string, pid, group int) {
	t.Helper()
	raw, err := reader.ReadBytes('\n')
	if err != nil {
		t.Fatal(err)
	}
	var notice struct {
		Phase      string
		PID, Group int
	}
	if err := json.Unmarshal(raw, &notice); err != nil ||
		notice.Phase != phase || notice.PID != pid || notice.Group != group {
		t.Fatalf("real helper notice differs: %s err=%v", raw, err)
	}
}

// Use the existing group-launch primitive and real Capture. No node service,
// provider, agent build or payload process is required for these kernel tests.
func recoveryOriginalGroup(t *testing.T) sessionProcess {
	t.Helper()
	place, err := procgroup.Here()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("/bin/sh", "-c", "exec sleep 60")
	mark := procgroup.NewMark()
	cmd.Env = append(os.Environ(), procgroup.MarkVariable+"="+mark)
	setProcessGroup(cmd)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })
	id, err := procgroup.Capture(cmd.Process.Pid, mark)
	if err != nil {
		t.Fatal(err)
	}
	return sessionProcess{Identity: id, Place: place}
}

func TestTerminalRecoveryRealOriginalGroupAndUnsplitPreparation(t *testing.T) {
	original := recoveryOriginalGroup(t)
	record := recoveryRecord("reserved", "reserved", "active", "stopping")
	record.Process = original
	for _, n := range []int{3, 4} {
		id := fmt.Sprintf("nt_%064x", n)
		owner := record.Terminals[id]
		owner.Process = recoveryOriginalGroup(t)
		record.Terminals[id] = owner
	}
	one := recoveryOwner(t, record)
	cmd, _, _, prep, mark := recoveryInertChild(t, record.Process.Identity)
	id := fmt.Sprintf("nt_%064x", 2)
	next := one.copyLocked()
	owner := next.Terminals[id]
	owner.Phase = "preparing"
	owner.Preparation = terminalPreparation{PID: prep.PID, Start: prep.Start, ParentGroup: prep.ParentGroup, Mark: mark}
	next.Terminals[id] = owner
	if err := one.commitLocked(next); err != nil {
		t.Fatal(err)
	}
	one.service.settle = procgroup.Settle
	before := one.copyLocked()
	if err := one.service.endRecordedGroup(one, time.Now().Add(recordedGroupWithin)); err != nil {
		t.Fatal(err)
	}
	recoveryAssertStopped(t, before, recoverySaved(t, one))
	if liveProcess(cmd.Process.Pid) || liveProcess(original.Leader) {
		t.Fatal("real aggregate was confirmed while an original unsplit owner still ran")
	}
	for _, owner := range before.Terminals {
		if owner.Process.Group != 0 && liveProcess(owner.Process.Leader) {
			t.Fatal("real aggregate omitted a separate active or stopping terminal group")
		}
	}
}

func TestTerminalRecoveryRealSplitUsesOriginalLeaderAndComparablePlace(t *testing.T) {
	original := recoveryOriginalGroup(t)
	record := recoveryRecord("reserved")
	record.Process = original
	one := recoveryOwner(t, record)
	cmd, gate, reader, prep, mark := recoveryInertChild(t, original.Identity)
	id := fmt.Sprintf("nt_%064x", 1)
	next := one.copyLocked()
	owner := next.Terminals[id]
	owner.Phase = "preparing"
	owner.Preparation = terminalPreparation{PID: prep.PID, Start: prep.Start, ParentGroup: prep.ParentGroup, Mark: mark}
	next.Terminals[id] = owner
	if err := one.commitLocked(next); err != nil {
		t.Fatal(err)
	}
	if _, err := gate.WriteString("SPLIT\n"); err != nil {
		t.Fatal(err)
	}
	recoveryNotice(t, reader, "active", cmd.Process.Pid, cmd.Process.Pid)
	actual, err := prep.CapturedGroup(mark)
	if err != nil || actual.Start != prep.Start || actual.Group != prep.PID {
		t.Fatalf("real split Capture: %+v err=%v", actual, err)
	}
	one.service.settle = procgroup.Settle
	before := one.copyLocked()
	// Even the direct transition-helper negatives obey its original-first
	// precondition. The split helper is still alive in its separately owned group.
	if err := procgroup.Settle(original.Identity, original.Place, original.Place, recordedGroupWithin); err != nil {
		t.Fatal(err)
	}
	for _, kind := range []string{"pid-reuse", "machine", "boot", "namespace"} {
		t.Run(kind, func(t *testing.T) {
			p, ran, here := prep, original.Place, original.Place
			switch kind {
			case "pid-reuse":
				p.Start++ // Deliberate corrupt identity must never signal the real child.
			case "machine":
				ran.Machine = "another-machine"
			case "boot":
				ran.Boot, ran.Machine = "another-boot", ""
			case "namespace":
				ran.Namespace = "another-namespace"
			}
			err := procgroup.SettlePreparation(p, original.Identity, mark, ran, here, time.Now().Add(50*time.Millisecond))
			if !errors.Is(err, procgroup.ErrUnproven) || !liveProcess(cmd.Process.Pid) {
				t.Fatalf("mismatched split proof signalled a real child: %v", err)
			}
		})
	}
	err = one.service.endRecordedGroup(one, time.Now().Add(500*time.Millisecond))
	if err != nil {
		t.Fatal("exact original split leader was not recovered:", err)
	}
	recoveryAssertStopped(t, before, recoverySaved(t, one))
	if liveProcess(cmd.Process.Pid) {
		t.Fatal("original split leader remained live after aggregate stop")
	}
	// The helper has never received EXEC; a verified leader Capture, not an
	// environment mark rewritten by Setenv, supplies its signal ownership.
}
