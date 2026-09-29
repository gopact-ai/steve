package cluster

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"slices"
	"time"

	adminsvc "github.com/gopact-ai/steve/internal/admin"
	"github.com/gopact-ai/steve/internal/coordination"
	"github.com/gopact-ai/steve/internal/i18n"
	"github.com/gopact-ai/steve/internal/sshconnect"
)

// clusterNodeRestartPath is where the coordinator keeps the restarts that
// members ran among the fleet's events.
const clusterNodeRestartPath = "/cluster/node-restart"

// RestartTarget names how a member is reached to restart its peer: the
// alias of the link this node keeps to it. This node's own process is not
// restarted over SSH; a member without a link here is restarted, and its
// automatic start watched, by the node that added it over SSH, which
// keeps its link.
func (b peerSSHBackend) RestartTarget(ctx context.Context, nodeID string) (string, error) {
	text := b.peer.text.For(ctx)
	if nodeID == b.peer.Config.NodeID {
		return "", errors.New(text.T(i18n.ClusterRestartSelf))
	}
	link, ok := b.peer.link(nodeID)
	if !ok {
		return "", errors.New(text.T(i18n.ClusterRestartElsewhere))
	}
	return link.Alias, nil
}

// Restarted waits until the machine answers the cluster again, on
// whichever build it runs: a restart keeps the program. The link is left
// as it is; its end on the machine is a process of its own and was not
// stopped.
func (b peerSSHBackend) Restarted(ctx context.Context, nodeID string) error {
	ctx, cancel := b.peer.whileOpen(ctx)
	defer cancel()
	sshconnect.Report(ctx, b.peer.text.For(ctx).T(i18n.ClusterRestartAwait))
	return awaitAnswerWithin(ctx, b.peer.askBuild(nodeID), awaitAskLimit, time.Second)
}

// awaitAnswerWithin asks the machine, every interval and each ask bounded
// by askLimit, until it answers at all.
func awaitAnswerWithin(ctx context.Context, ask func(context.Context) (string, error), askLimit, interval time.Duration) error {
	return awaitWithin(ctx, ask, askLimit, interval, func(string) bool { return true })
}

// Watched lists the machines this node keeps a link to that are still
// members, other than itself; none before its runtime is up. A member
// being removed is no longer watched.
func (b peerSSHBackend) Watched(context.Context) []string {
	runtime := b.peer.Runtime.Load()
	if runtime == nil {
		return nil
	}
	status := runtime.Status()
	b.peer.Mu.RLock()
	defer b.peer.Mu.RUnlock()
	return watchedMembers(status.State, b.peer.Config.Links, b.peer.Config.NodeID)
}

// watchedMembers is which machines with a link here automatic start
// watches, in order: members of the cluster in state, other than self,
// that are not being removed.
func watchedMembers(state coordination.State, links map[string]PeerLink, self string) []string {
	var watched []string
	for nodeID := range links {
		if _, member := state.Members[nodeID]; member && nodeID != self && !state.Removing[nodeID] {
			watched = append(watched, nodeID)
		}
	}
	slices.Sort(watched)
	return watched
}

// Answers asks the machine's own cluster service, over the link, whether
// it runs: one ask, bounded as each ask after a restart is.
func (b peerSSHBackend) Answers(ctx context.Context, nodeID string) bool {
	ctx, cancel := context.WithTimeout(ctx, awaitAskLimit)
	defer cancel()
	_, err := b.peer.askBuild(nodeID)(ctx)
	return err == nil
}

// Reachable is whether the SSH session this node keeps to the machine is
// up: a start runs over SSH, and a machine this node cannot reach is not
// one whose peer it can find gone.
func (b peerSSHBackend) Reachable(_ context.Context, nodeID string) bool {
	return b.peer.LinkStatuses()[nodeID].Connected
}

// RecordRestart hands a restart to the coordinator, which keeps it among
// the fleet's events; this node may be the coordinator itself. The
// coordinator records it as run by whichever node's certificate asked.
func (b peerSSHBackend) RecordRestart(ctx context.Context, record sshconnect.RestartRecord) error {
	runtime := b.peer.Runtime.Load()
	if runtime == nil {
		return errors.New(b.peer.text.For(ctx).T(i18n.ClusterServiceNotRunning))
	}
	state, err := runtime.ReadState(ctx)
	if err != nil {
		return err
	}
	coordinator, ok := state.Members[state.Coordinator.NodeID]
	if !ok {
		return coordination.ErrUnavailable
	}
	var recorded struct {
		OK bool `json:"ok"`
	}
	if err := b.peer.peerJSON(ctx, coordinator, http.MethodPost, clusterNodeRestartPath, record, &recorded); err != nil {
		return err
	}
	if !recorded.OK {
		return coordination.ErrUnavailable
	}
	return nil
}

// serveNodeRestart keeps a restart a member ran among the fleet's events,
// once this node's application runs as the coordinator's. Who ran it is
// the asking member, as its certificate says, whatever the record claims.
func (p *Peer) serveNodeRestart(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost || !p.authorizedPeerRequest(r, "node-restart") {
		http.Error(w, "owner authorization required", http.StatusForbidden)
		return
	}
	identity, err := coordination.CertificateIdentity(r.TLS.PeerCertificates[0])
	if err != nil {
		http.Error(w, "invalid peer identity", http.StatusForbidden)
		return
	}
	var record sshconnect.RestartRecord
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&record); err != nil || record.NodeID == "" || record.Outcome == "" || record.At.IsZero() {
		http.Error(w, "invalid request", http.StatusBadRequest)
		return
	}
	record.By = identity.NodeID
	ctx, cancel := context.WithTimeout(r.Context(), awaitAskLimit)
	defer cancel()
	active, err := p.Runtime.Load().WaitReady(ctx)
	if err != nil {
		HTTPError(w, err)
		return
	}
	p.Mu.RLock()
	var admin *adminsvc.Service
	if p.Application != nil && p.Application.Generation == active.Generation {
		admin = p.Application.Admin
	}
	p.Mu.RUnlock()
	if admin == nil {
		HTTPError(w, coordination.ErrNotReady)
		return
	}
	admin.RecordNodeRestart(record)
	WriteJSON(w, map[string]bool{"ok": true})
}
