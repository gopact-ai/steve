package cluster

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"

	"github.com/gopact-ai/steve/internal/coordination"
	"github.com/gopact-ai/steve/internal/sshconnect"
)

// linkSpec turns a persisted link into the session to keep open. This
// node's listeners are forwarded from the machine's loopback to where they
// are bound here; the machine's listeners are reached from loopback ports
// chosen here when the session opens.
func (p *Peer) linkSpec(link PeerLink) sshconnect.LinkSpec {
	return sshconnect.LinkSpec{Alias: link.Alias,
		Inbound:  []sshconnect.PortForward{{Listen: link.Remote.Raft, Target: loopbackOf(p.Config.RaftBindAddress)}, {Listen: link.Remote.API, Target: loopbackOf(p.Config.PeerBindAddress)}},
		Outbound: []sshconnect.PortForward{{Target: link.Peer.Raft}, {Target: link.Peer.API}}}
}

// loopbackOf is where a listener bound at address answers on this machine:
// its own host when it is a single address, else loopback.
func loopbackOf(address string) string {
	host, port, err := net.SplitHostPort(address)
	if err != nil {
		return address
	}
	if ip := net.ParseIP(host); host == "" || ip != nil && ip.IsUnspecified() {
		host = "127.0.0.1"
	}
	return net.JoinHostPort(host, port)
}

// openLink starts keeping a machine's session up and points this node's
// route to the machine at the session's local ports. The route is set at
// once: while the session is down, a dial fails fast at a closed loopback
// port instead of timing out against an address that was never reachable.
// The caller holds no lock.
func (p *Peer) openLink(nodeID string, link PeerLink) *sshconnect.Link {
	var opened *sshconnect.Link
	route := func(status sshconnect.LinkStatus) {
		// A link that was replaced or dropped reports its last status
		// while closing; only the machine's current link owns its route.
		p.Mu.RLock()
		current := p.links[nodeID] == opened
		p.Mu.RUnlock()
		if current && len(status.Outbound) == 2 && status.Outbound[0].Listen != "" && status.Outbound[1].Listen != "" {
			p.routes.Set(nodeID, coordination.Route{Raft: status.Outbound[0].Listen, API: status.Outbound[1].Listen})
		}
	}
	p.Mu.Lock()
	if p.links == nil {
		p.links = map[string]*sshconnect.Link{}
	}
	previous := p.links[nodeID]
	opened = sshconnect.OpenLink(p.linkCtx, p.linkSpec(link), sshconnect.LinkOptions{Launch: p.Options.SSHLaunch, OnChange: route})
	p.links[nodeID] = opened
	p.Mu.Unlock()
	route(opened.Status())
	if previous != nil {
		previous.Close()
	}
	return opened
}

// setLink records a machine's link in this node's configuration and opens
// it; a link already open with the same description is kept as it is.
func (p *Peer) setLink(nodeID string, link PeerLink) (*sshconnect.Link, error) {
	p.Mu.Lock()
	if p.closing {
		p.Mu.Unlock()
		return nil, errors.New("本节点正在关闭")
	}
	current, open := p.links[nodeID]
	same := open && p.Config.Links[nodeID] == link
	if !same {
		if p.Config.Links == nil {
			p.Config.Links = map[string]PeerLink{}
		}
		p.Config.Links[nodeID] = link
	}
	saved := p.Config
	p.Mu.Unlock()
	if same {
		return current, nil
	}
	if err := SaveClusterJSON(p.Options.ClusterPath, saved, false); err != nil {
		return nil, fmt.Errorf("保存隧道配置失败：%w", err)
	}
	return p.openLink(nodeID, link), nil
}

// dropLink closes a machine's link and forgets it, along with the route
// that pointed at it.
func (p *Peer) dropLink(nodeID string) error {
	p.Mu.Lock()
	link, open := p.links[nodeID]
	delete(p.links, nodeID)
	_, recorded := p.Config.Links[nodeID]
	delete(p.Config.Links, nodeID)
	saved := p.Config
	p.Mu.Unlock()
	p.routes.Delete(nodeID)
	if open {
		link.Close()
	}
	if !recorded {
		return nil
	}
	return SaveClusterJSON(p.Options.ClusterPath, saved, false)
}

// LinkStatuses reports every link this node keeps, by node ID.
func (p *Peer) LinkStatuses() map[string]sshconnect.LinkStatus {
	p.Mu.RLock()
	defer p.Mu.RUnlock()
	statuses := make(map[string]sshconnect.LinkStatus, len(p.links))
	for nodeID, link := range p.links {
		statuses[nodeID] = link.Status()
	}
	return statuses
}

// OpenEnrollmentLink brings up the session to the machine an enrollment is
// installing and waits until it carries traffic. The link is recorded
// first, so a restart of this node reopens it whether or not the
// enrollment went on to finish.
func (p *Peer) OpenEnrollmentLink(ctx context.Context, id string) error {
	p.enrollmentMu.Lock()
	record, err := p.loadEnrollment(id)
	p.enrollmentMu.Unlock()
	if errors.Is(err, os.ErrNotExist) {
		return ErrEnrollmentGone
	}
	if err != nil {
		return err
	}
	request := record.Request
	if request.Alias == "" || request.HubRoute.Raft == "" || request.HubRoute.API == "" {
		return errors.New("这次接入没有经由 SSH 隧道互联的计划")
	}
	_, raftPort, err := net.SplitHostPort(request.RaftAddress)
	if err != nil {
		return err
	}
	_, peerPort, err := net.SplitHostPort(request.PeerAddress)
	if err != nil {
		return err
	}
	link := PeerLink{Alias: request.Alias, Remote: request.HubRoute, Peer: coordination.Route{Raft: net.JoinHostPort("127.0.0.1", raftPort), API: net.JoinHostPort("127.0.0.1", peerPort)}}
	opened, err := p.setLink(record.NodeID, link)
	if err != nil {
		return err
	}
	return opened.WaitConnected(ctx)
}
