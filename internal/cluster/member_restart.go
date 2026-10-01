package cluster

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"time"

	"github.com/gopact-ai/steve/internal/attempt"
	"github.com/gopact-ai/steve/internal/coordination"
	"github.com/gopact-ai/steve/internal/ledger"
	"github.com/gopact-ai/steve/internal/nodewire"
	"github.com/gopact-ai/steve/internal/sshconnect"
)

// MemberRestarts is the active coordinator's port to the members that own
// machine links. It carries public operation identity, never SSH credentials.
type MemberRestarts interface {
	Find(context.Context, string, string) (RestartTarget, error)
	Start(context.Context, attempt.ForceRestart) error
	Status(context.Context, attempt.ForceRestart) (sshconnect.MemberRestart, error)
}

type RestartTarget struct{ ClusterID, Holder string }
type MemberRestartError struct{ Reason string }

func (e MemberRestartError) Error() string { return e.Reason }

type memberRestarts struct {
	peer   *Peer
	active Activation
}

func (p *Peer) MemberRestarts(active Activation) MemberRestarts { return memberRestarts{p, active} }

func (m memberRestarts) authority(ctx context.Context, node, by string) (coordination.State, nodewire.SessionAuthority, error) {
	var authority nodewire.SessionAuthority
	if err := m.active.Context.Err(); err != nil {
		return coordination.State{}, authority, err
	}
	state, err := m.peer.Runtime.Load().ReadState(ctx)
	if err != nil {
		return state, authority, err
	}
	if state.Coordinator.NodeID != m.active.NodeID || state.Coordinator.Epoch != m.active.Assignment.Epoch || state.WriterGeneration != m.active.WriterGeneration {
		return state, authority, coordination.ErrStaleEpoch
	}
	authority = nodewire.SessionAuthority{ClusterID: m.peer.Config.ClusterID, CoordinatorNodeID: m.active.NodeID, CoordinatorEpoch: m.active.Assignment.Epoch, WriterGeneration: m.active.WriterGeneration}
	if err := checkRestartTarget(state, node); err != nil {
		return state, authority, err
	}
	if err := m.active.Ledger.Read(ctx, func(tx *ledger.ReadTx) error { return restartOwner(tx, by) }); err != nil {
		return state, authority, err
	}
	return state, authority, nil
}

func (m memberRestarts) Find(ctx context.Context, node, by string) (RestartTarget, error) {
	state, authority, err := m.authority(ctx, node, by)
	if err != nil {
		return RestartTarget{}, err
	}
	if _, err := (peerSSHBackend{peer: m.peer}).RestartTarget(ctx, node); err == nil {
		return RestartTarget{m.peer.Config.ClusterID, m.peer.Config.NodeID}, nil
	}
	ids := make([]string, 0, len(state.Members))
	for id := range state.Members {
		if id != m.peer.Config.NodeID && id != node && !state.Removing[id] {
			ids = append(ids, id)
		}
	}
	sort.Strings(ids)
	for _, id := range ids {
		if ctx.Err() != nil {
			return RestartTarget{}, ctx.Err()
		}
		query := restartQuery(node, "", "", "", by, authority)
		var answer memberRestartResponse
		probeCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
		err := m.peer.peerJSON(probeCtx, state.Members[id], http.MethodGet, clusterMemberRestartPath+"?"+query.Encode(), nil, &answer)
		cancel()
		if err == nil && answer.Restartable {
			return RestartTarget{m.peer.Config.ClusterID, id}, nil
		}
	}
	return RestartTarget{}, MemberRestartError{"restart_no_holder"}
}

func (m memberRestarts) Start(ctx context.Context, op attempt.ForceRestart) error {
	state, authority, err := m.authority(ctx, op.NodeID, op.By)
	if err != nil {
		return err
	}
	if op.ClusterID != m.peer.Config.ClusterID {
		return errors.New("restart belongs to another cluster")
	}
	holder, ok := state.Members[op.Holder]
	if !ok || state.Removing[op.Holder] {
		return MemberRestartError{"restart_no_holder"}
	}
	var answer memberRestartResponse
	return m.peer.peerJSONStatus(ctx, holder, http.MethodPost, clusterMemberRestartPath, memberRestartRequest{Operation: op, Authority: authority}, &answer, http.StatusAccepted)
}

func (m memberRestarts) Status(ctx context.Context, op attempt.ForceRestart) (sshconnect.MemberRestart, error) {
	state, authority, err := m.authority(ctx, op.NodeID, op.By)
	if err != nil {
		return sshconnect.MemberRestart{}, err
	}
	holder, ok := state.Members[op.Holder]
	if !ok || state.Removing[op.Holder] {
		return sshconnect.MemberRestart{State: "lost"}, nil
	}
	if op.ClusterID != m.peer.Config.ClusterID {
		return sshconnect.MemberRestart{}, errors.New("restart belongs to another cluster")
	}
	var answer memberRestartResponse
	query := restartQuery(op.NodeID, op.ID, op.PlanID, op.Kind, op.By, authority)
	err = m.peer.peerJSON(ctx, holder, http.MethodGet, clusterMemberRestartPath+"?"+query.Encode(), nil, &answer)
	return answer.Operation, err
}

func restartQuery(node, request, plan, kind, by string, a nodewire.SessionAuthority) url.Values {
	return url.Values{"node": {node}, "request": {request}, "plan": {plan}, "kind": {kind}, "by": {by}, "epoch": {strconv.FormatUint(a.CoordinatorEpoch, 10)}, "generation": {strconv.FormatUint(a.WriterGeneration, 10)}}
}
