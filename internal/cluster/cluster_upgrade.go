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
// this node itself is upgraded by replacing the App. Whether this node
// coordinates is not asked here: it may stop and start coordinating while
// the upgrade runs, and the wait for the new build lives with that.
func (b peerSSHBackend) UpgradeTarget(_ context.Context, nodeID string) (sshconnect.UpgradeTarget, error) {
	if nodeID == b.peer.Config.NodeID {
		return sshconnect.UpgradeTarget{}, errors.New("本机随 App 一起升级，不从这里升级")
	}
	alias, err := b.MachineAlias(nodeID)
	if err != nil {
		return sshconnect.UpgradeTarget{}, err
	}
	find := b.findBinary
	if find == nil {
		find = desktop.BundledPeerBinary
	}
	return sshconnect.UpgradeTarget{Alias: alias, Version: nodewire.Version(), FindBinary: find}, nil
}

// MachineAlias is the alias of the link this node keeps to a member: how
// it is reached to browse its directories or send it a program. This node
// is not reached over SSH from itself.
func (b peerSSHBackend) MachineAlias(nodeID string) (string, error) {
	if nodeID == b.peer.Config.NodeID {
		return "", errors.New("本机不经 SSH 访问，直接选择本机上的目录")
	}
	link, ok := b.peer.link(nodeID)
	if !ok {
		return "", errors.New("本机没有记录到这台机器的 SSH 隧道")
	}
	return link.Alias, nil
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
	return awaitBuildWithin(ctx, b.peer.refresher, awaitAskLimit, time.Second, nodeID, nodewire.Version())
}

func (p *Peer) link(nodeID string) (PeerLink, bool) {
	p.Mu.RLock()
	defer p.Mu.RUnlock()
	link, ok := p.Config.Links[nodeID]
	return link, ok
}

type advertRefresh func(ctx context.Context, nodeID string) (nodewire.Advert, error)

// refresher is how this node, while it coordinates, asks a member for a
// fresh advert; nil when it does not coordinate.
func (p *Peer) refresher() advertRefresh {
	p.Mu.RLock()
	defer p.Mu.RUnlock()
	if p.Application == nil || p.Application.Admin == nil {
		return nil
	}
	return p.Application.Admin.Nodes.Refresh
}

// awaitAskLimit bounds one ask for a machine's advert: a connection the
// registry still holds through the old link may never answer, and the
// next ask dials again.
const awaitAskLimit = 10 * time.Second

// awaitBuildWithin asks the machine for its advert, every interval and
// each ask bounded by askLimit, until it answers with the wanted build.
// The registry dials again after the restart, so every ask reaches
// whatever process is up now. Restarting a member that led the cluster
// makes this node's application rebuild for a few seconds; the refresher
// is taken fresh before every ask and its absence is waited out.
func awaitBuildWithin(ctx context.Context, refresher func() advertRefresh, askLimit, interval time.Duration, nodeID, version string) error {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	seen, said := "", ""
	waiting := false
	for {
		if refresh := refresher(); refresh == nil {
			waiting = true
			if said != "rebuilding" {
				said = "rebuilding"
				sshconnect.Report(ctx, "本机的协调服务正在重启，等它恢复后再询问")
			}
		} else {
			waiting = false
			askCtx, cancel := context.WithTimeout(ctx, askLimit)
			advert, err := refresh(askCtx, nodeID)
			timedOut := askCtx.Err() != nil
			cancel()
			switch {
			case err == nil && advert.BuildVersion == version:
				return nil
			case err == nil:
				seen = advert.BuildVersion
			case ctx.Err() != nil:
			case timedOut && said != "timeout":
				said = "timeout"
				sshconnect.Report(ctx, "机器一直没有应答，重新询问")
			case !timedOut && err.Error() != said:
				said = err.Error()
				sshconnect.Report(ctx, "机器暂未应答："+said)
			}
		}
		select {
		case <-ctx.Done():
			switch {
			case seen != "":
				return fmt.Errorf("机器仍报告版本 %s", seen)
			case waiting:
				return errors.New("本机的协调服务一直没有恢复，无法确认机器的版本")
			}
			return errors.New("机器还没有重新应答")
		case <-ticker.C:
		}
	}
}
