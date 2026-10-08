//go:build linux || darwin

package acphost

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/gopact-ai/acp"
	"github.com/gopact-ai/steve/internal/permission"
	"github.com/gopact-ai/steve/internal/procgroup"
)

// hostTerminalOwner is an isolated Host contract fixture, not an execution
// authorizer or a substitute for the real managed terminal-admit RPC tests.
type hostTerminalOwner struct {
	mu           sync.Mutex
	stages       map[string]int
	stops        map[string]int
	failStop     bool
	admit        func(context.Context, TerminalIntent, func(context.Context) error, func() error) error
	beforeActive func()
}

// Keep this shape check equivalent to the durable Node owner, without a
// package import cycle or weakening the product validator.
func nodeEquivalentTerminalID(id string) bool {
	if len(id) != 67 || !strings.HasPrefix(id, "nt_") {
		return false
	}
	_, err := hex.DecodeString(id[3:])
	return err == nil
}

func (o *hostTerminalOwner) Reserve(_ context.Context, intent TerminalIntent) error {
	if !nodeEquivalentTerminalID(intent.ID) {
		return errors.New("terminal identity violates durable Node owner contract")
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.stages == nil {
		o.stages = map[string]int{}
		o.stops = map[string]int{}
	}
	if o.stages[intent.ID] != 0 {
		return errors.New("duplicate reserve")
	}
	o.stages[intent.ID] = 1
	return nil
}
func (o *hostTerminalOwner) Prepared(_ context.Context, intent TerminalIntent, p procgroup.Preparation, mark string) error {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.stages[intent.ID] != 1 || p.PID == 0 || p.ParentGroup == p.PID || mark == "" {
		return errors.New("invalid preparation order")
	}
	o.stages[intent.ID] = 2
	return nil
}
func (o *hostTerminalOwner) Active(_ context.Context, intent TerminalIntent, id procgroup.Identity, place procgroup.Place) error {
	if o.beforeActive != nil {
		o.beforeActive()
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.stages[intent.ID] != 2 || id.Leader != id.Group || id.Start == 0 || place.Boot == "" {
		return errors.New("invalid active identity")
	}
	o.stages[intent.ID] = 3
	return nil
}
func (o *hostTerminalOwner) Admit(ctx context.Context, intent TerminalIntent, consume func(context.Context) error, validate func() error) error {
	if o.admit != nil {
		return o.admit(ctx, intent, consume, validate)
	}
	o.mu.Lock()
	stage := o.stages[intent.ID]
	o.mu.Unlock()
	if stage != 3 {
		return errors.New("EXEC without durable active identity")
	}
	if err := validate(); err != nil {
		return err
	}
	// Test-only explicit grant; production grants are exclusively node-owned.
	return consume(ctx)
}
func (o *hostTerminalOwner) Stopped(_ context.Context, intent TerminalIntent) error {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.failStop {
		return errors.New("isolated durable stop failure")
	}
	if o.stops[intent.ID] != 0 {
		return errors.New("durable stop callback repeated")
	}
	o.stops[intent.ID]++
	o.stages[intent.ID] = 4
	return nil
}

func terminalHostFixture(t *testing.T, policy string) (*Host, *terminalHandler, *hostTerminalOwner, string, *collector) {
	t.Helper()
	parent := nativeTerminalParent(t)
	path := t.TempDir()
	workspace, err := prepareWorkspaceFiles(path)
	if err != nil {
		t.Fatal(err)
	}
	broker, err := permission.New(policy)
	if err != nil {
		t.Fatal(err)
	}
	owner := &hostTerminalOwner{}
	h := New(Config{Permission: broker, TerminalOwner: owner, NoRestart: true})
	ctx, cancel := context.WithCancel(t.Context())
	col := &collector{generation: 1, ctx: ctx, cancelAsk: cancel}
	h.proc = parent
	h.processes[1] = parent
	h.generation = 1
	h.alive = true
	h.sessions["session"] = &sessionState{workspace: workspace}
	h.collectors["session"] = col
	h.active["session"] = 1
	h.fileRoots = map[*workspaceFiles]uint64{workspace: 1}
	h.terminalHelperArgs = []string{"-test.run=^TestTerminalInertChildHelperProcess$"}
	handler := &clientHandler{h: h, generation: 1}
	handler.ready.Store(true)
	t.Cleanup(h.Close)
	return h, &terminalHandler{handler}, owner, path, col
}
func terminalCreate(t *testing.T, ch *terminalHandler, script string, limit *uint64) acp.TerminalID {
	t.Helper()
	response, err := ch.CreateTerminal(t.Context(), &acp.CreateTerminalRequest{SessionID: "session", Command: "/bin/sh", Args: []string{"-c", script}, OutputByteLimit: limit})
	if err != nil {
		t.Fatal(err)
	}
	return response.TerminalID
}
func terminalAwait(t *testing.T, ch *terminalHandler, id acp.TerminalID) *acp.WaitForTerminalExitResponse {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	result, err := ch.WaitForTerminalExit(ctx, &acp.WaitForTerminalExitRequest{SessionID: "session", TerminalID: id})
	if err != nil {
		t.Fatal(err)
	}
	return result
}

func TestHostTerminalFiveMethodsAndOnceOnlyDurableStop(t *testing.T) {
	_, ch, owner, _, _ := terminalHostFixture(t, "auto")
	id := terminalCreate(t, ch, `printf 'stdout'; printf 'stderr' >&2; exit 7`, nil)
	status := terminalAwait(t, ch, id)
	if status.ExitCode == nil || *status.ExitCode != 7 {
		t.Fatalf("status=%+v", status)
	}
	result, err := ch.TerminalOutput(t.Context(), &acp.TerminalOutputRequest{SessionID: "session", TerminalID: id})
	if err != nil || !strings.Contains(result.Output, "stdout") || !strings.Contains(result.Output, "stderr") || result.ExitStatus == nil {
		t.Fatalf("output=%+v err=%v", result, err)
	}
	for range 2 {
		if _, err := ch.KillTerminal(t.Context(), &acp.KillTerminalRequest{SessionID: "session", TerminalID: id}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := ch.ReleaseTerminal(t.Context(), &acp.ReleaseTerminalRequest{SessionID: "session", TerminalID: id}); err != nil {
		t.Fatal(err)
	}
	if _, err := ch.TerminalOutput(t.Context(), &acp.TerminalOutputRequest{SessionID: "session", TerminalID: id}); err == nil {
		t.Fatal("released ID remained valid")
	}
	owner.mu.Lock()
	defer owner.mu.Unlock()
	if owner.stops[string(id)] != 1 {
		t.Fatal("stop commit was not monotonic once-only")
	}
}

func TestHostTerminalWaitCancellationDoesNotKillPayload(t *testing.T) {
	_, ch, _, root, _ := terminalHostFixture(t, "auto")
	id := terminalCreate(t, ch, `while [ ! -f finish ]; do sleep .01; done; printf done`, nil)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := ch.WaitForTerminalExit(ctx, &acp.WaitForTerminalExitRequest{SessionID: "session", TerminalID: id}); err == nil {
		t.Fatal("cancelled waiter succeeded")
	}
	if err := os.WriteFile(filepath.Join(root, "finish"), []byte("finish"), 0600); err != nil {
		t.Fatal(err)
	}
	status := terminalAwait(t, ch, id)
	if status.ExitCode == nil || *status.ExitCode != 0 {
		t.Fatalf("wait cancellation killed payload: %+v", status)
	}
}

func TestHostTerminalCancelledCreateAfterGateDoesNotKillPayload(t *testing.T) {
	_, ch, owner, root, _ := terminalHostFixture(t, "auto")
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	owner.admit = func(grant context.Context, _ TerminalIntent, consume func(context.Context) error, validate func() error) error {
		if err := validate(); err != nil {
			return err
		}
		if err := consume(grant); err != nil {
			return err
		}
		cancel()
		return nil
	}
	response, err := ch.CreateTerminal(ctx, &acp.CreateTerminalRequest{SessionID: "session", Command: "/bin/sh", Args: []string{"-c", `while [ ! -f finish ]; do sleep .01; done; printf survived`}})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "finish"), []byte("finish"), 0600); err != nil {
		t.Fatal(err)
	}
	status := terminalAwait(t, ch, response.TerminalID)
	if status.ExitCode == nil || *status.ExitCode != 0 {
		t.Fatalf("create cancellation killed admitted payload: %+v", status)
	}
}

func TestHostTerminalCreateReleaseReusesMoreThan32Slots(t *testing.T) {
	h, ch, owner, _, _ := terminalHostFixture(t, "auto")
	for i := 0; i < 40; i++ {
		id := terminalCreate(t, ch, `printf reusable`, nil)
		terminalAwait(t, ch, id)
		if _, err := ch.ReleaseTerminal(t.Context(), &acp.ReleaseTerminalRequest{SessionID: "session", TerminalID: id}); err != nil {
			t.Fatalf("cycle %d: %v", i, err)
		}
	}
	h.mu.Lock()
	retained := len(h.terminals)
	h.mu.Unlock()
	if retained != 0 {
		t.Fatalf("%d released records leaked", retained)
	}
	owner.mu.Lock()
	defer owner.mu.Unlock()
	if len(owner.stops) != 40 {
		t.Fatalf("completed cycles=%d", len(owner.stops))
	}
}

func TestHostTerminalRejectsOriginalTurnOrRootChangeBeforeFreshGate(t *testing.T) {
	for _, boundary := range []string{"collector", "root", "parent-close"} {
		t.Run(boundary, func(t *testing.T) {
			h, ch, owner, root, col := terminalHostFixture(t, "auto")
			owner.admit = func(ctx context.Context, _ TerminalIntent, consume func(context.Context) error, validate func() error) error {
				switch boundary {
				case "collector":
					h.mu.Lock()
					h.collectors["session"] = &collector{generation: 1, ctx: col.ctx}
					h.mu.Unlock()
				case "root":
					if err := os.Rename(root, root+"-original"); err != nil {
						return err
					}
					t.Cleanup(func() { os.RemoveAll(root + "-original") })
					if err := os.Mkdir(root, 0700); err != nil {
						return err
					}
				case "parent-close":
					h.proc.(*localProcess).closeTerminalAdmission()
				}
				if err := validate(); err != nil {
					return err
				}
				return consume(ctx)
			}
			if _, err := ch.CreateTerminal(t.Context(), &acp.CreateTerminalRequest{SessionID: "session", Command: "/bin/sh", Args: []string{"-c", `printf forbidden > marker`}}); err == nil {
				t.Fatal("stale original owner admitted payload")
			}
			for _, p := range []string{root, root + "-original"} {
				if _, err := os.Stat(filepath.Join(p, "marker")); !os.IsNotExist(err) {
					t.Fatalf("payload reached %s: %v", p, err)
				}
			}
		})
	}
}

func TestHostTerminalExecutePermissionAndSafeRelativeCwd(t *testing.T) {
	_, ch, owner, root, col := terminalHostFixture(t, "read")
	askCount := 0
	col.ask = func(_ context.Context, q permission.Ask) (acp.RequestPermissionOutcome, error) {
		askCount++
		if q.Kind != acp.ToolKindExecute || q.Generation != 1 {
			return acp.CanceledRequestPermissionOutcome(), errors.New("wrong execution permission")
		}
		return permission.Choose(true, q.Options), nil
	}
	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(root, "escape")); err != nil {
		t.Fatal(err)
	}
	cwd := "escape/../" // os.Root must interpret the symlink before .., not clean it.
	if _, err := ch.CreateTerminal(t.Context(), &acp.CreateTerminalRequest{SessionID: "session", Command: "/bin/true", Cwd: &cwd}); err == nil {
		t.Fatal("symlink/.. escape accepted")
	}
	owner.mu.Lock()
	slots := len(owner.stages)
	owner.mu.Unlock()
	if slots != 0 {
		t.Fatal("invalid cwd reserved a native slot")
	}
	if err := os.Mkdir(filepath.Join(root, "sub"), 0700); err != nil {
		t.Fatal(err)
	}
	cwd = "sub"
	response, err := ch.CreateTerminal(t.Context(), &acp.CreateTerminalRequest{SessionID: "session", Command: "/bin/sh", Args: []string{"-c", `printf confined > marker`}, Cwd: &cwd})
	if err != nil {
		t.Fatal(err)
	}
	terminalAwait(t, ch, response.TerminalID)
	if raw, err := os.ReadFile(filepath.Join(root, "sub", "marker")); err != nil || string(raw) != "confined" {
		t.Fatalf("cwd=%q %v", raw, err)
	}
	if askCount != 2 {
		t.Fatalf("asks=%d", askCount)
	}
}

func TestHostTerminalProtectedEnvironmentAndBoundedUTF8Tail(t *testing.T) {
	_, ch, _, _, _ := terminalHostFixture(t, "auto")
	if _, err := ch.CreateTerminal(t.Context(), &acp.CreateTerminalRequest{SessionID: "session", Command: "/bin/true", Env: []acp.EnvVariable{{Name: procgroup.MarkVariable, Value: "foreign"}}}); err == nil {
		t.Fatal("protected mark override accepted")
	}
	limit := uint64(10)
	id := terminalCreate(t, ch, `printf 'opening中文-tail'; printf '\377'`, &limit)
	terminalAwait(t, ch, id)
	output, err := ch.TerminalOutput(t.Context(), &acp.TerminalOutputRequest{SessionID: "session", TerminalID: id})
	if err != nil || len(output.Output) > 10 || !utf8.ValidString(output.Output) || !output.Truncated {
		t.Fatalf("tail=%+v %v", output, err)
	}
}

func TestHostTerminalDurableStopFailureRetainsOutputAndExclusion(t *testing.T) {
	h, ch, owner, _, _ := terminalHostFixture(t, "auto")
	owner.mu.Lock()
	owner.failStop = true
	owner.mu.Unlock()
	id := terminalCreate(t, ch, `printf retained`, nil)
	// Wait until the real runtime has positive native group proof, not a mock
	// cmd.Wait completion. Its failed stop commit must still leave exclusion.
	h.mu.Lock()
	terminal := h.terminals[string(id)]
	h.mu.Unlock()
	terminal.phase.Lock()
	runtime := terminal.runtime
	terminal.phase.Unlock()
	select {
	case <-runtime.Done():
	case <-time.After(3 * time.Second):
		t.Fatal("runtime did not settle")
	}
	if _, err := ch.ReleaseTerminal(t.Context(), &acp.ReleaseTerminalRequest{SessionID: "session", TerminalID: id}); err == nil {
		t.Fatal("failed durable stop was called released")
	}
	h.mu.Lock()
	confirmed := h.terminalsConfirmedLocked(1)
	h.mu.Unlock()
	if confirmed {
		t.Fatal("native group proof hid failed durable stop")
	}
	output, err := ch.TerminalOutput(t.Context(), &acp.TerminalOutputRequest{SessionID: "session", TerminalID: id})
	if err != nil || output.Output != "retained" {
		t.Fatalf("retained output=%+v %v", output, err)
	}
	owner.mu.Lock()
	owner.failStop = false
	owner.mu.Unlock()
	if _, err := ch.ReleaseTerminal(t.Context(), &acp.ReleaseTerminalRequest{SessionID: "session", TerminalID: id}); err != nil {
		t.Fatal(err)
	}
}

func TestHostTerminalReleaseFreezesAttachedToolContent(t *testing.T) {
	h, ch, _, _, col := terminalHostFixture(t, "auto")
	id := terminalCreate(t, ch, `printf frozen`, nil)
	terminalAwait(t, ch, id)
	title := "terminal tool"
	notification := acp.SessionUpdate{SessionUpdate: acp.SessionUpdateTypeToolCall, ToolCallID: "tool", Title: &title, Content: []acp.ToolCallContent{acp.TerminalToolCallContent(id)}}
	col.handle(notification)
	h.terminalUpdate(col, notification)
	if _, err := ch.ReleaseTerminal(t.Context(), &acp.ReleaseTerminalRequest{SessionID: "session", TerminalID: id}); err != nil {
		t.Fatal(err)
	}
	update := acp.SessionUpdate{SessionUpdate: acp.SessionUpdateTypeToolCallUpdate, ToolCallID: "tool", Content: []acp.ToolCallContent{acp.TerminalToolCallContent(id)}}
	col.handle(update)
	h.terminalUpdate(col, update)
	col.mu.Lock()
	defer col.mu.Unlock()
	if len(col.tools) != 1 || col.tools[0].Output != "frozen" {
		t.Fatalf("frozen attached content lost: %+v", col.tools)
	}
}

func TestHostTerminalControlsHaveHeadroomWhenWaitersExhaustCalls(t *testing.T) {
	h, ch, _, _, col := terminalHostFixture(t, "auto")
	id := terminalCreate(t, ch, `sleep 30`, nil)
	h.mu.Lock()
	for i := 0; i < maxWorkspaceFileCalls; i++ {
		h.fileCalls[&workspaceFileCall{col: col, class: "terminal", cancel: func() {}}] = struct{}{}
	}
	h.mu.Unlock()
	_, err := ch.KillTerminal(t.Context(), &acp.KillTerminalRequest{SessionID: "session", TerminalID: id})
	h.mu.Lock()
	for call := range h.fileCalls {
		if call.stopRPC == nil {
			delete(h.fileCalls, call)
		}
	}
	h.mu.Unlock()
	if err != nil {
		t.Fatalf("normal callback saturation blocked cleanup: %v", err)
	}
}

func TestHostTerminalDescendantKillDoesNotKillOriginalAgent(t *testing.T) {
	h, ch, _, root, _ := terminalHostFixture(t, "auto")
	id := terminalCreate(t, ch, `(trap '' TERM; while :; do printf x >> writer; sleep .01; done) & wait`, nil)
	deadline := time.Now().Add(2 * time.Second)
	for {
		if info, err := os.Stat(filepath.Join(root, "writer")); err == nil && info.Size() > 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("descendant writer never started")
		}
		time.Sleep(time.Millisecond)
	}
	if _, err := ch.KillTerminal(t.Context(), &acp.KillTerminalRequest{SessionID: "session", TerminalID: id}); err != nil {
		t.Fatal(err)
	}
	before, err := os.Stat(filepath.Join(root, "writer"))
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(50 * time.Millisecond)
	after, err := os.Stat(filepath.Join(root, "writer"))
	if err != nil || after.Size() != before.Size() {
		t.Fatal("writer continued after confirmed group stop")
	}
	select {
	case <-h.proc.Exited():
		t.Fatal("independent terminal kill killed the original Agent")
	default:
	}
	output, err := ch.TerminalOutput(t.Context(), &acp.TerminalOutputRequest{SessionID: "session", TerminalID: id})
	if err != nil || output.ExitStatus == nil {
		t.Fatalf("kill lost ID/output: %+v %v", output, err)
	}
}

func TestHostTerminalRetireCollectorCleansWriterAndPropagatesStopCommitFailure(t *testing.T) {
	for _, fail := range []bool{false, true} {
		t.Run(fmt.Sprint(fail), func(t *testing.T) {
			h, ch, owner, root, col := terminalHostFixture(t, "auto")
			id := terminalCreate(t, ch, `printf started > writer; sleep 30`, nil)
			owner.mu.Lock()
			owner.failStop = fail
			owner.mu.Unlock()
			err := h.retireCollector("session", col)
			if (err != nil) != fail {
				t.Fatalf("retirement stop error=%v, fail=%v", err, fail)
			}
			if _, err := ch.TerminalOutput(t.Context(), &acp.TerminalOutputRequest{SessionID: "session", TerminalID: id}); err == nil {
				t.Fatal("retired collector admitted a callback")
			}
			_ = root
			owner.mu.Lock()
			owner.failStop = false
			owner.mu.Unlock()
		})
	}
}

func TestHostTerminalQuietTailFitsEncodedFrame(t *testing.T) {
	text := strings.Repeat("\x01", maxTerminalOutputBytes)
	retained, truncated := terminalWireText(text, nil)
	if !truncated || len(retained) == 0 || len(retained) >= len(text) {
		t.Fatal("JSON expansion was not bounded")
	}
	wire, err := json.Marshal(acp.TerminalOutputResponse{Output: retained, Truncated: true})
	if err != nil || len(wire) > maxTerminalOutputBytes-1024 {
		t.Fatal("encoded terminal response still exceeds its bound")
	}

}

func TestHostTerminalKillAfterStoppedSlotPrunedNeverRepeatsCallback(t *testing.T) {
	_, ch, owner, _, _ := terminalHostFixture(t, "auto")
	id := terminalCreate(t, ch, `printf kept`, nil)
	terminalAwait(t, ch, id)
	owner.mu.Lock()
	delete(owner.stages, string(id))
	owner.mu.Unlock()
	// A real Node may prune the stopped slot during the next reserve. Host's
	// successful durable confirmation must remain monotonic across that prune.
	next := terminalCreate(t, ch, `printf next`, nil)
	terminalAwait(t, ch, next)
	if _, err := ch.KillTerminal(t.Context(), &acp.KillTerminalRequest{SessionID: "session", TerminalID: id}); err != nil {
		t.Fatal(err)
	}
	if _, err := ch.ReleaseTerminal(t.Context(), &acp.ReleaseTerminalRequest{SessionID: "session", TerminalID: id}); err != nil {
		t.Fatal(err)
	}
}

func TestHostTerminalAggregateOutputBudgetAndReleasedReuse(t *testing.T) {
	h, ch, _, _, col := terminalHostFixture(t, "auto")
	full := uint64(maxTerminalOutputBytes)
	first := terminalCreate(t, ch, `printf one`, &full)
	terminalAwait(t, ch, first)
	next := terminalCreate(t, ch, `printf clipped`, nil)
	terminalAwait(t, ch, next)
	h.mu.Lock()
	bytes := h.terminalOutputBytes[col]
	h.mu.Unlock()
	if bytes > maxTerminalOutputBytes {
		t.Fatalf("aggregate output reserved %d", bytes)
	}
	result, err := ch.TerminalOutput(t.Context(), &acp.TerminalOutputRequest{SessionID: "session", TerminalID: next})
	if err != nil || result.Output != "" || !result.Truncated {
		t.Fatalf("zero-capacity drain=%+v %v", result, err)
	}
	if _, err := ch.ReleaseTerminal(t.Context(), &acp.ReleaseTerminalRequest{SessionID: "session", TerminalID: first}); err != nil {
		t.Fatal(err)
	}
	third := terminalCreate(t, ch, `printf reused`, nil)
	terminalAwait(t, ch, third)
	result, err = ch.TerminalOutput(t.Context(), &acp.TerminalOutputRequest{SessionID: "session", TerminalID: third})
	if err != nil || result.Output != "reused" {
		t.Fatalf("release did not restore output budget: %+v %v", result, err)
	}
}

func TestHostTerminalInvalidCrossSessionAndForeignGenerationIDs(t *testing.T) {
	h, ch, _, _, col := terminalHostFixture(t, "auto")
	id := terminalCreate(t, ch, `sleep 30`, nil)
	h.mu.Lock()
	h.sessions["foreign"] = &sessionState{workspace: h.sessions["session"].workspace}
	h.collectors["foreign"] = col
	h.active["foreign"] = 1
	h.mu.Unlock()
	if _, err := ch.KillTerminal(t.Context(), &acp.KillTerminalRequest{SessionID: "foreign", TerminalID: id}); err == nil {
		t.Fatal("foreign session killed original terminal")
	}
	stale := &clientHandler{h: h, generation: 2}
	stale.ready.Store(true)
	if _, err := (&terminalHandler{stale}).TerminalOutput(t.Context(), &acp.TerminalOutputRequest{SessionID: "session", TerminalID: id}); err == nil {
		t.Fatal("foreign generation read original output")
	}
	if _, err := ch.ReleaseTerminal(t.Context(), &acp.ReleaseTerminalRequest{SessionID: "session", TerminalID: id}); err != nil {
		t.Fatal(err)
	}
}

func TestHostTerminalCommandLookupFailureIsExplicitAndNoReplay(t *testing.T) {
	h, ch, owner, _, _ := terminalHostFixture(t, "auto")
	if _, err := ch.CreateTerminal(t.Context(), &acp.CreateTerminalRequest{SessionID: "session", Command: "no-such-isolated-terminal-command"}); err == nil {
		t.Fatal("unavailable command was acknowledged as launched")
	}
	h.mu.Lock()
	var terminal *hostTerminal
	for _, record := range h.terminals {
		terminal = record
	}
	h.mu.Unlock()
	if terminal == nil {
		t.Fatal("accepted EXEC gate's obligation was discarded")
	}
	terminal.phase.Lock()
	runtime := terminal.runtime
	terminal.phase.Unlock()
	select {
	case <-runtime.Done():
	case <-time.After(3 * time.Second):
		t.Fatal("failed helper launch did not settle")
	}
	if err := runtime.Execute(t.Context()); err == nil {
		t.Fatal("failed launch replayed its once-only gate")
	}
	if err := terminal.stop(t.Context(), h); err != nil {
		t.Fatal(err)
	}
	h.releaseTerminal(terminal)
	owner.mu.Lock()
	defer owner.mu.Unlock()
	if owner.stops[terminal.intent.ID] != 1 {
		t.Fatal("failed launch cleanup was not durably confirmed once")
	}
}

func TestHostTerminalNativeAgentWireFixture(t *testing.T) {
	if os.Getenv("GO_ACP_TERMINAL_AGENT_FIXTURE") != "1" {
		return
	}
	a := &filesParticipant{lifecycleParticipant: &lifecycleParticipant{version: acp.ProtocolVersionV1, caps: lifecycleCaps(31), workdir: os.Getenv("GO_ACP_TERMINAL_WORKSPACE"), servers: []acp.MCPServer{}}}
	a.initialize = func(_ context.Context, req *acp.InitializeRequest) error {
		if req.ClientCapabilities == nil || !req.ClientCapabilities.Terminal {
			return errors.New("complete local terminal capability not advertised")
		}
		if req.ClientCapabilities.Fs != nil {
			return errors.New("terminal-only client accidentally grants text-file access")
		}
		return nil
	}
	a.prompt = func(ctx context.Context, req *acp.PromptRequest) (*acp.PromptResponse, error) {
		terminal, err := a.client.CreateTerminal(ctx, &acp.CreateTerminalRequest{SessionID: req.SessionID, Command: "/bin/sh", Args: []string{"-c", `printf "$TERMINAL_PROFILE_SENTINEL"; exit 9`}})
		if err != nil {
			return nil, err
		}
		status, err := a.client.WaitForTerminalExit(ctx, &acp.WaitForTerminalExitRequest{SessionID: req.SessionID, TerminalID: terminal.TerminalID})
		if err != nil || status.ExitCode == nil || *status.ExitCode != 9 {
			return nil, errors.New("wire terminal exit status differs")
		}
		output, err := a.client.TerminalOutput(ctx, &acp.TerminalOutputRequest{SessionID: req.SessionID, TerminalID: terminal.TerminalID})
		if err != nil || output.Output != "original-profile" {
			return nil, errors.New("payload did not use original frozen profile")
		}
		if _, err := a.client.KillTerminal(ctx, &acp.KillTerminalRequest{SessionID: req.SessionID, TerminalID: terminal.TerminalID}); err != nil {
			return nil, err
		}
		if _, err := a.client.ReleaseTerminal(ctx, &acp.ReleaseTerminalRequest{SessionID: req.SessionID, TerminalID: terminal.TerminalID}); err != nil {
			return nil, err
		}
		if _, err := a.client.TerminalOutput(ctx, &acp.TerminalOutputRequest{SessionID: req.SessionID, TerminalID: terminal.TerminalID}); err == nil {
			return nil, errors.New("released ID remained valid on wire")
		}
		if err := a.client.Update(ctx, &acp.SessionNotification{SessionID: req.SessionID, Update: acp.AgentMessageChunkSessionUpdate(acp.TextContentBlock("terminal-wire-ok"))}); err != nil {
			return nil, err
		}
		return &acp.PromptResponse{StopReason: acp.StopReasonEndTurn}, nil
	}
	conn, err := acp.NewAgent(os.Stdin, os.Stdout, func(client *acp.ClientCaller) acp.AgentHandler { a.client = client; return a })
	if err != nil {
		os.Exit(2)
	}
	<-conn.Done()
	os.Exit(0)
}

func TestHostTerminalNativeWireFiveRPCAndOriginalFrozenProfile(t *testing.T) {
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	broker, err := permission.New("auto")
	if err != nil {
		t.Fatal(err)
	}
	owner := &hostTerminalOwner{}
	h := New(Config{Command: executable, Args: []string{"-test.run=^TestHostTerminalNativeAgentWireFixture$"}, Env: []string{"GO_ACP_TERMINAL_AGENT_FIXTURE=1", "GO_TERMINAL_INERT_CHILD=1", "GO_ACP_TERMINAL_WORKSPACE=" + root, "TERMINAL_PROFILE_SENTINEL=original-profile"}, Permission: broker, TerminalOwner: owner, NoRestart: true})
	h.terminalHelperArgs = []string{"-test.run=^TestTerminalInertChildHelperProcess$"}
	t.Cleanup(h.Close)
	sid, generation, err := h.OpenSession(t.Context(), "", SessionConfig{Workdir: root})
	if err != nil {
		t.Fatal(err)
	}
	// Mutation of the process-wide environment must not affect a terminal's
	// launch profile after the original agent has already been started.
	t.Setenv("TERMINAL_PROFILE_SENTINEL", "changed-after-agent-start")
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	answer, _, err := h.Prompt(ctx, sid, generation, "isolated terminal wire", nil)
	if err != nil || answer != "terminal-wire-ok" {
		t.Fatalf("wire result=%q err=%v", answer, err)
	}
	owner.mu.Lock()
	defer owner.mu.Unlock()
	if len(owner.stops) != 1 {
		t.Fatal("wire terminal never durably confirmed stop")
	}
}

func TestHostTerminalNoOwnerAndRawRemoteRemainUnadvertised(t *testing.T) {
	for _, owner := range []bool{false, true} {
		t.Run(fmt.Sprint(owner), func(t *testing.T) {
			root := t.TempDir()
			a := &filesParticipant{lifecycleParticipant: &lifecycleParticipant{version: acp.ProtocolVersionV1, caps: lifecycleCaps(31), workdir: root, servers: []acp.MCPServer{}}}
			config := Config{Transport: filesTransport{a}, NoRestart: true}
			if owner {
				config.TerminalOwner = &hostTerminalOwner{}
			}
			h := New(config)
			t.Cleanup(h.Close)
			_, _, err := h.OpenSession(t.Context(), "", SessionConfig{Workdir: root})
			if err != nil {
				t.Fatal(err)
			}
			if a.initialized != nil && a.initialized.Terminal {
				t.Fatal("raw stdio transport advertised local terminal access")
			}
		})
	}
}

func TestHostTerminalPermissionRejectionAndApprovalRevalidation(t *testing.T) {
	for _, mode := range []string{"deny", "reject", "replace-after-approve"} {
		t.Run(mode, func(t *testing.T) {
			policy := "read"
			if mode == "deny" {
				policy = "deny"
			}
			h, ch, owner, root, col := terminalHostFixture(t, policy)
			col.ask = func(_ context.Context, q permission.Ask) (acp.RequestPermissionOutcome, error) {
				if mode == "replace-after-approve" {
					h.mu.Lock()
					h.collectors["session"] = &collector{generation: 1, ctx: col.ctx}
					h.mu.Unlock()
				}
				return permission.Choose(mode == "replace-after-approve", q.Options), nil
			}
			if _, err := ch.CreateTerminal(t.Context(), &acp.CreateTerminalRequest{SessionID: "session", Command: "/bin/sh", Args: []string{"-c", `printf forbidden > marker`}}); err == nil {
				t.Fatal("execution permission/collector boundary ignored")
			}
			if _, err := os.Stat(filepath.Join(root, "marker")); !os.IsNotExist(err) {
				t.Fatal("refused execution created payload file")
			}
			owner.mu.Lock()
			defer owner.mu.Unlock()
			if len(owner.stages) != 0 {
				t.Fatal("permission refusal reserved a durable slot")
			}
		})
	}
}

func TestHostTerminalOriginalAgentStopIncludesIndependentTerminal(t *testing.T) {
	h, ch, _, _, _ := terminalHostFixture(t, "auto")
	id := terminalCreate(t, ch, `sleep 30`, nil)
	h.mu.Lock()
	record := h.terminals[string(id)]
	parent := h.proc.(*localProcess)
	h.mu.Unlock()
	record.phase.Lock()
	runtime := record.runtime
	record.phase.Unlock()
	parent.Kill()
	select {
	case <-parent.observed:
	case <-time.After(3 * time.Second):
		t.Fatal("original Agent did not settle")
	}
	select {
	case <-runtime.Done():
	case <-time.After(3 * time.Second):
		t.Fatal("independent terminal survived original Agent stop")
	}
	if !runtime.NativeStopped() {
		t.Fatal("independent group was not proved quiet")
	}
	if h.AllProcessesStopped() {
		t.Fatal("native proof concealed uncommitted terminal stop")
	}
	if _, err := ch.ReleaseTerminal(t.Context(), &acp.ReleaseTerminalRequest{SessionID: "session", TerminalID: id}); err != nil {
		t.Fatal(err)
	}
	if !h.AllProcessesStopped() {
		t.Fatal("confirmed aggregate native stop was not visible")
	}
}

func TestHostTerminalWaitingQuotaLeavesControlHeadroom(t *testing.T) {
	h, ch, _, _, col := terminalHostFixture(t, "auto")
	id := terminalCreate(t, ch, `sleep 30`, nil)
	h.mu.Lock()
	for range 8 {
		h.fileCalls[&workspaceFileCall{col: col, class: "terminal-wait", cancel: func() {}}] = struct{}{}
	}
	h.mu.Unlock()
	if _, err := ch.WaitForTerminalExit(t.Context(), &acp.WaitForTerminalExitRequest{SessionID: "session", TerminalID: id}); err == nil {
		t.Fatal("ninth long waiter was admitted")
	}
	_, err := ch.KillTerminal(t.Context(), &acp.KillTerminalRequest{SessionID: "session", TerminalID: id})
	h.mu.Lock()
	for call := range h.fileCalls {
		if call.stopRPC == nil {
			delete(h.fileCalls, call)
		}
	}
	h.mu.Unlock()
	if err != nil {
		t.Fatalf("blocked waiters stole cleanup headroom: %v", err)
	}
}

func TestHostTerminalPendingFreshGateNeverExecutesAndCancelCleans(t *testing.T) {
	h, ch, owner, root, _ := terminalHostFixture(t, "auto")
	ready := make(chan struct{})
	owner.beforeActive = func() {
		if _, err := os.Stat(filepath.Join(root, "marker")); !os.IsNotExist(err) {
			t.Error("preparation or SPLIT ran payload")
		}
	}
	owner.admit = func(ctx context.Context, _ TerminalIntent, _ func(context.Context) error, _ func() error) error {
		close(ready)
		<-ctx.Done()
		return ctx.Err()
	}
	ctx, cancel := context.WithCancel(t.Context())
	result := make(chan error, 1)
	go func() {
		_, err := ch.CreateTerminal(ctx, &acp.CreateTerminalRequest{SessionID: "session", Command: "/bin/sh", Args: []string{"-c", `printf forbidden > marker`}})
		result <- err
	}()
	select {
	case <-ready:
	case <-time.After(3 * time.Second):
		cancel()
		t.Fatal("did not register an inert ready gate")
	}
	if _, err := os.Stat(filepath.Join(root, "marker")); !os.IsNotExist(err) {
		cancel()
		t.Fatal("ready gate executed without a fresh grant")
	}
	cancel()
	select {
	case err := <-result:
		if err == nil {
			t.Fatal("cancelled ready gate succeeded")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("inert gate cancellation did not settle")
	}
	if _, err := os.Stat(filepath.Join(root, "marker")); !os.IsNotExist(err) {
		t.Fatal("cancelled gate replayed payload")
	}
	h.mu.Lock()
	retained := len(h.terminals)
	h.mu.Unlock()
	if retained != 0 {
		t.Fatal("cancelled inert helper slot remained owned after proof")
	}
}

func TestHostTerminalIdentityMatchesDurableOwner(t *testing.T) {
	seen := map[string]bool{}
	for range 128 {
		id, err := newTerminalID()
		if err != nil {
			t.Fatal(err)
		}
		if !nodeEquivalentTerminalID(id) || seen[id] {
			t.Fatalf("invalid or repeated terminal identity: %q", id)
		}
		seen[id] = true
	}
	for _, test := range []struct {
		name, id string
		valid    bool
	}{
		{"lowercase", "nt_" + strings.Repeat("a", 64), true},
		{"uppercase", "nt_" + strings.Repeat("A", 64), true},
		{"old-counter", "terminal-1-1", false},
		{"short", "nt_" + strings.Repeat("a", 63), false},
		{"long", "nt_" + strings.Repeat("a", 65), false},
		{"wrong-prefix", "xx_" + strings.Repeat("a", 64), false},
		{"non-hex", "nt_" + strings.Repeat("g", 64), false},
	} {
		t.Run(test.name, func(t *testing.T) {
			owner := &hostTerminalOwner{}
			err := owner.Reserve(t.Context(), TerminalIntent{ID: test.id, SessionID: "session", Generation: 1})
			if (err == nil) != test.valid {
				t.Fatalf("mock Node-equivalent reservation mismatch: %v", err)
			}
		})
	}
}
