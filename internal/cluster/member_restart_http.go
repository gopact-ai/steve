package cluster

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/gopact-ai/steve/internal/attempt"
	"github.com/gopact-ai/steve/internal/coordination"
	"github.com/gopact-ai/steve/internal/ledger"
	"github.com/gopact-ai/steve/internal/nodewire"
	"github.com/gopact-ai/steve/internal/platformconfig"
	"github.com/gopact-ai/steve/internal/sshconnect"
)

const clusterMemberRestartPath = "/cluster/member-restart"
const clusterMemberRestartClaimPath = "/cluster/member-restart/claim"

type memberRestartRequest struct {
	Verify    bool                      `json:"verify,omitempty"`
	Operation attempt.ForceRestart      `json:"operation"`
	Authority nodewire.SessionAuthority `json:"authority"`
	PlanID    string                    `json:"plan_id,omitempty"`
	Kind      string                    `json:"kind,omitempty"`
}
type memberRestartResponse struct {
	MembershipRevision uint64                   `json:"membership_revision"`
	Restartable        bool                     `json:"restartable"`
	Operation          sshconnect.MemberRestart `json:"operation"`
}

func checkRestartTarget(state coordination.State, node string) error {
	if node == state.Coordinator.NodeID {
		return MemberRestartError{"restart_self"}
	}
	if _, ok := state.Members[node]; !ok || state.Removing[node] {
		return MemberRestartError{"restart_no_holder"}
	}
	return nil
}

func restartOwner(tx ledger.Reader, by string) error {
	var raw string
	if err := tx.QueryRow("SELECT data FROM bindings WHERE kind=? AND id=?", "document", "platform-configuration").Scan(&raw); err != nil {
		return err
	}
	var declaration platformconfig.Declaration
	if err := json.Unmarshal([]byte(raw), &declaration); err != nil {
		return err
	}
	if by == "" || declaration.Settings.Gateway.OwnerID != by {
		return MemberRestartError{"restart_permission"}
	}
	return nil
}

// restartAuthority uses a quorum observation and catches a coordinator change
// before any new claim. Once the claim commits it is the one execution right.
func (p *Peer) restartAuthority(ctx context.Context, a nodewire.SessionAuthority, node string) (coordination.State, error) {
	runtime := p.Runtime.Load()
	if runtime == nil {
		return coordination.State{}, coordination.ErrUnavailable
	}
	state, err := runtime.ReadState(ctx)
	if err != nil {
		return state, err
	}
	if a.ClusterID != p.Config.ClusterID || a.CoordinatorNodeID != state.Coordinator.NodeID || a.CoordinatorEpoch != state.Coordinator.Epoch || a.WriterGeneration != state.WriterGeneration {
		return state, coordination.ErrStaleEpoch
	}
	if err := checkRestartTarget(state, node); err != nil {
		return state, err
	}
	return state, nil
}

func (p *Peer) serveMemberRestart(w http.ResponseWriter, r *http.Request) {
	if (r.Method != http.MethodGet && r.Method != http.MethodPost) || !p.authorizedPeerRequest(r, "member-restart") {
		http.Error(w, "coordinator authorization required", http.StatusForbidden)
		return
	}
	caller, err := coordination.CertificateIdentity(r.TLS.PeerCertificates[0])
	if err != nil {
		http.Error(w, "invalid peer identity", http.StatusForbidden)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()
	req := memberRestartRequest{}
	if r.Method == http.MethodPost {
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&req); err != nil {
			http.Error(w, "invalid request", http.StatusBadRequest)
			return
		}
	} else {
		q := r.URL.Query()
		epoch, _ := strconv.ParseUint(q.Get("epoch"), 10, 64)
		generation, _ := strconv.ParseUint(q.Get("generation"), 10, 64)
		req.Authority = nodewire.SessionAuthority{ClusterID: caller.ClusterID, CoordinatorNodeID: caller.NodeID, CoordinatorEpoch: epoch, WriterGeneration: generation}
		req.Operation = attempt.ForceRestart{NodeID: q.Get("node"), ID: q.Get("request"), PlanID: q.Get("plan"), Kind: q.Get("kind"), By: q.Get("by")}
	}
	state, err := p.restartAuthority(ctx, req.Authority, req.Operation.NodeID)
	if err != nil || caller.NodeID != state.Coordinator.NodeID {
		http.Error(w, "current coordinator authorization required", http.StatusForbidden)
		return
	}
	if err := p.waitRestartReplica(ctx, state); err != nil {
		HTTPError(w, err)
		return
	}
	if err := p.Runtime.Load().Ledger().Read(ctx, func(tx *ledger.ReadTx) error { return restartOwner(tx, req.Operation.By) }); err != nil {
		http.Error(w, "restart owner authorization required", http.StatusForbidden)
		return
	}
	service, err := p.restartService()
	if err != nil {
		HTTPError(w, err)
		return
	}
	status, err := service.RestartStatus(ctx, req.Operation.NodeID)
	if err != nil {
		HTTPError(w, err)
		return
	}
	answer := memberRestartResponse{Restartable: status.Restartable, MembershipRevision: state.Revision}
	if status.Activity != nil {
		answer.Operation = *status.Activity
	}
	if r.Method == http.MethodGet && req.Operation.ID == "" {
		WriteJSON(w, answer)
		return
	}
	if r.Method == http.MethodPost {
		if req.Operation.Holder != p.Config.NodeID || req.Operation.ClusterID != p.Config.ClusterID || req.Operation.Selection.MembershipRevision != state.Revision || req.Operation.Selection.MembershipRevision == 0 || !status.Restartable && req.Operation.Selection.JoinPlanID == "" {
			http.Error(w, "restart does not belong to this link holder", http.StatusForbidden)
			return
		}
		answer.Operation, err = service.BeginMemberRestart(ctx, req.Operation.NodeID, req.Operation.ID, req.Operation.ClusterID, func(claimCtx context.Context, plan, kind string) (bool, error) {
			req.PlanID, req.Kind = plan, kind
			return p.claimMemberRestart(claimCtx, req)
		}, func(checkCtx context.Context, plan, kind string) error {
			check := req
			check.PlanID, check.Kind, check.Verify = plan, kind, true
			yes, err := p.claimMemberRestart(checkCtx, check)
			if err == nil && !yes {
				return errors.New("restart validation was not accepted")
			}
			return err
		}, sshconnect.MemberRestartChoice{PlanID: req.Operation.Selection.JoinPlanID, Kind: req.Operation.Selection.JoinKind})
	} else {
		answer.Operation, err = service.MemberRestartStatus(ctx, req.Operation.NodeID, req.Operation.ID, req.Operation.PlanID, req.Operation.Kind)
	}
	if err != nil {
		HTTPError(w, err)
		return
	}
	if r.Method == http.MethodPost {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		w.WriteHeader(http.StatusAccepted)
	}
	WriteJSON(w, answer)
}

func (p *Peer) restartService() (*sshconnect.Service, error) {
	p.Mu.Lock()
	defer p.Mu.Unlock()
	if p.closing {
		return nil, coordination.ErrUnavailable
	}
	return p.localSSHLocked(), nil
}

func (p *Peer) waitRestartReplica(ctx context.Context, state coordination.State) error {
	ticker := time.NewTicker(5 * time.Millisecond)
	defer ticker.Stop()
	for {
		version, err := p.Runtime.Load().Ledger().ReplicaVersion()
		if err != nil {
			return err
		}
		if version >= state.AppVersion {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

func (p *Peer) claimMemberRestart(ctx context.Context, req memberRestartRequest) (bool, error) {
	state, err := p.restartAuthority(ctx, req.Authority, req.Operation.NodeID)
	if err != nil {
		return false, err
	}
	coordinator, ok := state.Members[state.Coordinator.NodeID]
	if !ok {
		return false, coordination.ErrUnavailable
	}
	var answer struct {
		Claimed  bool `json:"claimed"`
		Verified bool `json:"verified"`
	}
	err = p.peerJSON(ctx, coordinator, http.MethodPost, clusterMemberRestartClaimPath, req, &answer)
	if req.Verify {
		return answer.Verified, err
	}
	return answer.Claimed, err
}

func (p *Peer) serveMemberRestartClaim(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost || !p.authorizedPeerRequest(r, "member-restart-claim") {
		http.Error(w, "holder authorization required", http.StatusForbidden)
		return
	}
	caller, err := coordination.CertificateIdentity(r.TLS.PeerCertificates[0])
	if err != nil {
		http.Error(w, "invalid peer identity", http.StatusForbidden)
		return
	}
	var req memberRestartRequest
	if err = json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&req); err != nil || req.Operation.Holder != caller.NodeID {
		http.Error(w, "invalid restart claim", http.StatusForbidden)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()
	state, err := p.restartAuthority(ctx, req.Authority, req.Operation.NodeID)
	if err != nil || state.Coordinator.NodeID != p.Config.NodeID {
		http.Error(w, "coordinator changed", http.StatusForbidden)
		return
	}
	if req.Operation.Selection.MembershipRevision == 0 || state.Revision != req.Operation.Selection.MembershipRevision {
		http.Error(w, "restart member view changed", http.StatusConflict)
		return
	}
	if _, member := state.Members[caller.NodeID]; !member || state.Removing[caller.NodeID] {
		http.Error(w, "restart holder is no longer a member", http.StatusForbidden)
		return
	}
	active, err := p.Runtime.Load().WaitReady(ctx)
	if err != nil {
		HTTPError(w, err)
		return
	}
	if active.Assignment.Epoch != req.Authority.CoordinatorEpoch || active.WriterGeneration != req.Authority.WriterGeneration {
		HTTPError(w, coordination.ErrStaleEpoch)
		return
	}
	authorize := func(tx ledger.Reader, op attempt.ForceRestart) error {
		if op.ClusterID != p.Config.ClusterID || op.Holder != caller.NodeID || op.NodeID == p.Config.NodeID {
			return errors.New("restart identity differs from this claim")
		}
		return restartOwner(tx, op.By)
	}
	service := attempt.New(active.Ledger)
	if req.Verify {
		if err := p.waitRestartReplica(ctx, state); err != nil {
			HTTPError(w, err)
			return
		}
		control := memberRestarts{peer: p, active: active, discovery: &restartDiscovery{}}
		if err := control.verifyRestartSelection(ctx, req.Operation, req.PlanID, req.Kind); err != nil {
			HTTPError(w, err)
			return
		}
		if err := service.VerifyForceRestart(ctx, req.Operation, req.PlanID, req.Kind, authorize); err != nil {
			HTTPError(w, err)
			return
		}
		WriteJSON(w, map[string]bool{"verified": true})
		return
	}
	claimed, err := service.ClaimForceRestart(ctx, req.Operation, req.PlanID, req.Kind, authorize)
	if err != nil {
		HTTPError(w, err)
		return
	}
	WriteJSON(w, map[string]bool{"claimed": claimed})
}
