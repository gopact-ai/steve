package cluster

import (
	"context"
	"errors"

	"github.com/gopact-ai/steve/internal/coordination"
)

func (r *Runtime) TransportPeers() map[string]string { return r.service.TransportPeers() }

func (r *Runtime) rememberMembers() {
	if r.config.Client == nil {
		return
	}
	var members []coordination.Member
	for _, member := range r.service.Status().Members {
		members = append(members, member)
	}
	r.config.Client.RememberMembers(members)
}

// forwardable reports whether a control command this member answered with
// err goes to the member the client finds leading: this member is not the
// consensus leader, or it could not finish the command now, as when it lost
// leadership part-way or its replica stopped. The forwarded command keeps its
// ID, so a step this member already committed is answered from its receipt.
func (r *Runtime) forwardable(err error) bool {
	return r.config.Client != nil && (errors.Is(err, coordination.ErrNotLeader) || errors.Is(err, coordination.ErrUnavailable))
}

func (r *Runtime) ReadState(ctx context.Context) (coordination.State, error) {
	if r.ctx.Err() != nil {
		return coordination.State{}, ErrInactive
	}
	r.rememberMembers()
	state, err := r.service.ReadState(ctx)
	if errors.Is(err, coordination.ErrNotLeader) && r.config.Client != nil {
		return r.config.Client.ReadState(ctx)
	}
	return state, err
}

func (r *Runtime) propose(ctx context.Context, command coordination.AppCommand) (coordination.Result, error) {
	r.rememberMembers()
	result, err := r.service.ApplyApp(ctx, command)
	if errors.Is(err, coordination.ErrNotLeader) && r.config.Client != nil {
		return r.config.Client.ApplyApp(ctx, command)
	}
	return result, err
}

func (r *Runtime) beginWriter(ctx context.Context, request coordination.WriterRequest) (coordination.Result, error) {
	r.rememberMembers()
	result, err := r.service.BeginWriter(ctx, request)
	if errors.Is(err, coordination.ErrNotLeader) && r.config.Client != nil {
		return r.config.Client.BeginWriter(ctx, request)
	}
	return result, err
}

func (r *Runtime) Transfer(ctx context.Context, request coordination.TransferRequest) (coordination.Result, error) {
	r.rememberMembers()
	result, err := r.service.Transfer(ctx, request)
	if r.forwardable(err) {
		return r.config.Client.Transfer(ctx, request)
	}
	return result, err
}
func (r *Runtime) Join(ctx context.Context, request coordination.JoinRequest) (coordination.Result, error) {
	r.rememberMembers()
	result, err := r.service.Join(ctx, request)
	if r.forwardable(err) {
		return r.config.Client.Join(ctx, request)
	}
	return result, err
}
func (r *Runtime) Remove(ctx context.Context, request coordination.RemoveRequest) (coordination.Result, error) {
	r.rememberMembers()
	result, err := r.service.Remove(ctx, request)
	if r.forwardable(err) {
		return r.config.Client.Remove(ctx, request)
	}
	return result, err
}
func (r *Runtime) SetAutoFailover(ctx context.Context, request coordination.PolicyRequest) (coordination.Result, error) {
	r.rememberMembers()
	result, err := r.service.SetAutoFailover(ctx, request)
	if r.forwardable(err) {
		return r.config.Client.SetAutoFailover(ctx, request)
	}
	return result, err
}
func (r *Runtime) SetEligibility(ctx context.Context, request coordination.EligibilityRequest) (coordination.Result, error) {
	r.rememberMembers()
	result, err := r.service.SetEligibility(ctx, request)
	if r.forwardable(err) {
		return r.config.Client.SetEligibility(ctx, request)
	}
	return result, err
}

func (r *Runtime) MemberNames() map[string]string { return r.service.MemberNames() }

func (r *Runtime) Rename(ctx context.Context, request coordination.RenameRequest) (coordination.Result, error) {
	r.rememberMembers()
	result, err := r.service.Rename(ctx, request)
	if r.forwardable(err) {
		return r.config.Client.Rename(ctx, request)
	}
	return result, err
}

func (r *Runtime) SetVoting(ctx context.Context, request coordination.VotingRequest) (coordination.Result, error) {
	r.rememberMembers()
	result, err := r.service.SetVoting(ctx, request)
	if r.forwardable(err) {
		return r.config.Client.SetVoting(ctx, request)
	}
	return result, err
}
