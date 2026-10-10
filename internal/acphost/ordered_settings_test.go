package acphost

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sync"
	"testing"
	"time"

	"github.com/gopact-ai/acp"
)

// The test gate delays only return from a real outbound request write. The
// request has already reached the real SDK peer; responses and notifications
// still enter the production client handler on their actual wire contexts.
type orderedRequestGate struct {
	io.WriteCloser
	wait   func() error
	method string
}

func (g *orderedRequestGate) Write(frame []byte) (int, error) {
	n, err := g.WriteCloser.Write(frame)
	if err != nil {
		return n, err
	}
	var request struct {
		Method string `json:"method"`
	}
	if json.Unmarshal(frame, &request) == nil && request.Method == g.method && g.wait != nil {
		if err := g.wait(); err != nil {
			return n, err
		}
	}
	return n, nil
}

type orderedPeer struct {
	*lifecycleParticipant
	mu                 sync.Mutex
	initial            []acp.SessionConfigOption
	reply              []acp.SessionConfigOption
	before             *acp.SessionUpdate
	after              *acp.SessionUpdate
	pendingAfter       *acp.SessionNotification
	reject             bool
	restoreReply       *acp.LoadSessionResponse
	modeReply          *acp.SessionModeState
	modeBefore         *acp.SessionUpdate
	modeAfter          *acp.SessionUpdate
	initialModes       *acp.SessionModeState
	newAfter           *acp.SessionUpdate
	newNoticeWritten   chan struct{}
	newIDs             []acp.SessionID
	newCalls           int
	afterSentinel      *acp.SessionNotification
	configReplyWritten chan struct{}
	configReplyArmed   bool
	newEntered         chan struct{}
	newRelease         chan struct{}
	newBefore          []acp.SessionNotification
	newReplyError      error
	actualMode         acp.SessionModeID
	modeError          error
	modeEntered        chan struct{}
	modeRelease        chan struct{}
	modeReplyWritten   chan struct{}
	modeReplyArmed     bool
}

func (a *orderedPeer) NewSession(context.Context, *acp.NewSessionRequest) (*acp.NewSessionResponse, error) {
	a.record("new")
	sid := acp.SessionID("ordered")
	if len(a.newIDs) > 0 {
		sid = a.newIDs[a.newCalls]
		a.newCalls++
	}
	for _, notice := range a.newBefore {
		if err := a.client.Update(context.Background(), &notice); err != nil {
			return nil, err
		}
	}
	if a.newEntered != nil {
		close(a.newEntered)
		<-a.newRelease
	}
	if a.newReplyError != nil {
		return nil, a.newReplyError
	}
	if a.newAfter != nil {
		a.mu.Lock()
		a.pendingAfter = &acp.SessionNotification{SessionID: sid, Update: *a.newAfter}
		a.mu.Unlock()
	}
	return &acp.NewSessionResponse{SessionID: sid, ConfigOptions: &a.initial, Modes: a.initialModes}, nil
}
func (a *orderedPeer) SetSessionConfigOption(ctx context.Context, req *acp.SetSessionConfigOptionRequest) (*acp.SetSessionConfigOptionResponse, error) {
	a.record("configure")
	a.mu.Lock()
	a.configReplyArmed = true
	a.mu.Unlock()
	if a.before != nil {
		if err := a.client.Update(ctx, &acp.SessionNotification{SessionID: req.SessionID, Update: *a.before}); err != nil {
			return nil, err
		}
	}
	if a.after != nil {
		a.mu.Lock()
		a.pendingAfter = &acp.SessionNotification{SessionID: req.SessionID, Update: *a.after}
		a.mu.Unlock()
	}
	if a.reject {
		return nil, &acp.Error{Code: acp.ErrorCodeInvalidParams, Message: "explicit configuration rejection"}
	}
	return &acp.SetSessionConfigOptionResponse{ConfigOptions: a.reply}, nil
}

type orderedPeerWriter struct {
	output io.Writer
	agent  *orderedPeer
}

func (w orderedPeerWriter) Write(frame []byte) (int, error) {
	n, err := w.output.Write(frame)
	if err != nil {
		return n, err
	}
	var envelope struct {
		Result map[string]json.RawMessage `json:"result"`
		Error  json.RawMessage            `json:"error"`
	}
	if json.Unmarshal(frame, &envelope) != nil {
		return n, err
	}
	if envelope.Result != nil || len(envelope.Error) > 0 {
		w.agent.mu.Lock()
		if w.agent.modeReplyArmed && w.agent.modeReplyWritten != nil {
			w.agent.modeReplyArmed = false
			close(w.agent.modeReplyWritten)
		}
		if w.agent.configReplyArmed && w.agent.configReplyWritten != nil {
			w.agent.configReplyArmed = false
			close(w.agent.configReplyWritten)
		}
		notice := w.agent.pendingAfter
		w.agent.pendingAfter = nil
		w.agent.mu.Unlock()
		if notice != nil {
			go func() {
				_ = w.agent.client.Update(context.Background(), notice)
				if w.agent.afterSentinel != nil {
					_ = w.agent.client.Update(context.Background(), w.agent.afterSentinel)
				}
				if w.agent.newNoticeWritten != nil {
					close(w.agent.newNoticeWritten)
				}
			}()
		}
	}
	return n, nil
}

type orderedTransport struct {
	agent *orderedPeer
	gate  *orderedRequestGate
}

func (orderedTransport) Name() string { return "ordered-actual-wire" }
func (tr orderedTransport) Start(context.Context) (Process, error) {
	input, stdin := io.Pipe()
	stdout, output := io.Pipe()
	agent, err := acp.NewAgent(input, orderedPeerWriter{output: output, agent: tr.agent}, func(client *acp.ClientCaller) acp.AgentHandler { tr.agent.client = client; return tr.agent })
	if err != nil {
		_ = input.Close()
		_ = stdin.Close()
		_ = stdout.Close()
		_ = output.Close()
		return nil, err
	}
	tr.gate.WriteCloser = stdin
	return &orderedProcess{Process: &lifecycleProcess{conn: agent, stdin: stdin, stdout: stdout, output: output}, stdin: tr.gate}, nil
}

type orderedProcess struct {
	Process
	stdin io.WriteCloser
}

func (p *orderedProcess) Stdin() io.WriteCloser { return p.stdin }
func orderedHost(t *testing.T, a *orderedPeer, observe func(*sessionState) bool) *Host {
	t.Helper()
	a.lifecycleParticipant = &lifecycleParticipant{version: acp.ProtocolVersionV1, caps: lifecycleCaps(31)}
	gate := &orderedRequestGate{method: acp.MethodSessionSetConfigOption}
	h := New(Config{Transport: orderedTransport{agent: a, gate: gate}, NoRestart: true})
	t.Cleanup(h.Stop)
	if observe != nil {
		gate.wait = func() error {
			deadline := time.Now().Add(time.Second)
			for time.Now().Before(deadline) {
				h.mu.Lock()
				var state *sessionState
				if op := h.sessionOperations["ordered"]; op != nil {
					state = op.state
				} else {
					state = h.sessions["ordered"]
				}
				found := state != nil && observe(state)
				h.mu.Unlock()
				if found {
					return nil
				}
				time.Sleep(time.Millisecond)
			}
			return errors.New("later wire notification did not enter production handler")
		}
	}
	if _, _, err := h.OpenSession(t.Context(), "", SessionConfig{Workdir: t.TempDir()}); err != nil {
		t.Fatal(err)
	}
	return h
}
func orderedModeOptions(value bool, mode string) []acp.SessionConfigOption {
	options := wireBoolean(value)
	category := acp.SessionConfigOptionCategoryMode
	choices := acp.UngroupedSessionConfigSelectOptions{{Value: "agent", Name: "Agent"}, {Value: "reply", Name: "Reply"}, {Value: "later", Name: "Later"}}
	return append(options, acp.SessionConfigOption{ID: "mode", Name: "Mode", Type: acp.SessionConfigOptionTypeSelect, Category: &category, CurrentValue: acp.SessionConfigValueID(mode), Options: acp.SessionConfigSelectOptions{Ungrouped: &choices}})
}
func observedToggle(state *sessionState, value string) bool {
	for _, opt := range state.settings().Options {
		if opt.ID == "toggle" && opt.Current == value {
			return true
		}
	}
	return false
}
func TestOrderedEmptyReplyCannotEraseLaterConfigNotification(t *testing.T) {
	update := acp.ConfigOptionUpdateSessionUpdate(wireBoolean(true))
	a := &orderedPeer{initial: wireBoolean(false), reply: []acp.SessionConfigOption{}, after: &update}
	h := orderedHost(t, a, func(state *sessionState) bool { return observedToggle(state, "true") })
	if err := h.SetOption(t.Context(), "ordered", h.generation, "toggle", "true"); err != nil {
		t.Fatal(err)
	}
	assertOrderedToggle(t, h, true)
}
func TestOrderedNonemptyReplyCannotRollbackLaterNotification(t *testing.T) {
	update := acp.ConfigOptionUpdateSessionUpdate(wireBoolean(true))
	a := &orderedPeer{initial: wireBoolean(false), reply: wireBoolean(false), after: &update}
	h := orderedHost(t, a, func(state *sessionState) bool { return observedToggle(state, "true") })
	if err := h.SetOption(t.Context(), "ordered", h.generation, "toggle", "true"); err != nil {
		t.Fatal(err)
	}
	assertOrderedToggle(t, h, true)
}
func TestOrderedModeNotificationDoesNotDiscardValidFullListReply(t *testing.T) {
	update := acp.CurrentModeUpdateSessionUpdate("later")
	a := &orderedPeer{initial: orderedModeOptions(false, "agent"), reply: orderedModeOptions(true, "reply"), after: &update}
	h := orderedHost(t, a, func(state *sessionState) bool {
		state.mu.Lock()
		defer state.mu.Unlock()
		return state.modeID == "later"
	})
	if err := h.SetOption(t.Context(), "ordered", h.generation, "toggle", "true"); err != nil {
		t.Fatal(err)
	}
	assertOrderedToggle(t, h, true)
	var mode string
	for _, opt := range h.Options("ordered") {
		if opt.ID == "mode" {
			mode = fmt.Sprint(opt.CurrentValue)
		}
	}
	if mode != "later" {
		t.Fatalf("older mode selector overwrote later independent mode: %q", mode)
	}
}
func TestOrderedPriorNotificationCanBeCorrectedByReply(t *testing.T) {
	update := acp.ConfigOptionUpdateSessionUpdate(wireBoolean(true))
	a := &orderedPeer{initial: wireBoolean(false), reply: wireBoolean(false), before: &update}
	h := orderedHost(t, a, nil)
	if err := h.SetOption(t.Context(), "ordered", h.generation, "toggle", "true"); err != nil {
		t.Fatal(err)
	}
	assertOrderedToggle(t, h, false)
}
func TestOrderedEmptyReplyWithoutLaterNotificationReallyClears(t *testing.T) {
	a := &orderedPeer{initial: wireBoolean(false), reply: []acp.SessionConfigOption{}}
	h := orderedHost(t, a, nil)
	if err := h.SetOption(t.Context(), "ordered", h.generation, "toggle", "true"); err != nil {
		t.Fatal(err)
	}
	if len(h.Options("ordered")) != 0 {
		t.Fatal("legitimate full empty reply was not applied")
	}
}

func assertOrderedToggle(t *testing.T, h *Host, value bool) {
	t.Helper()
	for _, opt := range h.Options("ordered") {
		if opt.ID == "toggle" {
			if got, ok := opt.CurrentValue.(bool); !ok || got != value {
				t.Fatalf("ordered toggle Actual=%#v, want primitive %v", opt.CurrentValue, value)
			}
			return
		}
	}
	t.Fatalf("ordered SID lost toggle descriptor: %+v", h.Settings("ordered"))
}

func (a *orderedPeer) LoadSession(ctx context.Context, req *acp.LoadSessionRequest) (*acp.LoadSessionResponse, error) {
	return a.orderedRestore(ctx, req.SessionID, "load")
}
func (a *orderedPeer) ResumeSession(ctx context.Context, req *acp.ResumeSessionRequest) (*acp.ResumeSessionResponse, error) {
	response, err := a.orderedRestore(ctx, req.SessionID, "resume")
	if err != nil {
		return nil, err
	}
	return &acp.ResumeSessionResponse{ConfigOptions: response.ConfigOptions, Modes: response.Modes}, nil
}
func (a *orderedPeer) orderedRestore(ctx context.Context, sid acp.SessionID, method string) (*acp.LoadSessionResponse, error) {
	a.record(method)
	if a.before != nil {
		if err := a.client.Update(ctx, &acp.SessionNotification{SessionID: sid, Update: *a.before}); err != nil {
			return nil, err
		}
	}
	if a.after != nil {
		a.mu.Lock()
		a.pendingAfter = &acp.SessionNotification{SessionID: sid, Update: *a.after}
		a.mu.Unlock()
	}
	if a.restoreReply != nil {
		return a.restoreReply, nil
	}
	return &acp.LoadSessionResponse{ConfigOptions: &a.reply}, nil
}
func (a *orderedPeer) SetSessionMode(ctx context.Context, req *acp.SetSessionModeRequest) (*acp.SetSessionModeResponse, error) {
	a.record("set-mode")
	if a.modeEntered != nil {
		close(a.modeEntered)
		<-a.modeRelease
	}
	if a.modeError != nil {
		return nil, a.modeError
	}
	available := a.initialModes
	if available == nil && a.restoreReply != nil {
		available = a.restoreReply.Modes
	}
	valid := false
	if available != nil {
		for _, mode := range available.AvailableModes {
			if mode.ID == req.ModeID {
				valid = true
			}
		}
	}
	if !valid {
		return nil, &acp.Error{Code: acp.ErrorCodeInvalidParams, Message: "unavailable mode"}
	}
	a.mu.Lock()
	a.actualMode = req.ModeID
	a.modeReplyArmed = true
	a.mu.Unlock()
	if a.modeBefore != nil {
		if err := a.client.Update(ctx, &acp.SessionNotification{SessionID: req.SessionID, Update: *a.modeBefore}); err != nil {
			return nil, err
		}
	}
	if a.modeAfter != nil {
		a.mu.Lock()
		a.pendingAfter = &acp.SessionNotification{SessionID: req.SessionID, Update: *a.modeAfter}
		a.mu.Unlock()
	}
	return &acp.SetSessionModeResponse{}, nil
}
func TestOrderedWarmLoadAndResumeKeepLaterOptions(t *testing.T) {
	for _, method := range []string{"load", "resume"} {
		t.Run(method, func(t *testing.T) {
			update := acp.ConfigOptionUpdateSessionUpdate(wireBoolean(true))
			a := &orderedPeer{initial: wireBoolean(false), reply: []acp.SessionConfigOption{}, after: &update}
			h := orderedHost(t, a, nil)
			h.mu.Lock()
			transport := h.cfg.Transport.(orderedTransport)
			h.mu.Unlock()
			transport.gate.method = "session/" + method
			transport.gate.wait = func() error {
				deadline := time.Now().Add(time.Second)
				for time.Now().Before(deadline) {
					h.mu.Lock()
					op := h.sessionOperations["ordered"]
					found := op != nil && observedToggle(op.state, "true")
					h.mu.Unlock()
					if found {
						return nil
					}
					time.Sleep(time.Millisecond)
				}
				return errors.New("restore later notification did not enter pending state")
			}
			cfg := SessionConfig{Workdir: t.TempDir(), ReplayHistory: method == "load"}
			if method == "resume" {
				if err := h.CloseSession(t.Context(), "ordered"); err != nil {
					t.Fatal(err)
				}
			}
			if _, _, err := h.OpenSession(t.Context(), "ordered", cfg); err != nil {
				t.Fatal(err)
			}
			assertOrderedToggle(t, h, true)
		})
	}
}
func TestOrderedConfigErrorPreservesLaterConfirmation(t *testing.T) {
	update := acp.ConfigOptionUpdateSessionUpdate(wireBoolean(true))
	a := &orderedPeer{initial: wireBoolean(false), reject: true, after: &update}
	h := orderedHost(t, a, func(state *sessionState) bool { return observedToggle(state, "true") })
	var rpc *acp.Error
	if err := h.SetOption(t.Context(), "ordered", h.generation, "toggle", "true"); !errors.As(err, &rpc) {
		t.Fatalf("explicit error=%v", err)
	}
	assertOrderedToggle(t, h, true)
}
func TestOrderedShadowCopiesInboundClocks(t *testing.T) {
	update := acp.ConfigOptionUpdateSessionUpdate(wireBoolean(true))
	a := &orderedPeer{initial: wireBoolean(false), reply: []acp.SessionConfigOption{}, after: &update}
	h := orderedHost(t, a, func(state *sessionState) bool { return observedToggle(state, "true") })
	if err := h.SetOption(t.Context(), "ordered", h.generation, "toggle", "true"); err != nil {
		t.Fatal(err)
	}
	h.mu.Lock()
	state := h.sessions["ordered"]
	snapshot := copySessionState(state)
	h.mu.Unlock()
	state.mu.Lock()
	optionsSequence, modeSequence := state.optionsSequence, state.modeSequence
	state.mu.Unlock()
	if optionsSequence == 0 || snapshot.optionsSequence != optionsSequence || snapshot.modeSequence != modeSequence {
		t.Fatal("shadow/preserve lost real ingress clocks")
	}
	// A legacy/unsequenced initializer must not replace a real observation.
	snapshot.setOptions(wireBoolean(false))
	if !observedToggle(snapshot, "true") {
		t.Fatal("unsequenced fallback overwrote ordered Actual")
	}
}
func TestOrderedCurrentModePriorToFullListReplyCanBeCorrected(t *testing.T) {
	update := acp.CurrentModeUpdateSessionUpdate("later")
	a := &orderedPeer{initial: orderedModeOptions(false, "agent"), reply: orderedModeOptions(true, "reply"), before: &update}
	h := orderedHost(t, a, nil)
	if err := h.SetOption(t.Context(), "ordered", h.generation, "toggle", "true"); err != nil {
		t.Fatal(err)
	}
	for _, option := range h.Options("ordered") {
		if option.ID == "mode" && fmt.Sprint(option.CurrentValue) != "reply" {
			t.Fatalf("prior mode notice incorrectly blocked later correction: %+v", option)
		}
	}
	assertOrderedToggle(t, h, true)
}

func TestOrderedModeAckCannotOverrideLaterActualMode(t *testing.T) {
	notice := acp.CurrentModeUpdateSessionUpdate("after-mode")
	modes := &acp.SessionModeState{CurrentModeID: "agent", AvailableModes: []acp.SessionMode{{ID: "agent", Name: "Agent"}, {ID: "read-only", Name: "Read-only"}, {ID: "after-mode", Name: "After mode"}}}
	reply := wireBoolean(false)
	a := &orderedPeer{initial: wireBoolean(false), restoreReply: &acp.LoadSessionResponse{Modes: modes, ConfigOptions: &reply}, modeAfter: &notice}
	h := orderedHost(t, a, nil)
	gate := h.cfg.Transport.(orderedTransport).gate
	gate.method = acp.MethodSessionSetMode
	gate.wait = func() error {
		deadline := time.Now().Add(time.Second)
		for time.Now().Before(deadline) {
			h.mu.Lock()
			op := h.sessionOperations["ordered"]
			found := op != nil && op.state.settings().Mode == "After mode"
			h.mu.Unlock()
			if found {
				return nil
			}
			time.Sleep(time.Millisecond)
		}
		return errors.New("later mode notice did not reach the original restore state")
	}
	if _, _, err := h.OpenSession(t.Context(), "ordered", SessionConfig{Workdir: t.TempDir(), ReplayHistory: true}); err != nil {
		t.Fatal(err)
	}
	if got := h.Settings("ordered").Mode; got != "After mode" {
		t.Fatalf("set_mode ACK/request overwrote later Actual: %q", got)
	}
	requireMethods(t, a.lifecycleParticipant, []string{"initialize", "new", "load", "set-mode"})
}
func TestOrderedInitialFieldsHaveRealReceiptClocks(t *testing.T) {
	a := &orderedPeer{initial: orderedModeOptions(false, "reply"), initialModes: &acp.SessionModeState{CurrentModeID: "agent", AvailableModes: []acp.SessionMode{{ID: "agent", Name: "Agent"}, {ID: "reply", Name: "Reply"}}}}
	h := orderedHost(t, a, nil)
	h.mu.Lock()
	state := h.sessions["ordered"]
	state.mu.Lock()
	optionsSequence, modeSequence, modesSequence := state.optionsSequence, state.modeSequence, state.modesSequence
	state.mu.Unlock()
	h.mu.Unlock()
	if optionsSequence == 0 || modeSequence != optionsSequence || modesSequence != optionsSequence {
		t.Fatalf("initial receipt fields were not sequenced: options=%d mode=%d catalog=%d", optionsSequence, modeSequence, modesSequence)
	}
	// This preserves the existing cold constructor's modes->options tie rule,
	// not a claim about an unverified protocol MUST.
	if got := h.Settings("ordered").Mode; got != "Reply" {
		t.Fatalf("cold same-frame tie changed: %q", got)
	}
}

func TestOrderedReceiptCannotPublishAcrossOriginalGeneration(t *testing.T) {
	a := &orderedPeer{initial: wireBoolean(false), reply: wireBoolean(true), newIDs: []acp.SessionID{"ordered", "replacement"}, configReplyWritten: make(chan struct{})}
	a.lifecycleParticipant = &lifecycleParticipant{version: acp.ProtocolVersionV1, caps: lifecycleCaps(31)}
	release := make(chan struct{})
	defer close(release)
	gate := &orderedRequestGate{method: acp.MethodSessionSetConfigOption, wait: func() error { <-release; return nil }}
	h := New(Config{Transport: orderedTransport{agent: a, gate: gate}})
	t.Cleanup(h.Stop)
	sid, originalGeneration, err := h.OpenSession(t.Context(), "", SessionConfig{Workdir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- h.SetOption(t.Context(), sid, originalGeneration, "toggle", "true") }()
	awaitOperation(t, a.configReplyWritten)
	h.Stop() // A real stream exit/watch reset, not a fabricated generation number.
	replacement, newGeneration, err := h.OpenSession(t.Context(), "", SessionConfig{Workdir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	if originalGeneration == newGeneration || replacement == sid {
		t.Fatal("fixture did not actually replace the runtime/SID")
	}
	release <- struct{}{}
	if err := <-done; err == nil {
		t.Fatal("matched old receipt confirmed against a replacement context")
	}
	for _, option := range h.Options(replacement) {
		if option.ID == "toggle" && option.CurrentValue != false {
			t.Fatalf("old receipt mutated replacement Actual: %#v", option.CurrentValue)
		}
	}
}
func TestOrderedMetadataIsSIDLocalWithinOneConnection(t *testing.T) {
	a := &orderedPeer{initial: wireBoolean(false), reply: []acp.SessionConfigOption{}, newIDs: []acp.SessionID{"ordered", "other"}}
	h := orderedHost(t, a, nil)
	if _, _, err := h.OpenSession(t.Context(), "", SessionConfig{Workdir: t.TempDir()}); err != nil {
		t.Fatal(err)
	}
	if err := h.SetOption(t.Context(), "ordered", h.generation, "toggle", "true"); err != nil {
		t.Fatal(err)
	}
	if len(h.Options("ordered")) != 0 {
		t.Fatal("ordered SID did not accept its legitimate empty full response")
	}
	for _, option := range h.Options("other") {
		if option.ID == "toggle" && option.CurrentValue != false {
			t.Fatal("another SID's response mutated this session")
		}
	}
}
func TestOrderedConfigurationStillAllowsBidirectionalPermission(t *testing.T) {
	a := &operationParticipant{configEntered: make(chan struct{}), configRelease: make(chan struct{}), configReverse: true}
	h := operationHost(t, a)
	sid, gen, err := h.OpenSession(t.Context(), "", SessionConfig{Workdir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- h.SetOption(t.Context(), sid, gen, "model", "configured") }()
	awaitOperation(t, a.configEntered)
	// Peer reaches entered only after its real reverse permission RPC returns.
	close(a.configRelease)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if h.Settings(sid).Model != "Configured model" {
		t.Fatal("typed receipt did not publish the confirmed reply")
	}
}

func TestOrderedModeStandardSuccessACKConfirmsPeerActual(t *testing.T) {
	modes := &acp.SessionModeState{CurrentModeID: "agent", AvailableModes: []acp.SessionMode{{ID: "agent", Name: "Agent"}, {ID: "read-only", Name: "Read-only"}}}
	a := &orderedPeer{initial: orderedModeOptions(false, "agent"), initialModes: modes}
	h := orderedHost(t, a, nil)
	a.mu.Lock()
	actual := a.actualMode
	a.mu.Unlock()
	if actual != "read-only" {
		t.Fatalf("real peer did not execute the available requested mode: %q", actual)
	}
	if got := h.Settings("ordered").Mode; got != "Read-only" {
		t.Fatalf("successful standard {} ACK left Host Actual stale while peer=%q: %q", actual, got)
	}
	h.mu.Lock()
	state := h.sessions["ordered"]
	state.mu.Lock()
	optionsClock, modeClock := state.optionsSequence, state.modeSequence
	state.mu.Unlock()
	h.mu.Unlock()
	if modeClock <= optionsClock {
		t.Fatalf("successful set_mode ACK did not publish its actual inbound clock: mode=%d options=%d", modeClock, optionsClock)
	}
}

func TestOrderedModeErrorCannotConfirmRequestedTarget(t *testing.T) {
	modes := &acp.SessionModeState{CurrentModeID: "agent", AvailableModes: []acp.SessionMode{{ID: "agent", Name: "Agent"}, {ID: "read-only", Name: "Read-only"}}}
	a := &orderedPeer{initial: orderedModeOptions(false, "agent"), initialModes: modes, modeError: &acp.Error{Code: acp.ErrorCodeInvalidParams, Message: "mode rejected"}}
	h := orderedHost(t, a, nil)
	if got := h.Settings("ordered").Mode; got != "Agent" {
		t.Fatalf("mode rejection confirmed requested target: %q", got)
	}
}
func TestOrderedModeLocalCancelDoesNotConfirmLateStandardACK(t *testing.T) {
	modes := &acp.SessionModeState{CurrentModeID: "agent", AvailableModes: []acp.SessionMode{{ID: "agent", Name: "Agent"}, {ID: "read-only", Name: "Read-only"}}}
	a := &orderedPeer{initial: orderedModeOptions(false, "agent"), restoreReply: &acp.LoadSessionResponse{Modes: modes, ConfigOptions: func() *[]acp.SessionConfigOption { value := orderedModeOptions(false, "agent"); return &value }()}, modeEntered: make(chan struct{}), modeRelease: make(chan struct{})}
	h := orderedHost(t, a, nil)
	t.Cleanup(func() {
		select {
		case <-a.modeRelease:
		default:
			close(a.modeRelease)
		}
	})
	ctx, cancel := context.WithCancelCause(t.Context())
	defer cancel(nil)
	done := make(chan error, 1)
	go func() {
		_, _, err := h.OpenSession(ctx, "ordered", SessionConfig{Workdir: t.TempDir(), ReplayHistory: true})
		done <- err
	}()
	awaitOperation(t, a.modeEntered)
	cancel(&acp.Error{Code: acp.ErrorCodeRequestCanceled, Message: "local mode cancel"})
	<-done // Existing best-effort mode-error policy is unchanged.
	if got := h.Settings("ordered").Mode; got != "Agent" {
		t.Fatalf("local cancel confirmed requested mode before ACK: %q", got)
	}
	close(a.modeRelease)
	if _, err := h.ListSessions(t.Context()); err != nil {
		t.Fatal(err)
	}
	if got := h.Settings("ordered").Mode; got != "Agent" {
		t.Fatalf("late unmatched mode ACK confirmed requested mode: %q", got)
	}
}

func TestOrderedModeReceiptCannotConfirmAfterRealGenerationExit(t *testing.T) {
	modes := &acp.SessionModeState{CurrentModeID: "agent", AvailableModes: []acp.SessionMode{{ID: "agent", Name: "Agent"}, {ID: "read-only", Name: "Read-only"}}}
	options := wireBoolean(false)
	a := &orderedPeer{initial: options, restoreReply: &acp.LoadSessionResponse{Modes: modes, ConfigOptions: &options}, newIDs: []acp.SessionID{"ordered", "replacement"}, modeReplyWritten: make(chan struct{})}
	a.lifecycleParticipant = &lifecycleParticipant{version: 1, caps: lifecycleCaps(31)}
	release := make(chan struct{})
	defer close(release)
	gate := &orderedRequestGate{method: acp.MethodSessionSetMode, wait: func() error { <-release; return nil }}
	h := New(Config{Transport: orderedTransport{agent: a, gate: gate}})
	t.Cleanup(h.Stop)
	sid, oldGeneration, err := h.OpenSession(t.Context(), "", SessionConfig{Workdir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		_, _, err := h.OpenSession(t.Context(), sid, SessionConfig{Workdir: t.TempDir(), ReplayHistory: true})
		done <- err
	}()
	awaitOperation(t, a.modeReplyWritten)
	a.mu.Lock()
	actual := a.actualMode
	a.mu.Unlock()
	if actual != "read-only" {
		t.Fatal("real original peer did not execute its available mode")
	}
	h.Stop()
	replacement, newGeneration, err := h.OpenSession(t.Context(), "", SessionConfig{Workdir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	if newGeneration == oldGeneration || replacement == sid {
		t.Fatal("real runtime/SID was not replaced")
	}
	release <- struct{}{}
	if err := <-done; err == nil {
		t.Fatal("old mode receipt completed against replacement identity")
	}
	if got := h.Settings(replacement).Mode; got != "" {
		t.Fatalf("old queued mode ACK wrote replacement Actual: %q", got)
	}
}
