package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"regexp"
	"sync"
	"time"

	"github.com/gopact-ai/steve/internal/config"
	"github.com/gopact-ai/steve/internal/consoleapi"
	"github.com/gopact-ai/steve/internal/execution"
	"github.com/gopact-ai/steve/internal/ledger"
	"github.com/gopact-ai/steve/internal/nodewire"
	"github.com/gopact-ai/steve/internal/processrestart"
)

var errHubRestart = errors.New("hub restart requested")

type hubRestartExit struct{ service *hubServices }

func (e *hubRestartExit) Error() string { return errHubRestart.Error() }
func (e *hubRestartExit) Unwrap() error { return errHubRestart }

var restartID = regexp.MustCompile(`^[A-Za-z0-9_-]{1,128}$`)

const restartWatchTimeout = time.Minute

type hubRestartHistory struct {
	Incarnation int64                                  `json:"incarnation"`
	Latest      string                                 `json:"latest,omitempty"`
	Operations  map[string]consoleapi.RestartOperation `json:"operations"`
}
type hubServices struct {
	mu         sync.Mutex
	actionMu   sync.Mutex
	admin      *fleetAdmin
	executions *execution.Registry
	sealWrites func() (func(), error)
	stop       context.CancelFunc
	doc        ledger.Doc
	history    hubRestartHistory
	pending    string
	dispatched bool
	release    func()
	boot       *config.Config
}

func newHubServices(admin *fleetAdmin, executions *execution.Registry, seal func() (func(), error), stop context.CancelFunc, doc ledger.Doc) (*hubServices, error) {
	s := &hubServices{admin: admin, executions: executions, sealWrites: seal, stop: stop, doc: doc}
	if admin.cfg != nil {
		boot := *admin.cfg
		s.boot = &boot
	}
	raw, ok, err := doc.Load()
	if err != nil {
		return nil, err
	}
	if ok {
		if err := json.Unmarshal(raw, &s.history); err != nil {
			return nil, fmt.Errorf("read restart receipts: %w", err)
		}
	}
	if s.history.Operations == nil {
		s.history.Operations = map[string]consoleapi.RestartOperation{}
	}
	s.history.Incarnation++
	if err := s.persist(); err != nil {
		return nil, err
	}
	return s, nil
}
func (s *hubServices) persist() error {
	raw, err := json.Marshal(s.history)
	if err != nil {
		return err
	}
	return s.doc.Save(raw)
}
func (s *hubServices) ready() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for key, op := range s.history.Operations {
		if op.State == "accepted" && op.Incarnation != s.history.Incarnation {
			op.State = "restarted"
			op.Incarnation = s.history.Incarnation
			op.CompletedAt = time.Now().UTC()
			s.history.Operations[key] = op
		}
	}
	return s.persist()
}
func (s *hubServices) requested() bool { s.mu.Lock(); defer s.mu.Unlock(); return s.dispatched }
func (s *hubServices) failed(cause error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	op, ok := s.history.Operations[s.pending]
	if !ok {
		return
	}
	op.State = "failed"
	op.Error = cause.Error()
	op.CompletedAt = time.Now().UTC()
	s.history.Operations[s.pending] = op
	if err := s.persist(); err != nil {
		log.Printf("steve: persist restart failure: %v", err)
	}
}
func serviceFailure(code, message string) error {
	return &consoleapi.ServiceError{Code: code, Message: message}
}
func nodeOperation(st nodewire.RestartStatus) consoleapi.RestartOperation {
	return consoleapi.RestartOperation{CommandID: st.CommandID, State: st.State, Incarnation: st.Incarnation, PreviousIncarnation: st.PreviousIncarnation, RequestedAt: st.RequestedAt, CompletedAt: st.CompletedAt, Error: st.Error}
}
func nodeRestartError(err error) error {
	code := "unavailable"
	if errors.Is(err, nodewire.ErrRestartBusy) {
		code = "busy"
	}
	if errors.Is(err, nodewire.ErrRestartUnsupported) {
		code = "unsupported"
	}
	if errors.Is(err, nodewire.ErrRestartNotFound) {
		code = "not_found"
	}
	if errors.Is(err, nodewire.ErrRestartPreflight) {
		code = "invalid"
	}
	return serviceFailure(code, err.Error())
}
func (s *hubServices) Services(ctx context.Context) (consoleapi.ServicesView, error) {
	op, _ := s.RestartStatus(ctx, "hub", "")
	out := consoleapi.ServicesView{Services: []consoleapi.ManagedService{{Name: "hub", Kind: "hub", Label: nodeName(), Version: nodewire.Version(), Online: true, Supported: processrestart.Supported(), Operation: &op}}}
	if s.admin.nodes == nil {
		return out, nil
	}
	states := s.admin.nodes.Statuses()
	for _, n := range states {
		item := consoleapi.ManagedService{Name: n.Name, Kind: "node", Label: n.Name, Version: n.Advert.BuildVersion, Online: n.Up}
		for _, feature := range n.Advert.Features {
			if feature == nodewire.FeatureRestart {
				item.Supported = true
			}
		}
		if n.Up && item.Supported {
			query, cancel := context.WithTimeout(ctx, 2*time.Second)
			st, err := s.admin.nodes.RestartStatus(query, n.Name, "")
			cancel()
			if err == nil {
				op := nodeOperation(st)
				item.Operation = &op
				item.Supported = st.Supported
			}
		}
		out.Services = append(out.Services, item)
	}
	return out, nil
}
func (s *hubServices) RestartStatus(ctx context.Context, name, id string) (consoleapi.RestartOperation, error) {
	if name != "hub" {
		if s.admin.nodes == nil {
			return consoleapi.RestartOperation{}, serviceFailure("not_found", "Node not found")
		}
		st, err := s.admin.nodes.RestartStatus(ctx, name, id)
		if err != nil {
			return consoleapi.RestartOperation{}, nodeRestartError(err)
		}
		return nodeOperation(st), nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if id == "" {
		id = s.history.Latest
	}
	if op, ok := s.history.Operations[id]; ok {
		return op, nil
	}
	if id != "" {
		return consoleapi.RestartOperation{}, serviceFailure("not_found", "Restart request not found")
	}
	return consoleapi.RestartOperation{State: "idle", Incarnation: s.history.Incarnation}, nil
}
func (s *hubServices) seal(ctx context.Context, name string) (func(), error) {
	var releases []func()
	release := func() {
		for i := len(releases) - 1; i >= 0; i-- {
			releases[i]()
		}
	}
	httpRelease, err := s.sealWrites()
	if err != nil {
		return nil, err
	}
	releases = append(releases, httpRelease)
	if s.admin.console != nil {
		r, err := s.admin.console.SealIdle()
		if err != nil {
			release()
			return nil, serviceFailure("busy", "Wait for queued messages and current conversations to finish")
		}
		releases = append(releases, r)
	}
	if s.admin.coordinator != nil {
		r, err := s.admin.coordinator.SealIdle()
		if err != nil {
			release()
			return nil, serviceFailure("busy", "Wait for the current conversation or channel command to finish")
		}
		releases = append(releases, r)
	}
	if s.executions != nil {
		r, err := s.executions.SealIdle()
		if err != nil {
			release()
			return nil, serviceFailure("busy", "Wait for active work and unresolved executions to finish")
		}
		releases = append(releases, r)
	}
	s.admin.mu.Lock()
	cloning := len(s.admin.cloning) > 0
	s.admin.mu.Unlock()
	if cloning {
		release()
		return nil, serviceFailure("busy", "A workspace copy is still running")
	}
	if s.admin.attempts != nil {
		live, err := s.admin.attempts.Live(ctx)
		if err != nil {
			release()
			return nil, err
		}
		if len(live) > 0 {
			release()
			return nil, serviceFailure("busy", "An execution has not confirmed its exit")
		}
	}
	if s.admin.manager != nil {
		wait, cancel := context.WithTimeout(ctx, 15*time.Second)
		r, err := s.admin.manager.SuspendNode(wait, name, name == "hub")
		cancel()
		releases = append(releases, r)
		if err != nil {
			release()
			return nil, serviceFailure("busy", "Cached agent processes have not confirmed their exit; wait before restarting")
		}
	}
	return release, nil
}
func (s *hubServices) Restart(ctx context.Context, name string, req consoleapi.RestartRequest) (consoleapi.RestartOperation, error) {
	if !restartID.MatchString(req.CommandID) {
		return consoleapi.RestartOperation{}, serviceFailure("invalid", "A stable command_id is required")
	}
	if !s.actionMu.TryLock() {
		return consoleapi.RestartOperation{}, serviceFailure("busy", "Another restart request is being prepared")
	}
	defer s.actionMu.Unlock()
	if name == "hub" {
		s.mu.Lock()
		if op, ok := s.history.Operations[req.CommandID]; ok {
			s.mu.Unlock()
			return op, nil
		}
		s.mu.Unlock()
	}
	if name != "hub" {
		if s.admin.nodes == nil {
			return consoleapi.RestartOperation{}, serviceFailure("not_found", "Node not found")
		}
		// Replay before maintenance: the node remains the command's authority.
		status, err := s.admin.nodes.RestartStatus(ctx, name, req.CommandID)
		if err == nil {
			return nodeOperation(status), nil
		}
		if !errors.Is(err, nodewire.ErrRestartNotFound) {
			return consoleapi.RestartOperation{}, nodeRestartError(err)
		}
	}
	s.mu.Lock()
	pending := s.pending
	s.mu.Unlock()
	if pending != "" {
		return consoleapi.RestartOperation{}, serviceFailure("busy", "Another service restart is in progress")
	}
	if !processrestart.Supported() && name == "hub" {
		return consoleapi.RestartOperation{}, serviceFailure("unsupported", "Service restart is not supported on this platform")
	}
	release, err := s.seal(ctx, name)
	if err != nil {
		return consoleapi.RestartOperation{}, err
	}
	if name == "hub" {
		if err := s.preflight(); err != nil {
			release()
			return consoleapi.RestartOperation{}, err
		}
	}
	if name != "hub" {
		if err := s.waitNodeIdle(ctx, name); err != nil {
			release()
			return consoleapi.RestartOperation{}, err
		}
		st, err := s.admin.nodes.Restart(ctx, name, req.CommandID)
		if err != nil {
			release()
			return consoleapi.RestartOperation{}, nodeRestartError(err)
		}
		s.mu.Lock()
		s.pending = name + "/" + req.CommandID
		s.mu.Unlock()
		go s.watchNode(name, req.CommandID, release)
		return nodeOperation(st), nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	op := consoleapi.RestartOperation{CommandID: req.CommandID, State: "accepted", Incarnation: s.history.Incarnation, PreviousIncarnation: s.history.Incarnation, RequestedAt: time.Now().UTC()}
	s.history.Operations[req.CommandID] = op
	s.history.Latest = req.CommandID
	if err := s.persist(); err != nil {
		delete(s.history.Operations, req.CommandID)
		release()
		return consoleapi.RestartOperation{}, err
	}
	s.pending = req.CommandID
	s.release = release
	return op, nil
}
func (s *hubServices) preflight() error {
	if s.admin.path == "" {
		return nil
	}
	configMu.RLock()
	defer configMu.RUnlock()
	if s.admin.cfg != nil {
		if err := s.admin.cfg.CheckFileRevision(s.admin.path); err != nil {
			return serviceFailure("conflict", "The configuration file changed outside the console; validate and deploy it before restarting")
		}
	}
	if s.admin.clusterMode {
		// A coordinator activation uses the shared declaration over this
		// machine's local configuration. Its logical cluster identity and
		// internal listener intentionally differ from the installation file.
		cfg := s.admin.cfg
		if cfg == nil {
			return serviceFailure("invalid", "The current shared configuration is unavailable")
		}
		if err := cfg.ValidateChannels(); err != nil {
			return serviceFailure("invalid", "The saved channel configuration is invalid")
		}
		if _, err := cfg.AgentCatalog(); err != nil {
			return serviceFailure("invalid", "The shared Agent configuration is invalid")
		}
		if _, _, err := config.ProjectDeclarations(cfg); err != nil {
			return serviceFailure("invalid", "The shared project configuration is invalid")
		}
		return nil
	}
	cfg, err := config.Load(s.admin.path)
	if err != nil {
		return serviceFailure("invalid", "Saved configuration is invalid; fix it before restarting")
	}
	if _, port, err := net.SplitHostPort(cfg.Gateway.ReadModelAddr); err != nil || port == "0" {
		return serviceFailure("invalid", "Service restart requires a fixed console address")
	}
	if err := cfg.ValidateChannels(); err != nil {
		return serviceFailure("invalid", "Saved channel configuration is invalid; fix it before restarting")
	}
	if _, err := cfg.AgentCatalog(); err != nil {
		return serviceFailure("invalid", "Saved agent configuration is invalid; fix it before restarting")
	}
	if _, _, err := config.ProjectDeclarations(cfg); err != nil {
		return serviceFailure("invalid", "Saved project configuration is invalid; fix it before restarting")
	}
	if boot := s.boot; boot != nil {
		if cfg.Gateway.StatePath != boot.Gateway.StatePath || cfg.Gateway.HomePath != boot.Gateway.HomePath || cfg.Gateway.ReadModelAddr != boot.Gateway.ReadModelAddr || cfg.Gateway.ReadModelToken != boot.Gateway.ReadModelToken || cfg.EffectiveOwnerID() != boot.EffectiveOwnerID() || cfg.Gateway.HubID != "" && cfg.Gateway.HubID != boot.Gateway.HubID {
			return serviceFailure("invalid", "Changing service identity, storage, authentication or address requires deployment")
		}
	}
	if err := cfg.CheckFileRevision(s.admin.path); err != nil {
		return serviceFailure("conflict", "Configuration changed during restart preparation")
	}
	return nil
}
func (s *hubServices) waitNodeIdle(ctx context.Context, name string) error {
	wait, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	for {
		st, err := s.admin.nodes.RestartStatus(wait, name, "")
		if err != nil {
			return nodeRestartError(err)
		}
		if !st.Supported {
			return serviceFailure("unsupported", "The node does not support service restart")
		}
		if st.ActiveStreams == 0 && st.Processes == 0 {
			return nil
		}
		select {
		case <-wait.Done():
			return serviceFailure("busy", "The node is still waiting for process exit; retry after work has stopped")
		case <-time.After(100 * time.Millisecond):
		}
	}
}
func (s *hubServices) RestartAccepted(name, id string) {
	if name != "hub" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.pending != id || s.dispatched {
		return
	}
	s.dispatched = true
	s.stop()
}
func (s *hubServices) watchNode(name, id string, release func()) {
	defer release()
	defer func() { s.mu.Lock(); s.pending = ""; s.mu.Unlock() }()
	ctx, cancel := context.WithTimeout(context.Background(), restartWatchTimeout)
	defer cancel()
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			query, cancel := context.WithTimeout(ctx, 2*time.Second)
			st, err := s.admin.nodes.RestartStatus(query, name, id)
			cancel()
			if err == nil && (st.State == "restarted" || st.State == "failed") {
				return
			}
		}
	}
}
