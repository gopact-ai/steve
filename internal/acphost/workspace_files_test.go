package acphost

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gopact-ai/acp"
	"github.com/gopact-ai/steve/internal/permission"
)

type filesParticipant struct {
	*lifecycleParticipant
	initialized *acp.ClientCapabilities
	initialize  func(context.Context, *acp.InitializeRequest) error
	prompt      func(context.Context, *acp.PromptRequest) (*acp.PromptResponse, error)
}

func (a *filesParticipant) Initialize(ctx context.Context, req *acp.InitializeRequest) (*acp.InitializeResponse, error) {
	a.initialized = req.ClientCapabilities
	if a.initialize != nil {
		if err := a.initialize(ctx, req); err != nil {
			return nil, err
		}
	}
	return a.lifecycleParticipant.Initialize(ctx, req)
}
func (a *filesParticipant) Prompt(ctx context.Context, req *acp.PromptRequest) (*acp.PromptResponse, error) {
	return a.prompt(ctx, req)
}

type filesTransport struct{ agent *filesParticipant }

func (filesTransport) Name() string { return "workspace-files-fixture" }
func (tr filesTransport) Start(context.Context) (Process, error) {
	input, stdin := io.Pipe()
	stdout, output := io.Pipe()
	conn, err := acp.NewAgent(input, output, func(client *acp.ClientCaller) acp.AgentHandler {
		tr.agent.client = client
		return tr.agent
	})
	if err != nil {
		input.Close()
		stdin.Close()
		stdout.Close()
		output.Close()
		return nil, err
	}
	p := &lifecycleProcess{conn: conn, stdin: stdin, stdout: stdout, output: output}
	tr.agent.process = p
	return p, nil
}

func workspaceFilesHost(t *testing.T, enabled bool, policy string) (*Host, *filesParticipant, string, acp.SessionID, uint64) {
	t.Helper()
	if enabled && !workspaceFilesSupported {
		t.Skip("local text-file callbacks unavailable on this platform")
	}
	root := t.TempDir()
	a := &filesParticipant{lifecycleParticipant: &lifecycleParticipant{
		version: acp.ProtocolVersionV1, caps: lifecycleCaps(31), workdir: root, servers: []acp.MCPServer{},
	}}
	broker, err := permission.New(policy)
	if err != nil {
		t.Fatal(err)
	}
	h := New(Config{Transport: filesTransport{a}, Permission: broker, WorkspaceFiles: enabled, NoRestart: true})
	t.Cleanup(h.Close)
	sid, generation, err := h.OpenSession(t.Context(), "", SessionConfig{Workdir: root})
	if err != nil {
		t.Fatal(err)
	}
	return h, a, root, sid, generation
}

func TestWorkspaceFilesRealWireReadRangeAndWrite(t *testing.T) {
	h, a, root, sid, generation := workspaceFilesHost(t, true, "auto")
	if a.initialized == nil || a.initialized.Fs == nil || !a.initialized.Fs.ReadTextFile || !a.initialized.Fs.WriteTextFile {
		t.Fatal("node-local client did not advertise implemented text-file callbacks")
	}
	path := filepath.Join(root, "note.txt")
	if err := os.WriteFile(path, []byte("first\n中文\nlast"), 0600); err != nil {
		t.Fatal(err)
	}
	a.prompt = func(ctx context.Context, req *acp.PromptRequest) (*acp.PromptResponse, error) {
		line, limit := uint32(2), uint32(1)
		read, err := a.client.ReadTextFile(ctx, &acp.ReadTextFileRequest{SessionID: req.SessionID, Path: path, Line: &line, Limit: &limit})
		if err != nil {
			return nil, err
		}
		if read.Content != "中文\n" {
			return nil, fmt.Errorf("line range differs: %q", read.Content)
		}
		if _, err := a.client.WriteTextFile(ctx, &acp.WriteTextFileRequest{SessionID: req.SessionID, Path: path, Content: "updated 中文\n"}); err != nil {
			return nil, err
		}
		return &acp.PromptResponse{StopReason: acp.StopReasonEndTurn}, nil
	}
	if _, _, err := h.Prompt(t.Context(), sid, generation, "bounded workspace test", nil); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(path)
	if err != nil || string(got) != "updated 中文\n" {
		t.Fatalf("callback write: %q %v", got, err)
	}
}

func TestWorkspaceFilesDisabledKeepsMethodNotFound(t *testing.T) {
	h, a, root, sid, generation := workspaceFilesHost(t, false, "auto")
	if a.initialized != nil && a.initialized.Fs != nil && (a.initialized.Fs.ReadTextFile || a.initialized.Fs.WriteTextFile) {
		t.Fatal("remote or disabled client advertised local filesystem access")
	}
	a.prompt = func(ctx context.Context, req *acp.PromptRequest) (*acp.PromptResponse, error) {
		_, err := a.client.ReadTextFile(ctx, &acp.ReadTextFileRequest{SessionID: req.SessionID, Path: filepath.Join(root, "never-created")})
		var response *acp.Error
		if !errors.As(err, &response) || response.Code != acp.ErrorCodeMethodNotFound {
			return nil, fmt.Errorf("disabled callback: %v", err)
		}
		return &acp.PromptResponse{StopReason: acp.StopReasonEndTurn}, nil
	}
	if _, _, err := h.Prompt(t.Context(), sid, generation, "disabled workspace test", nil); err != nil {
		t.Fatal(err)
	}
}

func expectFilesRefused(err error) error {
	var response *acp.Error
	if !errors.As(err, &response) || response.Code == acp.ErrorCodeMethodNotFound {
		return fmt.Errorf("expected an implemented, explicit refusal: %v", err)
	}
	return nil
}

func TestWorkspaceFilesPermissionApprovalAndRejection(t *testing.T) {
	for _, test := range []struct {
		name, policy string
		answer       bool
		want         bool
	}{
		{"deny", "deny", false, false}, {"read-reject", "read", false, false},
		{"read-allow", "read", true, true}, {"write", "write", false, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			h, a, root, sid, generation := workspaceFilesHost(t, true, test.policy)
			path := filepath.Join(root, "answer.txt")
			asks := 0
			a.prompt = func(ctx context.Context, req *acp.PromptRequest) (*acp.PromptResponse, error) {
				_, err := a.client.WriteTextFile(ctx, &acp.WriteTextFileRequest{SessionID: req.SessionID, Path: path, Content: "approved"})
				if test.want && err != nil {
					return nil, err
				}
				if !test.want {
					if err := expectFilesRefused(err); err != nil {
						return nil, err
					}
				}
				return &acp.PromptResponse{StopReason: acp.StopReasonEndTurn}, nil
			}
			ask := func(_ context.Context, q permission.Ask) (acp.RequestPermissionOutcome, error) {
				asks++
				if q.SessionID != string(sid) || q.Generation != generation || q.Kind != acp.ToolKindEdit {
					return acp.CanceledRequestPermissionOutcome(), errors.New("question lost its original session")
				}
				return permission.Choose(test.answer, q.Options), nil
			}
			if _, _, err := h.PromptTurn(t.Context(), sid, generation, "permission test", nil, ask, nil, nil); err != nil {
				t.Fatal(err)
			}
			wantAsks := 0
			if test.policy == "read" {
				wantAsks = 1
			}
			if asks != wantAsks {
				t.Fatalf("questions=%d want=%d", asks, wantAsks)
			}
			data, err := os.ReadFile(path)
			if test.want {
				if err != nil || string(data) != "approved" {
					t.Fatalf("approved write=%q %v", data, err)
				}
			} else if !os.IsNotExist(err) {
				t.Fatalf("denial created file: %q %v", data, err)
			}
		})
	}
}

func TestWorkspaceFilesBoundedTextAndLineRanges(t *testing.T) {
	h, a, root, sid, generation := workspaceFilesHost(t, true, "auto")
	small := filepath.Join(root, "small.txt")
	if err := os.WriteFile(small, []byte("one\r\n中文\nlast"), 0600); err != nil {
		t.Fatal(err)
	}
	big := filepath.Join(root, "large.txt")
	if err := os.WriteFile(big, []byte("head\n"+strings.Repeat("x", maxWorkspaceScanBytes+1)), 0600); err != nil {
		t.Fatal(err)
	}
	encoded := filepath.Join(root, "escaped.txt")
	if err := os.WriteFile(encoded, []byte(strings.Repeat("\x01", maxWorkspaceTextBytes/2)), 0600); err != nil {
		t.Fatal(err)
	}
	invalid := filepath.Join(root, "invalid.txt")
	if err := os.WriteFile(invalid, []byte{0xff, '\n'}, 0600); err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name, path  string
		line, limit *uint32
		want        string
		reject      bool
	}{
		{"full", small, nil, nil, "one\r\n中文\nlast", false},
		{"middle", small, filesNumber(2), filesNumber(1), "中文\n", false},
		{"tail", small, filesNumber(3), nil, "last", false},
		{"beyond", small, filesNumber(99), nil, "", false},
		{"zero-limit", small, nil, filesNumber(0), "", false},
		{"zero-line", small, filesNumber(0), nil, "", true},
		{"head-large-tail", big, nil, filesNumber(1), "head\n", false},
		{"full-too-large", big, nil, nil, "", true},
		{"scan-too-large", big, filesNumber(3), filesNumber(1), "", true},
		{"encoded-too-large", encoded, nil, nil, "", true},
		{"invalid-utf8", invalid, nil, nil, "", true},
	}
	a.prompt = func(ctx context.Context, req *acp.PromptRequest) (*acp.PromptResponse, error) {
		for _, test := range cases {
			got, err := a.client.ReadTextFile(ctx, &acp.ReadTextFileRequest{SessionID: req.SessionID, Path: test.path, Line: test.line, Limit: test.limit})
			if test.reject {
				if err := expectFilesRefused(err); err != nil {
					return nil, fmt.Errorf("%s: %w", test.name, err)
				}
				continue
			}
			if err != nil || got.Content != test.want {
				return nil, fmt.Errorf("%s content differs: %v", test.name, err)
			}
		}
		path := filepath.Join(root, "never-created", "oversize.txt")
		if _, err := a.client.WriteTextFile(ctx, &acp.WriteTextFileRequest{SessionID: req.SessionID, Path: path, Content: strings.Repeat("x", maxWorkspaceTextBytes+1)}); err == nil {
			return nil, errors.New("oversized write accepted")
		}
		return &acp.PromptResponse{StopReason: acp.StopReasonEndTurn}, nil
	}
	if _, _, err := h.Prompt(t.Context(), sid, generation, "range bounds", nil); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, "never-created")); !os.IsNotExist(err) {
		t.Fatalf("oversized write created parent: %v", err)
	}
}

func filesNumber(n uint32) *uint32 { return &n }

func TestWorkspaceFilesConfinedPathsAndRootDrift(t *testing.T) {
	h, a, root, sid, generation := workspaceFilesHost(t, true, "auto")
	outside := t.TempDir()
	victim := filepath.Join(outside, "victim.txt")
	if err := os.WriteFile(victim, []byte("untouched"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "escape")); err != nil {
		t.Fatal(err)
	}
	a.prompt = func(ctx context.Context, req *acp.PromptRequest) (*acp.PromptResponse, error) {
		paths := []string{victim, filepath.Join(root, "escape", "victim.txt"), "relative.txt", root, filepath.Join(root, "../outside/victim.txt")}
		for _, path := range paths {
			if _, err := a.client.ReadTextFile(ctx, &acp.ReadTextFileRequest{SessionID: req.SessionID, Path: path}); err == nil {
				return nil, errors.New("outside/nonfile read accepted")
			}
			if _, err := a.client.WriteTextFile(ctx, &acp.WriteTextFileRequest{SessionID: req.SessionID, Path: path, Content: "damage"}); err == nil {
				return nil, errors.New("outside/nonfile write accepted")
			}
		}
		return &acp.PromptResponse{StopReason: acp.StopReasonEndTurn}, nil
	}
	if _, _, err := h.Prompt(t.Context(), sid, generation, "root confinement", nil); err != nil {
		t.Fatal(err)
	}
	body, err := os.ReadFile(victim)
	if err != nil || string(body) != "untouched" {
		t.Fatalf("outside victim changed: %q %v", body, err)
	}
	if _, _, err := h.OpenSession(t.Context(), sid, SessionConfig{Workdir: outside}); err == nil {
		t.Fatal("cached session accepted another workspace")
	}
	old := root + "-old"
	if err := os.Rename(root, old); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(old) })
	if err := os.Mkdir(root, 0700); err != nil {
		t.Fatal(err)
	}
	if _, _, err := h.OpenSession(t.Context(), sid, SessionConfig{Workdir: root}); err == nil {
		t.Fatal("cached session accepted replaced directory identity")
	}
	a.prompt = func(ctx context.Context, req *acp.PromptRequest) (*acp.PromptResponse, error) {
		_, err := a.client.WriteTextFile(ctx, &acp.WriteTextFileRequest{SessionID: req.SessionID, Path: filepath.Join(root, "replacement.txt"), Content: "no"})
		if err := expectFilesRefused(err); err != nil {
			return nil, err
		}
		return &acp.PromptResponse{StopReason: acp.StopReasonEndTurn}, nil
	}
	if _, _, err := h.Prompt(t.Context(), sid, generation, "changed root", nil); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, "replacement.txt")); !os.IsNotExist(err) {
		t.Fatal("write reached replacement root")
	}
}

func TestWorkspaceFilesUnknownAndIdleSessionsRefuseIO(t *testing.T) {
	h, a, root, sid, generation := workspaceFilesHost(t, true, "auto")
	path := filepath.Join(root, "untouched.txt")
	if err := os.WriteFile(path, []byte("unchanged"), 0600); err != nil {
		t.Fatal(err)
	}
	for _, id := range []acp.SessionID{sid, "not-owned"} {
		if _, err := a.client.ReadTextFile(t.Context(), &acp.ReadTextFileRequest{SessionID: id, Path: path}); err == nil {
			t.Fatal("idle/unknown session read granted")
		}
		if _, err := a.client.WriteTextFile(t.Context(), &acp.WriteTextFileRequest{SessionID: id, Path: path, Content: "damage"}); err == nil {
			t.Fatal("idle/unknown session write granted")
		}
	}
	a.prompt = func(ctx context.Context, req *acp.PromptRequest) (*acp.PromptResponse, error) {
		if _, err := a.client.ReadTextFile(ctx, &acp.ReadTextFileRequest{SessionID: "not-owned", Path: path}); err == nil {
			return nil, errors.New("another session borrowed current turn")
		}
		return &acp.PromptResponse{StopReason: acp.StopReasonEndTurn}, nil
	}
	if _, _, err := h.Prompt(t.Context(), sid, generation, "session binding", nil); err != nil {
		t.Fatal(err)
	}
	body, err := os.ReadFile(path)
	if err != nil || string(body) != "unchanged" {
		t.Fatal("refused callback changed file")
	}
}

func TestWorkspaceFilesLateApprovalCannotOutliveOriginalTurn(t *testing.T) {
	h, a, root, sid, generation := workspaceFilesHost(t, true, "read")
	path := filepath.Join(root, "late.txt")
	asked, reply, answer := make(chan struct{}), make(chan struct{}), make(chan struct{})
	t.Cleanup(func() {
		for _, c := range []chan struct{}{reply, answer} {
			select {
			case <-c:
			default:
				close(c)
			}
		}
	})
	writeDone := make(chan error, 1)
	a.prompt = func(_ context.Context, req *acp.PromptRequest) (*acp.PromptResponse, error) {
		go func() {
			_, err := a.client.WriteTextFile(context.Background(), &acp.WriteTextFileRequest{SessionID: req.SessionID, Path: path, Content: "late"})
			writeDone <- err
		}()
		<-reply
		return &acp.PromptResponse{StopReason: acp.StopReasonEndTurn}, nil
	}
	ask := func(_ context.Context, q permission.Ask) (acp.RequestPermissionOutcome, error) {
		close(asked)
		<-answer
		return permission.Choose(true, q.Options), nil
	}
	turn := make(chan error, 1)
	go func() {
		_, _, err := h.PromptTurn(t.Context(), sid, generation, "asynchronous request", nil, ask, nil, nil)
		turn <- err
	}()
	select {
	case <-asked:
	case <-time.After(2 * time.Second):
		t.Fatal("permission request not observed")
	}
	close(reply)
	var turnErr error
	select {
	case turnErr = <-turn:
	case <-time.After(4 * time.Second):
		t.Fatal("turn did not bound callback retirement")
	}
	if !errors.Is(turnErr, ErrStopUnconfirmed) || PromptSettled(turnErr) {
		t.Fatalf("pending callback became settled: %v", turnErr)
	}
	if err := h.CloseIdle(); !errors.Is(err, ErrSessionBusy) {
		t.Fatalf("idle close ignored accepted callback: %v", err)
	}
	if _, _, err := h.OpenSession(t.Context(), sid, SessionConfig{Workdir: root}); !errors.Is(err, ErrStopUnconfirmed) {
		t.Fatalf("pending writer reopened: %v", err)
	}
	close(answer)
	select {
	case err := <-writeDone:
		if err == nil {
			t.Fatal("late permission authorized expired writer")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("late callback did not settle")
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("late callback created file")
	}
	a.prompt = func(context.Context, *acp.PromptRequest) (*acp.PromptResponse, error) {
		return &acp.PromptResponse{StopReason: acp.StopReasonEndTurn}, nil
	}
	if _, _, err := h.Prompt(t.Context(), sid, generation, "next explicit turn", nil); err != nil {
		t.Fatal(err)
	}
}

func TestWorkspaceFilesReverseInitializeRefusesWithoutDeadlock(t *testing.T) {
	if !workspaceFilesSupported {
		t.Skip("local text-file callbacks unavailable")
	}
	root := t.TempDir()
	a := &filesParticipant{lifecycleParticipant: &lifecycleParticipant{version: acp.ProtocolVersionV1, caps: lifecycleCaps(31), workdir: root, servers: []acp.MCPServer{}}}
	a.initialize = func(ctx context.Context, _ *acp.InitializeRequest) error {
		_, err := a.client.ReadTextFile(ctx, &acp.ReadTextFileRequest{SessionID: "not-yet-owned", Path: filepath.Join(root, "note")})
		return expectFilesRefused(err)
	}
	h := New(Config{WorkspaceFiles: true, Transport: filesTransport{a}, NoRestart: true})
	t.Cleanup(h.Close)
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	if _, _, err := h.OpenSession(ctx, "", SessionConfig{Workdir: root}); err != nil {
		t.Fatalf("reverse initialization deadlocked or failed: %v", err)
	}
}

func TestWorkspaceFilesPendingOwnershipSurvivesAgentExit(t *testing.T) {
	h, a, root, sid, generation := workspaceFilesHost(t, true, "auto")
	started, release := make(chan struct{}), make(chan struct{})
	t.Cleanup(func() {
		select {
		case <-release:
		default:
			close(release)
		}
	})
	a.prompt = func(ctx context.Context, _ *acp.PromptRequest) (*acp.PromptResponse, error) {
		close(started)
		select {
		case <-release:
		case <-ctx.Done():
		}
		return &acp.PromptResponse{StopReason: acp.StopReasonEndTurn}, nil
	}
	turn := make(chan error, 1)
	go func() { _, _, err := h.Prompt(t.Context(), sid, generation, "ownership test", nil); turn <- err }()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("prompt did not start")
	}
	base := &clientHandler{h: h, generation: generation}
	base.ready.Store(true)
	handler := &workspaceFileHandler{base}
	calls := make([]*workspaceFileCall, 0, maxWorkspaceFileCalls)
	for i := 0; i < maxWorkspaceFileCalls; i++ {
		call, err := handler.beginFileCall(t.Context(), sid)
		if err != nil {
			t.Fatal(err)
		}
		calls = append(calls, call)
	}
	t.Cleanup(func() {
		for _, call := range calls {
			h.mu.Lock()
			_, pending := h.fileCalls[call]
			h.mu.Unlock()
			if pending {
				call.finish()
			}
		}
	})
	if _, err := handler.beginFileCall(t.Context(), sid); err == nil {
		t.Fatal("callback ownership count was unbounded")
	}
	h.Stop()
	// The in-process transport deliberately does not prove native stop.
	// Accepted client operations must still survive watcher map resets.
	if h.ProcessStopped(generation) || h.AllProcessesStopped() {
		t.Fatal("agent stop erased client operation ownership")
	}
	if err := h.CloseIdle(); !errors.Is(err, ErrSessionBusy) {
		t.Fatalf("idle close ignored pending client operations: %v", err)
	}
	if _, _, err := h.OpenSession(t.Context(), sid, SessionConfig{Workdir: root}); !errors.Is(err, ErrStopUnconfirmed) {
		t.Fatalf("agent exit admitted replacement over pending IO: %v", err)
	}
	old := calls[0]
	if err := old.valid(); err == nil {
		t.Fatal("old generation retained filesystem authority")
	}
	for _, call := range calls {
		call.finish()
	}
	select {
	case <-turn:
	case <-time.After(3 * time.Second):
		t.Fatal("old prompt did not retire")
	}
	h.mu.Lock()
	pending := len(h.fileCalls)
	h.mu.Unlock()
	if pending != 0 {
		t.Fatal("finished callbacks did not release their original ownership")
	}
	if h.ProcessStopped(generation) || h.AllProcessesStopped() {
		t.Fatal("finishing callbacks invented native process-stop evidence")
	}
}

func TestWorkspaceFilesCancelledRPCAndDistinctApprovals(t *testing.T) {
	h, a, root, sid, generation := workspaceFilesHost(t, true, "read")
	ids := map[string]bool{}
	a.prompt = func(ctx context.Context, req *acp.PromptRequest) (*acp.PromptResponse, error) {
		for _, name := range []string{"one", "two"} {
			if _, err := a.client.WriteTextFile(ctx, &acp.WriteTextFileRequest{SessionID: req.SessionID, Path: filepath.Join(root, name), Content: name}); err != nil {
				return nil, err
			}
		}
		return &acp.PromptResponse{StopReason: acp.StopReasonEndTurn}, nil
	}
	ask := func(_ context.Context, q permission.Ask) (acp.RequestPermissionOutcome, error) {
		if q.ToolCallID == "" || ids[q.ToolCallID] {
			return acp.CanceledRequestPermissionOutcome(), errors.New("different filesystem operations reused approval identity")
		}
		ids[q.ToolCallID] = true
		return permission.Choose(true, q.Options), nil
	}
	if _, _, err := h.PromptTurn(t.Context(), sid, generation, "two writes", nil, ask, nil, nil); err != nil {
		t.Fatal(err)
	}
	if len(ids) != 2 {
		t.Fatal("two writes did not request distinct approvals")
	}
	// A captured RPC context must be checked directly, not only through an
	// asynchronously scheduled AfterFunc cancellation.
	a.prompt = func(ctx context.Context, req *acp.PromptRequest) (*acp.PromptResponse, error) {
		base := &clientHandler{h: h, generation: generation}
		base.ready.Store(true)
		handler := &workspaceFileHandler{base}
		rpc, cancel := context.WithCancel(ctx)
		call, err := handler.beginFileCall(rpc, req.SessionID)
		if err != nil {
			return nil, err
		}
		defer call.finish()
		cancel()
		if err := call.valid(); err == nil {
			return nil, errors.New("cancelled original RPC remained authorized")
		}
		return &acp.PromptResponse{StopReason: acp.StopReasonEndTurn}, nil
	}
	if _, _, err := h.Prompt(t.Context(), sid, generation, "cancelled RPC guard", nil); err != nil {
		t.Fatal(err)
	}
}

func TestWorkspaceFilesFailedNewPublicationClosesUnpublishedRoot(t *testing.T) {
	if !workspaceFilesSupported {
		t.Skip("local text-file callbacks unavailable")
	}
	root := t.TempDir()
	options := make([]acp.SessionConfigOption, maxNewScratchOptions+1)
	for i := range options {
		options[i] = acp.BooleanSessionConfigOption(acp.SessionConfigID(fmt.Sprintf("toggle-%d", i)), "Toggle", false)
	}
	update := acp.ConfigOptionUpdateSessionUpdate(options)
	peer := &orderedPeer{
		initial:              wireBoolean(false),
		lifecycleParticipant: &lifecycleParticipant{version: acp.ProtocolVersionV1, caps: lifecycleCaps(31)},
		initialModes:         &acp.SessionModeState{CurrentModeID: "agent", AvailableModes: []acp.SessionMode{{ID: "agent", Name: "Agent"}, {ID: "read-only", Name: "Read only"}}},
		modeBefore:           &update,
	}
	broker, _ := permission.New("read")
	gate := &orderedRequestGate{method: acp.MethodSessionSetMode}
	h := New(Config{WorkspaceFiles: true, Transport: orderedTransport{agent: peer, gate: gate}, Permission: broker, NoRestart: true})
	t.Cleanup(h.Close)
	gate.wait = func() error {
		deadline := time.Now().Add(time.Second)
		for time.Now().Before(deadline) {
			h.mu.Lock()
			observed := h.newOpening != nil && h.newOpening.cause != nil
			h.mu.Unlock()
			if observed {
				return nil
			}
			time.Sleep(time.Millisecond)
		}
		return errors.New("bounded new-state refusal not observed")
	}
	_, _, err := h.OpenSession(t.Context(), "", SessionConfig{Workdir: root})
	if !errors.Is(err, ErrSessionOperationUnconfirmed) {
		t.Fatalf("new publication failure lost unknown owner: %v", err)
	}
	h.mu.Lock()
	roots := len(h.fileRoots)
	published := h.sessions["ordered"] != nil
	opening := h.newOpening
	h.mu.Unlock()
	if roots != 0 {
		t.Fatalf("failed new publication retained %d unpublished root descriptors", roots)
	}
	if published || opening == nil || opening.pending {
		t.Fatal("closing passive descriptors granted a session or erased unknown new owner")
	}
}

func TestWorkspaceFilesRootOwnershipSurvivesCloneAndRetiresOnClose(t *testing.T) {
	h, a, root, sid, generation := workspaceFilesHost(t, true, "auto")
	h.mu.Lock()
	original := h.sessions[sid].workspace
	h.mu.Unlock()
	if original == nil {
		t.Fatal("confirmed session has no pinned workspace")
	}
	for i := 0; i < 8; i++ {
		if _, _, err := h.OpenSession(t.Context(), sid, SessionConfig{Workdir: root}); err != nil {
			t.Fatal(err)
		}
	}
	cfg := SessionConfig{Workdir: root, ReplayHistory: true}
	if _, _, err := h.OpenSession(t.Context(), sid, cfg); err != nil {
		t.Fatal(err)
	}
	h.mu.Lock()
	same := h.sessions[sid].workspace == original
	roots := len(h.fileRoots)
	h.mu.Unlock()
	if !same || roots != 1 {
		t.Fatalf("settings/history clone duplicated or changed workspace owner: same=%v roots=%d", same, roots)
	}
	a.prompt = func(ctx context.Context, req *acp.PromptRequest) (*acp.PromptResponse, error) {
		if _, err := a.client.WriteTextFile(ctx, &acp.WriteTextFileRequest{SessionID: req.SessionID, Path: filepath.Join(root, "owned.txt"), Content: "root remained usable"}); err != nil {
			return nil, err
		}
		return &acp.PromptResponse{StopReason: acp.StopReasonEndTurn}, nil
	}
	if _, _, err := h.Prompt(t.Context(), sid, generation, "clone retains grant", nil); err != nil {
		t.Fatal(err)
	}
	if err := h.CloseSession(t.Context(), sid); err != nil {
		t.Fatal(err)
	}
	h.mu.Lock()
	roots = len(h.fileRoots)
	h.mu.Unlock()
	if roots != 0 {
		t.Fatal("protocol close leaked workspace descriptor owner")
	}
	opened, err := original.root.OpenRoot(".")
	if opened != nil {
		opened.Close()
	}
	if err == nil {
		t.Fatal("retired workspace root remained open")
	}
	if _, err := a.client.ReadTextFile(t.Context(), &acp.ReadTextFileRequest{SessionID: sid, Path: filepath.Join(root, "owned.txt")}); err == nil {
		t.Fatal("closed session retained file access")
	}
}
func TestWorkspaceFilesRootClosesAfterFinalPendingOperation(t *testing.T) {
	h, a, _, sid, generation := workspaceFilesHost(t, true, "auto")
	entered, release := make(chan struct{}), make(chan struct{})
	t.Cleanup(func() {
		select {
		case <-release:
		default:
			close(release)
		}
	})
	a.prompt = func(ctx context.Context, _ *acp.PromptRequest) (*acp.PromptResponse, error) {
		close(entered)
		select {
		case <-release:
		case <-ctx.Done():
		}
		return &acp.PromptResponse{StopReason: acp.StopReasonEndTurn}, nil
	}
	result := make(chan error, 1)
	go func() { _, _, err := h.Prompt(t.Context(), sid, generation, "root lifetime", nil); result <- err }()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("prompt not active")
	}
	base := &clientHandler{h: h, generation: generation}
	base.ready.Store(true)
	op, err := (&workspaceFileHandler{base}).beginFileCall(t.Context(), sid)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		h.mu.Lock()
		_, pending := h.fileCalls[op]
		h.mu.Unlock()
		if pending {
			op.finish()
		}
	})
	root := op.workspace.root
	h.Stop()
	child, err := root.OpenRoot(".")
	if err != nil {
		t.Fatalf("stop closed root beneath an accepted original operation: %v", err)
	}
	child.Close()
	if err := op.valid(); err == nil {
		t.Fatal("stopped original operation retained access authority")
	}
	op.finish()
	child, err = root.OpenRoot(".")
	if child != nil {
		child.Close()
	}
	if err == nil {
		t.Fatal("last retired operation leaked pinned root")
	}
	select {
	case <-result:
	case <-time.After(3 * time.Second):
		t.Fatal("prompt did not retire after final operation")
	}
	h.mu.Lock()
	roots := len(h.fileRoots)
	h.mu.Unlock()
	if roots != 0 {
		t.Fatal("passive root owner index leaked")
	}
}

func TestWorkspaceFilesCwdKeepsNativeSymlinkParentSemantics(t *testing.T) {
	if !workspaceFilesSupported {
		t.Skip("local text-file callbacks unavailable")
	}
	tree := t.TempDir()
	actual := filepath.Join(tree, "actual")
	if err := os.MkdirAll(filepath.Join(actual, "child"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("actual/child", filepath.Join(tree, "alias")); err != nil {
		t.Fatal(err)
	}
	inside := filepath.Join(actual, "inside.txt")
	sibling := filepath.Join(tree, "sibling.txt")
	if err := os.WriteFile(inside, []byte("inside"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(sibling, []byte("outside actual cwd"), 0600); err != nil {
		t.Fatal(err)
	}
	cwd := tree + "/alias/.."
	a := &filesParticipant{lifecycleParticipant: &lifecycleParticipant{version: acp.ProtocolVersionV1, caps: lifecycleCaps(31), workdir: cwd, servers: []acp.MCPServer{}}}
	broker, _ := permission.New("auto")
	h := New(Config{WorkspaceFiles: true, Transport: filesTransport{a}, Permission: broker, NoRestart: true})
	t.Cleanup(h.Close)
	sid, generation, err := h.OpenSession(t.Context(), "", SessionConfig{Workdir: cwd})
	if err != nil {
		t.Fatal(err)
	}
	a.prompt = func(ctx context.Context, req *acp.PromptRequest) (*acp.PromptResponse, error) {
		if _, err := a.client.ReadTextFile(ctx, &acp.ReadTextFileRequest{SessionID: req.SessionID, Path: sibling}); err == nil {
			return nil, errors.New("cwd normalization granted its real sibling")
		}
		read, err := a.client.ReadTextFile(ctx, &acp.ReadTextFileRequest{SessionID: req.SessionID, Path: cwd + "/inside.txt"})
		if err != nil {
			return nil, err
		}
		if read.Content != "inside" {
			return nil, errors.New("real session cwd file unavailable")
		}
		_, err = a.client.WriteTextFile(ctx, &acp.WriteTextFileRequest{SessionID: req.SessionID, Path: cwd + "/created.txt", Content: "right root"})
		if err != nil {
			return nil, err
		}
		return &acp.PromptResponse{StopReason: acp.StopReasonEndTurn}, nil
	}
	if _, _, err := h.Prompt(t.Context(), sid, generation, "raw cwd semantics", nil); err != nil {
		t.Fatal(err)
	}
	content, err := os.ReadFile(filepath.Join(actual, "created.txt"))
	if err != nil || string(content) != "right root" {
		t.Fatal("write used cleaned rather than actual cwd")
	}
	content, err = os.ReadFile(sibling)
	if err != nil || string(content) != "outside actual cwd" {
		t.Fatal("real cwd sibling changed")
	}
}
