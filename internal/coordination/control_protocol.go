package coordination

import (
	"context"
	"fmt"
	"sort"
)

// requireControlProtocol must run before any new-semantic log entry is written,
// not just before its configuration change. Before first activation, every
// admitted or configured replica counts, including nonvoters and incomplete
// joins. Checking only a quorum would let an old follower accept the log but
// apply different state.
// Callers hold admissionMu through submission, so a concurrent Join cannot
// insert an unchecked member between this check and activation.
func (s *Service) requireControlProtocol(ctx context.Context, state State) error {
	// The persisted activation floor and Join's check preserve capability
	// without requiring offline bystanders to return for vote maintenance or
	// transfer. Target progress and quorum checks still apply at the caller.
	// Administrator-forced downgrade is unsupported and not prevented here.
	if state.RequiredControlProtocol >= ControlProtocolVersion {
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, s.config.ApplyTimeout)
	defer cancel()
	for id, address := range state.Replicas {
		if member, ok := state.Members[id]; !ok || member.Address != address {
			return fmt.Errorf("%w: cannot verify control protocol for replica %s", ErrNotReady, id)
		}
	}
	ids := make([]string, 0, len(state.Members))
	for id := range state.Members {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		progress, err := s.probe(ctx, state.Members[id])
		if err != nil {
			return fmt.Errorf("%w: cannot verify control protocol for replica %s: %v", ErrNotReady, id, err)
		}
		if progress.ControlProtocol < ControlProtocolVersion {
			return fmt.Errorf("%w: replica %s has control protocol %d, need %d; upgrade all replicas before using voting changes or a nonvoter coordinator", ErrNotReady, id, progress.ControlProtocol, ControlProtocolVersion)
		}
	}
	return nil
}

func (s *Service) prepareVoting(ctx context.Context, request VotingRequest, fp string) error {
	s.admissionMu.Lock()
	defer s.admissionMu.Unlock()
	if err := s.barrier(ctx); err != nil {
		return err
	}
	if err := s.requireControlProtocol(ctx, s.fsm.read()); err != nil {
		return err
	}
	_, err := s.submit(ctx, command{Kind: "voting_prepare", ID: request.ID + "/prepare", Actor: request.Actor, Fingerprint: fp, Voting: request})
	return err
}

func (s *Service) prepareJoin(ctx context.Context, request *JoinRequest, fp string) error {
	s.admissionMu.Lock()
	defer s.admissionMu.Unlock()
	if err := s.barrier(ctx); err != nil {
		return err
	}
	progress, err := s.probe(ctx, request.Member)
	if err != nil {
		return fmt.Errorf("verify joining node: %w", err)
	}
	required := s.fsm.read().RequiredControlProtocol
	if progress.ControlProtocol < required {
		return fmt.Errorf("%w: joining node has control protocol %d, need %d; downgrade is unsupported", ErrNotReady, progress.ControlProtocol, required)
	}
	request.Member.FailureDomain = progress.FailureDomain
	request.Member.StorageLevel = progress.StorageLevel
	if request.Member.StorageLevel != "restricted" && request.Member.StorageLevel != "sealed" {
		return fmt.Errorf("%w: a full ledger replica requires explicit restricted storage authorization", ErrInvalid)
	}
	if request.Member.FailureDomain == "" {
		return fmt.Errorf("%w: joining node has no verified physical failure domain", ErrInvalid)
	}
	if s.config.AuthorizeReplica != nil {
		if err := s.config.AuthorizeReplica(ctx, request.Member); err != nil {
			return fmt.Errorf("%w: ledger replica authorization: %v", ErrInvalid, err)
		}
	}
	_, err = s.submit(ctx, command{Kind: "join_prepare", ID: request.ID + "/prepare", Actor: request.Actor, Fingerprint: fp, Member: request.Member, MemberControlProtocol: progress.ControlProtocol})
	return err
}
