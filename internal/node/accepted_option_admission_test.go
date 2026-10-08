//go:build unix

package node

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gopact-ai/acp"
	"github.com/gopact-ai/steve/internal/nodewire"
)

type nodeAdmissionBarrier struct {
	entered, release, waiting        chan struct{}
	enterOnce, releaseOnce, waitOnce sync.Once
}

func (b *nodeAdmissionBarrier) unblock() {
	b.releaseOnce.Do(func() { close(b.release) })
}

func newNodeAdmissionSession(t *testing.T, env []string) (*Server, nodewire.SessionRequest, *ownedSession, *nodeAdmissionBarrier) {
	t.Helper()
	s := NewServer(ServerConfig{
		Name: "worker", StateDir: t.TempDir(), WorkspaceRoot: t.TempDir(),
		Harnesses:         map[string]HarnessSpec{"mock": {Command: buildMockAgent(t), Env: env}},
		SessionAuthorizer: &sessionAuthorityTest{epoch: 1, writer: 1},
	})
	if err := s.startSessions(t.Context()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.sessions.Close)
	b := &nodeAdmissionBarrier{entered: make(chan struct{}), release: make(chan struct{}), waiting: make(chan struct{})}
	t.Cleanup(b.unblock)
	s.sessions.admissionHooks = &sessionAdmissionHooks{
		beforePrompt: func(ctx context.Context) {
			b.enterOnce.Do(func() { close(b.entered) })
			select {
			case <-b.release:
			case <-ctx.Done():
			}
		},
		optionWaiting: func() { b.waitOnce.Do(func() { close(b.waiting) }) },
	}
	req := nodeSessionRequest(nodewire.SessionActionOpen)
	req.Harness, req.Workdir, req.CommandID = "mock", t.TempDir(), "admission/open"
	opened, err := s.sessions.Do(t.Context(), "cluster-1", req)
	if err != nil {
		t.Fatal(err)
	}
	if !nodewire.IsManagedSession(opened.ID) || opened.State != nodewire.SessionIdle || opened.ProcessStopped {
		t.Fatalf("mock subprocess did not open the managed session: %+v", opened)
	}
	req.ID = opened.ID
	s.sessions.mu.Lock()
	one := s.sessions.sessions[opened.ID]
	s.sessions.mu.Unlock()
	return s, req, one, b
}

func awaitNodeAdmissionBarrier(t *testing.T, ctx context.Context, ready <-chan struct{}) {
	t.Helper()
	select {
	case <-ready:
	case <-ctx.Done():
		t.Fatal("admission barrier did not complete:", ctx.Err())
	}
}

type nodeAdmissionOptionResult struct {
	state nodewire.SessionState
	err   error
}

func startNodeAdmissionOption(ctx context.Context, s *Server, req nodewire.SessionRequest) <-chan nodeAdmissionOptionResult {
	done := make(chan nodeAdmissionOptionResult, 1)
	req.Action, req.InputSequence, req.Text = nodewire.SessionActionOption, 0, ""
	req.OptionID, req.OptionValue = "model", "mock-deep"
	go func() {
		state, err := s.sessions.Do(ctx, "cluster-1", req)
		done <- nodeAdmissionOptionResult{state, err}
	}()
	return done
}

func awaitNodeAdmissionOption(t *testing.T, ctx context.Context, done <-chan nodeAdmissionOptionResult) nodeAdmissionOptionResult {
	t.Helper()
	select {
	case result := <-done:
		return result
	case <-ctx.Done():
		t.Fatal("option did not leave admission wait:", ctx.Err())
		return nodeAdmissionOptionResult{}
	}
}

func inspectNodeAdmissionInput(t *testing.T, ctx context.Context, s *Server, req nodewire.SessionRequest) nodewire.SessionState {
	t.Helper()
	req.Action = nodewire.SessionActionAttach
	state, err := s.sessions.Do(ctx, "cluster-1", req)
	if err != nil {
		t.Fatal(err)
	}
	return state
}

func closeNodeAdmissionSession(t *testing.T, ctx context.Context, s *Server, req nodewire.SessionRequest, receipt nodewire.SessionReceipt) {
	t.Helper()
	req.Action = nodewire.SessionActionClose
	closed, err := s.sessions.Do(ctx, "cluster-1", req)
	if err != nil || closed.State != nodewire.SessionClosed || !closed.ProcessStopped || closed.Command == nil ||
		!closed.Command.ProcessStopped || closed.Command.Receipt != receipt {
		t.Fatalf("real native close changed the original command evidence: %+v, %v", closed, err)
	}
}

func TestAcceptedNodeOptionWaitsForOriginalHostAdmission(t *testing.T) {
	s, req, one, b := newNodeAdmissionSession(t, nil)
	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	defer cancel()
	req.Action, req.CommandID, req.InputSequence, req.Text = nodewire.SessionActionPrompt, "admission/input", 1, "slow"
	accepted, err := s.sessions.Do(ctx, "cluster-1", req)
	if err != nil || accepted.Command == nil || accepted.Command.State != nodewire.SessionCommandAccepted || accepted.Command.DispatchState != "not-dispatched" {
		t.Fatalf("input was not durably accepted before dispatch: %+v, %v", accepted, err)
	}
	awaitNodeAdmissionBarrier(t, ctx, b.entered)
	one.mu.Lock()
	admitted := one.promptAdmitted
	one.mu.Unlock()
	before := inspectNodeAdmissionInput(t, ctx, s, req)
	if before.Command == nil || before.Command.DispatchState != "dispatched" || before.Command.Settled || before.InputAccepted != 1 {
		t.Fatalf("barrier did not reach dispatched-before-Host admission: %+v", before)
	}
	done := startNodeAdmissionOption(ctx, s, req)
	awaitNodeAdmissionBarrier(t, ctx, b.waiting)
	select {
	case <-admitted:
		t.Fatal("Host admission was reported before the original turn entered Host")
	default:
	}
	select {
	case result := <-done:
		t.Fatalf("option overtook the original Host admission: %+v", result)
	default:
	}
	before = inspectNodeAdmissionInput(t, ctx, s, req)
	requireSettingsOption(t, before.Settings, "model", "mock-fast")
	b.unblock()
	changed := awaitNodeAdmissionOption(t, ctx, done)
	if changed.err != nil || changed.state.State != nodewire.SessionRunning || changed.state.Command == nil ||
		changed.state.Command.ID != req.CommandID || changed.state.Command.Settled || changed.state.InputAccepted != 1 {
		t.Fatalf("settings interrupted or replaced the original prompt: %+v, %v", changed.state, changed.err)
	}
	awaitNodeAdmissionBarrier(t, ctx, admitted)
	requireSettingsOption(t, changed.state.Settings, "model", "mock-deep")
	stop := req
	stop.Action = nodewire.SessionActionCancel
	finished, err := s.sessions.Do(ctx, "cluster-1", stop)
	if err != nil || finished.Command == nil || finished.Command.State != nodewire.SessionCommandCancelled || !finished.Command.Settled ||
		finished.Binding != req.Binding || finished.InputAccepted != 1 || finished.Command.Receipt.Validate() != nil {
		t.Fatalf("real slow prompt did not return its original cancellation receipt: %+v, %v", finished, err)
	}
	replayed, err := s.sessions.Do(ctx, "cluster-1", req)
	if err != nil || replayed.Command == nil || replayed.Command.Receipt != finished.Command.Receipt || replayed.InputAccepted != 1 {
		t.Fatalf("identical input retry changed terminal accounting: %+v, %v", replayed, err)
	}
	closeNodeAdmissionSession(t, ctx, s, req, finished.Command.Receipt)
}

func TestNodeOptionStillAppliesDuringANativeQuestion(t *testing.T) {
	s, req, _, b := newNodeAdmissionSession(t, nil)
	b.unblock()
	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	defer cancel()
	req.Action, req.CommandID, req.InputSequence, req.Text = nodewire.SessionActionPrompt, "question/input", 1, "askme"
	state, err := s.sessions.Do(ctx, "cluster-1", req)
	if err != nil {
		t.Fatal(err)
	}
	for len(state.Questions) == 0 {
		if state.Command != nil && !state.Command.State.Active() {
			t.Fatalf("native prompt ended before its reverse question: %+v", state.Command)
		}
		poll := req
		poll.Action, poll.After, poll.WaitMS = nodewire.SessionActionPoll, state.Sequence, 50
		state, err = s.sessions.Do(ctx, "cluster-1", poll)
		if err != nil {
			t.Fatal(err)
		}
	}
	question := state.Questions[0]
	if question.State != nodewire.SessionQuestionPending || question.CommandID != req.CommandID {
		t.Fatalf("native question lost its original input: %+v", question)
	}
	changed := awaitNodeAdmissionOption(t, ctx, startNodeAdmissionOption(ctx, s, req))
	if changed.err != nil || changed.state.Command == nil || changed.state.Command.Settled || len(changed.state.Questions) != 1 ||
		changed.state.Questions[0].ID != question.ID || changed.state.Questions[0].State != nodewire.SessionQuestionPending {
		t.Fatalf("settings waited for the entire turn or replaced its native question: %+v, %v", changed.state, changed.err)
	}
	requireSettingsOption(t, changed.state.Settings, "model", "mock-deep")
	answer := req
	answer.Action, answer.QuestionID = nodewire.SessionActionAnswer, question.ID
	answer.Answer = &nodewire.SessionAnswer{CommandID: "question/answer", Decision: "accept", Choice: "Blue"}
	if _, err := s.sessions.Do(ctx, "cluster-1", answer); err != nil {
		t.Fatal(err)
	}
	finished := waitRecordedInput(t, s.sessions, req)
	if finished.Command.State != nodewire.SessionCommandCompleted || !strings.Contains(finished.Command.Output, "accept:Blue") || finished.InputAccepted != 1 {
		t.Fatalf("native question did not complete its original prompt: %+v", finished)
	}
	closeNodeAdmissionSession(t, ctx, s, req, finished.Command.Receipt)
}

func TestNodeOptionAdmissionWaitHonorsCallerCancellation(t *testing.T) {
	s, req, one, b := newNodeAdmissionSession(t, nil)
	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	defer cancel()
	req.Action, req.CommandID, req.InputSequence, req.Text = nodewire.SessionActionPrompt, "cancel/input", 1, "hello"
	if _, err := s.sessions.Do(ctx, "cluster-1", req); err != nil {
		t.Fatal(err)
	}
	awaitNodeAdmissionBarrier(t, ctx, b.entered)
	optionCtx, cancelOption := context.WithCancel(ctx)
	defer cancelOption()
	done := startNodeAdmissionOption(optionCtx, s, req)
	awaitNodeAdmissionBarrier(t, ctx, b.waiting)
	cancelOption()
	result := awaitNodeAdmissionOption(t, ctx, done)
	if !errors.Is(result.err, context.Canceled) {
		t.Fatalf("option cancellation reached configure or hung: %+v, %v", result.state, result.err)
	}
	one.mu.Lock()
	admitted := one.promptAdmitted
	one.mu.Unlock()
	select {
	case <-admitted:
		t.Fatal("cancelled option invented Host admission")
	default:
	}
	state := inspectNodeAdmissionInput(t, ctx, s, req)
	requireSettingsOption(t, state.Settings, "model", "mock-fast")
	b.unblock()
	finished := waitRecordedInput(t, s.sessions, req)
	if finished.Command.State != nodewire.SessionCommandCompleted || finished.Command.Output != "echo: hello" || finished.InputAccepted != 1 {
		t.Fatalf("caller cancellation disturbed the original prompt: %+v", finished)
	}
	closeNodeAdmissionSession(t, ctx, s, req, finished.Command.Receipt)
}

func TestNodeOptionAdmissionWaitEndsOnPreAdmissionFailure(t *testing.T) {
	s, req, one, b := newNodeAdmissionSession(t, []string{"MOCKAGENT_NO_MEDIA=1"})
	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	defer cancel()
	req.Action, req.CommandID, req.InputSequence, req.Text = nodewire.SessionActionPrompt, "unsupported/input", 1, "not submitted"
	req.Media = []nodewire.SessionMedia{{MIME: "image/png", Data: []byte("synthetic image")}}
	if _, err := s.sessions.Do(ctx, "cluster-1", req); err != nil {
		t.Fatal(err)
	}
	awaitNodeAdmissionBarrier(t, ctx, b.entered)
	one.mu.Lock()
	admitted, runDone := one.promptAdmitted, one.runDone
	one.mu.Unlock()
	done := startNodeAdmissionOption(ctx, s, req)
	awaitNodeAdmissionBarrier(t, ctx, b.waiting)
	b.unblock()
	awaitNodeAdmissionBarrier(t, ctx, runDone)
	result := awaitNodeAdmissionOption(t, ctx, done)
	var refused *SessionError
	if !errors.As(result.err, &refused) || refused.Code != "busy" {
		t.Fatalf("failed admission did not wake/refuse the waiting option: %+v, %v", result.state, result.err)
	}
	select {
	case <-admitted:
		t.Fatal("pre-admission failure published admission")
	default:
	}
	state := inspectNodeAdmissionInput(t, ctx, s, req)
	if state.State != nodewire.SessionInterrupted || state.Command == nil || state.Command.State != nodewire.SessionCommandUncertain ||
		state.Command.Settled || state.Command.Receipt.Version != 0 || state.InputAccepted != 1 || !strings.Contains(state.Command.Error, "does not support") {
		t.Fatalf("local admission failure invented terminal native evidence: %+v", state)
	}
	requireSettingsOption(t, state.Settings, "model", "mock-fast")
	closeNodeAdmissionSession(t, ctx, s, req, state.Command.Receipt)
}

func TestNodeOptionAdmissionWaitEndsOnServiceClose(t *testing.T) {
	s, req, one, b := newNodeAdmissionSession(t, nil)
	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	defer cancel()
	req.Action, req.CommandID, req.InputSequence, req.Text = nodewire.SessionActionPrompt, "shutdown/input", 1, "slow"
	if _, err := s.sessions.Do(ctx, "cluster-1", req); err != nil {
		t.Fatal(err)
	}
	awaitNodeAdmissionBarrier(t, ctx, b.entered)
	done := startNodeAdmissionOption(ctx, s, req)
	awaitNodeAdmissionBarrier(t, ctx, b.waiting)
	closed := make(chan struct{})
	go func() { s.sessions.Close(); close(closed) }()
	result := awaitNodeAdmissionOption(t, ctx, done)
	var refused *SessionError
	if !errors.As(result.err, &refused) || refused.Code != "closed" {
		t.Fatalf("service close did not wake the waiting option: %+v, %v", result.state, result.err)
	}
	awaitNodeAdmissionBarrier(t, ctx, closed)
	// Cancellation can race Host admission; neither admission nor its absence
	// proves exit. This owner opened a real subprocess, not an in-memory one.
	stopped := one.state(req.CommandID)
	if !stopped.ProcessStopped || stopped.Command == nil || !stopped.Command.ProcessStopped || stopped.InputAccepted != 1 {
		t.Fatalf("service shutdown did not retain actual native stop evidence: %+v", stopped)
	}
}

func TestNodeOptionAdmissionWaitRechecksRevokedAuthority(t *testing.T) {
	s, req, one, b := newNodeAdmissionSession(t, nil)
	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	defer cancel()
	req.Action, req.CommandID, req.InputSequence, req.Text = nodewire.SessionActionPrompt, "revoked/input", 1, "askme"
	if _, err := s.sessions.Do(ctx, "cluster-1", req); err != nil {
		t.Fatal(err)
	}
	awaitNodeAdmissionBarrier(t, ctx, b.entered)
	done := startNodeAdmissionOption(ctx, s, req)
	awaitNodeAdmissionBarrier(t, ctx, b.waiting)
	authority := s.conf().SessionAuthorizer.(*sessionAuthorityTest)
	authority.mu.Lock()
	authority.writer = 2
	authority.mu.Unlock()
	b.unblock()
	result := awaitNodeAdmissionOption(t, ctx, done)
	var refused *SessionError
	if !errors.As(result.err, &refused) || refused.Code != "forbidden" {
		t.Fatalf("waiting option ignored current revocation: %+v %v", result.state, result.err)
	}
	one.mu.Lock()
	native, generation := one.record.UpstreamID, one.record.Generation
	one.mu.Unlock()
	observed, known := one.host.SettingsForGeneration(acp.SessionID(native), generation)
	if !known {
		t.Fatal("original native context changed during refused option")
	}
	requireSettingsOption(t, observed, "model", "mock-fast")
	// Restore this synthetic verifier only for explicit cleanup of the already
	// accepted input. The revoked option itself is never replayed.
	authority.mu.Lock()
	authority.writer = 1
	authority.mu.Unlock()
	stop := req
	stop.Action = nodewire.SessionActionAbort
	if _, err := s.sessions.Do(ctx, "cluster-1", stop); err != nil {
		t.Fatal(err)
	}
	stop.Action = nodewire.SessionActionClose
	closed, err := s.sessions.Do(ctx, "cluster-1", stop)
	if err != nil || !closed.ProcessStopped {
		t.Fatalf("original native cleanup failed: %+v %v", closed, err)
	}
}
