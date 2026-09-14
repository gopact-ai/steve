package node

import (
	"context"
	"errors"
	"testing"

	"github.com/gopact-ai/steve/internal/acphost"
	"github.com/gopact-ai/steve/internal/harness"
	"github.com/gopact-ai/steve/internal/nodewire"
	"github.com/gopact-ai/steve/internal/permission"
)

type receiptFailureTransport struct {
	acphost.LocalTransport
	afterStart func()
}

func (t receiptFailureTransport) Start(ctx context.Context) (acphost.Process, error) {
	p, err := t.LocalTransport.Start(ctx)
	if err == nil {
		t.afterStart()
	}
	return p, err
}

func TestOpenReceiptFailureReleasesProvenStoppedPluginUse(t *testing.T) {
	for _, mode := range []string{"before-start", "after-start", "cancelled-before-start"} {
		t.Run(mode, func(t *testing.T) {
			cfg, req, _ := resumedFixture(t, buildMockAgent(t))
			req.ID = ""
			s := NewServer(cfg)
			selection := nodeRuntimeFixture(t, s, "")
			runtime, err := s.pluginRuntimePool().Prepare(t.Context(), "native", selection, harness.Config{Command: cfg.Harnesses["mock"].Command, Permission: "read"})
			if err != nil {
				t.Fatal(err)
			}
			defer s.closePluginRuntimes()
			req.Plugin, req.Binding.PluginRuntimeID = runtime.Ref.Clone(), runtime.Ref.ID
			if err := s.startSessions(t.Context()); err != nil {
				t.Fatal(err)
			}
			defer s.sessions.Close()
			broker, err := permission.New("read")
			if err != nil {
				t.Fatal(err)
			}
			id := nodewire.SessionOpenID(req.Authority.ClusterID, req.Binding.NodeID, req.Binding.AttemptID, req.CommandID, req.Harness)
			s.sessions.mu.Lock()
			one, hostCfg, err := s.sessions.prepareOwnedSession(t.Context(), id, "open-hash", &req, cfg.Harnesses["mock"], broker, nil)
			s.sessions.mu.Unlock()
			if err != nil {
				t.Fatal(err)
			}
			failure := errors.New("session receipt storage unavailable")
			fail := func() { one.mu.Lock(); one.failure = failure; one.mu.Unlock() }
			started := false
			if mode == "before-start" {
				fail()
			} else if mode == "after-start" {
				one.host.Close()
				hostCfg.Transport = receiptFailureTransport{LocalTransport: acphost.LocalTransport{Command: hostCfg.Command, Args: hostCfg.Args, Env: hostCfg.Env, ProcessDir: hostCfg.ProcessDir}, afterStart: func() { started = true; fail() }}
				one.host = acphost.New(hostCfg)
			}
			defer one.host.Close()
			ctx := t.Context()
			if mode == "cancelled-before-start" {
				var cancel context.CancelFunc
				ctx, cancel = context.WithCancel(ctx)
				cancel()
				failure = context.Canceled
			}
			_, err = one.openNative(ctx, req, hostCfg)
			if !errors.Is(err, failure) || !one.host.AllProcessesStopped() || started != (mode == "after-start") {
				t.Fatalf("open failure lost stop/error proof: %v", err)
			}
			infos, err := s.pluginStore().RuntimeInfos()
			if err != nil || len(infos) != 1 || len(infos[0].Uses) != 1 || !infos[0].Uses[0].Stopped {
				t.Fatal("failed open left plugin runtime use active", err)
			}
		})
	}
}
