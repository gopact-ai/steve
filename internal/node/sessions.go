package node

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/gopact-ai/acp"
	"github.com/gopact-ai/steve/internal/acphost"
	"github.com/gopact-ai/steve/internal/nodewire"
	"github.com/gopact-ai/steve/internal/permission"
	steveruntime "github.com/gopact-ai/steve/internal/runtime"
	"github.com/gopact-ai/steve/internal/view"
)

// SessionAuthorizer checks the authenticated connection principal and committed
// coordinator/execution authority. Request fields are claims, never credentials.
type SessionAuthorizer interface {
	AuthorizeNodeSession(context.Context, string, nodewire.SessionAuthority, nodewire.SessionBinding, string) error
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
}

type ownedSession struct {
	service *SessionService
	mu      sync.Mutex
	record  sessionRecord
	host    *acphost.Host
	changed chan struct{}
	waiters map[string]chan struct{}
	runDone chan struct{}
	failure error
}

type sessionRecord struct {
	Format         int                                `json:"format"`
	ClusterID      string                             `json:"cluster_id"`
	Authority      nodewire.SessionAuthority          `json:"authority"`
	OpenID         string                             `json:"open_id"`
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
		if next.State.State != "closed" {
			next.State.State = "interrupted"
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
	if req.Action == "capabilities" {
		spec, ok := s.server.conf().Harnesses[req.Harness]
		if !ok {
			return nodewire.SessionState{}, sessionError("unavailable", "harness is not registered on this node")
		}
		broker, _ := permission.New(permission.PolicyRead)
		host := acphost.New(acphost.Config{NoRestart: true, Command: spec.Command, Args: spec.Args, ProcessDir: s.server.processDir(spec), Env: steveruntime.ApplyEnv(spec.Env, req.Harness, s.server.conf().StateDir), Permission: broker})
		defer host.Close()
		supported, err := host.SupportsHTTPMCP(ctx)
		return nodewire.SessionState{Binding: req.Binding, Harness: req.Harness, SupportsHTTPMCP: supported}, err
	}
	if req.Action == "open" && req.ID == "" {
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
	case "open", "attach", "settings":
		return one.state(req.CommandID), nil
	case "poll":
		if req.WaitMS < 0 || req.WaitMS > 25000 {
			return nodewire.SessionState{}, sessionError("invalid", "poll wait exceeds limit")
		}
		one.mu.Lock()
		changed, sequence := one.changed, one.record.State.Sequence
		one.mu.Unlock()
		if sequence <= req.After && req.WaitMS > 0 {
			timer := time.NewTimer(time.Duration(req.WaitMS) * time.Millisecond)
			defer timer.Stop()
			select {
			case <-changed:
			case <-timer.C:
			case <-ctx.Done():
				return nodewire.SessionState{}, ctx.Err()
			case <-s.ctx.Done():
				return nodewire.SessionState{}, sessionError("closed", "node session service closed")
			}
		}
		if err := s.authorize(ctx, principal, req); err != nil {
			return nodewire.SessionState{}, err
		}
		return one.state(req.CommandID), nil
	case "prompt":
		return one.prompt(req)
	case "answer":
		return one.answer(req)
	case "option":
		one.mu.Lock()
		if err := one.admitLocked(req); err != nil {
			one.mu.Unlock()
			return nodewire.SessionState{}, err
		}
		host, id, generation := one.host, one.record.UpstreamID, one.record.Generation
		if host == nil || one.record.State.State != "idle" || one.runningLocked() {
			one.mu.Unlock()
			return nodewire.SessionState{}, sessionError("busy", "settings require an idle live session")
		}
		next := one.copyLocked()
		next.State.State = "configuring"
		if err := one.commitLocked(next); err != nil {
			one.mu.Unlock()
			return nodewire.SessionState{}, err
		}
		one.mu.Unlock()
		optionErr := host.SetOption(ctx, acp.SessionID(id), generation, acp.SessionConfigID(req.OptionID), req.OptionValue)
		one.mu.Lock()
		next = one.copyLocked()
		next.State.Settings = host.Settings(acp.SessionID(id))
		if next.State.State == "configuring" {
			next.State.State = "idle"
			if host.ProcessStopped(generation) {
				next.State.State = "interrupted"
				next.State.ProcessStopped = true
			}
		}
		saveErr := one.commitLocked(next)
		one.mu.Unlock()
		return one.state(req.CommandID), errors.Join(optionErr, saveErr)
	case "cancel", "abort", "close":
		return one.stop(ctx, req)
	default:
		return nodewire.SessionState{}, sessionError("invalid", "unknown node session action")
	}
}

func (one *ownedSession) runningLocked() bool {
	c, ok := one.record.Commands[one.record.CurrentCommand]
	return ok && (c.State == "accepted" || c.State == "running")
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
	if req.Action == "open" && sessionConfigHash(req) != next.ConfigHash {
		return sessionError("conflict", "native session configuration changed")
	}
	if req.Binding != next.State.Binding {
		before, after := next.State.Binding, req.Binding
		if req.Action != "open" || one.runningLocked() || before.ProjectID != after.ProjectID || before.SessionID != after.SessionID || before.NodeID != after.NodeID || one.host == nil || next.State.State != "idle" {
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

func (s *SessionService) open(ctx context.Context, principal string, req nodewire.SessionRequest) (nodewire.SessionState, error) {
	if !sessionNameValid(req.CommandID) || req.Workdir == "" || req.Harness == "" {
		return nodewire.SessionState{}, sessionError("invalid", "open needs an idempotent command, harness and workspace")
	}
	permissionName := req.Permission
	if permissionName == "" {
		permissionName = permission.PolicyRead
	}
	broker, err := permission.New(permissionName)
	if err != nil {
		return nodewire.SessionState{}, err
	}
	hash := sessionHash(struct {
		Binding                      nodewire.SessionBinding
		Harness, Workdir, Permission string
		Servers                      []acp.MCPServer
	}{req.Binding, req.Harness, req.Workdir, permissionName, req.MCPServers})
	id := "ns_" + sessionHash([]string{req.Authority.ClusterID, req.Binding.NodeID, req.Binding.AttemptID, req.CommandID, req.Harness})
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nodewire.SessionState{}, sessionError("closed", "node session service is closed")
	}
	if one := s.sessions[id]; one != nil {
		s.mu.Unlock()
		one.mu.Lock()
		defer one.mu.Unlock()
		if one.record.OpenHash != hash {
			return nodewire.SessionState{}, sessionError("conflict", "open command already used with different input")
		}
		if err := one.admitLocked(req); err != nil {
			return nodewire.SessionState{}, err
		}
		return one.stateLocked(""), nil
	}
	if closed, exists, err := s.readRecord(id); err != nil {
		s.mu.Unlock()
		return nodewire.SessionState{}, err
	} else if exists {
		s.mu.Unlock()
		if closed.OpenHash != hash {
			return nodewire.SessionState{}, sessionError("conflict", "open command already used with different input")
		}
		if closed.State.State != "closed" {
			return nodewire.SessionState{}, sessionError("unavailable", "native session requires reconciliation")
		}
		return closed.State, nil
	}
	if len(s.sessions) >= 1024 {
		s.mu.Unlock()
		return nodewire.SessionState{}, sessionError("unavailable", "node session retention limit reached")
	}
	spec, ok := s.server.conf().Harnesses[req.Harness]
	if !ok || spec.Command == "" {
		s.mu.Unlock()
		return nodewire.SessionState{}, sessionError("unavailable", "harness is not registered on this node")
	}
	// Observing a previously accepted open is permitted during reconciliation;
	// creating a native process requires a fresh execution admission as well.
	start := req
	start.Action = "start"
	if err := s.authorize(ctx, principal, start); err != nil {
		s.mu.Unlock()
		return nodewire.SessionState{}, err
	}
	host := acphost.New(acphost.Config{NoRestart: true, Command: spec.Command, Args: spec.Args, ProcessDir: s.server.processDir(spec), Env: steveruntime.ApplyEnv(spec.Env, req.Harness, s.server.conf().StateDir), Permission: broker})
	one := &ownedSession{service: s, host: host, changed: make(chan struct{}), waiters: map[string]chan struct{}{}}
	one.record = sessionRecord{Format: 1, ClusterID: req.Authority.ClusterID, Authority: req.Authority, OpenID: req.CommandID, OpenHash: hash, ConfigHash: sessionConfigHash(req), State: nodewire.SessionState{ID: id, Binding: req.Binding, Harness: req.Harness, State: "opening", Questions: []nodewire.SessionQuestion{}}, CommandHashes: map[string]string{}, Commands: map[string]nodewire.SessionCommand{}}
	if err := one.commitLocked(one.record); err != nil {
		s.mu.Unlock()
		host.Close()
		return nodewire.SessionState{}, err
	}
	s.sessions[id] = one
	s.wg.Add(1)
	s.mu.Unlock()
	defer s.wg.Done()
	// The durable open belongs to the node. A lost caller response must not
	// kill a successfully created agent or make an identical retry start twice.
	openCtx, cancel := context.WithTimeout(s.ctx, 60*time.Second)
	defer cancel()
	native, generation, openErr := host.OpenSession(openCtx, "", acphost.SessionConfig{Workdir: req.Workdir, MCPServers: req.MCPServers})
	httpMCP := false
	if openErr == nil {
		httpMCP, _ = host.SupportsHTTPMCP(openCtx)
	}
	one.mu.Lock()
	next := one.copyLocked()
	next.UpstreamID, next.Generation = string(native), generation
	if openErr != nil {
		if next.State.State != "closing" && next.State.State != "closed" {
			next.State.State = "interrupted"
		}
	} else {
		if next.State.State != "closing" && next.State.State != "closed" {
			next.State.State = "idle"
		}
		next.State.Settings = host.Settings(native)
		option, choices := host.ModelChoices(native)
		next.State.ModelOption, next.State.ModelChoices = string(option), choices
		next.State.SupportsHTTPMCP = httpMCP
	}
	saveErr := one.commitLocked(next)
	one.mu.Unlock()
	if openErr != nil {
		host.Close()
		return one.state(""), openErr
	}
	if saveErr != nil {
		host.Close()
		return nodewire.SessionState{}, saveErr
	}
	return one.state(""), nil
}

func (one *ownedSession) prompt(req nodewire.SessionRequest) (nodewire.SessionState, error) {
	one.mu.Lock()
	defer one.mu.Unlock()
	if err := one.admitLocked(req); err != nil {
		return nodewire.SessionState{}, err
	}
	if !sessionNameValid(req.CommandID) || req.InputSequence == 0 || len(req.Text) > 1<<20 || len(req.Media) > 16 {
		return nodewire.SessionState{}, sessionError("invalid", "prompt identity or size is invalid")
	}
	hash := sessionHash(struct {
		Binding  nodewire.SessionBinding
		Sequence uint64
		Text     string
		Media    []nodewire.SessionMedia
	}{req.Binding, req.InputSequence, req.Text, req.Media})
	if old, ok := one.record.CommandHashes[req.CommandID]; ok {
		if old != hash {
			return nodewire.SessionState{}, sessionError("conflict", "input command already used with different prompt")
		}
		return one.stateLocked(req.CommandID), nil
	}
	if one.host == nil || one.record.State.State != "idle" || one.runningLocked() {
		return nodewire.SessionState{}, sessionError("busy", "original execution is running or cannot be reattached")
	}
	if req.InputSequence != one.record.State.InputAccepted+1 || len(one.record.Commands) >= 512 {
		return nodewire.SessionState{}, sessionError("conflict", "input sequence is stale or session retention limit reached")
	}
	next := one.copyLocked()
	next.CommandHashes[req.CommandID] = hash
	next.Commands[req.CommandID] = nodewire.SessionCommand{ID: req.CommandID, InputSequence: req.InputSequence, State: "accepted", DispatchState: "not-dispatched"}
	next.CurrentCommand = req.CommandID
	next.State.State = "running"
	next.State.InputAccepted = req.InputSequence
	next.State.Progress = view.Progress{}
	if err := one.commitLocked(next); err != nil {
		return nodewire.SessionState{}, err
	}
	one.service.mu.Lock()
	if one.service.closed {
		one.service.mu.Unlock()
		return nodewire.SessionState{}, sessionError("closed", "node session service is closed")
	}
	one.service.wg.Add(1)
	one.service.mu.Unlock()
	one.runDone = make(chan struct{})
	done := one.runDone
	go func() { defer one.service.wg.Done(); defer close(done); one.run(req) }()
	return one.stateLocked(req.CommandID), nil
}

func (one *ownedSession) run(req nodewire.SessionRequest) {
	one.mu.Lock()
	next := one.copyLocked()
	command := next.Commands[req.CommandID]
	if command.CancelRequested || command.State == "cancelled" || next.State.State == "closing" || next.State.State == "closed" {
		if command.State == "accepted" {
			command.State = "cancelled"
			command.Settled = true
			next.Commands[req.CommandID] = command
			_ = one.commitLocked(next)
		}
		one.mu.Unlock()
		return
	}
	if one.service.ctx.Err() != nil {
		command.State = "uncertain"
		command.Error = "node service stopped before dispatch"
		next.Commands[req.CommandID] = command
		next.State.State = "interrupted"
		_ = one.commitLocked(next)
		one.mu.Unlock()
		return
	}
	command.State = "running"
	// A crash from this durable boundary onward cannot prove that native
	// Prompt was never invoked, including failure before its RPC response.
	command.DispatchState = "dispatched"
	next.Commands[req.CommandID] = command
	if err := one.commitLocked(next); err != nil {
		one.mu.Unlock()
		return
	}
	native, generation, host := acp.SessionID(next.UpstreamID), next.Generation, one.host
	one.mu.Unlock()
	media := make([]acphost.Image, 0, len(req.Media))
	for _, item := range req.Media {
		media = append(media, acphost.Image{MIME: item.MIME, Data: item.Data, URI: item.URI})
	}
	output, activity, runErr := host.PromptTurn(one.service.ctx, native, generation, req.Text, media,
		func(ctx context.Context, ask permission.Ask) (acp.RequestPermissionOutcome, error) {
			return one.askPermission(ctx, req.CommandID, ask)
		},
		func(ctx context.Context, q view.Question) (view.Answer, error) {
			return one.askUser(ctx, req.CommandID, q)
		},
		func(progress view.Progress) {
			one.mu.Lock()
			next := one.copyLocked()
			next.State.Progress = progress
			next.State.Settings = progress.Settings
			err := one.commitLocked(next)
			one.mu.Unlock()
			if err != nil {
				go host.Abort(generation)
			}
		},
	)
	one.mu.Lock()
	defer one.mu.Unlock()
	next = one.copyLocked()
	command = next.Commands[req.CommandID]
	command.Output, command.Activity = output, activity
	command.Settled = acphost.PromptSettled(runErr)
	command.ProcessStopped = host.ProcessStopped(generation)
	next.State.ProcessStopped = command.ProcessStopped
	if runErr != nil {
		command.Error = runErr.Error()
	}
	switch {
	case errors.Is(runErr, acphost.ErrTurnCanceled):
		command.State = "cancelled"
	case command.Settled:
		command.State = "completed"
	default:
		command.State = "uncertain"
	}
	next.Commands[req.CommandID] = command
	if next.State.State != "closing" && next.State.State != "closed" {
		next.State.State = "idle"
		if !command.Settled {
			next.State.State = "interrupted"
		}
	}
	for i := range next.State.Questions {
		if next.State.Questions[i].CommandID == req.CommandID && next.State.Questions[i].State == "pending" {
			next.State.Questions[i].State = "interrupted"
		}
	}
	_ = one.commitLocked(next)
}

func (one *ownedSession) stop(ctx context.Context, req nodewire.SessionRequest) (nodewire.SessionState, error) {
	one.mu.Lock()
	if err := one.admitLocked(req); err != nil {
		one.mu.Unlock()
		return nodewire.SessionState{}, err
	}
	host, id, generation := one.host, one.record.UpstreamID, one.record.Generation
	runDone := one.runDone
	running := one.runningLocked()
	if req.CommandID == "" {
		req.CommandID = one.record.CurrentCommand
	}
	if req.CommandID != one.record.CurrentCommand && running {
		one.mu.Unlock()
		return nodewire.SessionState{}, sessionError("conflict", "cancellation targets another command")
	}
	if req.Action == "cancel" && running {
		next := one.copyLocked()
		command := next.Commands[next.CurrentCommand]
		command.CancelRequested = true
		if command.State == "accepted" {
			command.State = "cancelled"
			command.Settled = true
			next.State.State = "idle"
			running = false
		}
		next.Commands[next.CurrentCommand] = command
		if err := one.commitLocked(next); err != nil {
			one.mu.Unlock()
			return nodewire.SessionState{}, err
		}
	}
	if host == nil {
		one.mu.Unlock()
		return one.state(req.CommandID), sessionError("unavailable", "original native process cannot be contacted")
	}
	if req.Action == "close" && (running || one.record.State.State == "configuring" || one.record.State.State == "opening") {
		one.mu.Unlock()
		return one.state(req.CommandID), sessionError("busy", "close cannot terminate a running prompt")
	}
	if req.Action == "close" || req.Action == "abort" {
		next := one.copyLocked()
		next.State.State = "closing"
		if err := one.commitLocked(next); err != nil {
			one.mu.Unlock()
			return one.state(req.CommandID), err
		}
	}
	one.mu.Unlock()
	if req.Action != "cancel" {
		host.Close()
		if runDone != nil {
			select {
			case <-runDone:
			case <-ctx.Done():
				return one.state(req.CommandID), fmt.Errorf("%w: %w", acphost.ErrStopUnconfirmed, ctx.Err())
			}
		}
	}
	if req.Action == "cancel" {
		// A cancel can arrive after acceptance but before the agent has consumed
		// session/prompt. Retry the notification while this exact command remains
		// active; never let a late retry cancel the session's next command.
		ticker := time.NewTicker(25 * time.Millisecond)
		defer ticker.Stop()
		for {
			one.mu.Lock()
			active := one.runningLocked() && one.record.CurrentCommand == req.CommandID
			changed := one.changed
			if active {
				err := host.Cancel(ctx, acp.SessionID(id), generation)
				if err != nil {
					one.mu.Unlock()
					return one.state(req.CommandID), err
				}
			}
			one.mu.Unlock()
			if !active {
				break
			}
			select {
			case <-changed:
			case <-ticker.C:
			case <-ctx.Done():
				return one.state(req.CommandID), fmt.Errorf("%w: %w", acphost.ErrStopUnconfirmed, ctx.Err())
			}
		}
	}

	one.mu.Lock()
	next := one.copyLocked()
	next.State.ProcessStopped = host.ProcessStopped(generation)
	if req.Action == "close" || req.Action == "abort" {
		next.State.ProcessStopped = host.AllProcessesStopped()
	}
	if next.State.ProcessStopped {
		for id, command := range next.Commands {
			command.ProcessStopped = true
			next.Commands[id] = command
		}
	}
	command, hasCommand := next.Commands[req.CommandID]
	confirmed := next.State.ProcessStopped || (req.Action == "cancel" && (!hasCommand || command.Settled))
	if req.Action == "close" || req.Action == "abort" {
		if confirmed {
			next.State.State = "closed"
		} else {
			next.State.State = "interrupted"
		}
	}
	err := one.commitLocked(next)
	one.mu.Unlock()
	if err == nil && !confirmed {
		err = acphost.ErrStopUnconfirmed
	}
	if err == nil && next.State.State == "closed" {
		one.service.mu.Lock()
		if one.service.sessions[next.State.ID] == one {
			delete(one.service.sessions, next.State.ID)
		}
		one.service.mu.Unlock()
	}
	return one.state(req.CommandID), err
}

func (s *SessionService) directory() string {
	return filepath.Join(s.server.conf().StateDir, "node-sessions")
}

func sessionConfigHash(req nodewire.SessionRequest) string {
	policy := req.Permission
	if policy == "" {
		policy = permission.PolicyRead
	}
	return sessionHash(struct {
		Harness, Workdir, Permission string
		Servers                      []acp.MCPServer
	}{req.Harness, req.Workdir, policy, req.MCPServers})
}

func (s *SessionService) processesStopped() bool {
	s.mu.Lock()
	if s.unverifiedProcesses {
		s.mu.Unlock()
		return false
	}
	list := make([]*ownedSession, 0, len(s.sessions))
	for _, one := range s.sessions {
		list = append(list, one)
	}
	s.mu.Unlock()
	for _, one := range list {
		one.mu.Lock()
		host, generation, known := one.host, one.record.Generation, one.record.State.ProcessStopped
		one.mu.Unlock()
		if host != nil {
			if !host.ProcessStopped(generation) {
				return false
			}
		} else if !known {
			return false
		}
	}
	return true
}
