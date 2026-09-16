package cluster

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/gopact-ai/steve/internal/desktop"
	"github.com/gopact-ai/steve/internal/nodewire"
	"github.com/gopact-ai/steve/internal/sshconnect"
)

// UpgradeTarget names how a member is reached for an upgrade: the alias of
// the link this node keeps to it. Only machines enrolled over SSH have one;
// this node itself is upgraded by replacing the App.
func (b peerSSHBackend) UpgradeTarget(_ context.Context, nodeID string) (sshconnect.UpgradeTarget, error) {
	if nodeID == b.peer.Config.NodeID {
		return sshconnect.UpgradeTarget{}, errors.New("本机随 App 一起升级，不从这里升级")
	}
	link, ok := b.peer.link(nodeID)
	if !ok {
		return sshconnect.UpgradeTarget{}, errors.New("本机没有记录到这台机器的 SSH 隧道")
	}
	find := b.findBinary
	if find == nil {
		find = desktop.BundledPeerBinary
	}
	return sshconnect.UpgradeTarget{Alias: link.Alias, Version: nodewire.Version(), FindBinary: find}, nil
}

// Upgraded reopens the link so the machine's end of it runs the new
// program too, then waits until the machine reports this build.
func (b peerSSHBackend) Upgraded(ctx context.Context, nodeID string) error {
	link, ok := b.peer.link(nodeID)
	if !ok {
		return errors.New("本机没有记录到这台机器的 SSH 隧道")
	}
	sshconnect.Report(ctx, "重开 SSH 隧道，让隧道两端都运行新程序")
	if err := b.peer.openLink(nodeID, link).WaitConnected(ctx); err != nil {
		return fmt.Errorf("SSH 隧道未能重新建立：%w", err)
	}
	sshconnect.Report(ctx, "隧道已恢复，等待机器以新版本上报")
	return b.peer.awaitBuild(ctx, nodeID, nodewire.Version())
}

func (p *Peer) link(nodeID string) (PeerLink, bool) {
	p.Mu.RLock()
	defer p.Mu.RUnlock()
	link, ok := p.Config.Links[nodeID]
	return link, ok
}

// awaitBuild asks the machine for its advert until it answers with the
// wanted build. The registry dials again after the restart, so every ask
// reaches whatever process is up now.
func (p *Peer) awaitBuild(ctx context.Context, nodeID, version string) error {
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	seen, said := "", ""
	for {
		p.Mu.RLock()
		var admin = p.Application
		p.Mu.RUnlock()
		if admin != nil && admin.Admin != nil {
			advert, err := admin.Admin.Nodes.Refresh(ctx, nodeID)
			switch {
			case err == nil && advert.BuildVersion == version:
				return nil
			case err == nil:
				seen = advert.BuildVersion
			case ctx.Err() == nil && err.Error() != said:
				said = err.Error()
				sshconnect.Report(ctx, "机器暂未应答："+said)
			}
		}
		select {
		case <-ctx.Done():
			if seen != "" {
				return fmt.Errorf("机器仍报告版本 %s", seen)
			}
			return errors.New("机器还没有重新应答")
		case <-ticker.C:
		}
	}
}
