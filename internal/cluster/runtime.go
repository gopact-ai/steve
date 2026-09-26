// Package cluster combines a durable coordinator assignment with the replicated
// application ledger. Business activation is a separate lifecycle from Raft.
package cluster

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"runtime/pprof"
	"sync"
	"time"

	"github.com/gopact-ai/steve/internal/coordination"
	"github.com/gopact-ai/steve/internal/ledger"
)

var (
	ErrInactive        = errors.New("cluster: business generation is inactive")
	ErrShutdownTimeout = errors.New("cluster: business shutdown has not completed")
)

type Deactivate func(context.Context) error

type Config struct {
	LedgerDir     string
	LedgerOptions ledger.Options
	Coordination  coordination.Config
	// Client routes requests to the consensus leader. It is optional for a
	// single loopback node and remains owned by the caller.
	Client *coordination.Client
	// Activate reconstructs business stores from the supplied ledger. The
	// context is canceled before Deactivate runs. Deactivate must join all
	// users of those stores; the returned ledger is never reused by a later
	// generation. A nil Activate enables ledger access without starting tasks.
	Activate func(context.Context, Activation) (Deactivate, error)
	// PollInterval is how often the runtime compares its business generation
	// with the local replica, which reads nothing from other nodes and
	// appends nothing to the Raft log. Quorum reads happen only while a
	// generation starts.
	PollInterval time.Duration
	// ShutdownTimeout is how long a business generation may take to stop
	// before the runtime reports it as slow and records every goroutine's
	// stack under DiagnosticsDir. It also bounds Close.
	ShutdownTimeout time.Duration
	// ShutdownDeadline is how long the runtime keeps waiting for a slow stop.
	// Past it the generation is abandoned and the runtime ends with
	// ErrShutdownTimeout, so the owner of the process can restart it.
	ShutdownDeadline time.Duration
	// DiagnosticsDir receives goroutine dumps of slow stops; empty means a
	// "diagnostics" directory beside LedgerDir.
	DiagnosticsDir string
}

// keptDiagnostics bounds the goroutine dumps a node keeps.
const keptDiagnostics = 8

type Activation struct {
	Runtime          *Runtime
	NodeID           string
	Assignment       coordination.Assignment
	Version          uint64
	Generation       uint64
	WriterGeneration uint64
	Ledger           *ledger.Ledger
	Context          context.Context
}

// Status is an observation, not permission to execute. Every write verifies
// the current assignment against a majority independently of Ready.
type Status struct {
	coordination.Status
	Ready          bool   `json:"ready"`
	Generation     uint64 `json:"generation"`
	ReplicaVersion uint64 `json:"replica_version"`
	LastError      string `json:"last_error,omitempty"`
	Closed         bool   `json:"closed"`
}

type generation struct {
	Activation
	cancel  context.CancelFunc
	stop    Deactivate
	restore uint64
}

type Runtime struct {
	config  Config
	book    *ledger.Ledger
	service *coordination.Service
	ctx     context.Context
	cancel  context.CancelFunc

	mu        sync.Mutex
	current   *generation
	sequence  uint64
	restores  uint64
	restoring bool
	ready     bool
	closed    bool
	retryIn   time.Duration
	lastError error
	// inactive is the reason last logged for having no ready generation,
	// so a follower polling the same answer logs it once.
	inactive     string
	closeError   error
	cleanupError error
	failure      error
	changed      chan struct{}
	workerDone   chan struct{}
	closeDone    chan struct{}
	closeOnce    sync.Once
}

func Open(config Config) (*Runtime, error) {
	if config.LedgerDir == "" || config.Coordination.NodeID == "" || config.Coordination.ClusterID == "" || config.Coordination.DataDir == "" {
		return nil, fmt.Errorf("%w: ledger directory, node ID, cluster ID and consensus directory are required", coordination.ErrInvalid)
	}
	if config.Coordination.Application != nil || config.LedgerOptions.ReplicaWriter {
		return nil, fmt.Errorf("%w: the runtime owns the application replica", coordination.ErrInvalid)
	}
	if config.PollInterval <= 0 {
		config.PollInterval = 200 * time.Millisecond
	}
	if config.ShutdownTimeout <= 0 {
		config.ShutdownTimeout = 10 * time.Second
	}
	if config.ShutdownDeadline <= 0 {
		config.ShutdownDeadline = 2 * time.Minute
	}
	if config.ShutdownDeadline < config.ShutdownTimeout {
		config.ShutdownDeadline = config.ShutdownTimeout
	}
	if config.DiagnosticsDir == "" {
		config.DiagnosticsDir = filepath.Join(filepath.Dir(filepath.Clean(config.LedgerDir)), "diagnostics")
	}
	if config.Coordination.ApplyTimeout <= 0 {
		config.Coordination.ApplyTimeout = 5 * time.Second
	}
	book, err := ledger.Open(config.LedgerDir, config.LedgerOptions)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithCancel(context.Background())
	r := &Runtime{config: config, book: book, ctx: ctx, cancel: cancel, changed: make(chan struct{}), workerDone: make(chan struct{}), closeDone: make(chan struct{})}
	// Attach before opening Raft (which can replay immediately), and before
	// creating any business store. The FSM handle never authorizes writes.
	if err := book.AttachReplication(replicaOnly{}); err != nil {
		cancel()
		book.Close()
		return nil, err
	}
	config.Coordination.Application = application{runtime: r}
	if config.Coordination.Probe == nil && config.Client != nil {
		config.Coordination.Probe = config.Client.Probe
	}
	r.service, err = coordination.Open(config.Coordination)
	if err != nil {
		cancel()
		book.Close()
		return nil, err
	}
	go r.run()
	return r, nil
}

// Ledger exposes the local replica for reading. Business mutations use the
// generation-scoped ledger received by Activate or WaitReady.
func (r *Runtime) Ledger() *ledger.Ledger { return r.book }

func (r *Runtime) Status() Status {
	status := Status{Status: r.service.Status()}
	version, err := r.book.ReplicaVersion()
	status.ReplicaVersion = version
	r.mu.Lock()
	defer r.mu.Unlock()
	status.Ready, status.Generation, status.Closed = r.ready, r.sequence, r.closed
	if r.lastError != nil {
		status.LastError = r.lastError.Error()
	} else if err != nil {
		status.LastError = err.Error()
	}
	return status
}

func (r *Runtime) WaitReady(ctx context.Context) (Activation, error) {
	for {
		r.mu.Lock()
		if r.closed {
			err := errors.Join(ErrInactive, r.lastError)
			r.mu.Unlock()
			return Activation{}, err
		}
		if r.ready && r.current != nil && r.current.Context.Err() == nil {
			current := r.current
			active := current.Activation
			r.mu.Unlock()
			position, err := (&replicator{runtime: r, generation: current, book: active.Ledger}).Prepare(ctx)
			if err == nil {
				active.Version = position.Version
				return active, nil
			}
			if ctx.Err() != nil {
				return Activation{}, ctx.Err()
			}
			if current.Context.Err() != nil || errors.Is(err, ErrInactive) || errors.Is(err, coordination.ErrStaleEpoch) {
				continue
			}
			return Activation{}, err
		}
		changed := r.changed
		r.mu.Unlock()
		select {
		case <-ctx.Done():
			return Activation{}, ctx.Err()
		case <-changed:
		}
	}
}

func (r *Runtime) notifyLocked() {
	close(r.changed)
	r.changed = make(chan struct{})
}

func (r *Runtime) invalidate(reason error, restored bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if restored {
		r.restores++
	}
	r.ready = false
	if reason != nil {
		r.lastError = reason
	}
	if r.current != nil {
		if r.current.Context.Err() == nil {
			r.logInactiveLocked(fmt.Sprintf("cluster: business generation %d invalidated", r.current.Generation), reason, restored, r.current.Generation)
		}
		r.current.cancel()
	} else if reason != nil || restored {
		r.logInactiveLocked("cluster: business generation inactive", reason, restored, 0)
	}
	r.notifyLocked()
}

// logInactiveLocked records why this node has no ready business generation.
// A reason already reported is not repeated; not being the coordinator is
// the normal state of a follower and is logged at information level.
func (r *Runtime) logInactiveLocked(message string, reason error, restored bool, generation uint64) {
	text := "replica restored from a snapshot"
	if reason != nil {
		text = reason.Error()
	}
	if generation == 0 && text == r.inactive {
		return
	}
	r.inactive = text
	attrs := []any{"node", r.config.Coordination.NodeID, "cause", text}
	if generation != 0 {
		attrs = append(attrs, "generation", generation)
	}
	if errors.Is(reason, coordination.ErrNotCoordinator) {
		slog.Info(message, attrs...)
		return
	}
	slog.Warn(message, attrs...)
}

func (r *Runtime) valid(g *generation) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return !r.closed && !r.restoring && r.current == g && g.restore == r.restores && g.Context.Err() == nil
}

func (r *Runtime) revoke(g *generation, err error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.current == g {
		if g.Context.Err() == nil {
			r.logInactiveLocked(fmt.Sprintf("cluster: business generation %d revoked", g.Generation), err, false, g.Generation)
		}
		g.cancel()
		r.ready = false
		r.lastError = err
		r.notifyLocked()
	}
}

// run keeps this node's business generation in step with the coordinator
// assignment. Each tick reads the local replica, which costs nothing: a quorum
// read appends a barrier to the Raft log and, on a member that does not lead,
// asks the leader for the whole state. The local view only decides whether
// this node looks like the coordinator. A generation starts on a quorum read
// that confirms it, and every write verifies the assignment against a majority
// again, so the local view never authorizes anything. A running generation is
// kept while the local replica still names it and hears from a consensus
// leader. It is given up when the replica names another, at once when this
// node stops leading consensus without knowing a successor, and after
// ApplyTimeout when a follower stops hearing from its leader.
func (r *Runtime) run() {
	defer close(r.workerDone)
	ticker := time.NewTicker(r.config.PollInterval)
	defer ticker.Stop()
	var state tickState
	for {
		if r.ctx.Err() != nil {
			err := r.retire()
			r.mu.Lock()
			r.cleanupError = errors.Join(r.cleanupError, err)
			r.mu.Unlock()
			return
		}
		status := r.service.Status()
		if !status.Healthy {
			r.shutdown(fmt.Errorf("%w: the consensus replica is no longer healthy", coordination.ErrApplication))
			continue
		}
		if err := r.step(observation{Status: status, at: time.Now()}, &state); err != nil {
			r.invalidate(err, false)
			r.retire()
		}
		select {
		case <-r.ctx.Done():
		case <-ticker.C:
		}
	}
}

// observation is what one tick reads from the local replica.
type observation struct {
	coordination.Status
	at time.Time
}

// tickState is what the runtime loop carries from one tick to the next.
type tickState struct {
	// heard is when this replica last knew a consensus leader, and leading
	// whether that leader was this node.
	heard   time.Time
	leading bool
	// denied is the applied index of the latest quorum read that found this
	// node not coordinating: a replica behind it that still names this node
	// is stale, and asking again would only repeat the answer.
	denied uint64
}

// step is one tick of run. It returns why this node has no business
// generation to keep, or nil once the generation the local replica names
// is running.
func (r *Runtime) step(seen observation, s *tickState) error {
	if seen.LeaderID != "" {
		s.heard, s.leading = seen.at, seen.LeaderID == r.config.Coordination.NodeID
	}
	err := r.coordinates(seen.State)
	if err == nil && seen.AppliedIndex < s.denied {
		err = coordination.ErrNotCoordinator
	}
	// A leader forgets itself only when it steps down, which it does on its
	// own once its lease finds no majority: a majority may already follow
	// another leader. A follower's leader is merely late until ApplyTimeout.
	if err == nil && seen.LeaderID == "" && s.leading {
		err = fmt.Errorf("%w: this replica stopped leading consensus and knows no other leader", coordination.ErrUnavailable)
	}
	if err == nil && seen.at.Sub(s.heard) > r.config.Coordination.ApplyTimeout {
		err = fmt.Errorf("%w: this replica has heard from no consensus leader for %s", coordination.ErrUnavailable, r.config.Coordination.ApplyTimeout)
	}
	if err == nil {
		r.mu.Lock()
		if r.restoring {
			err = coordination.ErrNotReady
		}
		r.mu.Unlock()
	}
	if err == nil && !r.keeps(seen.State) {
		err = r.start(s)
	}
	return err
}

// coordinates reports why state does not make this node the coordinator, or
// nil if it does.
func (r *Runtime) coordinates(state coordination.State) error {
	id := r.config.Coordination.NodeID
	if state.Coordinator.NodeID != id || state.Coordinator.Epoch == 0 || !state.IsActiveReplica(id) || (state.AutoFailover && state.Voters[id] == "") {
		return coordination.ErrNotCoordinator
	}
	return nil
}

// keeps reports whether the current generation runs for state's assignment
// and writer generation.
func (r *Runtime) keeps(state coordination.State) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	current := r.current
	return current != nil && current.Assignment == state.Coordinator && current.WriterGeneration == state.WriterGeneration && current.restore == r.restores && current.Context.Err() == nil
}

// start replaces the current generation with one for the assignment a quorum
// read confirms, once the local replica has caught up with that read. A read
// that finds this node not coordinating is recorded in s.denied.
func (r *Runtime) start(s *tickState) error {
	ctx, cancel := context.WithTimeout(r.ctx, r.config.Coordination.ApplyTimeout)
	state, err := r.ReadState(ctx)
	if err == nil {
		if err = r.coordinates(state); err != nil {
			s.denied = state.AppliedIndex
		}
	}
	var version uint64
	if err == nil {
		version, err = r.waitApplied(ctx, state.AppliedIndex, state.AppVersion)
	}
	cancel()
	if err != nil || r.keeps(state) {
		return err
	}
	r.retire()
	if r.ctx.Err() == nil {
		if err := r.activate(state.Coordinator, version, state.WriterGeneration); err != nil {
			r.holdActivation(err)
		}
	}
	return nil
}

func (r *Runtime) activate(assignment coordination.Assignment, version, expectedWriter uint64) error {
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		return ErrInactive
	}
	if r.restoring {
		r.mu.Unlock()
		return nil
	}
	r.sequence++
	started := time.Now()
	ctx, cancel := context.WithCancel(r.ctx)
	g := &generation{Activation: Activation{Runtime: r, NodeID: r.config.Coordination.NodeID, Assignment: assignment, Version: version, Generation: r.sequence, Context: ctx}, cancel: cancel, restore: r.restores}
	r.current = g
	r.mu.Unlock()
	var random [16]byte
	rand.Read(random[:])
	fence, err := r.beginWriter(ctx, coordination.WriterRequest{ID: "writer-" + hex.EncodeToString(random[:]), CallerNodeID: g.NodeID, CoordinatorEpoch: assignment.Epoch, ExpectedGeneration: expectedWriter})
	if err != nil {
		r.revoke(g, err)
		return nil
	}
	// The runtime loop, which otherwise notices a replica that stays behind,
	// is waiting on this activation: the wait bounds itself.
	if _, err := r.awaitApplied(ctx, fence.Index, fence.AppVersion); err != nil {
		r.revoke(g, err)
		return nil
	}
	g.WriterGeneration, g.Version = fence.WriterGeneration, fence.AppVersion
	opts := r.config.LedgerOptions
	opts.ReplicaWriter = true
	writer, err := ledger.Open(r.config.LedgerDir, opts)
	if err != nil {
		if !r.valid(g) {
			return nil
		}
		return fmt.Errorf("open business ledger: %w", err)
	}
	g.Ledger = writer
	if err := writer.AttachReplication(&replicator{runtime: r, generation: g, book: writer}); err != nil {
		return err
	}
	if !r.valid(g) {
		return nil
	}
	if _, err := (&replicator{runtime: r, generation: g, book: writer}).Prepare(ctx); err != nil {
		r.revoke(g, err)
		return nil
	}
	if r.config.Activate != nil {
		g.stop, err = r.invokeActivation(g)
		if err != nil {
			if errors.Is(err, context.Canceled) && !r.valid(g) {
				return nil
			}
			return fmt.Errorf("activate business generation: %w", err)
		}
	}
	position, err := (&replicator{runtime: r, generation: g, book: writer}).Prepare(ctx)
	if err != nil {
		r.revoke(g, err)
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed || g.Context.Err() != nil || g.restore != r.restores {
		return nil
	}
	g.Version = position.Version
	r.ready = true
	r.retryIn = 0
	r.lastError = nil
	r.inactive = ""
	r.notifyLocked()
	slog.Info(fmt.Sprintf("cluster: business generation %d ready", g.Generation), "node", g.NodeID, "generation", g.Generation, "epoch", assignment.Epoch, "writer_generation", g.WriterGeneration, "took", time.Since(started).Round(time.Millisecond))
	return nil
}

// holdActivation keeps this replica in the cluster when its own application
// could not be built. Consensus is a separate lifecycle: the node keeps
// replicating and voting, the business generation stays inactive with the
// reason readable, and the next attempt waits out a delay that grows to
// thirty seconds so a build that keeps failing does not spin.
func (r *Runtime) holdActivation(cause error) {
	r.mu.Lock()
	generation := r.sequence
	r.mu.Unlock()
	slog.Error(fmt.Sprintf("cluster: business generation did not start: %v", cause), "node", r.config.Coordination.NodeID, "generation", generation)
	r.invalidate(cause, false)
	r.retire()
	if r.ctx.Err() != nil {
		return
	}
	r.mu.Lock()
	if r.retryIn *= 2; r.retryIn < 5*r.config.PollInterval {
		r.retryIn = 5 * r.config.PollInterval
	} else if r.retryIn > 30*time.Second {
		r.retryIn = 30 * time.Second
	}
	delay := r.retryIn
	r.mu.Unlock()
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-r.ctx.Done():
	case <-timer.C:
	}
}

type activationResult struct {
	stop Deactivate
	err  error
}

// invokeActivation keeps checking authority while the application constructs
// its stores and servers. A slow initializer cannot delay role revocation.
func (r *Runtime) invokeActivation(g *generation) (Deactivate, error) {
	result := make(chan activationResult, 1)
	go func() {
		stop, err := r.config.Activate(g.Context, g.Activation)
		result <- activationResult{stop: stop, err: err}
	}()
	ticker := time.NewTicker(r.config.PollInterval)
	defer ticker.Stop()
	for {
		select {
		case finished := <-result:
			return finished.stop, finished.err
		case <-g.Context.Done():
			finished := make(chan error, 1)
			var late activationResult
			go func() { late = <-result; finished <- late.err }()
			if _, ok := r.awaitStop(g.Generation, "activation", finished); !ok {
				// The late activation's users still hold its stores; they
				// are released when it returns, and never reused.
				go func() {
					<-finished
					if late.stop != nil {
						_ = late.stop(context.Background())
					}
				}()
				r.shutdown(ErrShutdownTimeout)
				return nil, ErrShutdownTimeout
			}
			return late.stop, late.err
		case <-ticker.C:
			ctx, cancel := context.WithTimeout(g.Context, r.config.Coordination.ApplyTimeout)
			state, err := r.ReadState(ctx)
			cancel()
			if err == nil {
				if state.Coordinator != g.Assignment {
					err = coordination.ErrStaleEpoch
				} else if state.WriterGeneration != g.WriterGeneration {
					err = coordination.ErrStaleWriter
				}
			}
			if err != nil {
				r.revoke(g, err)
			}
		}
	}
}

// retire waits for business users to stop before closing their ledger. A
// callback that ignores cancellation can delay cleanup, but never permit a
// later generation to reuse its stores or keep Close blocked indefinitely.
// An error the stop reports is logged and returned for the record: the
// generation has stopped, so it does not end the runtime. Only a stop that
// outlives the deadline does, and retire ends the runtime itself then.
func (r *Runtime) retire() error {
	r.mu.Lock()
	g := r.current
	if g == nil {
		r.mu.Unlock()
		return nil
	}
	g.cancel()
	r.ready = false
	r.notifyLocked()
	cause := "requested"
	if r.lastError != nil {
		cause = r.lastError.Error()
	}
	r.mu.Unlock()
	started := time.Now()
	slog.Info(fmt.Sprintf("cluster: business generation %d retiring", g.Generation), "node", g.NodeID, "generation", g.Generation, "cause", cause)
	var stopErr error
	if g.stop != nil {
		ctx, cancel := context.WithTimeout(context.Background(), r.config.ShutdownDeadline)
		result := make(chan error, 1)
		go func() { result <- g.stop(ctx) }()
		var stopped bool
		stopErr, stopped = r.awaitStop(g.Generation, "stop", result)
		cancel()
		if !stopped {
			// Its users may still hold the ledger. It is closed once they
			// return; no later generation reuses it.
			go func() {
				<-result
				if g.Ledger != nil {
					_ = g.Ledger.Close()
				}
			}()
			r.mu.Lock()
			if r.current == g {
				r.current = nil
			}
			r.notifyLocked()
			r.mu.Unlock()
			r.shutdown(ErrShutdownTimeout)
			return ErrShutdownTimeout
		}
	}
	if g.Ledger != nil {
		stopErr = errors.Join(stopErr, g.Ledger.Close())
	}
	r.mu.Lock()
	if r.current == g {
		r.current = nil
	}
	r.notifyLocked()
	r.mu.Unlock()
	attrs := []any{"node", g.NodeID, "generation", g.Generation, "took", time.Since(started).Round(time.Millisecond)}
	if stopErr != nil {
		slog.Warn(fmt.Sprintf("cluster: business generation %d stopped", g.Generation), append(attrs, "error", stopErr.Error())...)
	} else {
		slog.Info(fmt.Sprintf("cluster: business generation %d stopped", g.Generation), attrs...)
	}
	return stopErr
}

// awaitStop waits for a generation's business users to finish. Past the
// shutdown timeout it reports the stop as slow together with a dump of
// every goroutine, which names the component still holding on; past the
// deadline it gives up and returns false.
func (r *Runtime) awaitStop(generation uint64, phase string, result <-chan error) (error, bool) {
	started := time.Now()
	slow := time.NewTimer(r.config.ShutdownTimeout)
	defer slow.Stop()
	deadline := time.NewTimer(r.config.ShutdownDeadline)
	defer deadline.Stop()
	for {
		select {
		case err := <-result:
			return err, true
		case <-slow.C:
			attrs := []any{"node", r.config.Coordination.NodeID, "generation", generation, "phase", phase, "waited", time.Since(started).Round(time.Millisecond), "deadline", r.config.ShutdownDeadline}
			if path, err := r.dumpGoroutines(generation, phase); err != nil {
				attrs = append(attrs, "dump_error", err.Error())
			} else {
				attrs = append(attrs, "goroutines", path)
			}
			slog.Warn(fmt.Sprintf("cluster: business generation %d stop is slow", generation), attrs...)
		case <-deadline.C:
			slog.Error(fmt.Sprintf("cluster: business generation %d did not stop by the deadline; the runtime ends so the process can be restarted", generation), "node", r.config.Coordination.NodeID, "generation", generation, "phase", phase, "waited", time.Since(started).Round(time.Millisecond))
			return ErrShutdownTimeout, false
		}
	}
}

// dumpGoroutines writes every goroutine's stack to a new file and keeps
// only the most recent few dumps.
func (r *Runtime) dumpGoroutines(generation uint64, phase string) (string, error) {
	dir := r.config.DiagnosticsDir
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	name := fmt.Sprintf("slow-%s-%s-generation-%d.txt", phase, time.Now().UTC().Format("20060102T150405.000Z"), generation)
	path := filepath.Join(dir, name)
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return "", err
	}
	werr := pprof.Lookup("goroutine").WriteTo(file, 2)
	if err := errors.Join(werr, file.Close()); err != nil {
		return "", err
	}
	if old, err := filepath.Glob(filepath.Join(dir, "slow-*.txt")); err == nil && len(old) > keptDiagnostics {
		// Names sort by time within a phase; order across phases by age.
		type aged struct {
			path string
			at   time.Time
		}
		var files []aged
		for _, p := range old {
			if info, err := os.Stat(p); err == nil {
				files = append(files, aged{p, info.ModTime()})
			}
		}
		for len(files) > keptDiagnostics {
			oldest := 0
			for i := range files {
				if files[i].at.Before(files[oldest].at) {
					oldest = i
				}
			}
			_ = os.Remove(files[oldest].path)
			files = append(files[:oldest], files[oldest+1:]...)
		}
	}
	return path, nil
}

func (r *Runtime) shutdown(reason error) {
	r.closeOnce.Do(func() {
		if reason != nil {
			slog.Error("cluster: runtime stopping: "+reason.Error(), "node", r.config.Coordination.NodeID)
		}
		r.mu.Lock()
		r.closed = true
		r.ready = false
		if reason != nil {
			r.lastError = reason
			r.failure = errors.Join(r.failure, reason)
		}
		failure := r.failure
		if r.current != nil {
			r.current.cancel()
		}
		r.cancel()
		r.notifyLocked()
		r.mu.Unlock()
		go func() {
			serviceDone := make(chan error, 1)
			go func() { serviceDone <- r.service.Close() }()
			<-r.workerDone
			serviceErr := <-serviceDone
			bookErr := r.book.Close()
			r.mu.Lock()
			r.closeError = errors.Join(failure, r.cleanupError, serviceErr, bookErr)
			r.mu.Unlock()
			close(r.closeDone)
		}()
	})
}

// FailGeneration stops this runtime when its current business application
// exits unexpectedly. It never waits for Deactivate, so an exiting runner can
// report failure before signaling the completion Deactivate must join.
func (r *Runtime) FailGeneration(generation uint64, cause error) {
	if cause == nil {
		return
	}
	r.mu.Lock()
	if r.closed || r.current == nil || r.current.Generation != generation || r.current.Context.Err() != nil {
		r.mu.Unlock()
		return
	}
	slog.Info(fmt.Sprintf("cluster: business generation %d ended the runtime: %v", r.current.Generation, cause), "node", r.config.Coordination.NodeID, "generation", r.current.Generation)
	r.closed = true
	r.ready = false
	r.lastError = cause
	r.failure = cause
	r.current.cancel()
	r.cancel()
	r.notifyLocked()
	r.mu.Unlock()
	r.shutdown(nil)
}

// Done closes once this runtime has finished shutting down, for any
// reason. Failure names the cause when it stopped on its own.
func (r *Runtime) Done() <-chan struct{} { return r.closeDone }

func (r *Runtime) Failure() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.failure
}

func (r *Runtime) Close() error {
	r.shutdown(nil)
	timer := time.NewTimer(r.config.ShutdownTimeout)
	defer timer.Stop()
	select {
	case <-r.closeDone:
		r.mu.Lock()
		defer r.mu.Unlock()
		return r.closeError
	case <-timer.C:
		return ErrShutdownTimeout
	}
}

func (r *Runtime) RPCHandler(options coordination.RPCOptions) http.Handler {
	return coordination.NewRPCHandler(r.service, options)
}
