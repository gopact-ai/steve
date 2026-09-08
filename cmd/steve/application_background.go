package main

import (
	"context"
	"sync"

	adminsvc "github.com/gopact-ai/steve/internal/admin"
	"github.com/gopact-ai/steve/internal/config"
	"github.com/gopact-ai/steve/internal/node"
)

type applicationBackground struct {
	ctx    context.Context
	cancel context.CancelFunc
	mu     sync.Mutex
	closed bool
	wg     sync.WaitGroup
}

func newApplicationBackground(parent context.Context) *applicationBackground {
	ctx, cancel := context.WithCancel(parent)
	return &applicationBackground{ctx: ctx, cancel: cancel}
}

func (b *applicationBackground) Go(run func(context.Context)) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed || b.ctx.Err() != nil {
		return
	}
	b.wg.Add(1)
	go func() { defer b.wg.Done(); run(b.ctx) }()
}

func (b *applicationBackground) Close() {
	b.mu.Lock()
	b.closed = true
	b.cancel()
	b.mu.Unlock()
	b.wg.Wait()
}

func newLocalObservation(ctx context.Context, cfg *config.Config) (*adminsvc.LocalObservation, func()) {
	observation := &adminsvc.LocalObservation{Launch: node.NewLaunchProbe()}
	background := newApplicationBackground(ctx)
	background.Go(func(ctx context.Context) {
		observation.Launch.Run(ctx, func() []string {
			adminsvc.ConfigMu.RLock()
			defer adminsvc.ConfigMu.RUnlock()
			out := make([]string, 0, len(cfg.Harnesses)+len(cfg.Gateway.Tools))
			for _, h := range cfg.Harnesses {
				out = append(out, h.Command)
			}
			return append(out, cfg.Gateway.Tools...)
		})
	})
	return observation, background.Close
}
