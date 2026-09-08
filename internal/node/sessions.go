package node

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"
	"sync"
	"time"

	"github.com/gopact-ai/steve/internal/acphost"
	"github.com/gopact-ai/steve/internal/nodewire"
	"github.com/gopact-ai/steve/internal/permission"
	steveruntime "github.com/gopact-ai/steve/internal/runtime"
	"github.com/gopact-ai/steve/internal/view"
)

// SessionAuthorizer checks the authenticated connection principal and committed
// coordinator/execution authority. Request fields are claims, never credentials.
type SessionAuthorizer interface {
	AuthorizeNodeSession(context.Context, string, nodewire.SessionAuthority, nodewire.SessionBinding, nodewire.SessionAction) error
}

type SessionError struct{ Code, Message string }

func (e *SessionError) Error() string         { return "node session: " + e.Message }
func sessionError(code, message string) error { return &SessionError{code, message} }

type SessionService struct {
	server              *Server
	ctx                 context.Context
	cancel              context.CancelFunc
	mu                  sync.Mutex
	sessions            map[string]*ownedSession
	closed              bool
	unverifiedProcesses bool
	wg                  sync.WaitGroup
	capMu               sync.Mutex
	capabilities        map[string]harnessCapabilities
}

// harnessCapabilities is what starting a harness once said about the agent
// behind it. The answer belongs to the binary, not to a session: the hub
// asks before every turn, and each answer used to cost an adapter process.
type harnessCapabilities struct {
	spec            string
	supportsHTTPMCP bool
	at              time.Time
}

// capabilityTTL bounds how long a probe answers for a binary nobody has
// started since; a session open refreshes it with the agent's own answer.
const capabilityTTL = time.Hour

func (s *SessionService) hostConfig(harnessID string, spec HarnessSpec, broker *permission.Broker) acphost.Config {
	return acphost.Config{NoRestart: true, Command: spec.Command, Args: spec.Args, ProcessDir: s.server.processDir(spec), Env: steveruntime.ApplyEnv(spec.Env, harnessID, s.server.conf().StateDir), Permission: broker}
}

// capabilityKey names the binary a capability answer was taken from: the
// same command, arguments, directory and environment.
func capabilityKey(cfg acphost.Config) string {
	return sessionHash(struct {
		Command, ProcessDir string
		Args, Env           []string
	}{cfg.Command, cfg.ProcessDir, cfg.Args, cfg.Env})
}

func (s *SessionService) cachedCapabilities(harnessID, key string) (harnessCapabilities, bool) {
	s.capMu.Lock()
	defer s.capMu.Unlock()
	c, ok := s.capabilities[harnessID]
	if !ok || c.spec != key || time.Since(c.at) > capabilityTTL {
		return harnessCapabilities{}, false
	}
	return c, true
}

func (s *SessionService) rememberCapabilities(harnessID, key string, supportsHTTPMCP bool) {
	s.capMu.Lock()
	defer s.capMu.Unlock()
	if s.capabilities == nil {
		s.capabilities = map[string]harnessCapabilities{}
	}
	s.capabilities[harnessID] = harnessCapabilities{spec: key, supportsHTTPMCP: supportsHTTPMCP, at: time.Now()}
}

func (s *SessionService) forgetCapabilities(harnessID string) {
	s.capMu.Lock()
	defer s.capMu.Unlock()
	delete(s.capabilities, harnessID)
}

type ownedSession struct {
	service         *SessionService
	mu              sync.Mutex
	record          sessionRecord
	host            *acphost.Host
	changed         chan struct{}
	waiters         map[string]chan struct{}
	runDone         chan struct{}
	openDone        chan struct{}
	openCancel      context.CancelFunc
	failure         error
	pendingProgress *view.Progress
	progressTimer   *time.Timer
}

type sessionRecord struct {
	Format         int                                `json:"format"`
	ClusterID      string                             `json:"cluster_id"`
	Authority      nodewire.SessionAuthority          `json:"authority"`
	OpenID         string                             `json:"open_id"`
	OpenCancelled  bool                               `json:"open_cancelled,omitempty"`
	OpenHash       string                             `json:"open_hash"`
	ConfigHash     string                             `json:"config_hash"`
	UpstreamID     string                             `json:"upstream_id"`
	Generation     uint64                             `json:"generation"`
	State          nodewire.SessionState              `json:"state"`
	CommandHashes  map[string]string                  `json:"command_hashes"`
	Commands       map[string]nodewire.SessionCommand `json:"commands"`
	CurrentCommand string                             `json:"current_command,omitempty"`
}

func sessionHash(value any) string {
	raw, _ := json.Marshal(value)
	h := sha256.Sum256(raw)
	return hex.EncodeToString(h[:])
}
func sessionIDValid(id string) bool {
	if !strings.HasPrefix(id, "ns_") || len(id) != 67 {
		return false
	}
	_, err := hex.DecodeString(id[3:])
	return err == nil
}
func sessionNameValid(id string) bool {
	return id != "" && len(id) <= 512 && !strings.ContainsAny(id, "\x00\r\n")
}

func (s *Server) startSessions(ctx context.Context) error {
	if s.conf().SessionAuthorizer == nil {
		return nil
	}
	if s.conf().StateDir == "" || s.conf().Name == "" {
		return sessionError("invalid", "node sessions require durable node state and identity")
	}
	ctx, cancel := context.WithCancel(ctx)
	service := &SessionService{server: s, ctx: ctx, cancel: cancel, sessions: map[string]*ownedSession{}}
	if err := service.load(); err != nil {
		cancel()
		return err
	}
	s.sessions = service
	return nil
}

func (s *SessionService) Close() {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return
	}
	s.closed = true
	list := make([]*ownedSession, 0, len(s.sessions))
	for _, one := range s.sessions {
		list = append(list, one)
	}
	s.cancel()
	s.mu.Unlock()
	for _, one := range list {
		if one.host != nil {
			one.host.Close()
		}
	}
	s.wg.Wait()
	for _, one := range list {
		one.mu.Lock()
		next := one.copyLocked()
		if next.State.State != nodewire.SessionClosed {
			next.State.State = nodewire.SessionInterrupted
		}
		if one.host != nil {
			next.State.ProcessStopped = one.host.AllProcessesStopped()
		}
		if next.State.ProcessStopped {
			for id, command := range next.Commands {
				command.ProcessStopped = true
				next.Commands[id] = command
			}
		}
		// A commit that fails is latched in one.failure and answers the
		// next request for this session; there is no one else to tell.
		_ = one.commitLocked(next)
		one.mu.Unlock()
	}
}

func (s *SessionService) authorize(ctx context.Context, principal string, req nodewire.SessionRequest) error {
	a, b := req.Authority, req.Binding
	if !sessionNameValid(a.ClusterID) || !sessionNameValid(a.CoordinatorNodeID) || a.CoordinatorEpoch == 0 || a.WriterGeneration == 0 || !sessionNameValid(b.ProjectID) || !sessionNameValid(b.SessionID) || !sessionNameValid(b.TaskID) || !sessionNameValid(b.AttemptID) || b.NodeID != s.server.conf().Name || b.ExecutionEpoch == 0 {
		return sessionError("invalid", "complete committed coordinator and execution identities are required")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	verifier := s.server.conf().SessionAuthorizer
	if verifier == nil {
		return sessionError("forbidden", "node sessions are not authorized")
	}
	if err := verifier.AuthorizeNodeSession(ctx, principal, a, b, req.Action); err != nil {
		return sessionError("forbidden", err.Error())
	}
	return nil
}

func (s *SessionService) Do(ctx context.Context, principal string, req nodewire.SessionRequest) (nodewire.SessionState, error) {
	if err := s.authorize(ctx, principal, req); err != nil {
		return nodewire.SessionState{}, err
	}
	s.mu.Lock()
	closed := s.closed
	s.mu.Unlock()
	if closed {
		return nodewire.SessionState{}, sessionError("closed", "node session service is closed")
	}
	switch req.Action {
	case nodewire.SessionActionInspectOpen:
		return s.inspectOpen(ctx, req)
	case nodewire.SessionActionCancelOpen:
		return s.cancelOpen(ctx, req)
	case nodewire.SessionActionCapabilities:
		return s.probeCapabilities(ctx, req)
	}
	if req.Action == nodewire.SessionActionOpen && req.ID == "" {
		return s.open(ctx, principal, req)
	}
	if !sessionIDValid(req.ID) {
		return nodewire.SessionState{}, sessionError("invalid", "invalid node session identity")
	}
	s.mu.Lock()
	one := s.sessions[req.ID]
	s.mu.Unlock()
	if one == nil {
		return s.closedState(req)
	}
	one.mu.Lock()
	if err := one.admitLocked(req); err != nil {
		one.mu.Unlock()
		return nodewire.SessionState{}, err
	}
	one.mu.Unlock()
	switch req.Action {
	case nodewire.SessionActionOpen:
		return one.open(req)
	case nodewire.SessionActionAttach:
		return one.attach(req)
	case nodewire.SessionActionSettings:
		return one.settings(req)
	case nodewire.SessionActionPoll:
		return one.poll(ctx, principal, req)
	case nodewire.SessionActionPrompt:
		return one.prompt(req)
	case nodewire.SessionActionAnswer:
		return one.answer(req)
	case nodewire.SessionActionOption:
		return one.option(ctx, req)
	case nodewire.SessionActionCancel:
		return one.cancel(ctx, req)
	case nodewire.SessionActionAbort:
		return one.abort(ctx, req)
	case nodewire.SessionActionClose:
		return one.close(ctx, req)
	case nodewire.SessionActionStart:
		// Only open may issue the start authorization challenge.
		return nodewire.SessionState{}, sessionError("invalid", "unknown node session action")
	default:
		return nodewire.SessionState{}, sessionError("invalid", "unknown node session action")
	}
}

func (one *ownedSession) runningLocked() bool {
	c, ok := one.record.Commands[one.record.CurrentCommand]
	return ok && c.State.Active()
}

func (one *ownedSession) admitLocked(req nodewire.SessionRequest) error {
	if one.failure != nil {
		return one.failure
	}
	old := one.record.Authority
	a := req.Authority
	if a.ClusterID != one.record.ClusterID || a.CoordinatorEpoch < old.CoordinatorEpoch || a.WriterGeneration < old.WriterGeneration {
		return sessionError("forbidden", "stale coordinator activation")
	}
	if a.CoordinatorEpoch == old.CoordinatorEpoch && a.CoordinatorNodeID != old.CoordinatorNodeID {
		return sessionError("forbidden", "coordinator identity differs at the same epoch")
	}
	next := one.copyLocked()
	if req.Action == nodewire.SessionActionOpen && sessionConfigHash(req) != next.ConfigHash {
		return sessionError("conflict", "native session configuration changed")
	}
	if req.Binding != next.State.Binding {
		before, after := next.State.Binding, req.Binding
		if req.Action != nodewire.SessionActionOpen || one.runningLocked() || before.ProjectID != after.ProjectID || before.SessionID != after.SessionID || before.NodeID != after.NodeID || one.host == nil || next.State.State != nodewire.SessionIdle {
			return sessionError("conflict", "session belongs to another execution")
		}
		next.State.Binding = req.Binding
	}
	if next.Authority != a || next.State.Binding != one.record.State.Binding {
		next.Authority = a
		return one.commitLocked(next)
	}
	return nil
}
