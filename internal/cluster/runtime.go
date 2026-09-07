// Package cluster combines a durable coordinator assignment with the replicated
// application ledger. Business activation is a separate lifecycle from Raft.
package cluster

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
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
	Activate        func(context.Context, Activation) (Deactivate, error)
	PollInterval    time.Duration
	ShutdownTimeout time.Duration
}

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

	mu           sync.Mutex
	current      *generation
	sequence     uint64
	restores     uint64
	restoring    bool
	ready        bool
	closed       bool
	lastError    error
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
		r.current.cancel()
	}
	r.notifyLocked()
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
		g.cancel()
		r.ready = false
		r.lastError = err
		r.notifyLocked()
	}
}

func (r *Runtime) run() {
	defer close(r.workerDone)
	ticker := time.NewTicker(r.config.PollInterval)
	defer ticker.Stop()
	for {
		if r.ctx.Err() != nil {
			err := r.retire()
			r.mu.Lock()
			r.cleanupError = errors.Join(r.cleanupError, err)
			r.mu.Unlock()
			return
		}
		if !r.service.Status().Healthy {
			r.shutdown(coordination.ErrApplication)
			continue
		}
		ctx, cancel := context.WithTimeout(r.ctx, r.config.Coordination.ApplyTimeout)
		state, err := r.ReadState(ctx)
		if err == nil && (state.Coordinator.NodeID != r.config.Coordination.NodeID || state.Coordinator.Epoch == 0 || state.Voters[r.config.Coordination.NodeID] == "") {
			err = coordination.ErrNotCoordinator
		}
		var version uint64
		if err == nil {
			r.mu.Lock()
			if r.restoring {
				err = coordination.ErrNotReady
			}
			r.mu.Unlock()
		}
		if err == nil {
			version, err = r.waitApplied(ctx, state.AppliedIndex, state.AppVersion)
		}
		cancel()
		if err != nil {
			r.invalidate(err, false)
			if err := r.retire(); err != nil {
				r.shutdown(err)
			}
		} else {
			r.mu.Lock()
			current := r.current
			keep := current != nil && current.Assignment == state.Coordinator && current.WriterGeneration == state.WriterGeneration && current.restore == r.restores && current.Context.Err() == nil
			r.mu.Unlock()
			if !keep {
				if err := r.retire(); err != nil {
					r.shutdown(err)
				} else if err := r.activate(state.Coordinator, version, state.WriterGeneration); err != nil {
					r.shutdown(err)
				}
			}
		}
		select {
		case <-r.ctx.Done():
		case <-ticker.C:
		}
	}
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
	ctx, cancel := context.WithCancel(r.ctx)
	g := &generation{Activation: Activation{Runtime: r, NodeID: r.config.Coordination.NodeID, Assignment: assignment, Version: version, Generation: r.sequence, Context: ctx}, cancel: cancel, restore: r.restores}
	r.current = g
	r.mu.Unlock()
	var random [16]byte
	if _, err := rand.Read(random[:]); err != nil {
		return err
	}
	fence, err := r.beginWriter(ctx, coordination.WriterRequest{ID: "writer-" + hex.EncodeToString(random[:]), CallerNodeID: g.NodeID, CoordinatorEpoch: assignment.Epoch, ExpectedGeneration: expectedWriter})
	if err != nil {
		r.revoke(g, err)
		return nil
	}
	if _, err := r.waitApplied(ctx, fence.Index, fence.AppVersion); err != nil {
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
	r.lastError = nil
	r.notifyLocked()
	return nil
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
			timer := time.NewTimer(r.config.ShutdownTimeout)
			defer timer.Stop()
			select {
			case finished := <-result:
				return finished.stop, finished.err
			case <-timer.C:
				r.shutdown(ErrShutdownTimeout)
				finished := <-result
				return finished.stop, finished.err
			}
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
	r.mu.Unlock()
	var stopErr error
	if g.stop != nil {
		ctx, cancel := context.WithTimeout(context.Background(), r.config.ShutdownTimeout)
		result := make(chan error, 1)
		go func() { result <- g.stop(ctx) }()
		select {
		case stopErr = <-result:
		case <-ctx.Done():
			r.shutdown(ErrShutdownTimeout)
			stopErr = errors.Join(ErrShutdownTimeout, <-result)
		}
		cancel()
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
	return stopErr
}

func (r *Runtime) shutdown(reason error) {
	r.closeOnce.Do(func() {
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
