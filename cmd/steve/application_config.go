package main

import (
	"context"
	"fmt"
	"sync"

	adminsvc "github.com/gopact-ai/steve/internal/admin"
	"github.com/gopact-ai/steve/internal/agentmcp"
	"github.com/gopact-ai/steve/internal/cluster"
	"github.com/gopact-ai/steve/internal/config"
	"github.com/gopact-ai/steve/internal/ledger"
	"github.com/gopact-ai/steve/internal/platformconfig"
	"github.com/gopact-ai/steve/internal/project"
)

type applicationConfiguration struct {
	mu      sync.Mutex
	ctx     context.Context
	store   *platformconfig.Store
	local   platformconfig.LocalNode
	value   platformconfig.Declaration
	runtime *cluster.Runtime
}

func newApplicationConfiguration(ctx context.Context, activation cluster.Activation, worker cluster.PeerWorkerDescriptor) *applicationConfiguration {
	return &applicationConfiguration{ctx: ctx, store: platformconfig.New(activation.Ledger), runtime: activation.Runtime,
		local: platformconfig.LocalNode{ID: activation.NodeID, Config: config.Node{Addr: worker.Address, Token: worker.Token, Level: "restricted"}}}
}

func (s *applicationConfiguration) Configure(cfg *config.Config) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	d, err := s.store.Bootstrap(s.ctx, cfg, s.local)
	if err != nil {
		return fmt.Errorf("load shared work configuration: %w", err)
	}
	if err := d.Apply(cfg); err != nil {
		return err
	}
	s.value = d
	// In a cluster all tools belong to an identified execution service,
	// including tools installed on this same physical computer.
	cfg.Harnesses = map[string]config.Harness{}
	cfg.MCPServers = map[string]config.MCPServer{}
	return nil
}

func (s *applicationConfiguration) Save(path string, cfg *config.Config) error {
	return s.SaveContext(s.ctx, path, cfg)
}

func (s *applicationConfiguration) Revision() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return fmt.Sprintf("shared:%d", s.value.Revision)
}

func (s *applicationConfiguration) SaveContext(parent context.Context, path string, cfg *config.Config) (saveErr error) {
	if cfg == nil {
		return fmt.Errorf("application configuration is required")
	}
	ctx, cancel := context.WithCancel(parent)
	stop := context.AfterFunc(s.ctx, cancel)
	defer func() { stop(); cancel() }()
	s.mu.Lock()
	defer s.mu.Unlock()
	defer func() {
		saveErr = adminsvc.RedactChannelError(saveErr, s.value.Credentials.FeishuAppSecret, cfg.Feishu.AppSecret)
	}()
	if err := s.ctx.Err(); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := cfg.CheckFileRevision(path); err != nil {
		return err
	}
	if s.runtime != nil {
		state, err := s.runtime.ReadState(ctx)
		if err != nil {
			return err
		}
		for id := range state.Members {
			if node, ok := cfg.Nodes[id]; ok && !project.LevelRestricted.Admits(project.Level(node.Level).OrDefault()) {
				return fmt.Errorf("节点 %s 保存完整协作账本，数据等级不能低于 restricted；低等级机器只能作为执行节点接入", id)
			}
		}
	}
	candidate, err := s.value.WithCandidate(cfg)
	if err != nil {
		return err
	}
	next, err := s.store.SaveWithGuard(ctx, s.value.Revision, candidate, func(tx *ledger.Tx) error { return agentmcp.AuthorizeContext(ctx, applicationMCPTx{tx}) })
	if err != nil {
		return err
	}
	s.value = next
	return nil
}
