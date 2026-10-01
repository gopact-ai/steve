package cluster

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"sync"

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

type RestartTarget struct {
	ClusterID, Holder string
	Selection         attempt.ForceRestartSelection
}
type MemberRestartError struct{ Reason string }

func (e MemberRestartError) Error() string { return e.Reason }

type memberRestarts struct {
	peer      *Peer
	active    Activation
	discovery *restartDiscovery
}

func (p *Peer) MemberRestarts(active Activation) MemberRestarts {
	return memberRestarts{peer: p, active: active, discovery: &restartDiscovery{}}
}

// Progress belongs to this activation and is shared by concurrent calls. Only
// choosing the next probe holds the lock; a slow member never holds it over RPC.
// A replacement coordinator may start afresh, but the persisted phase deadline
// still bounds discovery even across repeated replacements.
type restartDiscovery struct {
	mu    sync.Mutex
	after string
}

func (d *restartDiscovery) next(ids []string, tried map[string]bool) string {
	d.mu.Lock()
	defer d.mu.Unlock()
	start := sort.Search(len(ids), func(i int) bool { return ids[i] > d.after })
	for i := range len(ids) {
		id := ids[(start+i)%len(ids)]
		if !tried[id] {
			d.after = id
			tried[id] = true
			return id
		}
	}
	return ""
}

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
	candidates, err := m.discover(ctx, node, by, state, authority)
	if err != nil {
		return RestartTarget{}, err
	}
	return chooseRestartTarget(m.peer.Config.ClusterID, state.Revision, candidates)
}

func (m memberRestarts) Start(ctx context.Context, op attempt.ForceRestart) error {
	state, authority, err := m.authority(ctx, op.NodeID, op.By)
	if err != nil {
		return err
	}
	if op.ClusterID != m.peer.Config.ClusterID {
		return errors.New("restart belongs to another cluster")
	}
	if err := m.verifyRestartSelection(ctx, op, "", ""); err != nil {
		return err
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
