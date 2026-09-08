package coordination

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"sort"
	"time"

	"github.com/hashicorp/raft"
)

// membershipRevoker is what the failover loop asks of a stream layer after
// membership changed: drop pooled connections whose peer is no longer
// authorized. TLSStreamLayer has it; a loopback-only layer need not, and
// then nothing is revoked.
type membershipRevoker interface {
	RevokeUnauthorized()
}

func (s *Service) run() {
	defer s.workers.Done()
	ticker := time.NewTicker(s.config.ProbeInterval)
	defer ticker.Stop()
	var unavailableSince time.Time
	var observed Assignment
	for {
		select {
		case <-s.ctx.Done():
			return
		case <-s.fsm.failed:
			s.closed.Store(true)
			s.raft.Shutdown()
			return
		case <-ticker.C:
		case <-s.fsm.membershipChanged:
		}
		if stream, ok := s.config.StreamLayer.(membershipRevoker); ok {
			stream.RevokeUnauthorized()
		}
		if s.raft.State() != raft.Leader {
			unavailableSince = time.Time{}
			observed = Assignment{}
			continue
		}
		state := s.fsm.read()
		if state.Coordinator.Epoch == 0 {
			if s.config.Bootstrap {
				s.initialize()
			}
			continue
		}
		if !state.AutoFailover || !state.CanAutoFailover() || state.Coordinator.NodeID == s.config.NodeID {
			unavailableSince = time.Time{}
			observed = state.Coordinator
			continue
		}
		if observed != state.Coordinator {
			observed = state.Coordinator
			unavailableSince = time.Time{}
		}
		member := state.Members[state.Coordinator.NodeID]
		if _, err := s.probe(s.ctx, member); err == nil {
			unavailableSince = time.Time{}
			continue
		}
		if unavailableSince.IsZero() {
			unavailableSince = time.Now()
			continue
		}
		if time.Since(unavailableSince) < s.config.FailoverTimeout {
			continue
		}
		s.tryFailover(observed)
	}
}

func (s *Service) initialize() {
	s.opMu.Lock()
	defer s.opMu.Unlock()
	ctx, cancel := context.WithTimeout(s.ctx, s.config.ApplyTimeout)
	defer cancel()
	member := Member{NodeID: s.config.NodeID, Address: string(s.transport.LocalAddr()), APIAddress: s.config.APIAddress, Name: s.config.Name, AutoEligible: false, FailureDomain: s.config.FailureDomain, StorageLevel: s.config.StorageLevel}
	s.submit(ctx, command{Kind: "initialize", ID: "initialize/" + s.config.ClusterID, Actor: "system", Fingerprint: fingerprint("initialize", member), Member: member})
}

func (s *Service) tryFailover(expected Assignment) {
	s.opMu.Lock()
	defer s.opMu.Unlock()
	ctx, cancel := context.WithTimeout(s.ctx, s.config.ApplyTimeout)
	defer cancel()
	if err := s.barrier(ctx); err != nil {
		return
	}
	state := s.fsm.read()
	if state.Coordinator != expected || !state.AutoFailover || !state.CanAutoFailover() {
		return
	}
	if _, err := s.probe(ctx, state.Members[state.Coordinator.NodeID]); err == nil {
		return
	}
	var candidates []string
	for id := range state.Voters {
		if id != state.Coordinator.NodeID && state.Members[id].AutoEligible && !state.Removing[id] {
			candidates = append(candidates, id)
		}
	}
	sort.Strings(candidates)
	// Prefer the internal leader when eligible: its FSM has already crossed the
	// quorum barrier. Other candidates need the same verified applied progress.
	for i, id := range candidates {
		if id == s.config.NodeID {
			candidates[0], candidates[i] = candidates[i], candidates[0]
			break
		}
	}
	for _, id := range candidates {
		progress, err := s.probe(ctx, state.Members[id])
		if err != nil || progress.AppliedIndex < state.AppliedIndex || progress.AppVersion < state.AppVersion {
			continue
		}
		var random [16]byte
		if _, err := rand.Read(random[:]); err != nil {
			return
		}
		_, err = s.transfer(ctx, TransferRequest{ID: "automatic/" + hex.EncodeToString(random[:]), Actor: "system", ExpectedEpoch: expected.Epoch, TargetNodeID: id, Reason: "coordinator_unreachable"}, true)
		if err == nil {
			return
		}
	}
}
