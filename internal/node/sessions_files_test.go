//go:build unix

package node

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gopact-ai/steve/internal/harness"
	"github.com/gopact-ai/steve/internal/nodewire"
)

func TestNodeOwnedSessionWorkspaceFileCallbacks(t *testing.T) {
	testNodeOwnedSessionWorkspaceFileCallbacks(t, false)
}

func TestNodeOwnedPluginSessionWorkspaceFileCallbacks(t *testing.T) {
	testNodeOwnedSessionWorkspaceFileCallbacks(t, true)
}

func testNodeOwnedSessionWorkspaceFileCallbacks(t *testing.T, withPlugin bool) {
	t.Helper()
	bin := buildMockAgent(t)
	originalWorkspace, processDir := t.TempDir(), t.TempDir()
	const source = "first line\nsecond line\nthird line\n"
	const content = "written through ACP\n"
	const decoy = "process directory content\n"
	const readPath = "source.txt"
	const writePath = "nested/result.txt"
	for _, seed := range []struct{ path, text string }{
		{filepath.Join(originalWorkspace, readPath), source},
		{filepath.Join(processDir, readPath), decoy},
		{filepath.Join(processDir, writePath), decoy},
	} {
		if err := os.MkdirAll(filepath.Dir(seed.path), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(seed.path, []byte(seed.text), 0600); err != nil {
			t.Fatal(err)
		}
	}
	sourceNode := NewServer(ServerConfig{
		Name: "worker", StateDir: t.TempDir(), WorkspaceRoot: processDir,
		Harnesses:         map[string]HarnessSpec{"mock": {Command: bin}},
		SessionAuthorizer: &sessionAuthorityTest{epoch: 1, writer: 1},
	})
	defer sourceNode.closePluginRuntimes()
	req := nodeSessionRequest(nodewire.SessionActionOpen)
	req.Harness, req.Workdir, req.CommandID = "mock", originalWorkspace, "workspace-files/open"
	if withPlugin {
		selection := nodeRuntimeFixture(t, sourceNode, "")
		runtime, err := sourceNode.pluginRuntimePool().Prepare(t.Context(), "workspace-files-runtime", selection,
			harness.Config{Command: bin, ProcessDir: processDir, Permission: "read"})
		if err != nil {
			t.Fatal(err)
		}
		req.Plugin = &runtime.Ref
		req.Binding.PluginRuntimeID = runtime.Ref.ID
	}
	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	defer cancel()
	if err := sourceNode.startSessions(ctx); err != nil {
		t.Fatal(err)
	}
	defer sourceNode.sessions.Close()
	state, err := sourceNode.sessions.Do(ctx, "cluster-1", req)
	if err != nil {
		t.Fatal(err)
	}
	if !nodewire.IsManagedSession(state.ID) || state.ContextID != state.ID || state.Binding != req.Binding || state.State != nodewire.SessionIdle || state.ProcessStopped {
		t.Fatalf("open did not create the admitted live node session: %+v", state)
	}
	if withPlugin && (state.Plugin == nil || state.Plugin.ID != req.Plugin.ID) {
		t.Fatalf("plugin host lost its prepared runtime: %+v", state.Plugin)
	}
	payload, err := json.Marshal(struct {
		Read    string `json:"read"`
		Write   string `json:"write"`
		Content string `json:"content"`
		Line    uint32 `json:"line"`
		Limit   uint32 `json:"limit"`
	}{readPath, writePath, content, 2, 1})
	if err != nil {
		t.Fatal(err)
	}
	req.ID, req.Action, req.CommandID = state.ID, nodewire.SessionActionPrompt, "workspace-files/input"
	req.InputSequence, req.Text = 1, "workspacefiles "+string(payload)
	state, err = sourceNode.sessions.Do(ctx, "cluster-1", req)
	if err != nil {
		t.Fatal(err)
	}
	if state.InputAccepted != 1 || state.Command == nil || state.Command.DispatchState != "not-dispatched" || state.Command.Settled {
		t.Fatalf("input lacks its original acceptance receipt: %+v", state)
	}
	for len(state.Questions) == 0 {
		if state.Command != nil && state.Command.Settled {
			t.Fatalf("file script ended before requesting write permission: %+v", state.Command)
		}
		poll := req
		poll.Action, poll.After, poll.WaitMS = nodewire.SessionActionPoll, state.Sequence, 50
		state, err = sourceNode.sessions.Do(ctx, "cluster-1", poll)
		if err != nil {
			t.Fatal(err)
		}
	}
	if len(state.Questions) != 1 {
		t.Fatalf("expected one workspace write question: %+v", state.Questions)
	}
	question := state.Questions[0]
	if question.State != nodewire.SessionQuestionPending || question.CommandID != req.CommandID || question.Question.Kind != "permission" || question.Permission == nil ||
		question.Permission.ToolName != "Write workspace text file" || !strings.Contains(question.Permission.Reason, writePath) {
		t.Fatalf("write callback did not become a node-owned permission question: %+v", question)
	}
	if state.State != nodewire.SessionRunning || state.Command == nil || state.Command.DispatchState != "dispatched" || state.Command.Settled || state.Command.Receipt.Version != 0 || state.ProcessStopped {
		t.Fatalf("pending write was counted as terminal: %+v", state)
	}
	if _, err := os.Stat(filepath.Join(originalWorkspace, writePath)); !os.IsNotExist(err) {
		t.Fatalf("write occurred before node permission: %v", err)
	}
	answer := req
	answer.Action, answer.QuestionID = nodewire.SessionActionAnswer, question.ID
	answer.Answer = &nodewire.SessionAnswer{CommandID: "workspace-files/allow", Decision: "accept", Choice: "workspace-file-allow"}
	if _, err := sourceNode.sessions.Do(ctx, "cluster-1", answer); err != nil {
		t.Fatal(err)
	}
	state = waitRecordedInput(t, sourceNode.sessions, req)
	command := state.Command
	if state.Binding != req.Binding || state.State != nodewire.SessionIdle || state.InputAccepted != 1 || state.NextInputSequence != 0 || state.ProcessStopped ||
		command == nil || command.ID != req.CommandID || command.InputSequence != 1 || command.State != nodewire.SessionCommandCompleted ||
		!command.Settled || command.ProcessStopped || command.DispatchState != "dispatched" || command.Error != "" || command.ErrorCode != "" {
		t.Fatalf("file turn has incorrect terminal accounting: %+v", state)
	}
	if len(state.Questions) != 1 || state.Questions[0].ID != question.ID || state.Questions[0].State != nodewire.SessionQuestionAnswered ||
		state.Questions[0].Answer == nil || *state.Questions[0].Answer != *answer.Answer {
		t.Fatalf("terminal receipt lost the original write decision: %+v", state.Questions)
	}
	var result struct {
		Cwd     string `json:"cwd"`
		Read    string `json:"read"`
		Range   string `json:"range"`
		Written string `json:"written"`
	}
	if err := json.Unmarshal([]byte(command.Output), &result); err != nil {
		t.Fatalf("file script did not return its callback results: %v; output=%q", err, command.Output)
	}
	if result.Cwd != originalWorkspace || result.Read != source || result.Range != "second line\n" || result.Written != content {
		t.Fatalf("ACP callbacks did not use the session's original workspace: %+v", result)
	}
	for _, expected := range []struct{ path, text string }{
		{filepath.Join(originalWorkspace, readPath), source},
		{filepath.Join(originalWorkspace, writePath), content},
		{filepath.Join(processDir, readPath), decoy},
		{filepath.Join(processDir, writePath), decoy},
	} {
		raw, err := os.ReadFile(expected.path)
		if err != nil || string(raw) != expected.text {
			t.Fatalf("workspace content differs at %q: %q, %v", expected.path, raw, err)
		}
	}
	if state.Progress.Answer != command.Output {
		t.Fatalf("terminal progress differs from the recorded callback output: %+v", state.Progress)
	}
	receipt, err := nodewire.NewSessionReceipt(state)
	if err != nil || command.Receipt != receipt {
		t.Fatalf("terminal callback evidence was not sealed under the original binding: %+v / %+v, %v", command.Receipt, receipt, err)
	}
	replayed, err := sourceNode.sessions.Do(ctx, "cluster-1", req)
	if err != nil || replayed.Command == nil || replayed.Command.Receipt != receipt || replayed.Command.Output != command.Output || replayed.InputAccepted != 1 || len(replayed.Questions) != 1 {
		t.Fatalf("identical input retry changed terminal accounting: %+v, %v", replayed, err)
	}
	if withPlugin {
		info, err := sourceNode.pluginStore().RuntimeInfo(req.Plugin.ID)
		if err != nil || len(info.Uses) != 1 || info.Uses[0].ID != "session/"+state.ID || info.Uses[0].Kind != "session" || info.Uses[0].Stopped {
			t.Fatalf("settled file prompt ended its still-live plugin process use: %+v, %v", info, err)
		}
	}
	stop := req
	stop.Action = nodewire.SessionActionClose
	closed, err := sourceNode.sessions.Do(ctx, "cluster-1", stop)
	if err != nil || closed.State != nodewire.SessionClosed || !closed.ProcessStopped || closed.Command == nil || !closed.Command.ProcessStopped || closed.Command.Receipt != receipt {
		t.Fatalf("close failed to prove exit while preserving the terminal receipt: %+v, %v", closed, err)
	}
	if withPlugin {
		info, err := sourceNode.pluginStore().RuntimeInfo(req.Plugin.ID)
		if err != nil || len(info.Uses) != 1 || info.Uses[0].ID != "session/"+state.ID || !info.Uses[0].Stopped {
			t.Fatalf("confirmed close did not end the original plugin process use: %+v, %v", info, err)
		}
	}
}
