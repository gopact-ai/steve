package node

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"time"

	"github.com/gopact-ai/acp"
	"github.com/gopact-ai/steve/internal/acphost"
	"github.com/gopact-ai/steve/internal/nodewire"
	"github.com/gopact-ai/steve/internal/permission"
	"github.com/gopact-ai/steve/internal/view"
)

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
	id := nodewire.SessionOpenID(req.Authority.ClusterID, req.Binding.NodeID, req.Binding.AttemptID, req.CommandID, req.Harness)
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
		if closed.OpenCancelled {
			return nodewire.SessionState{}, sessionError("cancelled", "the original native open was durably cancelled before creation")
		}
		if closed.OpenHash != hash {
			return nodewire.SessionState{}, sessionError("conflict", "open command already used with different input")
		}
		if closed.State.State != nodewire.SessionClosed {
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
	start.Action = nodewire.SessionActionStart
	if err := s.authorize(ctx, principal, start); err != nil {
		s.mu.Unlock()
		return nodewire.SessionState{}, err
	}
	hostCfg := s.hostConfig(req.Harness, spec, broker)
	host := acphost.New(hostCfg)
	one := &ownedSession{service: s, host: host, changed: make(chan struct{}), waiters: map[string]chan struct{}{}}
	one.record = sessionRecord{Format: 1, ClusterID: req.Authority.ClusterID, Authority: req.Authority, OpenID: req.CommandID, OpenHash: hash, ConfigHash: sessionConfigHash(req), State: nodewire.SessionState{ID: id, Binding: req.Binding, Harness: req.Harness, State: nodewire.SessionOpening, Questions: []nodewire.SessionQuestion{}}, CommandHashes: map[string]string{}, Commands: map[string]nodewire.SessionCommand{}}
	if err := one.commitLocked(one.record); err != nil {
		s.mu.Unlock()
		host.Close()
		return nodewire.SessionState{}, err
	}
	openCtx, cancel := context.WithTimeout(s.ctx, 60*time.Second)
	one.openCancel, one.openDone = cancel, make(chan struct{})
	defer cancel()
	defer close(one.openDone)
	s.sessions[id] = one
	s.wg.Add(1)
	s.mu.Unlock()
	defer s.wg.Done()
	return one.openNative(openCtx, req, hostCfg)
}

func (one *ownedSession) openNative(openCtx context.Context, req nodewire.SessionRequest, hostCfg acphost.Config) (nodewire.SessionState, error) {
	s, host := one.service, one.host
	// The durable open belongs to the node. A lost caller response must not
	// kill a successfully created agent or make an identical retry start twice.
	native, generation, openErr := host.OpenSession(openCtx, "", acphost.SessionConfig{Workdir: req.Workdir, MCPServers: req.MCPServers})
	httpMCP := false
	if openErr == nil {
		// The agent that actually runs is the authority on what it accepts:
		// its answer replaces whatever a probe said.
		if supported, err := host.SupportsHTTPMCP(openCtx); err == nil {
			httpMCP = supported
			s.rememberCapabilities(req.Harness, capabilityKey(hostCfg), supported)
		}
	} else {
		s.forgetCapabilities(req.Harness)
	}
	one.mu.Lock()
	next := one.copyLocked()
	next.UpstreamID, next.Generation = string(native), generation
	if openErr != nil {
		if next.State.State != nodewire.SessionClosing && next.State.State != nodewire.SessionClosed {
			next.State.State = nodewire.SessionInterrupted
		}
	} else {
		if next.State.State != nodewire.SessionClosing && next.State.State != nodewire.SessionClosed {
			next.State.State = nodewire.SessionIdle
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
	if one.host == nil || one.record.State.State != nodewire.SessionIdle || one.runningLocked() {
		return nodewire.SessionState{}, sessionError("busy", "original execution is running or cannot be reattached")
	}
	if req.InputSequence != one.record.State.InputAccepted+1 || len(one.record.Commands) >= 512 {
		return nodewire.SessionState{}, sessionError("conflict", "input sequence is stale or session retention limit reached")
	}
	next := one.copyLocked()
	next.CommandHashes[req.CommandID] = hash
	next.Commands[req.CommandID] = nodewire.SessionCommand{ID: req.CommandID, InputSequence: req.InputSequence, State: nodewire.SessionCommandAccepted, DispatchState: "not-dispatched"}
	next.CurrentCommand = req.CommandID
	next.State.State = nodewire.SessionRunning
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
	if command.CancelRequested || command.State == nodewire.SessionCommandCancelled || next.State.State == nodewire.SessionClosing || next.State.State == nodewire.SessionClosed {
		if command.State == nodewire.SessionCommandAccepted {
			command.State = nodewire.SessionCommandCancelled
			command.Settled = true
			next.Commands[req.CommandID] = command
			// A failed commit is latched in one.failure for the next request.
			_ = one.commitLocked(next)
		}
		one.mu.Unlock()
		return
	}
	if one.service.ctx.Err() != nil {
		command.State = nodewire.SessionCommandUncertain
		command.Error = "node service stopped before dispatch"
		next.Commands[req.CommandID] = command
		next.State.State = nodewire.SessionInterrupted
		// A failed commit is latched in one.failure for the next request.
		_ = one.commitLocked(next)
		one.mu.Unlock()
		return
	}
	command.State = nodewire.SessionCommandRunning
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
			if err := one.updateProgress(req.CommandID, progress); err != nil {
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
		command.State = nodewire.SessionCommandCancelled
	case command.Settled:
		command.State = nodewire.SessionCommandCompleted
	default:
		command.State = nodewire.SessionCommandUncertain
	}
	next.Commands[req.CommandID] = command
	if next.State.State != nodewire.SessionClosing && next.State.State != nodewire.SessionClosed {
		next.State.State = nodewire.SessionIdle
		if !command.Settled {
			next.State.State = nodewire.SessionInterrupted
		}
	}
	for i := range next.State.Questions {
		if next.State.Questions[i].CommandID == req.CommandID && next.State.Questions[i].State == "pending" {
			next.State.Questions[i].State = "interrupted"
		}
	}
	// A failed commit is latched in one.failure for the next request.
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
	openCancel, openDone := one.openCancel, one.openDone
	running := one.runningLocked()
	if req.CommandID == "" {
		req.CommandID = one.record.CurrentCommand
	}
	if req.CommandID != one.record.CurrentCommand && running {
		one.mu.Unlock()
		return nodewire.SessionState{}, sessionError("conflict", "cancellation targets another command")
	}
	if req.Action == nodewire.SessionActionCancel && running {
		next := one.copyLocked()
		command := next.Commands[next.CurrentCommand]
		command.CancelRequested = true
		if command.State == nodewire.SessionCommandAccepted {
			command.State = nodewire.SessionCommandCancelled
			command.Settled = true
			next.State.State = nodewire.SessionIdle
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
	if req.Action == nodewire.SessionActionClose && (running || one.record.State.State == nodewire.SessionConfiguring || one.record.State.State == nodewire.SessionOpening) {
		one.mu.Unlock()
		return one.state(req.CommandID), sessionError("busy", "close cannot terminate a running prompt")
	}
	if req.Action == nodewire.SessionActionClose || req.Action == nodewire.SessionActionAbort {
		next := one.copyLocked()
		next.State.State = nodewire.SessionClosing
		if err := one.commitLocked(next); err != nil {
			one.mu.Unlock()
			return one.state(req.CommandID), err
		}
	}
	one.mu.Unlock()
	if req.Action != nodewire.SessionActionCancel {
		if openCancel != nil {
			openCancel()
		}
		host.Close()
		if openDone != nil {
			select {
			case <-openDone:
			case <-ctx.Done():
				return one.state(req.CommandID), fmt.Errorf("%w: %w", acphost.ErrStopUnconfirmed, ctx.Err())
			}
		}
		if runDone != nil {
			select {
			case <-runDone:
			case <-ctx.Done():
				return one.state(req.CommandID), fmt.Errorf("%w: %w", acphost.ErrStopUnconfirmed, ctx.Err())
			}
		}
	}
	if req.Action == nodewire.SessionActionCancel {
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

	return one.settleStop(req, host, generation)
}

func (one *ownedSession) settleStop(req nodewire.SessionRequest, host *acphost.Host, generation uint64) (nodewire.SessionState, error) {
	one.mu.Lock()
	next := one.copyLocked()
	next.State.ProcessStopped = host.ProcessStopped(generation)
	if req.Action == nodewire.SessionActionClose || req.Action == nodewire.SessionActionAbort {
		next.State.ProcessStopped = host.AllProcessesStopped()
	}
	if next.State.ProcessStopped {
		for id, command := range next.Commands {
			command.ProcessStopped = true
			next.Commands[id] = command
		}
	}
	command, hasCommand := next.Commands[req.CommandID]
	confirmed := next.State.ProcessStopped || (req.Action == nodewire.SessionActionCancel && (!hasCommand || command.Settled))
	if req.Action == nodewire.SessionActionClose || req.Action == nodewire.SessionActionAbort {
		if confirmed {
			next.State.State = nodewire.SessionClosed
		} else {
			next.State.State = nodewire.SessionInterrupted
		}
	}
	err := one.commitLocked(next)
	one.mu.Unlock()
	if err == nil && !confirmed {
		err = acphost.ErrStopUnconfirmed
	}
	if err == nil && next.State.State == nodewire.SessionClosed {
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
