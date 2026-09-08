package node

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"maps"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/gopact-ai/steve/internal/adapter"
	"github.com/gopact-ai/steve/internal/agenttools"
	"github.com/gopact-ai/steve/internal/nodewire"
	steveruntime "github.com/gopact-ai/steve/internal/runtime"
)

type agentAdapterInstaller interface {
	Ensure(context.Context, string) (adapter.Installed, error)
}

type enrollDependencies struct {
	discover  func() []agenttools.Candidate
	installer agentAdapterInstaller
	prepare   func(string, []string) error
}

func (s *Server) agentEnrollmentDependencies() enrollDependencies {
	return enrollDependencies{discover: func() []agenttools.Candidate { return agenttools.Discover(agenttools.Options{}) }, installer: &adapter.Installer{Dir: filepath.Join(s.conf().StateDir, "adapters")}, prepare: steveruntime.PrepareSelected}
}

func (s *Server) discoverAgents() agenttools.Discovery {
	return s.discoverAgentsWith(s.agentEnrollmentDependencies().discover)
}

// LocalAgentDiscovery serves an already authorized in-process desktop caller
// without establishing a second primary nodewire connection.
func (s *Server) LocalAgentDiscovery() agenttools.Discovery { return s.discoverAgents() }

// LocalEnrollAgent shares the same discovery, pinned-installation and CAS
// path as nodewire. The owning desktop API authenticates its caller.
func (s *Server) LocalEnrollAgent(ctx context.Context, request agenttools.InstallRequest) (agenttools.Enrollment, error) {
	return s.enrollAgentWith(ctx, request, s.agentEnrollmentDependencies())
}

func (s *Server) discoverAgentsWith(discover func() []agenttools.Candidate) agenttools.Discovery {
	s.settingsMu.Lock()
	defer s.settingsMu.Unlock()
	out := agenttools.Discovery{Revision: s.settings().Revision, Agents: discover()}
	cfg := s.conf()
	for i := range out.Agents {
		_, out.Agents[i].Configured = cfg.Harnesses[out.Agents[i].Harness]
	}
	return out
}

func (s *Server) checkEnrollmentRevision(expected string) error {
	if expected == "" || expected != s.settings().Revision {
		return nodewire.ErrSettingsRevisionConflict
	}
	if s.conf().Source == "" {
		return errors.New("node configuration has no durable source; restart with a node configuration file")
	}
	current, err := nodeSettingsFileRevision(s.conf().Source)
	if err != nil {
		return fmt.Errorf("read node configuration revision: %w", err)
	}
	if current != s.settingsFileRevision {
		return fmt.Errorf("%w: node configuration was edited externally; restart before enrolling", nodewire.ErrSettingsRevisionConflict)
	}
	return nil
}

// enrollAgentWith prepares only a selected built-in tool. Slow adapter work
// happens before the final settings CAS; a concurrent edit is never overwritten.
func (s *Server) enrollAgentWith(ctx context.Context, req agenttools.InstallRequest, deps enrollDependencies) (agenttools.Enrollment, error) {
	s.enrollmentMu.Lock()
	defer s.enrollmentMu.Unlock()
	canonical, ok := agenttools.Lookup(req.CandidateID)
	if !ok {
		return agenttools.Enrollment{}, agenttools.ErrUnsupported
	}
	if err := ctx.Err(); err != nil {
		return agenttools.Enrollment{}, err
	}
	s.settingsMu.Lock()
	err := s.checkEnrollmentRevision(req.ExpectedRevision)
	before := s.conf()
	s.settingsMu.Unlock()
	if err != nil {
		return agenttools.Enrollment{}, err
	}
	var selected agenttools.Candidate
	for _, candidate := range deps.discover() {
		if candidate.ID == req.CandidateID {
			selected = candidate
			break
		}
	}
	declared, err := agenttools.Registration(selected)
	if err != nil {
		return agenttools.Enrollment{}, err
	}
	existing, exists := before.Harnesses[canonical.Harness]
	if exists {
		if !matchesEnrolledTool(existing, declared) {
			return agenttools.Enrollment{}, agenttools.ErrConfigured
		}
	}
	h := HarnessSpec{Adapter: declared.Adapter, Command: declared.Command, Args: declared.Args}
	if declared.Adapter != "" {
		installed, err := deps.installer.Ensure(ctx, declared.Adapter)
		if err != nil {
			return agenttools.Enrollment{}, fmt.Errorf("install selected adapter: %w", err)
		}
		pin, ok := adapter.Catalog[declared.Adapter]
		if !ok || installed.Name != declared.Adapter || installed.Package != pin.Package || installed.Version != pin.Version || !agenttools.Executable(installed.Command) {
			return agenttools.Enrollment{}, errors.New("installed adapter does not match the selected pinned version")
		}
		h.Command = installed.Command
		if exists && existing.Command != installed.Command {
			return agenttools.Enrollment{}, agenttools.ErrConfigured
		}
	}
	if exists {
		s.settingsMu.Lock()
		defer s.settingsMu.Unlock()
		if err := s.checkEnrollmentRevision(req.ExpectedRevision); err != nil {
			return agenttools.Enrollment{}, err
		}
		if err := ctx.Err(); err != nil {
			return agenttools.Enrollment{}, err
		}
		return agenttools.Enrollment{CandidateID: req.CandidateID, Harness: canonical.Harness, Revision: s.settings().Revision}, nil
	}
	if err := ctx.Err(); err != nil {
		return agenttools.Enrollment{}, err
	}
	// Executable discovery is only a candidate. Check again immediately before
	// publishing, after an adapter download may have taken several minutes.
	var refreshed agenttools.Candidate
	for _, candidate := range deps.discover() {
		if candidate.ID == selected.ID {
			refreshed = candidate
			break
		}
	}
	if refreshed.Executable != selected.Executable {
		return agenttools.Enrollment{}, fmt.Errorf("%w: selected executable changed; refresh discovery", agenttools.ErrUnavailable)
	}
	if _, err := agenttools.Registration(refreshed); err != nil {
		return agenttools.Enrollment{}, err
	}
	s.settingsMu.Lock()
	defer s.settingsMu.Unlock()
	if err := s.checkEnrollmentRevision(req.ExpectedRevision); err != nil {
		return agenttools.Enrollment{}, err
	}
	if err := ctx.Err(); err != nil {
		return agenttools.Enrollment{}, err
	}
	next := s.conf()
	next.Harnesses = maps.Clone(next.Harnesses)
	if next.Harnesses == nil {
		next.Harnesses = map[string]HarnessSpec{}
	}
	if _, exists := next.Harnesses[canonical.Harness]; exists {
		return agenttools.Enrollment{}, nodewire.ErrSettingsRevisionConflict
	}
	// Local runtime and skill publication share the final settings boundary.
	// A concurrent settings/skill update cannot expose a half-prepared runtime.
	if err := deps.prepare(next.StateDir, []string{canonical.Harness}); err != nil {
		return agenttools.Enrollment{}, fmt.Errorf("prepare selected agent runtime: %w", err)
	}
	if hash := s.currentSkills(); hash != "" {
		if err := s.materializeSkillsFor(hash, []string{canonical.Harness}); err != nil {
			return agenttools.Enrollment{}, fmt.Errorf("prepare selected agent skills: %w", err)
		}
	}
	if err := ctx.Err(); err != nil {
		return agenttools.Enrollment{}, err
	}
	if err := s.checkEnrollmentRevision(req.ExpectedRevision); err != nil {
		return agenttools.Enrollment{}, err
	}
	next.Harnesses[canonical.Harness] = h
	writeErr := writeConfig(next)
	if writeErr != nil && !settingsCommitted(writeErr) {
		return agenttools.Enrollment{}, writeErr
	}
	s.settingsFileRevision, _ = nodeSettingsFileRevision(next.Source)
	s.cfg.Store(&next)
	s.launch.Wake()
	log.Printf("steve-node: selected tool registered candidate=%s harness=%s adapter=%s", req.CandidateID, canonical.Harness, h.Adapter)
	return agenttools.Enrollment{CandidateID: req.CandidateID, Harness: canonical.Harness, Revision: s.settings().Revision}, writeErr
}

func matchesEnrolledTool(existing HarnessSpec, declared agenttools.Declaration) bool {
	if existing.Adapter != declared.Adapter {
		return false
	}
	if declared.Adapter != "" {
		return len(existing.Args) == 0
	}
	return existing.Command == declared.Command && slices.Equal(existing.Args, declared.Args)
}

type agentToolsReply struct {
	Discovery  *agenttools.Discovery  `json:"discovery,omitempty"`
	Enrollment *agenttools.Enrollment `json:"enrollment,omitempty"`
	ErrorCode  string                 `json:"error_code,omitempty"`
	Error      string                 `json:"error,omitempty"`
}

func (s *Server) configureAgentTools(stream *nodewire.Stream) {
	out := agentToolsReply{}
	var err error
	base := s.ctx
	if base == nil {
		base = context.Background()
	}
	ctx, cancel := context.WithTimeout(base, 5*time.Minute)
	defer cancel()
	stop := context.AfterFunc(ctx, func() { _ = stream.Close() })
	defer stop()
	go func() {
		select {
		case <-stream.Done():
			cancel()
		case <-ctx.Done():
		}
	}()
	switch stream.Request().Command {
	case "discover-agents":
		discovery := s.discoverAgents()
		out.Discovery = &discovery
	case "enroll-agent":
		var request agenttools.InstallRequest
		decoder := json.NewDecoder(io.LimitReader(stream, 16<<10))
		decoder.DisallowUnknownFields()
		if err = decoder.Decode(&request); err != nil {
			break
		}
		var result agenttools.Enrollment
		result, err = s.enrollAgentWith(ctx, request, s.agentEnrollmentDependencies())
		if result.Harness != "" {
			out.Enrollment = &result
		}
	}
	if err != nil {
		out.Error = err.Error()
		switch {
		case errors.Is(err, nodewire.ErrSettingsRevisionConflict):
			out.ErrorCode = nodewire.SettingsRevisionConflictCode
		case errors.Is(err, agenttools.ErrUnsupported):
			out.ErrorCode = "agent_unsupported"
		case errors.Is(err, agenttools.ErrUnavailable):
			out.ErrorCode = "agent_unavailable"
		case errors.Is(err, agenttools.ErrConfigured):
			out.ErrorCode = "agent_configured"
		case settingsCommitted(err):
			out.ErrorCode = "settings_committed"
		}
	}
	if err := json.NewEncoder(stream).Encode(out); err != nil {
		log.Printf("steve-node: agent tools reply: %v", err)
	}
}

func agentToolsError(reply agentToolsReply) error {
	if reply.Error == "" {
		return nil
	}
	var cause error
	switch strings.TrimSpace(reply.ErrorCode) {
	case nodewire.SettingsRevisionConflictCode:
		cause = nodewire.ErrSettingsRevisionConflict
	case "agent_unsupported":
		cause = agenttools.ErrUnsupported
	case "agent_unavailable":
		cause = agenttools.ErrUnavailable
	case "agent_configured":
		cause = agenttools.ErrConfigured
	case "settings_committed":
		return &settingsCommittedError{err: errors.New(reply.Error)}
	default:
		return errors.New(reply.Error)
	}
	return fmt.Errorf("%w: %s", cause, reply.Error)
}
