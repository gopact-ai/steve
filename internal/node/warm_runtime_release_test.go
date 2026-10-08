package node

import (
	"context"
	"errors"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/gopact-ai/steve/internal/acphost"
	"github.com/gopact-ai/steve/internal/harness"
	"github.com/gopact-ai/steve/internal/nodewire"
	"github.com/gopact-ai/steve/internal/plugins"
	"github.com/gopact-ai/steve/internal/procgroup"
)

// The fixture uses a real temporary SQLite store and a real original plugin
// use. Its empty Host starts no native process, provider or network listener.
func warmReleaseFixture(t *testing.T, cold bool, format int) (*ownedSession, nodewire.SessionRequest, plugins.RuntimeRef) {
	t.Helper()
	server := NewServer(ServerConfig{Name: "worker", StateDir: t.TempDir()})
	selection := nodeRuntimeFixture(t, server, "")
	runtime, err := server.pluginRuntimePool().Prepare(t.Context(), "warm-release", selection,
		harness.Config{Command: "never-executed", Permission: "read"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(server.closePluginRuntimes)
	ctx, cancel := context.WithCancel(t.Context())
	t.Cleanup(cancel)
	service := &SessionService{
		server: server, ctx: ctx, cancel: cancel,
		sessions: map[string]*ownedSession{}, unverifiedProcesses: map[string]bool{},
	}
	t.Cleanup(service.closeRecords)
	req := nodeSessionRequest(nodewire.SessionActionOpen)
	req.Harness, req.CommandID, req.TerminalAdmission = "mock", "original-open", format == 2
	req.Plugin, req.Binding.PluginRuntimeID = runtime.Ref.Clone(), runtime.Ref.ID
	id := "ns_" + strings.Repeat("e", 64)
	req.ID = id
	record := sessionRecord{
		Format: format, ClusterID: req.Authority.ClusterID, Authority: req.Authority,
		OpenID: req.CommandID, ConfigHash: sessionConfigHash(req),
		State: nodewire.SessionState{
			ID: id, ContextID: id, Binding: req.Binding, Harness: req.Harness,
			State: nodewire.SessionInterrupted, Plugin: runtime.Ref.Clone(), TerminalAdmission: req.TerminalAdmission,
		},
		Commands: map[string]nodewire.SessionCommand{}, CommandHashes: map[string]string{},
	}
	if cold {
		// An original preparation that never started native work already has
		// a durable stop, but cancelling its opening still requires a commit.
		record.State.State, record.State.ProcessStopped = nodewire.SessionOpening, true
	}
	one := &ownedSession{
		service: service, host: acphost.New(acphost.Config{}),
		changed: make(chan struct{}), processConfigHash: processConfigHash(req),
	}
	t.Cleanup(one.host.Close)
	if !one.host.AllProcessesStopped() {
		t.Fatal("empty Host unexpectedly owns native work")
	}
	if err := one.commitLocked(record); err != nil {
		t.Fatal(err)
	}
	if !cold {
		service.sessions[id] = one
	}
	if err := server.pluginStore().BeginRuntimeUse(t.Context(), runtime.Ref, "session/"+id, "session"); err != nil {
		t.Fatal(err)
	}
	warmReleaseUse(t, one, runtime.Ref, false)
	return one, req, runtime.Ref
}

func warmReleaseUse(t *testing.T, one *ownedSession, ref plugins.RuntimeRef, stopped bool) {
	t.Helper()
	// Read the use through another Store, not an in-memory expectation.
	store := &plugins.Store{Dir: one.service.server.pluginStore().Dir}
	info, err := store.RuntimeInfo(ref.ID)
	if err != nil || len(info.Uses) != 1 {
		t.Fatalf("original plugin use disappeared: %+v err=%v", info, err)
	}
	use := info.Uses[0]
	if use.ID != "session/"+one.record.State.ID || use.Kind != "session" ||
		use.Stopped != stopped || !reflect.DeepEqual(use.Runtime, ref) {
		t.Fatalf("original runtime use changed or was released early: %+v", use)
	}
}

func warmReleaseSaved(t *testing.T, one *ownedSession) sessionRecord {
	t.Helper()
	one.service.recordsMu.Lock()
	store, storeErr := one.service.records, one.service.recordsErr
	one.service.recordsMu.Unlock()
	if store != nil && storeErr == nil {
		record, exists, err := store.read(one.record.State.ID, "")
		if err != nil || !exists {
			t.Fatalf("original session evidence disappeared: exists=%v err=%v", exists, err)
		}
		return record
	}
	// Close() intentionally closes its SQLite connection. Reopen only this
	// fixture's isolated database to check the durable evidence afterwards.
	store, err := openSessionRecords(filepath.Join(one.service.directory(), "sessions.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.close()
	record, exists, err := store.read(one.record.State.ID, "")
	if err != nil || !exists {
		t.Fatalf("original session evidence disappeared: exists=%v err=%v", exists, err)
	}
	return record
}

func warmReleaseInvoke(one *ownedSession, req nodewire.SessionRequest, mode string, cause error) error {
	switch mode {
	case "archive":
		return one.service.archiveStoppedSession(req)
	case "failed-open":
		_, err := one.stateAfterFailedOpen(cause)
		return err
	case "service-close":
		one.service.Close()
		return one.failure
	case "recorded-cancel":
		req.Action = nodewire.SessionActionCancelOpen
		one.service.mu.Lock()
		defer one.service.mu.Unlock()
		_, _, err := one.service.reconcileRecordedOpenLocked(req.ID, req)
		return err
	default:
		panic("unknown warm-release test mode")
	}
}

func TestWarmRuntimeUseRetainedWhenSQLiteStopCommitFails(t *testing.T) {
	for _, mode := range []string{"archive", "failed-open", "service-close", "recorded-cancel"} {
		t.Run(mode, func(t *testing.T) {
			one, req, ref := warmReleaseFixture(t, mode == "recorded-cancel", 2)
			before := warmReleaseSaved(t, one)
			store, err := one.service.recordsStore()
			if err != nil {
				t.Fatal(err)
			}
			// Failure comes from the real transaction, not a manually assigned
			// one.failure or a mock that skipped committing the proposed stop.
			if _, err := store.db.Exec(`CREATE TRIGGER deny_warm_stop BEFORE UPDATE OF header ON sessions
				BEGIN SELECT RAISE(ABORT,'forced_warm_release_failure'); END`); err != nil {
				t.Fatal(err)
			}
			cause := errors.New("original native open failed")
			err = warmReleaseInvoke(one, req, mode, cause)
			if err == nil || !strings.Contains(err.Error(), "forced_warm_release_failure") {
				t.Fatalf("warm path did not reach its SQLite failure trigger: %v", err)
			}
			if mode == "failed-open" && !errors.Is(err, cause) {
				t.Fatal("failed-open lost the original error")
			}
			if after := warmReleaseSaved(t, one); !reflect.DeepEqual(before, after) {
				t.Fatal("failed stop commit changed original durable evidence")
			}
			warmReleaseUse(t, one, ref, false)
			if mode == "archive" && one.service.sessions[req.ID] != one {
				t.Fatal("failed archive discarded its original live owner")
			}
			if err := one.service.server.pluginStore().RetireRuntime(t.Context(), ref); err != nil {
				t.Fatal(err)
			}
			if err := one.service.server.pluginStore().CheckRuntimeRemovable(ref); !errors.Is(err, plugins.ErrRuntimeBusy) {
				t.Fatalf("failed durable stop made its original runtime removable: %v", err)
			}
		})
	}
}

func TestWarmRuntimeUseReleasedAfterSuccessfulDurableStop(t *testing.T) {
	for _, mode := range []string{"archive", "failed-open", "service-close", "recorded-cancel"} {
		t.Run(mode, func(t *testing.T) {
			one, req, ref := warmReleaseFixture(t, mode == "recorded-cancel", 2)
			before := warmReleaseSaved(t, one)
			cause := errors.New("original native open failed")
			err := warmReleaseInvoke(one, req, mode, cause)
			if mode == "failed-open" {
				if !errors.Is(err, cause) {
					t.Fatalf("failed-open lost its original error: %v", err)
				}
			} else if err != nil {
				t.Fatal(err)
			}
			after := warmReleaseSaved(t, one)
			if !after.State.ProcessStopped || !sessionTerminalsStopped(after) ||
				after.State.Sequence != before.State.Sequence+1 || after.Process != before.Process ||
				!reflect.DeepEqual(after.State.Plugin, before.State.Plugin) {
				t.Fatal("runtime use was released without its original committed aggregate stop")
			}
			warmReleaseUse(t, one, ref, true)
			if mode == "archive" && one.service.sessions[req.ID] != nil {
				t.Fatal("successful archive kept the old live owner")
			}
			if err := one.service.server.pluginStore().RetireRuntime(t.Context(), ref); err != nil {
				t.Fatal(err)
			}
			if err := one.service.server.pluginStore().CheckRuntimeRemovable(ref); err != nil {
				t.Fatalf("durably stopped original use remained busy: %v", err)
			}
		})
	}
}

func TestWarmRuntimeReleaseRejectsProposedStopInsteadOfDurableEvidence(t *testing.T) {
	for _, mode := range []string{"unsaved-stop", "unsaved-terminal-stop", "latched-write-failure", "unavailable-store", "different-process", "different-runtime", "different-sequence"} {
		t.Run(mode, func(t *testing.T) {
			one, _, ref := warmReleaseFixture(t, false, 2)
			if mode == "unsaved-terminal-stop" {
				// These synthetic cleanup IDs exercise only the metadata/durable
				// boundary. They are never passed to any native process operation.
				next := one.copyLocked()
				next.UpstreamID, next.Generation, next.State.InputAccepted = "original-agent", 1, 1
				next.Process = sessionProcess{
					Identity: procgroup.Identity{Group: 600001, Leader: 600001, Start: 100, Mark: "original-agent-mark"},
					Place:    procgroup.Place{Machine: "synthetic-machine", Boot: "synthetic-boot"},
				}
				id := "nt_" + strings.Repeat("f", 64)
				next.Terminals = map[string]sessionTerminal{id: {
					ID: id, UpstreamID: next.UpstreamID, Generation: next.Generation,
					CommandID: "original-input", InputSequence: 1, Binding: next.State.Binding,
					Authority: next.Authority, Phase: "reserved",
				}}
				if err := one.commitLocked(next); err != nil {
					t.Fatal(err)
				}
			}
			if mode == "different-process" || mode == "different-runtime" || mode == "different-sequence" {
				next := one.copyLocked()
				next.State.ProcessStopped = true // Empty Host: no native process ever started.
				if err := one.commitLocked(next); err != nil {
					t.Fatal(err)
				}
			}
			before := warmReleaseSaved(t, one)
			proposed := one.copyLocked()
			proposed.State.ProcessStopped = true
			switch mode {
			case "unsaved-terminal-stop":
				for id, owner := range proposed.Terminals {
					owner.Phase = "stopped"
					proposed.Terminals[id] = owner
				}
			case "latched-write-failure":
				store, err := one.service.recordsStore()
				if err != nil {
					t.Fatal(err)
				}
				if _, err := store.db.Exec(`CREATE TRIGGER deny_warm_stop BEFORE UPDATE OF header ON sessions
					BEGIN SELECT RAISE(ABORT,'forced_warm_release_failure'); END`); err != nil {
					t.Fatal(err)
				}
				if err := one.commitLocked(proposed); err == nil {
					t.Fatal("SQLite trigger did not reject the proposed stop")
				}
			case "unavailable-store":
				one.service.closeRecords()
			case "different-process":
				proposed.Process.Identity = procgroup.Identity{Group: 600009, Leader: 600009, Start: 100, Mark: "other-group"}
			case "different-runtime":
				proposed.State.Plugin = proposed.State.Plugin.Clone()
				proposed.State.Plugin.Selection.Project = "other-project"
				if err := proposed.State.Plugin.Validate(); err != nil {
					t.Fatalf("test requires a well-formed but different runtime: %v", err)
				}
			case "different-sequence":
				proposed.State.Sequence++
			}
			if err := one.service.endStoppedRuntime(proposed); err == nil {
				t.Fatal("proposed or unavailable durable stop released the original use")
			}
			warmReleaseUse(t, one, ref, false)
			if after := warmReleaseSaved(t, one); !reflect.DeepEqual(before, after) {
				t.Fatal("runtime-release refusal changed durable session evidence")
			}
		})
	}
}

func TestWarmRuntimeUseRetainedWhenHostStopOmitsTerminal(t *testing.T) {
	for _, mode := range []string{"archive", "failed-open", "service-close"} {
		t.Run(mode, func(t *testing.T) {
			one, req, ref := warmReleaseFixture(t, false, 2)
			// Model a retained reservation whose owner has not recorded cleanup.
			// The empty Host's transport claim must not manufacture that fact.
			next := one.copyLocked()
			next.UpstreamID, next.Generation, next.State.InputAccepted = "original-agent", 1, 1
			next.Process = sessionProcess{
				Identity: procgroup.Identity{Group: 600001, Leader: 600001, Start: 100, Mark: "original-agent-mark"},
				Place:    procgroup.Place{Machine: "synthetic-machine", Boot: "synthetic-boot"},
			}
			id := "nt_" + strings.Repeat("f", 64)
			next.Terminals = map[string]sessionTerminal{id: {
				ID: id, UpstreamID: next.UpstreamID, Generation: next.Generation,
				CommandID: "original-input", InputSequence: 1, Binding: next.State.Binding,
				Authority: next.Authority, Phase: "reserved",
			}}
			if err := one.commitLocked(next); err != nil {
				t.Fatal(err)
			}
			before := warmReleaseSaved(t, one)
			if err := warmReleaseInvoke(one, req, mode, errors.New("original native open failed")); err == nil {
				t.Fatal("Host stop omitted a retained terminal but its commit succeeded")
			}
			if after := warmReleaseSaved(t, one); !reflect.DeepEqual(before, after) ||
				after.State.ProcessStopped || sessionTerminalsStopped(after) {
				t.Fatal("Host stop changed original terminal cleanup ownership")
			}
			warmReleaseUse(t, one, ref, false)
		})
	}
}

func TestWarmRuntimeFormat1PhysicalStopCleanupSurvivesSQLiteReceiptFailure(t *testing.T) {
	for _, mode := range []string{"archive", "failed-open", "service-close", "recorded-cancel"} {
		t.Run(mode, func(t *testing.T) {
			one, req, ref := warmReleaseFixture(t, mode == "recorded-cancel", 1)
			before := warmReleaseSaved(t, one)
			if before.Format != 1 || len(before.Terminals) != 0 || before.State.TerminalAdmission {
				t.Fatal("legacy fixture must have no terminal-aware ownership")
			}
			store, err := one.service.recordsStore()
			if err != nil {
				t.Fatal(err)
			}
			if _, err := store.db.Exec(`CREATE TRIGGER deny_warm_stop BEFORE UPDATE OF header ON sessions
				BEGIN SELECT RAISE(ABORT,'forced_warm_release_failure'); END`); err != nil {
				t.Fatal(err)
			}
			cause := errors.New("original native open failed")
			err = warmReleaseInvoke(one, req, mode, cause)
			if err == nil || !strings.Contains(err.Error(), "forced_warm_release_failure") {
				t.Fatalf("legacy receipt failure was hidden or its trigger was missed: %v", err)
			}
			if mode == "failed-open" && !errors.Is(err, cause) {
				t.Fatal("legacy failed-open lost the original cause")
			}
			if !one.host.AllProcessesStopped() {
				t.Fatal("legacy test lacks real Host physical-stop evidence")
			}
			if after := warmReleaseSaved(t, one); !reflect.DeepEqual(before, after) {
				t.Fatal("failed legacy receipt commit changed durable evidence")
			}
			warmReleaseUse(t, one, ref, true)
		})
	}
}

func TestWarmRuntimeFormat1PhysicalCleanupDoesNotRequireReadableReceipt(t *testing.T) {
	one, _, ref := warmReleaseFixture(t, false, 1)
	before := warmReleaseSaved(t, one)
	one.host.Close()
	if !one.host.AllProcessesStopped() {
		t.Fatal("legacy cleanup lacks real Host physical-stop evidence")
	}
	physical := one.copyLocked()
	physical.State.ProcessStopped = true
	one.service.closeRecords()
	if err := one.service.endStoppedRuntime(physical); err != nil {
		t.Fatalf("legacy physical cleanup acquired a new receipt-read dependency: %v", err)
	}
	warmReleaseUse(t, one, ref, true)
	if after := warmReleaseSaved(t, one); !reflect.DeepEqual(before, after) {
		t.Fatal("legacy physical cleanup manufactured a durable stop receipt")
	}
}
