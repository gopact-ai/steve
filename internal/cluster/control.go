package cluster

import (
	"context"
	"errors"
	"fmt"

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
// err goes to the member the client finds leading. It does when this member
// is not the consensus leader, or when it could not finish the command and
// no longer leads: it lost leadership part-way, its replica stopped or its
// service closed. A leader that could not finish a command, as when a
// snapshot or configuration change did not complete in time, answers with
// its own error: the client would route the command back to this member.
// The forwarded command keeps its ID, so a step this member already
// committed is answered from its receipt.
func (r *Runtime) forwardable(err error) bool {
	if r.config.Client == nil {
		return false
	}
	if errors.Is(err, coordination.ErrNotLeader) {
		return true
	}
	return (errors.Is(err, coordination.ErrUnavailable) || errors.Is(err, coordination.ErrApplication)) && !r.service.Status().IsLeader
}

// control runs a control command on this member and, when forwardable, sends
// it to the member the client finds leading. A forwarded command that fails
// reports what this member answered as well.
func control[Request any](r *Runtime, ctx context.Context, request Request, local, forward func(context.Context, Request) (coordination.Result, error)) (coordination.Result, error) {
	r.rememberMembers()
	result, err := local(ctx, request)
	if !r.forwardable(err) {
		return result, err
	}
	result, forwardErr := forward(ctx, request)
	if forwardErr != nil {
		return result, fmt.Errorf("%w (this member answered: %v)", forwardErr, err)
	}
	return result, nil
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
	return control(r, ctx, request, r.service.Transfer, r.config.Client.Transfer)
}
func (r *Runtime) Join(ctx context.Context, request coordination.JoinRequest) (coordination.Result, error) {
	return control(r, ctx, request, r.service.Join, r.config.Client.Join)
}
func (r *Runtime) Remove(ctx context.Context, request coordination.RemoveRequest) (coordination.Result, error) {
	return control(r, ctx, request, r.service.Remove, r.config.Client.Remove)
}
func (r *Runtime) SetAutoFailover(ctx context.Context, request coordination.PolicyRequest) (coordination.Result, error) {
	return control(r, ctx, request, r.service.SetAutoFailover, r.config.Client.SetAutoFailover)
}
func (r *Runtime) SetEligibility(ctx context.Context, request coordination.EligibilityRequest) (coordination.Result, error) {
	return control(r, ctx, request, r.service.SetEligibility, r.config.Client.SetEligibility)
}

func (r *Runtime) MemberNames() map[string]string { return r.service.MemberNames() }

func (r *Runtime) Rename(ctx context.Context, request coordination.RenameRequest) (coordination.Result, error) {
	return control(r, ctx, request, r.service.Rename, r.config.Client.Rename)
}

func (r *Runtime) SetVoting(ctx context.Context, request coordination.VotingRequest) (coordination.Result, error) {
	return control(r, ctx, request, r.service.SetVoting, r.config.Client.SetVoting)
}
