package admin

import (
	"context"
	"errors"

	"github.com/gopact-ai/steve/internal/console"
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
	a.configStore().RLock()
	defer a.configStore().RUnlock()
	if err := a.checkAgentNodeTarget(name, target); err != nil {
		return nil, err
	}
	return entries, nil
}

func (a *Service) ImportNativeHistory(ctx context.Context, name string, req consoleapi.NativeImportRequest) (consoleapi.ImportedSession, error) {
	if !a.ClusterMode || a.Coordinator == nil || a.Console == nil || a.Catalog == nil || a.Projects == nil {
		return consoleapi.ImportedSession{}, errors.New("历史会话迁移需要已启用集群的服务")
	}
	if previous, exists, err := a.Console.ImportedConversation(name, req); err != nil || exists {
		return previous, err
	}
	conversation, err := console.NativeImportConversation(req.CommandID)
	if err != nil {
		return consoleapi.ImportedSession{}, err
	}
	target, err := a.nodeForAgentEnrollment(name)
	if err != nil {
		return consoleapi.ImportedSession{}, err
	}
	selected, ok := a.Catalog.Resolve(req.Agent)
	if !ok || selected.ID != req.Agent || selected.Node != name || selected.Harness != req.Source.Harness {
		return consoleapi.ImportedSession{}, errors.New("请选择运行在来源机器上且使用相同工具的 Agent")
	}
	var ref nativehistory.Reference
	if req.Project == "" {
		// The isolated snapshot is also the durable source receipt for retries
		// after project registration fails or the original history disappears.
		ref, err = a.Nodes.ImportNativeHistory(ctx, name, nativehistory.ImportRequest{CommandID: req.CommandID, Source: req.Source, NativeID: req.NativeID, Revision: req.Revision})
		if err != nil {
			return consoleapi.ImportedSession{}, err
		}
		if err := ref.Validate(selected.Harness, ref.SourceWorkdir); err != nil {
			return consoleapi.ImportedSession{}, err
		}
		req.Project, err = a.nativeImportProject(ctx, name, target, selected, ref.SourceWorkdir)
		if err != nil {
			return consoleapi.ImportedSession{}, err
		}
	}
	workdir, err := a.Coordinator.PreflightNativeImport(ctx, conversation, req.Project, selected)
	if err != nil {
		return consoleapi.ImportedSession{}, err
	}
	if ref.ID == "" {
		ref, err = a.Nodes.ImportNativeHistory(ctx, name, nativehistory.ImportRequest{CommandID: req.CommandID, Source: req.Source, NativeID: req.NativeID, Revision: req.Revision, Workdir: workdir})
		if err != nil {
			return consoleapi.ImportedSession{}, err
		}
	}
	if err := ref.Validate(selected.Harness, workdir); err != nil {
		return consoleapi.ImportedSession{}, err
	}
	a.Mu.Lock()
	defer a.Mu.Unlock()
	a.configStore().RLock()
	err = a.checkNativeImportTarget(name, target, selected)
	a.configStore().RUnlock()
	if err != nil {
		return consoleapi.ImportedSession{}, err
	}
	return a.Console.EnsureImportedConversation(ctx, req.CommandID, consoleapi.ImportedSession{Node: name, Project: req.Project, Agent: selected.ID, Reference: ref}, func(conversation string) error {
		return a.Coordinator.ImportNativeSession(ctx, conversation, req.Project, selected, ref)
	})
}
