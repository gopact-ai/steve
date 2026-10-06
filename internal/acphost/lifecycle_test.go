package acphost

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"reflect"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gopact-ai/acp"
)

// lifecycleParticipant exchanges real ACP frames in both directions without
// launching an installed agent or accessing credentials. Prompt tests submit
// only explicitly requested deterministic input; lifecycle calls cannot prompt.
type lifecycleParticipant struct {
	mu            sync.Mutex
	client        *acp.ClientCaller
	caps          *acp.AgentCapabilities
	version       acp.ProtocolVersion
	calls         []string
	process       *lifecycleProcess
	rawCaps       string
	fail          string
	pages         []string
	emptyPage     bool
	listCalls     int
	sessions      map[acp.SessionID]bool
	newCount      int
	workdir       string
	servers       []acp.MCPServer
	loadStarted   chan struct{}
	loadRelease   chan struct{}
	promptStarted chan struct{}
	promptRelease chan struct{}
}

func (a *lifecycleParticipant) record(method string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.calls = append(a.calls, method)
}
func (a *lifecycleParticipant) methods() []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]string(nil), a.calls...)
}
func (a *lifecycleParticipant) Initialize(_ context.Context, req *acp.InitializeRequest) (*acp.InitializeResponse, error) {
	a.record("initialize")
	if req.ProtocolVersion != acp.ProtocolVersionV1 {
		return nil, errors.New("client did not request V1")
	}
	return &acp.InitializeResponse{ProtocolVersion: a.version, AgentCapabilities: a.caps}, nil
}
func (a *lifecycleParticipant) NewSession(_ context.Context, req *acp.NewSessionRequest) (*acp.NewSessionResponse, error) {
	a.record("new")
	if err := a.config(req.Cwd, req.MCPServers); err != nil {
		return nil, err
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	a.newCount++
	sid := acp.SessionID(fmt.Sprintf("fresh-%d", a.newCount))
	if a.sessions == nil {
		a.sessions = map[acp.SessionID]bool{}
	}
	a.sessions[sid] = true
	return &acp.NewSessionResponse{SessionID: sid}, nil
}
func (a *lifecycleParticipant) Prompt(ctx context.Context, req *acp.PromptRequest) (*acp.PromptResponse, error) {
	a.record("prompt")
	if a.promptStarted == nil {
		return nil, errors.New("lifecycle operations must never submit a business prompt")
	}
	// An explicit deterministic test turn also exercises a reverse client RPC.
	resp, err := a.client.RequestPermission(ctx, &acp.RequestPermissionRequest{SessionID: req.SessionID, ToolCall: acp.ToolCallUpdate{ToolCallID: "test-tool"}, Options: []acp.PermissionOption{{OptionID: "deny", Name: "Deny", Kind: acp.PermissionOptionKindRejectOnce}}})
	if err != nil {
		return nil, err
	}
	if resp.Outcome.Outcome != acp.RequestPermissionOutcomeTypeSelected || resp.Outcome.OptionID != "deny" {
		return nil, errors.New("client did not deny deterministic permission request")
	}
	close(a.promptStarted)
	select {
	case <-a.promptRelease:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	if err := a.client.Update(ctx, &acp.SessionNotification{SessionID: req.SessionID, Update: acp.AgentMessageChunkSessionUpdate(acp.TextContentBlock("fresh answer"))}); err != nil {
		return nil, err
	}
	return &acp.PromptResponse{StopReason: acp.StopReasonEndTurn}, nil
}
func (a *lifecycleParticipant) Cancel(context.Context, *acp.CancelNotification) error {
	a.record("cancel")
	return nil
}
func (a *lifecycleParticipant) LoadSession(ctx context.Context, req *acp.LoadSessionRequest) (*acp.LoadSessionResponse, error) {
	a.record("load")
	if err := a.check("load"); err != nil {
		return nil, err
	}
	if err := a.config(req.Cwd, req.MCPServers); err != nil {
		return nil, err
	}
	if a.loadStarted != nil {
		close(a.loadStarted)
		select {
		case <-a.loadRelease:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	// Actual reverse notifications make load's history replay observable on the wire.
	if err := a.client.Update(ctx, &acp.SessionNotification{SessionID: req.SessionID, Update: acp.UserMessageChunkSessionUpdate(acp.TextContentBlock("old user input"))}); err != nil {
		return nil, err
	}
	if err := a.client.Update(ctx, &acp.SessionNotification{SessionID: req.SessionID, Update: acp.AgentMessageChunkSessionUpdate(acp.TextContentBlock("old answer"))}); err != nil {
		return nil, err
	}
	return &acp.LoadSessionResponse{}, nil
}
func (a *lifecycleParticipant) ResumeSession(_ context.Context, req *acp.ResumeSessionRequest) (*acp.ResumeSessionResponse, error) {
	a.record("resume")
	if err := a.check("resume"); err != nil {
		return nil, err
	}
	if err := a.config(req.Cwd, req.MCPServers); err != nil {
		return nil, err
	}
	return &acp.ResumeSessionResponse{}, nil
}

type lifecycleTransport struct{ agent *lifecycleParticipant }

func (lifecycleTransport) Name() string { return "deterministic-acp" }
func (tr lifecycleTransport) Start(context.Context) (Process, error) {
	input, stdin := io.Pipe()
	stdout, output := io.Pipe()
	conn, err := acp.NewAgent(input, lifecycleWireWriter{output: output, rawCaps: tr.agent.rawCaps}, func(client *acp.ClientCaller) acp.AgentHandler { tr.agent.client = client; return tr.agent })
	if err != nil {
		_ = input.Close()
		_ = stdin.Close()
		_ = stdout.Close()
		_ = output.Close()
		return nil, err
	}
	p := &lifecycleProcess{conn: conn, stdin: stdin, stdout: stdout, output: output}
	tr.agent.process = p
	return p, nil
}

type lifecycleProcess struct {
	conn   *acp.Conn
	stdin  *io.PipeWriter
	stdout *io.PipeReader
	output *io.PipeWriter
}

func (p *lifecycleProcess) Stdin() io.WriteCloser   { return p.stdin }
func (p *lifecycleProcess) Stdout() io.ReadCloser   { return p.stdout }
func (p *lifecycleProcess) Exited() <-chan struct{} { return p.conn.Done() }
func (p *lifecycleProcess) Wait() error             { <-p.conn.Done(); return p.output.Close() }

// In-memory protocol closure is deliberately not native process-stop evidence.
func (*lifecycleProcess) Stopped() bool { return false }
func (p *lifecycleProcess) Kill() {
	_ = p.conn.Close()
	_ = p.stdin.Close()
	_ = p.output.Close()
	_ = p.stdout.Close()
}
func lifecycleHost(t *testing.T, a *lifecycleParticipant) *Host {
	t.Helper()
	h := New(Config{Transport: lifecycleTransport{agent: a}, NoRestart: true})
	t.Cleanup(h.Stop)
	return h
}

func TestInitializeRejectsUnsupportedProtocol(t *testing.T) {
	for _, version := range []acp.ProtocolVersion{0, 2, 99} {
		t.Run(fmt.Sprint(version), func(t *testing.T) {
			a := &lifecycleParticipant{version: version}
			h := lifecycleHost(t, a)
			sid, _, err := h.OpenSession(t.Context(), "", SessionConfig{Workdir: t.TempDir()})
			if err == nil || !strings.Contains(err.Error(), "protocol") {
				t.Fatalf("unsupported protocol opened session %q: %v", sid, err)
			}
			if got := a.methods(); !reflect.DeepEqual(got, []string{"initialize"}) {
				t.Fatalf("calls after rejected initialize = %v", got)
			}
			select {
			case <-a.process.Exited():
			default:
				t.Fatal("rejected initialize left its owned transport running")
			}
		})
	}
}

func TestOpenExistingSessionPrefersResume(t *testing.T) {
	a := &lifecycleParticipant{version: acp.ProtocolVersionV1, caps: &acp.AgentCapabilities{LoadSession: true, SessionCapabilities: &acp.SessionCapabilities{Resume: &acp.SessionResumeCapabilities{}}}}
	h := lifecycleHost(t, a)
	sid, _, err := h.OpenSession(t.Context(), "stored", SessionConfig{Workdir: t.TempDir()})
	if err != nil || sid != "stored" {
		t.Fatalf("resume = %q, %v", sid, err)
	}
	if got := a.methods(); !reflect.DeepEqual(got, []string{"initialize", "resume"}) {
		t.Fatalf("existing transcript should not replay history; calls = %v", got)
	}
}

func (a *lifecycleParticipant) config(cwd string, servers []acp.MCPServer) error {
	if a.workdir != "" && (cwd != a.workdir || !reflect.DeepEqual(servers, a.servers)) {
		return fmt.Errorf("session workspace or MCP arguments changed: cwd=%q want=%q servers=%#v want=%#v", cwd, a.workdir, servers, a.servers)
	}
	return nil
}
func (a *lifecycleParticipant) check(method string) error {
	if a.fail == method {
		return &acp.Error{Code: acp.ErrorCodeMethodNotFound, Message: "advertised method unavailable"}
	}
	return nil
}
func (a *lifecycleParticipant) CloseSession(ctx context.Context, req *acp.CloseSessionRequest) (*acp.CloseSessionResponse, error) {
	a.record("close")
	if a.fail == "close-timeout" {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	if err := a.check("close"); err != nil {
		return nil, err
	}
	// Closing releases active context, not the agent's persistent history.
	return &acp.CloseSessionResponse{}, nil
}
func (a *lifecycleParticipant) DeleteSession(_ context.Context, req *acp.DeleteSessionRequest) (*acp.DeleteSessionResponse, error) {
	a.record("delete")
	if err := a.check("delete"); err != nil {
		return nil, err
	}
	a.mu.Lock()
	delete(a.sessions, req.SessionID)
	a.mu.Unlock()
	return &acp.DeleteSessionResponse{}, nil
}
func (a *lifecycleParticipant) ListSessions(_ context.Context, req *acp.ListSessionsRequest) (*acp.ListSessionsResponse, error) {
	a.record("list")
	if err := a.check("list"); err != nil {
		return nil, err
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	var ids []string
	for id := range a.sessions {
		ids = append(ids, string(id))
	}
	sort.Strings(ids)
	var sessions []acp.SessionInfo
	for _, id := range ids {
		sessions = append(sessions, acp.SessionInfo{SessionID: acp.SessionID(id), Cwd: "/test"})
	}
	a.listCalls++
	if len(a.pages) > 0 {
		if a.listCalls > 1 && (req.Cursor == nil || *req.Cursor != a.pages[(a.listCalls-2)%len(a.pages)]) {
			return nil, errors.New("client changed opaque cursor")
		}
		cursor := a.pages[(a.listCalls-1)%len(a.pages)]
		if a.emptyPage && a.listCalls == 1 {
			sessions = nil
		}
		return &acp.ListSessionsResponse{Sessions: sessions, NextCursor: &cursor}, nil
	}
	if req.Cursor == nil && len(sessions) > 1 {
		cursor := "page-2"
		return &acp.ListSessionsResponse{Sessions: sessions[:1], NextCursor: &cursor}, nil
	}
	if req.Cursor != nil {
		if *req.Cursor != "page-2" {
			return nil, errors.New("wrong continuation cursor")
		}
		sessions = sessions[1:]
	}
	return &acp.ListSessionsResponse{Sessions: sessions}, nil
}

// Override only the initialize frame so absent/null/object are exercised on
// the real wire, rather than normalized away by typed capability marshaling.
type lifecycleWireWriter struct {
	output  io.Writer
	rawCaps string
}

func (w lifecycleWireWriter) Write(frame []byte) (int, error) {
	if w.rawCaps == "" {
		return w.output.Write(frame)
	}
	var envelope map[string]json.RawMessage
	if err := json.Unmarshal(frame, &envelope); err != nil {
		return 0, err
	}
	var result map[string]json.RawMessage
	if json.Unmarshal(envelope["result"], &result) == nil && result["protocolVersion"] != nil {
		if w.rawCaps == "absent" {
			delete(result, "agentCapabilities")
		} else {
			result["agentCapabilities"] = json.RawMessage(w.rawCaps)
		}
		raw, err := json.Marshal(result)
		if err != nil {
			return 0, err
		}
		envelope["result"] = raw
		out, err := json.Marshal(envelope)
		if err != nil {
			return 0, err
		}
		if _, err = w.output.Write(append(out, '\n')); err != nil {
			return 0, err
		}
		return len(frame), nil
	}
	return w.output.Write(frame)
}

func lifecycleCaps(mask int) *acp.AgentCapabilities {
	caps := &acp.AgentCapabilities{LoadSession: mask&1 != 0, SessionCapabilities: &acp.SessionCapabilities{}}
	if mask&2 != 0 {
		caps.SessionCapabilities.Resume = &acp.SessionResumeCapabilities{}
	}
	if mask&4 != 0 {
		caps.SessionCapabilities.Close = &acp.SessionCloseCapabilities{}
	}
	if mask&8 != 0 {
		caps.SessionCapabilities.List = &acp.SessionListCapabilities{}
	}
	if mask&16 != 0 {
		caps.SessionCapabilities.Delete = &acp.SessionDeleteCapabilities{}
	}
	return caps
}
func requireMethods(t *testing.T, a *lifecycleParticipant, want []string) {
	t.Helper()
	if got := a.methods(); !reflect.DeepEqual(got, want) {
		t.Fatalf("ACP methods = %v, want %v", got, want)
	}
}
func knownSession(h *Host, sid acp.SessionID) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.sessions[sid] != nil
}

// Each five-bit combination verifies restore intent, optional RPC gating,
// local bookkeeping versus protocol acknowledgement, and no implicit prompt.
func TestLifecycleCapabilityMatrix32(t *testing.T) {
	for mask := 0; mask < 32; mask++ {
		t.Run(fmt.Sprintf("%05b", mask), func(t *testing.T) {
			cfg := SessionConfig{Workdir: t.TempDir(), MCPServers: []acp.MCPServer{acp.StdioMCPServer("test", "test-mcp", []string{"with space", ""}, []acp.EnvVariable{{Name: "TEST_VALUE", Value: ""}})}}
			a := &lifecycleParticipant{version: acp.ProtocolVersionV1, caps: lifecycleCaps(mask), workdir: cfg.Workdir, servers: cfg.MCPServers, sessions: map[acp.SessionID]bool{"stored": true, "history": true}}
			h := lifecycleHost(t, a)
			supported, err := h.SupportsResume(t.Context())
			if err != nil || supported != (mask&3 != 0) {
				t.Fatalf("resume availability = %v, %v", supported, err)
			}
			want := []string{"initialize"}
			first, _, err := h.OpenSession(t.Context(), "", cfg)
			if err != nil {
				t.Fatal(err)
			}
			want = append(want, "new")
			second, _, err := h.OpenSession(t.Context(), "", cfg)
			if err != nil {
				t.Fatal(err)
			}
			want = append(want, "new")
			sid, _, err := h.OpenSession(t.Context(), "stored", cfg)
			switch {
			case mask&2 != 0:
				want = append(want, "resume")
			case mask&1 != 0:
				want = append(want, "load")
			default:
				if !errors.Is(err, ErrResumeUnsupported) || sid != "" {
					t.Fatalf("unsupported restore = %q, %v", sid, err)
				}
			}
			if mask&3 != 0 && (err != nil || sid != "stored") {
				t.Fatalf("restore = %q, %v", sid, err)
			}
			cfg.ReplayHistory = true
			sid, _, err = h.OpenSession(t.Context(), "history", cfg)
			if mask&1 != 0 {
				want = append(want, "load")
				if err != nil || sid != "history" {
					t.Fatalf("history replay = %q, %v", sid, err)
				}
			} else if !errors.Is(err, ErrLoadUnsupported) || sid != "" {
				t.Fatalf("unsupported history replay = %q, %v", sid, err)
			}
			list, err := h.ListSessions(t.Context())
			if mask&8 != 0 {
				want = append(want, "list", "list")
				if err != nil || len(list) != 4 {
					t.Fatalf("paginated list = %v, %v", list, err)
				}
			} else if !errors.Is(err, ErrListUnsupported) {
				t.Fatalf("unsupported list = %v", err)
			}
			if err := h.CloseSession(t.Context(), first); err != nil {
				t.Fatal(err)
			}
			if mask&4 != 0 {
				want = append(want, "close")
			}
			if knownSession(h, first) || !knownSession(h, second) {
				t.Fatal("close forgot the wrong session")
			}
			// A protocol ACK (or local forget) must not be promoted into host-wide stop.
			select {
			case <-a.process.Exited():
				t.Fatal("closing one session stopped the shared host")
			default:
			}
			if h.ProcessStopped(h.generation) {
				t.Fatal("session close invented native cleanup evidence")
			}
			err = h.DeleteSession(t.Context(), second)
			if mask&16 != 0 {
				want = append(want, "delete")
				if err != nil || knownSession(h, second) {
					t.Fatalf("delete = %v, retained=%v", err, knownSession(h, second))
				}
			} else if !errors.Is(err, ErrDeleteUnsupported) || !knownSession(h, second) {
				t.Fatalf("unsupported delete = %v", err)
			}
			if mask&8 != 0 {
				list, err = h.ListSessions(t.Context())
				want = append(want, "list", "list")
				expected := 4
				if mask&16 != 0 {
					expected--
				}
				if err != nil || len(list) != expected {
					t.Fatalf("close must preserve history, delete only removes its list entry: %v, %v", list, err)
				}
			}
			requireMethods(t, a, want)
		})
	}
}

func TestLifecycleCapabilityWirePresence(t *testing.T) {
	cases := []struct {
		name, raw string
		mask      int
	}{
		{"agent-absent", "absent", 0}, {"agent-null", "null", 0}, {"agent-empty", "{}", 0},
		{"session-absent", `{"loadSession":false}`, 0}, {"session-null", `{"loadSession":null,"sessionCapabilities":null}`, 0},
		{"session-empty", `{"sessionCapabilities":{}}`, 0},
		{"method-null", `{"sessionCapabilities":{"resume":null,"close":null,"list":null,"delete":null}}`, 0},
		{"method-empty", `{"loadSession":true,"sessionCapabilities":{"resume":{},"close":{},"list":{},"delete":{}}}`, 31},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			a := &lifecycleParticipant{version: acp.ProtocolVersionV1, rawCaps: tc.raw}
			h := lifecycleHost(t, a)
			cfg := SessionConfig{Workdir: t.TempDir()}
			supported, err := h.SupportsResume(t.Context())
			if err != nil || supported != (tc.mask&3 != 0) {
				t.Fatalf("wire resume support=%v, %v", supported, err)
			}
			sid, _, err := h.OpenSession(t.Context(), "", cfg)
			if err != nil {
				t.Fatal(err)
			}
			want := []string{"initialize", "new"}
			_, _, err = h.OpenSession(t.Context(), "stored", cfg)
			if tc.mask&2 != 0 {
				want = append(want, "resume")
				if err != nil {
					t.Fatal(err)
				}
			} else if !errors.Is(err, ErrResumeUnsupported) {
				t.Fatalf("absent restore=%v", err)
			}
			cfg.ReplayHistory = true
			_, _, err = h.OpenSession(t.Context(), "history", cfg)
			if tc.mask&1 != 0 {
				want = append(want, "load")
				if err != nil {
					t.Fatal(err)
				}
			} else if !errors.Is(err, ErrLoadUnsupported) {
				t.Fatalf("absent load=%v", err)
			}
			_, err = h.ListSessions(t.Context())
			if tc.mask&8 != 0 {
				want = append(want, "list")
				if err != nil {
					t.Fatal(err)
				}
			} else if !errors.Is(err, ErrListUnsupported) {
				t.Fatalf("absent list=%v", err)
			}
			if err = h.CloseSession(t.Context(), sid); err != nil {
				t.Fatal(err)
			}
			if tc.mask&4 != 0 {
				want = append(want, "close")
			}
			err = h.DeleteSession(t.Context(), sid)
			if tc.mask&16 != 0 {
				want = append(want, "delete")
				if err != nil {
					t.Fatal(err)
				}
			} else if !errors.Is(err, ErrDeleteUnsupported) {
				t.Fatalf("absent delete=%v", err)
			}
			requireMethods(t, a, want)
		})
	}
}

func TestAdvertisedLifecycleFailureDoesNotFallback(t *testing.T) {
	for _, method := range []string{"load", "resume", "close", "list", "delete"} {
		t.Run(method, func(t *testing.T) {
			a := &lifecycleParticipant{version: acp.ProtocolVersionV1, caps: lifecycleCaps(31), fail: method}
			h := lifecycleHost(t, a)
			cfg := SessionConfig{Workdir: t.TempDir()}
			sid, generation, err := h.OpenSession(t.Context(), "", cfg)
			if err != nil {
				t.Fatal(err)
			}
			switch method {
			case "load":
				cfg.ReplayHistory = true
				_, _, err = h.OpenSession(t.Context(), "stored", cfg)
			case "resume":
				_, _, err = h.OpenSession(t.Context(), "stored", cfg)
			case "close":
				err = h.CloseSession(t.Context(), sid)
			case "list":
				_, err = h.ListSessions(t.Context())
			case "delete":
				err = h.DeleteSession(t.Context(), sid)
			}
			var rpc *acp.Error
			if !errors.As(err, &rpc) || rpc.Code != acp.ErrorCodeMethodNotFound || !strings.Contains(err.Error(), "session/"+method) {
				t.Fatalf("advertised %s failure=%v", method, err)
			}
			if _, known := h.SettingsForGeneration(sid, generation); !known {
				t.Fatal("failed lifecycle method lost the original session")
			}
			requireMethods(t, a, []string{"initialize", "new", method})
		})
	}
}

func TestListSessionsRejectsCursorCycles(t *testing.T) {
	for _, pages := range [][]string{{"repeat"}, {"a", "b"}} {
		t.Run(strings.Join(pages, "-"), func(t *testing.T) {
			a := &lifecycleParticipant{version: acp.ProtocolVersionV1, caps: lifecycleCaps(8), pages: pages, sessions: map[acp.SessionID]bool{"stored": true}}
			h := lifecycleHost(t, a)
			ctx, cancel := context.WithTimeout(t.Context(), time.Second)
			defer cancel()
			list, err := h.ListSessions(ctx)
			if err == nil || !strings.Contains(err.Error(), "cursor") || errors.Is(err, context.DeadlineExceeded) || list != nil {
				t.Fatalf("cursor cycle was not explicitly rejected: list=%v err=%v", list, err)
			}
			if a.listCalls != len(pages)+1 {
				t.Fatalf("cycle rejection required %d calls, want %d", a.listCalls, len(pages)+1)
			}
		})
	}
}
func TestListSessionsDoesNotTreatEmptyPageAsEnd(t *testing.T) {
	a := &lifecycleParticipant{version: acp.ProtocolVersionV1, caps: lifecycleCaps(8), pages: []string{"again"}, emptyPage: true, sessions: map[acp.SessionID]bool{"stored": true}}
	h := lifecycleHost(t, a)
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	// The empty first page has a cursor. Following it reveals a repeated cursor,
	// not a successful empty list; an empty page is not a terminal declaration.
	_, err := h.ListSessions(ctx)
	if err == nil || !strings.Contains(err.Error(), "cursor") || a.listCalls != 2 {
		t.Fatalf("empty continuation page = %v, calls=%d", err, a.listCalls)
	}
}

func TestReplayHistoryRequiresStoredSessionAndLoad(t *testing.T) {
	a := &lifecycleParticipant{version: acp.ProtocolVersionV1, caps: lifecycleCaps(2)}
	h := lifecycleHost(t, a)
	cfg := SessionConfig{Workdir: t.TempDir(), ReplayHistory: true}
	if _, _, err := h.OpenSession(t.Context(), "", cfg); err == nil {
		t.Fatal("history replay silently created a new session")
	}
	requireMethods(t, a, nil)
	if _, _, err := h.OpenSession(t.Context(), "stored", cfg); !errors.Is(err, ErrLoadUnsupported) {
		t.Fatalf("resume substituted for required history replay: %v", err)
	}
	requireMethods(t, a, []string{"initialize"})
}

func TestReplayHistoryReloadsAlreadyOpenSession(t *testing.T) {
	a := &lifecycleParticipant{version: acp.ProtocolVersionV1, caps: lifecycleCaps(3)}
	h := lifecycleHost(t, a)
	cfg := SessionConfig{Workdir: t.TempDir()}
	if _, _, err := h.OpenSession(t.Context(), "stored", cfg); err != nil {
		t.Fatal(err)
	}
	if _, _, err := h.OpenSession(t.Context(), "stored", cfg); err != nil {
		t.Fatal(err)
	}
	requireMethods(t, a, []string{"initialize", "resume"})
	cfg.ReplayHistory = true
	if _, _, err := h.OpenSession(t.Context(), "stored", cfg); err != nil {
		t.Fatal(err)
	}
	requireMethods(t, a, []string{"initialize", "resume", "load"})
}

func TestActiveLifecycleOperationsRefuseBeforeRPC(t *testing.T) {
	a := &lifecycleParticipant{version: acp.ProtocolVersionV1, caps: lifecycleCaps(31), promptStarted: make(chan struct{}), promptRelease: make(chan struct{})}
	h := lifecycleHost(t, a)
	cfg := SessionConfig{Workdir: t.TempDir()}
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	sid, generation, err := h.OpenSession(ctx, "", cfg)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		out, _, err := h.Prompt(ctx, sid, generation, "deterministic test input", nil)
		if err == nil && out != "fresh answer" {
			err = fmt.Errorf("history contaminated answer: %q", out)
		}
		done <- err
	}()
	defer func() {
		select {
		case <-a.promptRelease:
		default:
			close(a.promptRelease)
		}
	}()
	select {
	case <-a.promptStarted:
	case <-ctx.Done():
		t.Fatal("deterministic prompt did not start")
	}
	if err := h.CloseSession(ctx, sid); !errors.Is(err, ErrSessionBusy) {
		t.Fatalf("active close=%v", err)
	}
	if err := h.DeleteSession(ctx, sid); !errors.Is(err, ErrSessionBusy) {
		t.Fatalf("active delete=%v", err)
	}
	cfg.ReplayHistory = true
	if _, _, err := h.OpenSession(ctx, sid, cfg); !errors.Is(err, ErrSessionBusy) {
		t.Fatalf("active history replay=%v", err)
	}
	requireMethods(t, a, []string{"initialize", "new", "prompt"})
	close(a.promptRelease)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if err := h.CloseSession(ctx, sid); err != nil {
		t.Fatal(err)
	}
	requireMethods(t, a, []string{"initialize", "new", "prompt", "close"})
}

// Re-exec only this test binary as a deterministic ACP participant. It never
// loads a user's agent, model, startup configuration or credentials.
func TestLifecycleOwnedProcessHelper(t *testing.T) {
	if os.Getenv("STEVE_TEST_ACP_PROTOCOL_HELPER") != "1" {
		return
	}
	a := &lifecycleParticipant{version: 2}
	conn, err := acp.NewAgent(os.Stdin, os.Stdout, func(c *acp.ClientCaller) acp.AgentHandler { a.client = c; return a })
	if err != nil {
		os.Exit(2)
	}
	<-conn.Done()
	os.Exit(0)
}
func TestInitializeMismatchStopsOwnedProcess(t *testing.T) {
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	h := New(Config{Command: executable, Args: []string{"-test.run=^TestLifecycleOwnedProcessHelper$"}, Env: []string{"STEVE_TEST_ACP_PROTOCOL_HELPER=1"}, ProcessDir: t.TempDir(), NoRestart: true})
	t.Cleanup(h.Stop)
	if _, _, err := h.OpenSession(t.Context(), "", SessionConfig{Workdir: t.TempDir()}); err == nil || !strings.Contains(err.Error(), "protocol version") {
		t.Fatalf("mismatched initialize=%v", err)
	}
	if !h.AllProcessesStopped() {
		t.Fatal("version mismatch did not confirm stop of its owned native process")
	}
	if _, _, err := h.OpenSession(t.Context(), "", SessionConfig{Workdir: t.TempDir()}); !errors.Is(err, ErrClosed) {
		t.Fatalf("NoRestart reopened rejected process: %v", err)
	}
}

func TestLoadedHistoryDoesNotBecomeNextPromptAnswer(t *testing.T) {
	release := make(chan struct{})
	close(release)
	a := &lifecycleParticipant{version: acp.ProtocolVersionV1, caps: lifecycleCaps(31), promptStarted: make(chan struct{}), promptRelease: release}
	h := lifecycleHost(t, a)
	sid, generation, err := h.OpenSession(t.Context(), "stored", SessionConfig{Workdir: t.TempDir(), ReplayHistory: true})
	if err != nil {
		t.Fatal(err)
	}
	requireMethods(t, a, []string{"initialize", "load"})
	out, _, err := h.Prompt(t.Context(), sid, generation, "deterministic next input", nil)
	if err != nil || out != "fresh answer" {
		t.Fatalf("load replay contaminated the next explicitly submitted turn: %q, %v", out, err)
	}
	requireMethods(t, a, []string{"initialize", "load", "prompt"})
}

func TestHistoryReplayExcludesConcurrentSessionOperations(t *testing.T) {
	a := &lifecycleParticipant{version: acp.ProtocolVersionV1, caps: lifecycleCaps(31), loadStarted: make(chan struct{}), loadRelease: make(chan struct{})}
	h := lifecycleHost(t, a)
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	cfg := SessionConfig{Workdir: t.TempDir()}
	sid, generation, err := h.OpenSession(ctx, "stored", cfg)
	if err != nil {
		t.Fatal(err)
	}
	cfg.ReplayHistory = true
	done := make(chan error, 1)
	go func() { _, _, err := h.OpenSession(ctx, sid, cfg); done <- err }()
	defer func() {
		select {
		case <-a.loadRelease:
		default:
			close(a.loadRelease)
		}
	}()
	select {
	case <-a.loadStarted:
	case <-ctx.Done():
		t.Fatal("load did not start")
	}
	if _, _, err := h.Prompt(ctx, sid, generation, "must not overlap replay", nil); !errors.Is(err, ErrSessionBusy) {
		t.Errorf("prompt overlapped replay: %v", err)
	}
	if err := h.CloseSession(ctx, sid); !errors.Is(err, ErrSessionBusy) {
		t.Errorf("close overlapped replay: %v", err)
	}
	if err := h.DeleteSession(ctx, sid); !errors.Is(err, ErrSessionBusy) {
		t.Errorf("delete overlapped replay: %v", err)
	}
	cfg.ReplayHistory = false
	if _, _, err := h.OpenSession(ctx, sid, cfg); err == nil {
		t.Error("cached open pretended replay had completed")
	}
	requireMethods(t, a, []string{"initialize", "resume", "load"})
	close(a.loadRelease)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if _, known := h.SettingsForGeneration(sid, generation); !known {
		t.Fatal("completed replay lost its session")
	}
}

func TestEmptyListDoesNotPreventNativeResume(t *testing.T) {
	a := &lifecycleParticipant{version: acp.ProtocolVersionV1, caps: lifecycleCaps(10)}
	h := lifecycleHost(t, a)
	sessions, err := h.ListSessions(t.Context())
	if err != nil || len(sessions) != 0 {
		t.Fatalf("empty discovery=%v, %v", sessions, err)
	}
	sid, _, err := h.OpenSession(t.Context(), "stored", SessionConfig{Workdir: t.TempDir()})
	if err != nil || sid != "stored" {
		t.Fatalf("empty discovery was treated as lost history: %q, %v", sid, err)
	}
	requireMethods(t, a, []string{"initialize", "list", "resume"})
}

func TestListSessionsFollowsEmptyContinuationPage(t *testing.T) {
	a := &lifecycleParticipant{version: acp.ProtocolVersionV1, caps: lifecycleCaps(8), pages: []string{"next", ""}, emptyPage: true, sessions: map[acp.SessionID]bool{"stored": true}}
	h := lifecycleHost(t, a)
	sessions, err := h.ListSessions(t.Context())
	if err != nil || len(sessions) != 1 || sessions[0].SessionID != "stored" {
		t.Fatalf("empty continuation page hid later results: %v, %v", sessions, err)
	}
	requireMethods(t, a, []string{"initialize", "list", "list"})
}

func TestCloseTimeoutRetainsSessionWithoutClaimingNativeCleanup(t *testing.T) {
	a := &lifecycleParticipant{version: acp.ProtocolVersionV1, caps: lifecycleCaps(4), fail: "close-timeout"}
	h := lifecycleHost(t, a)
	sid, generation, err := h.OpenSession(t.Context(), "", SessionConfig{Workdir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
	defer cancel()
	if err := h.CloseSession(ctx, sid); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("close timeout=%v", err)
	}
	if _, known := h.SettingsForGeneration(sid, generation); !known {
		t.Fatal("unacknowledged close lost its session")
	}
	select {
	case <-a.process.Exited():
		t.Fatal("unacknowledged close terminated the shared host")
	default:
	}
	if h.ProcessStopped(generation) {
		t.Fatal("unacknowledged close fabricated native cleanup evidence")
	}
	requireMethods(t, a, []string{"initialize", "new", "close"})
}
