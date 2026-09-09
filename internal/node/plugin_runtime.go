package node

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/gopact-ai/acp"
	"github.com/gopact-ai/steve/internal/ability"
	"github.com/gopact-ai/steve/internal/harness"
	"github.com/gopact-ai/steve/internal/plugins"
	steveruntime "github.com/gopact-ai/steve/internal/runtime"
)

// PluginRuntimePool owns node-local brokers for immutable session profiles.
// The returned ACP server descriptions contain only loopback capabilities.
type PluginRuntimePool struct {
	Store    *plugins.Store
	StateDir string
	Launcher string
	mu       sync.Mutex
	brokers  map[string]*runtimeBroker
	closing  bool
}

type runtimeBroker struct {
	broker  *Broker
	servers []acp.MCPServer
	cancel  context.CancelFunc
	done    chan error
}

type PluginRuntime struct {
	Ref          plugins.RuntimeRef
	Config       harness.Config
	Servers      []acp.MCPServer
	Instructions string
}

func (PluginRuntime) MarshalJSON() ([]byte, error) {
	return nil, errors.New("plugin runtime contains node-local configuration")
}

func (p *PluginRuntimePool) Prepare(ctx context.Context, commandID string, selection plugins.Selection, cfg harness.Config) (PluginRuntime, error) {
	profiles := steveruntime.PluginProfiles{Store: p.Store, StateDir: p.StateDir}
	record, err := profiles.Prepare(ctx, commandID, selection, cfg)
	if err != nil {
		return PluginRuntime{}, err
	}
	cfg, err = profiles.Config(record)
	if err != nil {
		return PluginRuntime{}, err
	}
	return PluginRuntime{Ref: record.Ref, Config: cfg}, nil
}

func (p *PluginRuntimePool) Load(ctx context.Context, ref plugins.RuntimeRef) (PluginRuntime, error) {
	profiles := steveruntime.PluginProfiles{Store: p.Store, StateDir: p.StateDir}
	record, err := p.Store.Runtime(ref)
	if err != nil {
		return PluginRuntime{}, err
	}
	cfg, err := profiles.Config(record)
	if err != nil {
		return PluginRuntime{}, err
	}
	instructions, err := p.Store.RuntimeInstructions(ref.Selection)
	if err != nil {
		return PluginRuntime{}, err
	}
	cfg.PluginInstructions = instructions
	servers, err := p.servers(ctx, ref)
	if err != nil {
		return PluginRuntime{}, err
	}
	return PluginRuntime{Ref: ref, Config: cfg, Servers: servers, Instructions: instructions}, nil
}

func (p *PluginRuntimePool) servers(ctx context.Context, ref plugins.RuntimeRef) ([]acp.MCPServer, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closing {
		return nil, errors.New("plugin runtime pool is closing")
	}
	records, err := p.Store.Selection(ref.Selection)
	if err != nil {
		return nil, err
	}
	specs := map[string]MCPSpec{}
	for _, record := range records {
		resolved, err := p.Store.ResolveServers(record.Deployment, plugins.Environment{})
		if err != nil {
			return nil, err
		}
		for name, spec := range resolved {
			native, err := plugins.NativeName(record.Deployment.PackageID, "mcp", name)
			if err != nil {
				return nil, err
			}
			if _, exists := specs[native]; exists {
				return nil, plugins.ErrConflict
			}
			specs[native] = MCPSpec{Type: spec.Type, Command: spec.Command, Args: spec.Args, Env: spec.Env, URL: spec.URL, Headers: spec.Headers}
		}
	}
	if entry := p.brokers[ref.ID]; entry != nil {
		return clonePluginServers(entry.servers), nil
	}
	if len(specs) == 0 {
		return nil, nil
	}
	entry, err := p.startBroker(ctx, ref, specs)
	if err != nil {
		return nil, err
	}
	if p.brokers == nil {
		p.brokers = map[string]*runtimeBroker{}
	}
	p.brokers[ref.ID] = entry
	return clonePluginServers(entry.servers), nil
}

func (p *PluginRuntimePool) startBroker(ctx context.Context, ref plugins.RuntimeRef, specs map[string]MCPSpec) (*runtimeBroker, error) {
	dir := p.Store.RuntimeDir(ref.ID)
	// Unix sockets have short path limits; the opaque profile prefix is enough
	// to separate endpoints, while the port file remains beside the full record.
	socket, err := p.Store.RuntimeSocket(ctx, ref)
	if err != nil {
		return nil, err
	}
	broker := NewBroker(BrokerConfig{Socket: socket, MCPServers: specs, PortFile: filepath.Join(dir, "mcp.port"), Launcher: p.Launcher, StrictPort: true})
	started := make(chan error, 1)
	broker.ready = func(err error) { started <- err }
	owner, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- broker.Serve(owner) }()
	select {
	case err := <-started:
		if err != nil {
			cancel()
			<-done
			return nil, err
		}
	case <-ctx.Done():
		cancel()
		<-done
		return nil, ctx.Err()
	}
	entry := &runtimeBroker{broker: broker, cancel: cancel, done: done}
	names := make([]string, 0, len(specs))
	for name := range specs {
		names = append(names, name)
	}
	slices.Sort(names)
	for _, name := range names {
		token, err := p.Store.RuntimeMCPToken(ctx, ref, name)
		if err != nil {
			cancel()
			<-done
			return nil, err
		}
		binding, err := broker.bindRuntime(name, ref.ID, ref.Selection.Harness, token)
		if err != nil {
			cancel()
			<-done
			return nil, err
		}
		server := acp.MCPServer{Name: name, Type: acp.MCPServerType(binding.Transport), Command: binding.Command, Args: binding.Args, URL: binding.URL}
		entry.servers = append(entry.servers, server)
	}
	return entry, nil
}

func (p *PluginRuntimePool) Close() error {
	p.mu.Lock()
	p.closing = true
	entries := p.brokers
	p.brokers = nil
	p.mu.Unlock()
	var errs []error
	for id, entry := range entries {
		entry.cancel()
		entry.broker.Release(id)
		errs = append(errs, <-entry.done)
		entry.broker.connections.Wait()
		if entry.broker.proxyDone != nil {
			<-entry.broker.proxyDone
		}
	}
	return errors.Join(errs...)
}

func (p *PluginRuntimePool) Drop(id string) error {
	p.mu.Lock()
	entry := p.brokers[id]
	delete(p.brokers, id)
	p.mu.Unlock()
	if entry == nil {
		return nil
	}
	entry.broker.Release(id)
	entry.cancel()
	err := <-entry.done
	entry.broker.connections.Wait()
	if entry.broker.proxyDone != nil {
		<-entry.broker.proxyDone
	}
	return err
}

func (p *PluginRuntimePool) MarshalJSON() ([]byte, error) {
	return nil, fmt.Errorf("plugin runtime pool is node-local")
}

func (s *Server) pluginRuntimePool() *PluginRuntimePool {
	s.pluginMu.Lock()
	defer s.pluginMu.Unlock()
	if s.pluginRuntime == nil {
		s.pluginRuntime = &PluginRuntimePool{Store: s.pluginStore(), StateDir: s.conf().StateDir, closing: s.pluginClosing}
	}
	return s.pluginRuntime
}

func (s *Server) closePluginRuntimes() {
	s.pluginMu.Lock()
	s.pluginClosing = true
	pool := s.pluginRuntime
	s.pluginMu.Unlock()
	if pool != nil {
		if err := pool.Close(); err != nil {
			fmt.Fprintln(os.Stderr, "steve-node: close plugin runtimes:", err)
		}
	}
}

func (b *Broker) bindRuntime(name, owner, harnessID, token string) (ability.Binding, error) {
	binding, err := b.Bind(name, owner, harnessID)
	if err != nil {
		return binding, err
	}
	var previous string
	if binding.Transport == "stdio" {
		previous = binding.Args[len(binding.Args)-1]
	} else {
		previous = binding.URL[strings.LastIndex(binding.URL, "/")+1:]
	}
	b.mu.Lock()
	record, ok := b.bindings[previous]
	if ok {
		delete(b.bindings, previous)
		record.id = token
		record.expires = time.Time{}
		b.bindings[token] = record
	}
	b.mu.Unlock()
	if !ok {
		return binding, ErrUnbindable
	}
	if binding.Transport == "stdio" {
		binding.Args[len(binding.Args)-1] = token
	} else {
		binding.URL = b.proxyAddr() + "/b/" + token
	}
	return binding, nil
}

func clonePluginServers(servers []acp.MCPServer) []acp.MCPServer {
	out := slices.Clone(servers)
	for i := range out {
		out[i].Args = slices.Clone(out[i].Args)
		out[i].Env = slices.Clone(out[i].Env)
		out[i].Headers = slices.Clone(out[i].Headers)
	}
	return out
}
