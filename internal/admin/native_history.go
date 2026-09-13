package admin

import (
	"context"
	"errors"

	"github.com/gopact-ai/steve/internal/consoleapi"
	"github.com/gopact-ai/steve/internal/nativehistory"
)

func (a *Service) NativeHistory(ctx context.Context, name string, source nativehistory.Source) ([]nativehistory.Entry, error) {
	target, err := a.nodeForAgentEnrollment(name)
	if err != nil {
		return nil, err
	}
	entries, err := a.Nodes.NativeHistory(ctx, name, source)
	if err != nil {
		return nil, err
	}
	ConfigMu.RLock()
	defer ConfigMu.RUnlock()
	if err := a.checkAgentNodeTarget(name, target); err != nil {
		return nil, err
	}
	return entries, nil
}

func (a *Service) ImportNativeHistory(ctx context.Context, name string, req consoleapi.NativeImportRequest) (consoleapi.ImportedSession, error) {
	if !a.ClusterMode || a.Coordinator == nil || a.Console == nil || a.Catalog == nil {
		return consoleapi.ImportedSession{}, errors.New("历史会话迁移需要已启用集群的服务")
	}
	target, err := a.nodeForAgentEnrollment(name)
	if err != nil {
		return consoleapi.ImportedSession{}, err
	}
	selected, ok := a.Catalog.Resolve(req.Agent)
	if !ok || selected.ID != req.Agent || selected.Node != name || selected.Harness != req.Source.Harness {
		return consoleapi.ImportedSession{}, errors.New("请选择运行在来源机器上且使用相同工具的 Agent")
	}
	ref, err := a.Nodes.ImportNativeHistory(ctx, name, nativehistory.ImportRequest{CommandID: req.CommandID, Source: req.Source, NativeID: req.NativeID, Revision: req.Revision})
	if err != nil {
		return consoleapi.ImportedSession{}, err
	}
	a.Mu.Lock()
	defer a.Mu.Unlock()
	ConfigMu.RLock()
	err = a.checkAgentNodeTarget(name, target)
	current, exists := a.Cfg.Agents[selected.ID]
	ConfigMu.RUnlock()
	if err != nil {
		return consoleapi.ImportedSession{}, err
	}
	if !exists || current.Node != name || current.Harness != selected.Harness {
		return consoleapi.ImportedSession{}, errors.New("Agent 配置已变化，请重新选择")
	}
	return a.Console.EnsureImportedConversation(ctx, req.CommandID, consoleapi.ImportedSession{Node: name, Project: req.Project, Agent: selected.ID, Reference: ref}, func(conversation string) error {
		return a.Coordinator.ImportNativeSession(ctx, conversation, req.Project, selected, ref)
	})
}
