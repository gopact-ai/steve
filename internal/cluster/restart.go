package cluster

import (
	"fmt"
	"log/slog"
)

// RestartGeneration revokes one application instance while keeping the peer
// and consensus service running. The normal activation loop joins its users,
// commits a new writer fence, and rebuilds every business store from the ledger.
func (r *Runtime) RestartGeneration(generation uint64) error {
	return r.RequestRebuild(generation, nil)
}

// RequestRebuild fences uncertain application caches without disabling a
// healthy consensus replica. New stores are opened only after the old users
// stop and a fresh writer generation is committed. Replica storage failures
// still stop the service through its FSM health path.
func (r *Runtime) RequestRebuild(generation uint64, cause error) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed || r.current == nil || r.current.Generation != generation || r.current.Context.Err() != nil {
		return ErrInactive
	}
	r.ready = false
	if cause != nil {
		r.lastError = cause
		slog.Error(fmt.Sprintf("cluster: rebuilding application generation %d: %v", generation, cause), "generation", generation)
	}
	r.current.cancel()
	r.notifyLocked()
	return nil
}
