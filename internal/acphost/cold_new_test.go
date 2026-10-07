package acphost

import (
	"context"
	"errors"
	"fmt"
	"github.com/gopact-ai/acp"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestOrderedColdNewUnknownSIDStillLosesConfirmedLaterOptions(t *testing.T) {
	a := &orderedPeer{initial: wireBoolean(false), newIDs: []acp.SessionID{"known", "cold-new"}}
	a.lifecycleParticipant = &lifecycleParticipant{version: acp.ProtocolVersionV1, caps: lifecycleCaps(31)}
	gate := &orderedRequestGate{}
	h := New(Config{Transport: orderedTransport{agent: a, gate: gate}, NoRestart: true})
	t.Cleanup(h.Stop)
	if _, _, err := h.OpenSession(t.Context(), "", SessionConfig{Workdir: t.TempDir()}); err != nil {
		t.Fatal(err)
	}
	update := acp.ConfigOptionUpdateSessionUpdate(wireBoolean(true))
	a.newAfter = &update
	a.afterSentinel = &acp.SessionNotification{SessionID: "known", Update: acp.CurrentModeUpdateSessionUpdate("sentinel")}
	gate.method = acp.MethodSessionNew
	gate.wait = func() error {
		deadline := time.Now().Add(time.Second)
		for time.Now().Before(deadline) {
			if h.Settings("known").Mode == "sentinel" {
				return nil
			}
			time.Sleep(time.Millisecond)
		}
		return errors.New("unknown SID notice did not precede a processed known-SID sentinel")
	}
	if _, _, err := h.OpenSession(context.Background(), "", SessionConfig{Workdir: t.TempDir()}); err != nil {
		t.Fatal(err)
	}
	for _, option := range h.Options("cold-new") {
		if option.ID == "toggle" {
			if option.CurrentValue != true {
				t.Fatalf("cold-new later confirmed Actual lost: %#v", option.CurrentValue)
			}
			return
		}
	}
	t.Fatal("cold-new confirmed descriptor disappeared")
}

func coldNotice(sid acp.SessionID, update acp.SessionUpdate) acp.SessionNotification {
	return acp.SessionNotification{SessionID: sid, Update: update}
}
func TestColdNewPriorFieldsRemainIndependentAndReplyCanCorrect(t *testing.T) {
	options := wireBoolean(true)
	a := &orderedPeer{initial: wireBoolean(false), newBefore: []acp.SessionNotification{
		coldNotice("ordered", acp.ConfigOptionUpdateSessionUpdate(options)),
		coldNotice("ordered", acp.CurrentModeUpdateSessionUpdate("independent-mode")),
		coldNotice("ordered", acp.AvailableCommandsUpdateSessionUpdate([]acp.AvailableCommand{{Name: "safe", Description: "Confirmed command"}})),
	}}
	h := orderedHost(t, a, nil)
	assertOrderedToggle(t, h, false) // New reply later corrects the prior options.
	h.mu.Lock()
	state := h.sessions["ordered"]
	state.mu.Lock()
	mode, commands := state.modeID, len(state.commands)
	state.mu.Unlock()
	scratch := h.newOpening
	h.mu.Unlock()
	if mode != "independent-mode" || commands != 1 || scratch != nil {
		t.Fatalf("whole-snapshot merge lost independent facts: mode=%q commands=%d scratch=%v", mode, commands, scratch != nil)
	}
}
func TestColdNewActualEmptyFullListClearsOnlyOptions(t *testing.T) {
	a := &orderedPeer{initial: []acp.SessionConfigOption{}, newBefore: []acp.SessionNotification{
		coldNotice("ordered", acp.ConfigOptionUpdateSessionUpdate(wireBoolean(true))),
		coldNotice("ordered", acp.CurrentModeUpdateSessionUpdate("still-mode")),
		coldNotice("ordered", acp.AvailableCommandsUpdateSessionUpdate([]acp.AvailableCommand{{Name: "safe", Description: "Command"}})),
	}}
	h := orderedHost(t, a, nil)
	if len(h.Options("ordered")) != 0 || h.Settings("ordered").Mode != "still-mode" {
		t.Fatal("empty response dropped independent mode or failed to clear options")
	}
	h.mu.Lock()
	commands := len(h.sessions["ordered"].commands)
	h.mu.Unlock()
	if commands != 1 {
		t.Fatal("options empty dropped commands")
	}
}
func TestColdNewSinglePendingAdmissionLeavesExistingSIDUsable(t *testing.T) {
	a := &orderedPeer{initial: wireBoolean(false), reply: wireBoolean(true), newIDs: []acp.SessionID{"ordered", "second"}}
	h := orderedHost(t, a, nil)
	a.newEntered = make(chan struct{})
	a.newRelease = make(chan struct{})
	t.Cleanup(func() {
		select {
		case <-a.newRelease:
		default:
			close(a.newRelease)
		}
	})
	a.newBefore = []acp.SessionNotification{coldNotice("second", acp.ConfigOptionUpdateSessionUpdate(wireBoolean(true)))}
	created := make(chan error, 1)
	go func() {
		_, _, err := h.OpenSession(t.Context(), "", SessionConfig{Workdir: t.TempDir()})
		created <- err
	}()
	awaitOperation(t, a.newEntered)
	if err := h.CloseIdle(); !errors.Is(err, ErrSessionBusy) {
		t.Fatalf("idle shutdown ignored anonymous opening reservation: %v", err)
	}
	h.mu.Lock()
	_, published := h.sessions["second"]
	h.mu.Unlock()
	if published {
		t.Fatal("unknown SID was granted a formal session before matched reply")
	}
	if _, _, err := h.OpenSession(t.Context(), "", SessionConfig{Workdir: t.TempDir()}); !errors.Is(err, ErrSessionBusy) {
		t.Fatalf("second concurrent New=%v", err)
	}
	if a.newCalls != 2 {
		t.Fatalf("busy New emitted an extra RPC: %d", a.newCalls)
	}
	if err := h.SetOption(t.Context(), "ordered", h.generation, "toggle", "true"); err != nil {
		t.Fatalf("existing SID config blocked by New: %v", err)
	}
	a.promptStarted = make(chan struct{})
	a.promptRelease = make(chan struct{})
	close(a.promptRelease)
	if out, _, err := h.Prompt(t.Context(), "ordered", h.generation, "existing safe test prompt", nil); err != nil || out != "fresh answer" {
		t.Fatalf("existing SID prompt blocked: %q %v", out, err)
	}
	close(a.newRelease)
	if err := <-created; err != nil {
		t.Fatal(err)
	}
}
func TestColdNewRejectionClosesScratchWithoutCreatingSession(t *testing.T) {
	a := &orderedPeer{initial: wireBoolean(false), newBefore: []acp.SessionNotification{coldNotice("ordered", acp.ConfigOptionUpdateSessionUpdate(wireBoolean(true)))}, newReplyError: &acp.Error{Code: acp.ErrorCodeInvalidParams, Message: "rejected New"}}
	a.lifecycleParticipant = &lifecycleParticipant{version: 1, caps: lifecycleCaps(31)}
	h := New(Config{Transport: orderedTransport{agent: a, gate: &orderedRequestGate{}}, NoRestart: true})
	t.Cleanup(h.Stop)
	_, _, err := h.OpenSession(t.Context(), "", SessionConfig{Workdir: t.TempDir()})
	var response *acp.Error
	if !errors.As(err, &response) {
		t.Fatalf("expected typed rejection: %v", err)
	}
	h.mu.Lock()
	scratch, known := h.newOpening, h.sessions["ordered"]
	h.mu.Unlock()
	if scratch != nil || known != nil {
		t.Fatal("rejection retained scratch or invented a session")
	}
	a.newBefore = nil
	a.newReplyError = nil
	if _, _, err := h.OpenSession(t.Context(), "", SessionConfig{Workdir: t.TempDir()}); err != nil {
		t.Fatalf("explicit rejection prevented a later explicit New: %v", err)
	}
}
func TestColdNewCollisionRejectsAndPreservesOriginalPointer(t *testing.T) {
	a := &orderedPeer{initial: wireBoolean(false), newIDs: []acp.SessionID{"ordered", "ordered"}}
	h := orderedHost(t, a, nil)
	h.mu.Lock()
	original := h.sessions["ordered"]
	h.mu.Unlock()
	if _, _, err := h.OpenSession(t.Context(), "", SessionConfig{Workdir: t.TempDir()}); err == nil {
		t.Fatal("provider reused an existing SID without rejection")
	}
	h.mu.Lock()
	same := h.sessions["ordered"] == original
	gate := h.newOpening
	h.mu.Unlock()
	if !same || gate == nil || gate.pending {
		t.Fatal("collision erased original context or lost the uncertain opening obligation")
	}
	assertOrderedToggle(t, h, false)
	if _, _, err := h.OpenSession(t.Context(), "", SessionConfig{Workdir: t.TempDir()}); !errors.Is(err, ErrSessionOperationUnconfirmed) {
		t.Fatalf("collision automatically retried New: %v", err)
	}
}
func TestColdNewScratchLimitsFailExplicitlyAndDoNotPublish(t *testing.T) {
	cases := []struct {
		name    string
		notices []acp.SessionNotification
		initial []acp.SessionConfigOption
	}{
		{"SID count", func() []acp.SessionNotification {
			var notices []acp.SessionNotification
			for i := 0; i < maxNewScratchSIDs+1; i++ {
				notices = append(notices, coldNotice(acp.SessionID(fmt.Sprint("candidate-", i)), acp.CurrentModeUpdateSessionUpdate("mode")))
			}
			return notices
		}(), wireBoolean(false)},
		{"update count", func() []acp.SessionNotification {
			var notices []acp.SessionNotification
			for i := 0; i < maxNewScratchUpdates+1; i++ {
				notices = append(notices, coldNotice("ordered", acp.CurrentModeUpdateSessionUpdate("mode")))
			}
			return notices
		}(), wireBoolean(false)},
		{"string bytes", []acp.SessionNotification{coldNotice("ordered", acp.CurrentModeUpdateSessionUpdate(acp.SessionModeID(strings.Repeat("x", maxNewScratchString+1))))}, wireBoolean(false)},
		{"options fields", nil, func() []acp.SessionConfigOption {
			var options []acp.SessionConfigOption
			for i := 0; i < maxNewScratchOptions+1; i++ {
				options = append(options, acp.BooleanSessionConfigOption(acp.SessionConfigID(fmt.Sprint("toggle", i)), "Toggle", false))
			}
			return options
		}()},
		{"per SID bytes", []acp.SessionNotification{coldNotice("ordered", acp.AvailableCommandsUpdateSessionUpdate(func() []acp.AvailableCommand {
			var commands []acp.AvailableCommand
			for i := 0; i < 9; i++ {
				commands = append(commands, acp.AvailableCommand{Name: fmt.Sprint("cmd", i), Description: strings.Repeat("x", maxNewScratchString)})
			}
			return commands
		}()))}, wireBoolean(false)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			a := &orderedPeer{initial: tc.initial, newBefore: tc.notices}
			a.lifecycleParticipant = &lifecycleParticipant{version: 1, caps: lifecycleCaps(31)}
			h := New(Config{Transport: orderedTransport{agent: a, gate: &orderedRequestGate{}}, NoRestart: true})
			t.Cleanup(h.Stop)
			if _, _, err := h.OpenSession(t.Context(), "", SessionConfig{Workdir: t.TempDir()}); !errors.Is(err, errNewScratchLimit) {
				t.Fatalf("bounded overflow=%v", err)
			}
			h.mu.Lock()
			opening := h.newOpening
			known := h.sessions["ordered"]
			op := h.sessionOperations["ordered"]
			h.mu.Unlock()
			if opening == nil || opening.accepting || len(opening.candidates) != 0 || known != nil || op == nil {
				t.Fatal("overflow published/retained unbounded state or lost known-SID cleanup obligation")
			}
			if _, _, err := h.OpenSession(t.Context(), "", SessionConfig{Workdir: t.TempDir()}); !errors.Is(err, ErrSessionOperationUnconfirmed) {
				t.Fatalf("overflow blindly retried: %v", err)
			}
		})
	}
}

func TestColdNewUnknownClosesIntakeAndNeverAutomaticallyRetries(t *testing.T) {
	a := &orderedPeer{initial: wireBoolean(false), reply: wireBoolean(true), newIDs: []acp.SessionID{"ordered", "unbound"}}
	h := orderedHost(t, a, nil)
	a.newEntered = make(chan struct{})
	a.newRelease = make(chan struct{})
	t.Cleanup(func() {
		select {
		case <-a.newRelease:
		default:
			close(a.newRelease)
		}
	})
	a.newBefore = []acp.SessionNotification{coldNotice("unbound", acp.ConfigOptionUpdateSessionUpdate(wireBoolean(true)))}
	ctx, cancel := context.WithCancelCause(t.Context())
	defer cancel(nil)
	done := make(chan error, 1)
	go func() { _, _, err := h.OpenSession(ctx, "", SessionConfig{Workdir: t.TempDir()}); done <- err }()
	awaitOperation(t, a.newEntered)
	cause := &acp.Error{Code: acp.ErrorCodeRequestCanceled, Message: "local New cancellation"}
	cancel(cause)
	if err := <-done; !errors.Is(err, ErrSessionOperationUnconfirmed) || !errors.Is(err, cause) {
		t.Fatalf("unknown New=%v", err)
	}
	h.mu.Lock()
	opening := h.newOpening
	h.mu.Unlock()
	if opening == nil || opening.pending || opening.accepting || len(opening.candidates) != 0 || opening.bytes != 0 {
		t.Fatal("unknown creation kept intake/data or lost original obligation")
	}
	if _, _, err := h.OpenSession(t.Context(), "", SessionConfig{Workdir: t.TempDir()}); !errors.Is(err, ErrSessionOperationUnconfirmed) {
		t.Fatalf("unknown New reissued: %v", err)
	}
	if err := h.SetOption(t.Context(), "ordered", h.generation, "toggle", "true"); err != nil {
		t.Fatalf("existing known SID config blocked by unnamed debt: %v", err)
	}
	close(a.newRelease)
	// A later existing-SID notification and real list reply drain the peer's
	// canceled New response. They do not turn that unmatched reply into proof.
	if err := a.client.Update(t.Context(), &acp.SessionNotification{SessionID: "ordered", Update: acp.CurrentModeUpdateSessionUpdate("after-late")}); err != nil {
		t.Fatal(err)
	}
	if _, err := h.ListSessions(t.Context()); err != nil {
		t.Fatal(err)
	}
	if _, _, err := h.OpenSession(t.Context(), "", SessionConfig{Workdir: t.TempDir()}); !errors.Is(err, ErrSessionOperationUnconfirmed) {
		t.Fatalf("late unmatched reply lifted New admission: %v", err)
	}
	h.mu.Lock()
	_, known := h.sessions["unbound"]
	h.mu.Unlock()
	if known || a.newCalls != 2 {
		t.Fatal("late unknown SID was published or a third New reached wire")
	}
}
func TestColdNewGenerationExitDoesNotInventStopOrStageLaterValues(t *testing.T) {
	a := &orderedPeer{initial: wireBoolean(false), newIDs: []acp.SessionID{"ordered", "unbound"}}
	h := orderedHost(t, a, nil)
	generation := h.generation
	a.newEntered = make(chan struct{})
	a.newRelease = make(chan struct{})
	t.Cleanup(func() {
		select {
		case <-a.newRelease:
		default:
			close(a.newRelease)
		}
	})
	done := make(chan error, 1)
	go func() { _, _, err := h.OpenSession(t.Context(), "", SessionConfig{Workdir: t.TempDir()}); done <- err }()
	awaitOperation(t, a.newEntered)
	h.mu.Lock()
	proc := h.proc
	h.mu.Unlock()
	proc.Kill() // Only this deterministic test peer's real ACP stream exits.
	close(a.newRelease)
	select {
	case err := <-done:
		if !errors.Is(err, ErrSessionOperationUnconfirmed) {
			t.Fatalf("EOF New=%v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("stream exit did not end New")
	}
	waitCloseWatch(t, h)
	if h.ProcessStopped(generation) {
		t.Fatal("stream loss invented positive native stop")
	}
	if _, _, err := h.OpenSession(t.Context(), "", SessionConfig{Workdir: t.TempDir()}); !errors.Is(err, ErrSessionOperationUnconfirmed) {
		t.Fatalf("watch reset lost unnamed obligation: %v", err)
	}
	h.mu.Lock()
	opening := h.newOpening
	h.mu.Unlock()
	if opening == nil || opening.accepting || len(opening.candidates) != 0 {
		t.Fatal("ended generation still owns mutable scratch")
	}
}
func TestColdNewAggregateByteAndMetadataBounds(t *testing.T) {
	manyCommands := func() []acp.AvailableCommand {
		var values []acp.AvailableCommand
		for i := 0; i < 6; i++ {
			values = append(values, acp.AvailableCommand{Name: fmt.Sprint("cmd", i), Description: strings.Repeat("x", maxNewScratchString)})
		}
		return values
	}
	aggregate := []acp.SessionNotification{}
	for i := 0; i < 6; i++ {
		aggregate = append(aggregate, coldNotice(acp.SessionID(fmt.Sprint("candidate", i)), acp.AvailableCommandsUpdateSessionUpdate(manyCommands())))
	}
	metaNodes := map[string]any{}
	for i := 0; i < maxNewScratchMetaNodes+1; i++ {
		metaNodes[fmt.Sprint("n", i)] = true
	}
	deep := any(true)
	for i := 0; i < maxNewScratchMetaDepth+1; i++ {
		deep = map[string]any{"next": deep}
	}
	for _, tc := range []struct {
		name    string
		notices []acp.SessionNotification
	}{
		{"aggregate bytes", aggregate},
		{"metadata nodes", []acp.SessionNotification{coldNotice("ordered", acp.ConfigOptionUpdateSessionUpdate(func() []acp.SessionConfigOption {
			values := wireBoolean(false)
			values[0].Meta = acp.Meta(metaNodes)
			return values
		}()))}},
		{"metadata depth", []acp.SessionNotification{coldNotice("ordered", acp.ConfigOptionUpdateSessionUpdate(func() []acp.SessionConfigOption {
			values := wireBoolean(false)
			values[0].Meta = acp.Meta{"tree": deep}
			return values
		}()))}},
		{"commands fields", []acp.SessionNotification{coldNotice("ordered", acp.AvailableCommandsUpdateSessionUpdate(func() []acp.AvailableCommand {
			var values []acp.AvailableCommand
			for i := 0; i < maxNewScratchCommands+1; i++ {
				values = append(values, acp.AvailableCommand{Name: fmt.Sprint("cmd", i), Description: "Command"})
			}
			return values
		}()))}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a := &orderedPeer{initial: wireBoolean(false), newBefore: tc.notices}
			a.lifecycleParticipant = &lifecycleParticipant{version: 1, caps: lifecycleCaps(31)}
			h := New(Config{Transport: orderedTransport{agent: a, gate: &orderedRequestGate{}}, NoRestart: true})
			t.Cleanup(h.Stop)
			if _, _, err := h.OpenSession(t.Context(), "", SessionConfig{Workdir: t.TempDir()}); !errors.Is(err, errNewScratchLimit) {
				t.Fatalf("bounds error=%v", err)
			}
			h.mu.Lock()
			opening := h.newOpening
			published := len(h.sessions)
			h.mu.Unlock()
			if opening == nil || opening.bytes != 0 || len(opening.candidates) != 0 || published != 0 {
				t.Fatal("bounded failure left retained candidates or publication")
			}
		})
	}
}
func TestColdNewOutsideWindowNeverCreatesAnOwner(t *testing.T) {
	a := &orderedPeer{initial: wireBoolean(false)}
	h := orderedHost(t, a, nil)
	if err := a.client.Update(t.Context(), &acp.SessionNotification{SessionID: "outside", Update: acp.ConfigOptionUpdateSessionUpdate(wireBoolean(true))}); err != nil {
		t.Fatal(err)
	}
	if _, err := h.ListSessions(t.Context()); err != nil {
		t.Fatal(err)
	}
	h.mu.Lock()
	opening := h.newOpening
	state := h.sessions["outside"]
	h.mu.Unlock()
	if opening != nil || state != nil {
		t.Fatal("outside-window notification became an opening or formal session")
	}
}

type coldOwnedAgent struct {
	*lifecycleParticipant
	marker  string
	created int
}

func (a *coldOwnedAgent) NewSession(ctx context.Context, _ *acp.NewSessionRequest) (*acp.NewSessionResponse, error) {
	a.created++
	if a.created == 1 {
		options := wireBoolean(false)
		return &acp.NewSessionResponse{SessionID: "owned-known", ConfigOptions: &options}, nil
	}
	if err := os.WriteFile(a.marker, []byte(fmt.Sprint(a.created)), 0600); err != nil {
		return nil, err
	}
	<-ctx.Done()
	return nil, ctx.Err()
}
func TestColdNewOwnedProcessHelper(t *testing.T) {
	marker := os.Getenv("STEVE_TEST_COLD_NEW_MARKER")
	if marker == "" {
		return
	}
	peer := &coldOwnedAgent{lifecycleParticipant: &lifecycleParticipant{version: 1, caps: lifecycleCaps(31)}, marker: marker}
	conn, err := acp.NewAgent(os.Stdin, os.Stdout, func(client *acp.ClientCaller) acp.AgentHandler { peer.client = client; return peer })
	if err != nil {
		os.Exit(2)
	}
	<-conn.Done()
	os.Exit(0)
}
func TestColdNewOnlyOriginalOwnedGroupStopRetiresUnnamedUnknown(t *testing.T) {
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(t.TempDir(), "new-count")
	h := New(Config{Command: executable, Args: []string{"-test.run=^TestColdNewOwnedProcessHelper$"}, Env: []string{"STEVE_TEST_COLD_NEW_MARKER=" + marker}, ProcessDir: t.TempDir()})
	t.Cleanup(h.Stop)
	if _, _, err := h.OpenSession(t.Context(), "", SessionConfig{Workdir: t.TempDir()}); err != nil {
		t.Fatal(err)
	}
	originalGeneration := h.generation
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan error, 1)
	go func() { _, _, err := h.OpenSession(ctx, "", SessionConfig{Workdir: t.TempDir()}); done <- err }()
	deadline := time.Now().Add(time.Second)
	for {
		if _, err := os.Stat(marker); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("New never reached real owned helper")
		}
		time.Sleep(time.Millisecond)
	}
	cancel()
	if err := <-done; !errors.Is(err, ErrSessionOperationUnconfirmed) {
		t.Fatalf("unnamed unknown=%v", err)
	}
	if h.ProcessStopped(originalGeneration) {
		t.Fatal("cancel invented group stop")
	}
	if _, _, err := h.OpenSession(t.Context(), "", SessionConfig{Workdir: t.TempDir()}); !errors.Is(err, ErrSessionOperationUnconfirmed) {
		t.Fatalf("live unnamed unknown allowed another New: %v", err)
	}
	raw, err := os.ReadFile(marker)
	if err != nil || string(raw) != "2" {
		t.Fatalf("extra New reached helper: %q %v", raw, err)
	}
	h.Stop() // Explicit original owner action; never implicit scratch failure cleanup.
	if !h.ProcessStopped(originalGeneration) {
		t.Fatal("original owned group stop was not confirmed")
	}
	if _, newGeneration, err := h.OpenSession(t.Context(), "", SessionConfig{Workdir: t.TempDir()}); err != nil || newGeneration == originalGeneration {
		t.Fatalf("confirmed original stop did not retire admission: %d %v", newGeneration, err)
	}
}

func TestColdNewKnownSIDShadowRemainsBoundedUntilPublication(t *testing.T) {
	modes := &acp.SessionModeState{CurrentModeID: "agent", AvailableModes: []acp.SessionMode{{ID: "agent", Name: "Agent"}, {ID: "read-only", Name: "Read-only"}}}
	a := &orderedPeer{initial: wireBoolean(false), initialModes: modes, modeEntered: make(chan struct{}), modeRelease: make(chan struct{})}
	a.lifecycleParticipant = &lifecycleParticipant{version: 1, caps: lifecycleCaps(31)}
	h := New(Config{Transport: orderedTransport{agent: a, gate: &orderedRequestGate{}}, NoRestart: true})
	t.Cleanup(h.Stop)
	t.Cleanup(func() {
		select {
		case <-a.modeRelease:
		default:
			close(a.modeRelease)
		}
	})
	done := make(chan error, 1)
	go func() { _, _, err := h.OpenSession(t.Context(), "", SessionConfig{Workdir: t.TempDir()}); done <- err }()
	awaitOperation(t, a.modeEntered)
	var commands []acp.AvailableCommand
	for i := 0; i < 9; i++ {
		commands = append(commands, acp.AvailableCommand{Name: fmt.Sprint("cmd", i), Description: strings.Repeat("x", maxNewScratchString)})
	}
	if err := a.client.Update(t.Context(), &acp.SessionNotification{SessionID: "ordered", Update: acp.AvailableCommandsUpdateSessionUpdate(commands)}); err != nil {
		t.Fatal(err)
	}
	if _, err := h.ListSessions(t.Context()); err != nil {
		t.Fatal(err)
	} // Actual notification barrier drained.
	h.mu.Lock()
	published := h.sessions["ordered"] != nil
	h.mu.Unlock()
	if published {
		t.Fatal("pre-publication known SID received a formal session")
	}
	close(a.modeRelease)
	if err := <-done; !errors.Is(err, errNewScratchLimit) {
		t.Fatalf("post-match oversized notification silently published: %v", err)
	}
	h.mu.Lock()
	opening := h.newOpening
	operation := h.sessionOperations["ordered"]
	published = h.sessions["ordered"] != nil
	h.mu.Unlock()
	if published || opening == nil || opening.pending || operation == nil || operation.pending {
		t.Fatal("post-match overflow discarded exact cleanup obligation or published partial state")
	}
}
func TestColdNewLimitsGroupsChoicesAndInitialModes(t *testing.T) {
	for _, shape := range []string{"groups", "choices", "modes", "SID bytes"} {
		t.Run(shape, func(t *testing.T) {
			options := wireBoolean(false)
			a := &orderedPeer{initial: options}
			switch shape {
			case "groups":
				groups := acp.GroupedSessionConfigSelectOptions{}
				for i := 0; i < maxNewScratchGroups+1; i++ {
					groups = append(groups, acp.SessionConfigSelectGroup{Group: acp.SessionConfigGroupID(fmt.Sprint("g", i)), Name: "Group", Options: []acp.SessionConfigSelectOption{}})
				}
				a.initial = []acp.SessionConfigOption{acp.SelectSessionConfigOption("choice", "Choice", "x", acp.SessionConfigSelectOptions{Groups: &groups})}
			case "choices":
				choices := acp.UngroupedSessionConfigSelectOptions{}
				for i := 0; i < maxNewScratchChoices+1; i++ {
					choices = append(choices, acp.SessionConfigSelectOption{Value: acp.SessionConfigValueID(fmt.Sprint("v", i)), Name: "Choice"})
				}
				a.initial = []acp.SessionConfigOption{acp.SelectSessionConfigOption("choice", "Choice", "v0", acp.SessionConfigSelectOptions{Ungrouped: &choices})}
			case "modes":
				values := []acp.SessionMode{}
				for i := 0; i < maxNewScratchModes+1; i++ {
					values = append(values, acp.SessionMode{ID: acp.SessionModeID(fmt.Sprint("m", i)), Name: "Mode"})
				}
				a.initialModes = &acp.SessionModeState{CurrentModeID: "m0", AvailableModes: values}
			case "SID bytes":
				a.newIDs = []acp.SessionID{acp.SessionID(strings.Repeat("x", maxNewScratchSIDBytes+1))}
			}
			a.lifecycleParticipant = &lifecycleParticipant{version: 1, caps: lifecycleCaps(31)}
			h := New(Config{Transport: orderedTransport{agent: a, gate: &orderedRequestGate{}}, NoRestart: true})
			t.Cleanup(h.Stop)
			if _, _, err := h.OpenSession(t.Context(), "", SessionConfig{Workdir: t.TempDir()}); !errors.Is(err, errNewScratchLimit) {
				t.Fatalf("%s oversized typed reply was accepted: %v", shape, err)
			}
			h.mu.Lock()
			published := len(h.sessions)
			debt := h.newOpening
			h.mu.Unlock()
			if published != 0 || debt == nil || debt.pending {
				t.Fatal("oversized matched reply lost exact owner or entered authorization state")
			}
			if shape == "SID bytes" && debt.sid != "" {
				t.Fatal("overlong returned identifier escaped retained owner byte bounds")
			}
		})
	}
}
