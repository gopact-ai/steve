package admin

import (
	"context"
	"fmt"
	"runtime"
	"sort"

	"github.com/gopact-ai/steve/internal/consoleapi"
	"github.com/gopact-ai/steve/internal/nodewire"
)

func (a *Service) Versions(ctx context.Context) (consoleapi.Versions, error) {
	ConfigMu.RLock()
	hubID := ""
	peers := []consoleapi.HubPeerInfo{}
	if a.Cfg != nil {
		hubID = a.Cfg.Gateway.HubID
		for id, peer := range a.Cfg.Gateway.Peers {
			peers = append(peers, consoleapi.HubPeerInfo{ID: id, Name: peer.Name, URL: peer.URL})
		}
	}
	ConfigMu.RUnlock()
	v := consoleapi.Versions{Hub: nodewire.Version(), HubID: hubID, ProtocolMin: nodewire.ProtocolMin, ProtocolMax: nodewire.ProtocolVersion, Nodes: []consoleapi.VersionNode{}}
	sort.Slice(peers, func(i, j int) bool { return peers[i].ID < peers[j].ID })
	v.Peers = peers
	if a.Projects != nil {
		owners, err := a.Projects.ListOwnership(ctx)
		if err != nil {
			return v, err
		}
		v.Projects = owners
	}
	if a.Nodes != nil {
		for _, n := range a.Nodes.Statuses() {
			v.Nodes = append(v.Nodes, consoleapi.VersionNode{Name: n.Name, Version: n.Advert.BuildVersion, OS: n.Advert.OS, Arch: n.Advert.Arch, Online: n.Up, MatchesHub: n.Advert.BuildVersion == v.Hub, Protocol: n.Advert.Version, Features: n.Advert.Features})
		}
	}
	if a.releases != nil {
		v.DiscoveryConfigured = true
		platforms := map[string][3]string{"hub": {"steve", runtime.GOOS, runtime.GOARCH}}
		for _, node := range v.Nodes {
			if node.OS != "" && node.Arch != "" {
				platforms[node.OS+"/"+node.Arch] = [3]string{"steve-node", node.OS, node.Arch}
			}
		}
		for _, platform := range platforms {
			manifest, err := a.releases.Latest(ctx, platform[0], platform[1], platform[2])
			if err != nil {
				return v, fmt.Errorf("release discovery: %w", err)
			}
			v.Latest = append(v.Latest, manifest)
		}
	}
	return v, nil
}
