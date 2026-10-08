package node

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gopact-ai/acp"
	"github.com/gopact-ai/steve/internal/acphost"
	"github.com/gopact-ai/steve/internal/nodewire"
	"github.com/gopact-ai/steve/internal/procgroup"
)

func readyTerminalAdmission(t *testing.T) (*ownedSession, nodewire.SessionRequest, *int) {
	t.Helper()
	dir := t.TempDir()
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	store, err := openSessionRecords(filepath.Join(dir, "sessions.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.close() })
	record := terminalOwnerRecord(t, "active")
	record.Format = 2
	record.State.State = nodewire.SessionRunning
	record.State.TerminalAdmission = true
	record.State.Sequence = 1
	record.Commands[record.CurrentCommand] = nodewire.SessionCommand{
		ID: record.CurrentCommand, InputSequence: 1, State: nodewire.SessionCommandRunning,
	}
	id := "nt_" + strings.Repeat("b", 64)
	owner := record.Terminals[id]
	owner.Process = sessionProcess{Identity: procgroup.Identity{Group: 123, Leader: 123, Start: 456, Mark: "terminal-mark"}, Place: record.Process.Place}
	record.Terminals[id] = owner
	if err := store.save(sessionRecord{}, record); err != nil {
		t.Fatal(err)
	}
	one := &ownedSession{
		service: &SessionService{records: store, ctx: t.Context(), server: NewServer(ServerConfig{Name: "worker", SessionAuthorizer: &sessionAuthorityTest{epoch: 1, writer: 1}})},
		record:  record, changed: make(chan struct{}),
		terminalStarts:    map[string]*readyTerminalStart{},
		terminalAdmission: true,
	}
	one.service.sessions = map[string]*ownedSession{record.State.ID: one}
	req := nodeSessionRequest(nodewire.SessionActionTerminalAdmit)
	req.ID, req.CommandID, req.InputSequence = record.State.ID, record.CurrentCommand, 1
	start := nodewire.TerminalStart{ID: id, CommandID: req.CommandID, InputSequence: 1, Generation: 1}
	req.TerminalStart = &start
	count := new(int)
	one.terminalStarts[id] = &readyTerminalStart{
		Start: start, Validate: func() error { return nil },
		Consume: func(context.Context) error { *count++; return nil },
	}
	return one, req, count
}

func TestTerminalAdmissionRejectsDifferentInputSequence(t *testing.T) {
	for _, sequence := range []uint64{0, 2} {
		t.Run(string(rune('0'+sequence)), func(t *testing.T) {
			one, req, count := readyTerminalAdmission(t)
			req.InputSequence = sequence
			if _, err := one.service.Do(t.Context(), "cluster-1", req); err == nil {
				t.Fatal("a different original input consumed the terminal payload gate")
			}
			if *count != 0 || one.record.Terminals[req.TerminalStart.ID].PayloadStarted {
				t.Fatal("refused input changed once-only payload state")
			}
		})
	}
}

func TestTerminalAdmissionConsumesOriginalGateAtMostOnce(t *testing.T) {
	one, req, count := readyTerminalAdmission(t)
	if _, err := one.service.Do(t.Context(), "cluster-1", req); err != nil {
		t.Fatal(err)
	}
	if _, err := one.service.Do(t.Context(), "cluster-1", req); err != nil {
		t.Fatal(err)
	}
	if *count != 1 || !one.record.Terminals[req.TerminalStart.ID].PayloadStarted {
		t.Fatal("original admission was omitted or replayed")
	}
}

func TestTerminalAdmissionRequiresBilateralNegotiation(t *testing.T) {
	one, req, count := readyTerminalAdmission(t)
	one.terminalAdmission = false
	if len(one.projectedTerminalStartsLocked(req.CommandID)) != 0 {
		t.Fatal("legacy parent received an implicitly executable terminal intent")
	}
	if _, err := one.service.Do(t.Context(), "cluster-1", req); err == nil || *count != 0 {
		t.Fatal("unnegotiated native session consumed the terminal payload")
	}
}

func TestTerminalConfigHashBindsAdmissionWithoutChangingLegacyHash(t *testing.T) {
	req := nodeSessionRequest(nodewire.SessionActionOpen)
	req.Harness, req.Workdir, req.Permission = "mock", "/workspace", "read"
	want := sessionHash(struct {
		Plugin                       any `json:"plugin,omitempty"`
		Harness, Workdir, Permission string
		Servers                      []acp.MCPServer
		NativeImport                 any `json:"native_import,omitempty"`
	}{nil, req.Harness, req.Workdir, req.Permission, req.MCPServers, nil})
	if sessionConfigHash(req) != want || processConfigHash(req) != want {
		t.Fatal("false negotiation changed the original configuration hash")
	}
	req.TerminalAdmission = true
	if sessionConfigHash(req) == want || processConfigHash(req) == want {
		t.Fatal("admission capability can change without changing the original configuration")
	}
}

func TestTerminalOwnerServiceContextCannotExecutePayload(t *testing.T) {
	one, req, count := readyTerminalAdmission(t)
	one.terminalStarts = nil
	o := nodeTerminalOwner{one}
	intent := acphost.TerminalIntent{ID: req.TerminalStart.ID, SessionID: acp.SessionID(one.record.UpstreamID), Generation: 1}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() {
		done <- o.Admit(ctx, intent, func(context.Context) error { *count++; return nil }, func() error { return nil })
	}()
	deadline := time.Now().Add(time.Second)
	for {
		one.mu.Lock()
		pending := len(one.projectedTerminalStartsLocked(req.CommandID)) == 1
		one.mu.Unlock()
		if pending {
			break
		}
		if time.Now().After(deadline) {
			cancel()
			<-done
			t.Fatal("original durable terminal did not expose its inert pending gate")
		}
		time.Sleep(time.Millisecond)
	}
	if *count != 0 {
		t.Fatal("node service context executed payload without a fresh request")
	}
	cancel()
	if err := <-done; err != context.Canceled {
		t.Fatalf("abandoned inert gate returned %v", err)
	}
	if _, err := one.service.Do(t.Context(), "cluster-1", req); err == nil || *count != 0 {
		t.Fatal("canceled old gate became executable later")
	}
}

func TestTerminalOwnerFreshAdmissionReturnsTheOnceOnlyOutcome(t *testing.T) {
	one, req, count := readyTerminalAdmission(t)
	one.terminalStarts = nil
	o := nodeTerminalOwner{one}
	intent := acphost.TerminalIntent{ID: req.TerminalStart.ID, SessionID: acp.SessionID(one.record.UpstreamID), Generation: 1}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		done <- o.Admit(ctx, intent, func(context.Context) error { *count++; cancel(); return nil }, func() error { return nil })
	}()
	deadline := time.Now().Add(time.Second)
	for {
		one.mu.Lock()
		pending := len(one.projectedTerminalStartsLocked(req.CommandID)) == 1
		one.mu.Unlock()
		if pending {
			break
		}
		if time.Now().After(deadline) {
			cancel()
			<-done
			t.Fatal("terminal gate did not become ready")
		}
		time.Sleep(time.Millisecond)
	}
	if _, err := one.service.Do(t.Context(), "cluster-1", req); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil || *count != 1 {
		t.Fatalf("successful gate was canceled or not consumed: %v count=%d", err, *count)
	}
	if _, err := one.service.Do(t.Context(), "cluster-1", req); err != nil || *count != 1 {
		t.Fatal("repeated original admission changed the consumed payload")
	}
}

func TestTerminalAdmissionRechecksAuthorityAfterOriginalOwnerWait(t *testing.T) {
	one, req, count := readyTerminalAdmission(t)
	var revoked atomic.Bool
	first := make(chan struct{})
	var checks atomic.Int32
	one.service.server = NewServer(ServerConfig{Name: "worker", SessionAuthorizer: sessionAuthorizerFunc(
		func(context.Context, string, nodewire.SessionAuthority, nodewire.SessionBinding, nodewire.SessionAction) error {
			if checks.Add(1) == 1 {
				close(first)
			}
			if revoked.Load() {
				return errNoReceiptAuthority
			}
			return nil
		},
	)})
	one.mu.Lock()
	done := make(chan error, 1)
	go func() {
		_, err := one.service.Do(t.Context(), "cluster-1", req)
		done <- err
	}()
	<-first
	revoked.Store(true)
	one.mu.Unlock()
	if err := <-done; err == nil {
		t.Fatal("terminal payload borrowed authority from before the original owner wait")
	}
	if *count != 0 || one.record.Terminals[req.TerminalStart.ID].PayloadStarted {
		t.Fatal("revoked fresh terminal authority still consumed the payload")
	}
}

func TestTerminalAdmissionStartsWithBoundedAuthorityWindow(t *testing.T) {
	one, req, count := readyTerminalAdmission(t)
	var bounded bool
	started := time.Now()
	one.service.server = NewServer(ServerConfig{Name: "worker", SessionAuthorizer: sessionAuthorizerFunc(
		func(ctx context.Context, _ string, _ nodewire.SessionAuthority, _ nodewire.SessionBinding, _ nodewire.SessionAction) error {
			deadline, exists := ctx.Deadline()
			bounded = exists && deadline.After(started) && !deadline.After(started.Add(nodewire.TerminalAdmissionWindow+100*time.Millisecond))
			return errNoReceiptAuthority
		},
	)})
	if _, err := one.service.Do(t.Context(), "cluster-1", req); err == nil {
		t.Fatal("refused terminal authority was ignored")
	}
	if !bounded || *count != 0 {
		t.Fatal("terminal authority has no bounded original payload-consumption window")
	}
}

func TestTerminalOwnerCommitsEachOriginalCleanupStageBeforeAdmission(t *testing.T) {
	one, req, _ := readyTerminalAdmission(t)
	one.terminalStarts = nil
	o := nodeTerminalOwner{one}
	intent := acphost.TerminalIntent{ID: "nt_" + strings.Repeat("c", 64), SessionID: acp.SessionID(one.record.UpstreamID), Generation: 1}
	start := one.record.State.Sequence
	if err := o.Reserve(t.Context(), intent); err != nil {
		t.Fatal(err)
	}
	if one.record.State.Sequence != start+1 || one.record.Terminals[intent.ID].Phase != "reserved" {
		t.Fatal("terminal preparation ran without a durable original reservation")
	}
	preparation := procgroup.Preparation{PID: 321, Start: 789, ParentGroup: one.record.Process.Group}
	if err := o.Prepared(t.Context(), intent, preparation, "original-terminal-mark"); err != nil {
		t.Fatal(err)
	}
	if one.record.State.Sequence != start+2 || one.record.Terminals[intent.ID].Phase != "preparing" {
		t.Fatal("split gate has no durable original child transition")
	}
	identity := procgroup.Identity{Group: preparation.PID, Leader: preparation.PID, Start: preparation.Start, Mark: "original-terminal-mark"}
	if err := o.Active(t.Context(), intent, identity, one.record.Process.Place); err != nil {
		t.Fatal(err)
	}
	owner := one.record.Terminals[intent.ID]
	if one.record.State.Sequence != start+3 || owner.Phase != "active" || owner.PayloadStarted || owner.Binding != req.Binding || owner.CommandID != req.CommandID || owner.InputSequence != req.InputSequence {
		t.Fatal("active cleanup changed the original input tuple or admitted its payload")
	}
	if err := o.Stopped(t.Context(), intent); err != nil {
		t.Fatal(err)
	}
	stopped := one.record.Terminals[intent.ID]
	if stopped.Phase != "stopped" || stopped.Process != owner.Process || stopped.PayloadStarted {
		t.Fatal("stopped cleanup replaced its original group or executed a canceled payload")
	}
	if err := o.Active(t.Context(), intent, identity, one.record.Process.Place); err == nil {
		t.Fatal("old stopped intent was reopened")
	}
}

func TestTerminalOwnerFailedActiveCommitNeverPublishesPayloadIntent(t *testing.T) {
	one, _, count := readyTerminalAdmission(t)
	one.terminalStarts = nil
	o := nodeTerminalOwner{one}
	intent := acphost.TerminalIntent{ID: "nt_" + strings.Repeat("c", 64), SessionID: acp.SessionID(one.record.UpstreamID), Generation: 1}
	if err := o.Reserve(t.Context(), intent); err != nil {
		t.Fatal(err)
	}
	preparation := procgroup.Preparation{PID: 321, Start: 789, ParentGroup: one.record.Process.Group}
	if err := o.Prepared(t.Context(), intent, preparation, "original-terminal-mark"); err != nil {
		t.Fatal(err)
	}
	store, _ := one.service.recordsStore()
	if _, err := store.db.Exec(`CREATE TRIGGER refuse_active BEFORE UPDATE OF header ON sessions BEGIN SELECT RAISE(ABORT,'isolated active failure'); END`); err != nil {
		t.Fatal(err)
	}
	identity := procgroup.Identity{Group: preparation.PID, Leader: preparation.PID, Start: preparation.Start, Mark: "original-terminal-mark"}
	if err := o.Active(t.Context(), intent, identity, one.record.Process.Place); err == nil {
		t.Fatal("failed active publication was ignored")
	}
	if one.record.Terminals[intent.ID].Phase != "preparing" || one.failure == nil {
		t.Fatal("failed publication changed the durable preparation owner")
	}
	if err := o.Admit(t.Context(), intent, func(context.Context) error { *count++; return nil }, func() error { return nil }); err == nil || *count != 0 || len(one.terminalStarts) != 0 {
		t.Fatal("failed active commit exposed an executable terminal intent")
	}
}

func TestTerminalAdmissionConcurrentRequestsDoNotReplayOriginalGate(t *testing.T) {
	one, req, count := readyTerminalAdmission(t)
	var wg sync.WaitGroup
	results := make(chan error, 8)
	for range 8 {
		wg.Go(func() {
			_, err := one.service.Do(t.Context(), "cluster-1", req)
			results <- err
		})
	}
	wg.Wait()
	close(results)
	for err := range results {
		if err != nil {
			var classified *SessionError
			if !errors.As(err, &classified) || classified.Code != "busy" {
				t.Fatal(err)
			}
		}
	}
	if _, err := one.service.Do(t.Context(), "cluster-1", req); err != nil {
		t.Fatal(err)
	}
	if *count != 1 || len(one.terminalStarts) != 0 {
		t.Fatal("concurrent original observers consumed a payload more than once")
	}
}

func TestTerminalAdmissionDeadlineDuringValidationNeverOpensPayload(t *testing.T) {
	for _, cancelAt := range []int{1, 2} {
		one, req, count := readyTerminalAdmission(t)
		ctx, cancel := context.WithCancel(t.Context())
		checks := 0
		gate := one.terminalStarts[req.TerminalStart.ID]
		gate.Validate = func() error {
			checks++
			if checks == cancelAt {
				cancel()
			}
			return nil
		}
		if _, err := one.service.Do(ctx, "cluster-1", req); err == nil || *count != 0 {
			t.Fatal("validation consumed payload after its original authority window ended")
		}
		if cancelAt == 1 && one.record.Terminals[req.TerminalStart.ID].PayloadStarted {
			t.Fatal("already canceled admission still consumed the durable once-only marker")
		}
		cancel()
	}
}

func TestTerminalAdmissionFailedGateReplayReturnsFailureWithoutExecutingAgain(t *testing.T) {
	for _, failure := range []string{"consume", "after-commit-cancel"} {
		t.Run(failure, func(t *testing.T) {
			one, req, count := readyTerminalAdmission(t)
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			gate := one.terminalStarts[req.TerminalStart.ID]
			if failure == "consume" {
				gate.Consume = func(context.Context) error { *count++; return errors.New("original terminal launch not confirmed") }
			} else {
				checks := 0
				gate.Validate = func() error {
					checks++
					if checks == 2 {
						cancel()
					}
					return nil
				}
			}
			if _, err := one.service.Do(ctx, "cluster-1", req); err == nil {
				t.Fatal("original failed gate returned success")
			}
			calls := *count
			if _, err := one.service.Do(t.Context(), "cluster-1", req); err == nil || *count != calls {
				t.Fatal("consumed-but-failed original gate became successful or was replayed")
			}
		})
	}
}

func TestTerminalAdmissionConsumedMarkerWithoutLiveOutcomeIsUnconfirmed(t *testing.T) {
	one, req, count := readyTerminalAdmission(t)
	next := one.copyLocked()
	owner := next.Terminals[req.TerminalStart.ID]
	owner.PayloadStarted = true
	next.Terminals[req.TerminalStart.ID] = owner
	if err := one.commitLocked(next); err != nil {
		t.Fatal(err)
	}
	one.terminalStarts = nil
	if _, err := one.service.Do(t.Context(), "cluster-1", req); err == nil || *count != 0 {
		t.Fatal("durable consumed marker manufactured a successful live outcome")
	}
}

func TestTerminalAdmissionDoesNotWaitForOwnerAfterItsLastChallenge(t *testing.T) {
	one, req, count := readyTerminalAdmission(t)
	last := make(chan struct{})
	var checks atomic.Int32
	var revoked atomic.Bool
	one.service.server = NewServer(ServerConfig{Name: "worker", SessionAuthorizer: sessionAuthorizerFunc(
		func(context.Context, string, nodewire.SessionAuthority, nodewire.SessionBinding, nodewire.SessionAction) error {
			if checks.Add(1) == 2 {
				one.mu.Lock()
				close(last)
				return nil
			}
			if revoked.Load() {
				return errNoReceiptAuthority
			}
			return nil
		},
	)})
	done := make(chan error, 1)
	go func() {
		_, err := one.service.Do(t.Context(), "cluster-1", req)
		done <- err
	}()
	<-last
	revoked.Store(true)
	select {
	case err := <-done:
		one.mu.Unlock()
		if err == nil {
			t.Fatal("contended original owner was treated as a fresh gate grant")
		}
	case <-time.After(100 * time.Millisecond):
		one.mu.Unlock()
		<-done
		t.Fatal("terminal waited again after its last authenticated challenge")
	}
	if *count != 0 || one.record.Terminals[req.TerminalStart.ID].PayloadStarted {
		t.Fatal("contended last challenge consumed the original payload")
	}
	revoked.Store(false)
	if _, err := one.service.Do(t.Context(), "cluster-1", req); err != nil || *count != 1 {
		t.Fatal("refused contention lost the unconsumed original gate")
	}
}

func TestTerminalStoppedOwnerWaitHonorsOriginalCleanupDeadline(t *testing.T) {
	one, req, _ := readyTerminalAdmission(t)
	intent := acphost.TerminalIntent{ID: req.TerminalStart.ID, SessionID: acp.SessionID(one.record.UpstreamID), Generation: 1}
	before := one.record.State.Sequence
	ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
	defer cancel()
	one.mu.Lock()
	done := make(chan error, 1)
	go func() { done <- (nodeTerminalOwner{one}).Stopped(ctx, intent) }()
	select {
	case err := <-done:
		one.mu.Unlock()
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("contended original cleanup returned %v", err)
		}
	case <-time.After(300 * time.Millisecond):
		one.mu.Unlock()
		<-done
		t.Fatal("original cleanup deadline could not interrupt its owner-lock wait")
	}
	if one.record.State.Sequence != before || one.record.Terminals[intent.ID].Phase != "active" {
		t.Fatal("expired stop callback committed cleanup after its owner wait")
	}
	if err := (nodeTerminalOwner{one}).Stopped(t.Context(), intent); err != nil || one.record.Terminals[intent.ID].Phase != "stopped" {
		t.Fatal("fresh original cleanup could not reconcile the retained slot")
	}
}
