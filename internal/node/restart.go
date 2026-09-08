package node

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/gopact-ai/steve/internal/ledger"
	"github.com/gopact-ai/steve/internal/node/journal"
	"github.com/gopact-ai/steve/internal/nodewire"
)

var ErrRestartRequested = errors.New("node restart requested after graceful shutdown")

type restartRecord struct {
	Hub string `json:"hub"`
	nodewire.RestartStatus
}

type restartControl struct {
	mu                           sync.Mutex
	enabled, draining, requested bool
	active                       int
	cancel                       context.CancelFunc
	records                      map[string]restartRecord
	pending                      string
	check                        func() error
}

// EnableRestart must be called before Serve by a launcher that handles
// ErrRestartRequested by re-executing its own executable after Serve returns.
func (s *Server) EnableRestart() {
	s.restart.mu.Lock()
	s.restart.enabled = true
	s.restart.mu.Unlock()
}

// SetRestartCheck installs the launcher's read-only preflight. It runs with
// admission sealed, before a durable accepted receipt, and must not call
// back into Server. Configure this before Serve starts.
func (s *Server) SetRestartCheck(check func() error) {
	s.restart.mu.Lock()
	s.restart.check = check
	s.restart.mu.Unlock()
}

func (s *Server) beginWork() (func(), error) {
	s.restart.mu.Lock()
	defer s.restart.mu.Unlock()
	if s.restart.draining {
		return nil, nodewire.ErrRestartBusy
	}
	s.restart.active++
	s.workWG.Add(1)
	return func() {
		s.restart.mu.Lock()
		s.restart.active--
		s.restart.mu.Unlock()
		s.workWG.Done()
	}, nil
}

func (s *Server) restartPath() string {
	return filepath.Join(s.conf().StateDir, "instance-control", "restart.json")
}

func (s *Server) saveRestarts(records map[string]restartRecord) error {
	raw, err := json.Marshal(records)
	if err != nil {
		return err
	}
	return (&ledger.FileDocument{Path: s.restartPath()}).Save(raw)
}

func (s *Server) startRestartControl(cancel context.CancelFunc) error {
	s.restart.mu.Lock()
	defer s.restart.mu.Unlock()
	s.restart.cancel = cancel
	s.restart.records = map[string]restartRecord{}
	if s.conf().StateDir == "" {
		return nil
	}
	raw, err := os.ReadFile(s.restartPath())
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if err := json.Unmarshal(raw, &s.restart.records); err != nil {
		return fmt.Errorf("read restart receipts: %w", err)
	}
	if s.restart.records == nil {
		return errors.New("invalid empty restart receipt document")
	}
	for key, record := range s.restart.records {
		if record.Hub == "" || !journal.ValidID(record.CommandID) || key != record.Hub+"/"+record.CommandID || record.Incarnation <= 0 || record.PreviousIncarnation <= 0 || (record.State != nodewire.RestartStateAccepted && record.State != nodewire.RestartStateRestarted && record.State != nodewire.RestartStateFailed) {
			return errors.New("invalid restart receipt")
		}
		s.generation = max(s.generation, record.Incarnation+1)
	}
	for key, record := range s.restart.records {
		if record.State == nodewire.RestartStateAccepted {
			record.State = nodewire.RestartStateRestarted
			record.Incarnation = s.generation
			record.CompletedAt = time.Now().UTC()
			s.restart.records[key] = record
		}
	}
	return s.saveRestarts(s.restart.records)
}

// RestartFailed records failure to exec after the service has drained. The
// receipt remains terminal so retrying the same command cannot loop restarts.
func (s *Server) RestartFailed(cause error) error {
	s.restart.mu.Lock()
	defer s.restart.mu.Unlock()
	record, ok := s.restart.records[s.restart.pending]
	if !ok {
		return errors.New("no accepted restart")
	}
	record.State, record.Error, record.CompletedAt = nodewire.RestartStateFailed, cause.Error(), time.Now().UTC()
	s.restart.records[s.restart.pending] = record
	return s.saveRestarts(s.restart.records)
}

func (s *Server) restartStatusLocked() nodewire.RestartStatus {
	status := nodewire.RestartStatus{State: nodewire.RestartStateIdle, Supported: s.restart.enabled && s.conf().StateDir != "", Incarnation: s.generation, ActiveStreams: s.restart.active}
	if s.restart.draining {
		status.State = nodewire.RestartStateDraining
	}
	s.processMu.Lock()
	known := map[string]bool{}
	for _, p := range s.processes {
		p.mu.Lock()
		known[p.id] = true
		if p.exit == "" {
			status.Processes++
		}
		p.mu.Unlock()
	}
	s.processMu.Unlock()
	// A process from a crashed incarnation may outlive its parent. Missing
	// exit evidence is busy, even when this incarnation has no in-memory
	// process object that could prove it stopped.
	if s.conf().StateDir != "" {
		entries, err := os.ReadDir(filepath.Join(s.conf().StateDir, "streams"))
		if err != nil && !os.IsNotExist(err) {
			status.Error = "cannot inspect process exit evidence: " + err.Error()
		}
		for _, entry := range entries {
			if !entry.IsDir() || known[entry.Name()] {
				continue
			}
			if _, err := os.Stat(filepath.Join(s.conf().StateDir, "streams", entry.Name(), "ended")); err != nil {
				status.Processes++
			}
		}
	}
	return status
}

func (s *Server) restartCommand(hub string, req nodewire.RestartRequest) (nodewire.RestartReply, bool) {
	s.restart.mu.Lock()
	defer s.restart.mu.Unlock()
	out := nodewire.RestartReply{Status: s.restartStatusLocked()}
	fail := func(code, message string) (nodewire.RestartReply, bool) {
		out.ErrorCode, out.Error = code, message
		return out, false
	}
	s.hubMu.Lock()
	owner := s.hubName
	s.hubMu.Unlock()
	if hub == "" || hub != owner {
		return fail("ownership", "restart requires the current node owner")
	}
	if req.Action != "restart" && req.Action != "get" {
		return fail("invalid", "unknown restart action")
	}
	if req.CommandID != "" && !journal.ValidID(req.CommandID) {
		return fail("invalid", "invalid command_id")
	}
	key := hub + "/" + req.CommandID
	if record, ok := s.restart.records[key]; ok {
		out.Status = record.RestartStatus
		out.Status.Supported = s.restart.enabled && s.conf().StateDir != ""
		return out, false
	}
	if req.Action == "get" {
		if req.CommandID != "" {
			return fail("not_found", "restart command not found")
		}
		var latest restartRecord
		for _, record := range s.restart.records {
			if record.Hub == hub && (latest.CommandID == "" || record.RequestedAt.After(latest.RequestedAt)) {
				latest = record
			}
		}
		if latest.CommandID != "" {
			current := out.Status
			out.Status = latest.RestartStatus
			out.Status.Incarnation, out.Status.Supported = current.Incarnation, current.Supported
			out.Status.ActiveStreams, out.Status.Processes = current.ActiveStreams, current.Processes
		}
		return out, false
	}
	if req.CommandID == "" {
		return fail("invalid", "command_id is required")
	}
	if !out.Status.Supported {
		return fail("unsupported", nodewire.ErrRestartUnsupported.Error())
	}
	if out.Status.Error != "" {
		return fail("inspection", out.Status.Error)
	}
	if s.restart.draining || out.Status.ActiveStreams != 0 || out.Status.Processes != 0 {
		return fail("busy", nodewire.ErrRestartBusy.Error())
	}
	if s.ctx == nil || s.ctx.Err() != nil {
		return fail("busy", "node is stopping")
	}
	s.restart.draining = true
	if s.restart.check != nil {
		if err := s.restart.check(); err != nil {
			s.restart.draining = false
			return fail("preflight", "restart configuration is not ready: "+err.Error())
		}
	}
	status := out.Status
	status.CommandID, status.State, status.PreviousIncarnation, status.RequestedAt = req.CommandID, nodewire.RestartStateAccepted, s.generation, time.Now().UTC()
	s.restart.records[key] = restartRecord{Hub: hub, RestartStatus: status}
	if err := s.saveRestarts(s.restart.records); err != nil {
		delete(s.restart.records, key)
		s.restart.draining = false
		return fail("storage", "restart receipt could not be persisted: "+err.Error())
	}
	s.restart.pending = key
	out.Status = status
	return out, true
}

func (s *Server) restartStream(hub string, stream *nodewire.Stream) {
	defer stream.Close()
	var req nodewire.RestartRequest
	if err := json.NewDecoder(io.LimitReader(stream, 4096)).Decode(&req); err != nil {
		if err := json.NewEncoder(stream).Encode(nodewire.RestartReply{ErrorCode: "invalid", Error: "invalid restart request"}); err != nil {
			log.Printf("steve-node: restart reply: %v", err)
		}
		return
	}
	reply, accepted := s.restartCommand(hub, req)
	// Stream.Write flushes each frame synchronously to the socket. Even if
	// the peer loses the reply, its durable command receipt prevents reexec.
	if err := json.NewEncoder(stream).Encode(reply); err != nil {
		log.Printf("steve-node: restart reply: %v", err)
	}
	// The reply is complete before the restart proceeds; the close only
	// hands the stream back early.
	_ = stream.Close()
	if accepted {
		s.restart.mu.Lock()
		s.restart.requested = true
		cancel := s.restart.cancel
		s.restart.mu.Unlock()
		cancel()
	}
}

func (r *Registry) Restart(ctx context.Context, name, commandID string) (nodewire.RestartStatus, error) {
	return r.restartRequest(ctx, name, nodewire.RestartRequest{Action: "restart", CommandID: commandID})
}

func (r *Registry) RestartStatus(ctx context.Context, name, commandID string) (nodewire.RestartStatus, error) {
	return r.restartRequest(ctx, name, nodewire.RestartRequest{Action: "get", CommandID: commandID})
}

func (r *Registry) restartRequest(ctx context.Context, name string, req nodewire.RestartRequest) (nodewire.RestartStatus, error) {
	c, err := r.connect(ctx, name)
	if err != nil {
		return nodewire.RestartStatus{}, err
	}
	if !nodewire.HasFeature(c.getAdvert().Features, nodewire.FeatureRestart) {
		return nodewire.RestartStatus{}, nodewire.ErrRestartUnsupported
	}
	stream, err := c.mux.Open(nodewire.OpenRequest{Kind: nodewire.StreamRestart})
	if err != nil {
		return nodewire.RestartStatus{}, err
	}
	defer stream.Close()
	stop := context.AfterFunc(ctx, func() { _ = stream.Close() })
	defer stop()
	if err := json.NewEncoder(stream).Encode(req); err != nil {
		return nodewire.RestartStatus{}, err
	}
	var reply nodewire.RestartReply
	if err := json.NewDecoder(stream).Decode(&reply); err != nil {
		return nodewire.RestartStatus{}, err
	}
	if reply.Error != "" {
		if reply.ErrorCode == "busy" {
			return reply.Status, fmt.Errorf("%w: active_streams=%d processes=%d", nodewire.ErrRestartBusy, reply.Status.ActiveStreams, reply.Status.Processes)
		}
		if reply.ErrorCode == "unsupported" {
			return reply.Status, nodewire.ErrRestartUnsupported
		}
		if reply.ErrorCode == "not_found" {
			return reply.Status, nodewire.ErrRestartNotFound
		}
		if reply.ErrorCode == "preflight" {
			return reply.Status, fmt.Errorf("%w: %s", nodewire.ErrRestartPreflight, reply.Error)
		}
		return reply.Status, fmt.Errorf("node restart %s: %s", reply.ErrorCode, reply.Error)
	}
	return reply.Status, nil
}
