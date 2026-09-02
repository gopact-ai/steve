// Package node is the hub's side of the connection layer: it dials each
// configured steve-node, holds one multiplexed connection per node, and
// hands out acphost transports that run agents there.
//
// Reachability is not this package's job. Hosts are made mutually
// addressable by the network layer (a mesh, an internal network, a VPN);
// rebuilding NAT traversal here would grow into an unmaintained half of a
// VPN. What this package owns is: who is up, what they can run, and how a
// session's bytes get there.
package node

import (
	"context"
	"fmt"
	"log"
	"net"
	"sort"
	"sync"
	"time"

	"github.com/gopact-ai/steve/internal/nodewire"
)

// Config is one remote node as the operator declared it. Everything about
// what the node can *run* comes from its advert, never from here.
type Config struct {
	Addr  string
	Token string
	// DialTimeout bounds one connection attempt; zero takes the default.
	DialTimeout time.Duration
}

// Status is a node as the registry currently knows it — the roster's raw
// material and what `/status` and `steve doctor` report.
type Status struct {
	Name      string
	Addr      string
	Up        bool
	Since     time.Time
	Advert    nodewire.Advert
	LastError string
}

// Registry keeps one connection per node, dialing lazily and redialing after
// a drop. It never blocks a turn on a node it cannot reach: the dial fails
// fast and the error names the node.
type Registry struct {
	hub string
	// mcpDial connects to whatever the reverse MCP streams should reach —
	// the hub's loopback agentmcp listener. Nil disables the reverse channel.
	mcpDial func(ctx context.Context) (net.Conn, error)

	mu    sync.Mutex
	confs map[string]Config
	live  map[string]*conn
	last  map[string]*Status
}

const defaultDialTimeout = 10 * time.Second

func NewRegistry(hub string, configs map[string]Config) *Registry {
	confs := make(map[string]Config, len(configs))
	for name, cfg := range configs {
		confs[name] = cfg
	}
	return &Registry{
		hub: hub, confs: confs,
		live: map[string]*conn{}, last: map[string]*Status{},
	}
}

// SetMCPDialer wires the reverse channel. Remote agents reach the hub's
// messaging server through a loopback listener on their own machine, which
// this registry forwards; without a dialer the node simply gets no such
// listener and its agents lose the send primitive rather than the session.
func (r *Registry) SetMCPDialer(dial func(ctx context.Context) (net.Conn, error)) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.mcpDial = dial
}

// Names lists configured nodes in a stable order.
func (r *Registry) Names() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	names := make([]string, 0, len(r.confs))
	for name := range r.confs {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// EnsureConnected dials any node that is not currently connected, so a
// caller about to make a placement decision is looking at the fleet rather
// than at whatever the registry last happened to learn.
func (r *Registry) EnsureConnected(ctx context.Context) {
	for _, name := range r.Names() {
		r.mu.Lock()
		live := r.live[name]
		r.mu.Unlock()
		if live != nil && live.alive() {
			continue
		}
		dialCtx, cancel := context.WithTimeout(ctx, defaultDialTimeout)
		_, _ = r.connect(dialCtx, name)
		cancel()
	}
}

// Statuses reports every configured node, connected or not. A node that has
// never answered still appears — a roster that hides what is broken is worse
// than no roster.
func (r *Registry) Statuses() []Status {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]Status, 0, len(r.confs))
	for name, cfg := range r.confs {
		// "Never contacted" is not the same as "tried and failed", and a
		// roster that conflates them explains a cold start as an outage.
		status := Status{Name: name, Addr: cfg.Addr, LastError: "not contacted yet"}
		if seen := r.last[name]; seen != nil {
			status = *seen
		}
		if c := r.live[name]; c != nil && c.alive() {
			status.Up = true
		} else {
			status.Up = false
		}
		out = append(out, status)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// Advert returns what the node last told us it can run.
func (r *Registry) Advert(ctx context.Context, name string) (nodewire.Advert, error) {
	c, err := r.connect(ctx, name)
	if err != nil {
		return nodewire.Advert{}, err
	}
	return c.advert, nil
}

// MCPEndpoint is the URL an agent on this node should call to reach the
// hub's messaging server. It is a loopback address on the node's own
// machine; the node forwards it here over the connection it already holds.
func (r *Registry) MCPEndpoint(ctx context.Context, name string) (string, error) {
	c, err := r.connect(ctx, name)
	if err != nil {
		return "", err
	}
	if c.advert.MCPPort == 0 {
		return "", fmt.Errorf("node %q offers no reverse messaging channel", name)
	}
	return fmt.Sprintf("http://127.0.0.1:%d/mcp", c.advert.MCPPort), nil
}

// Probe dials every node once and reports the outcome. This is what
// `steve doctor` runs: availability comes from a real connection, not from
// a line in a config file.
func (r *Registry) Probe(ctx context.Context) []Status {
	for _, name := range r.Names() {
		if _, err := r.connect(ctx, name); err != nil {
			log.Printf("node: probe %s: %v", name, err)
		}
	}
	return r.Statuses()
}

// RedialEvery is how often a disconnected node is retried. Nodes come back —
// a restart, a network blip — and re-acquiring them must not wait for someone
// to send a message that happens to need one.
const RedialEvery = 15 * time.Second

// Start dials every node and keeps redialing the ones that are down until the
// context ends.
//
// Without this the registry only connects when something asks it to, so a hub
// that has just started reports every node as down and refuses every
// placement — the roster would be describing the registry's ignorance rather
// than the fleet.
func (r *Registry) Start(ctx context.Context) {
	r.Probe(ctx)
	go func() {
		ticker := time.NewTicker(RedialEvery)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				for _, name := range r.Names() {
					r.mu.Lock()
					live := r.live[name]
					r.mu.Unlock()
					if live != nil && live.alive() {
						continue
					}
					dialCtx, cancel := context.WithTimeout(ctx, defaultDialTimeout)
					_, _ = r.connect(dialCtx, name)
					cancel()
				}
			}
		}
	}()
}

// Close drops every connection. Sessions on them fail as if the nodes went
// offline, which is exactly what has happened.
func (r *Registry) Close() {
	r.mu.Lock()
	live := make([]*conn, 0, len(r.live))
	for _, c := range r.live {
		live = append(live, c)
	}
	r.live = map[string]*conn{}
	r.mu.Unlock()
	for _, c := range live {
		c.close()
	}
}

// connect returns a live connection, dialing if needed. Concurrent callers
// for the same node share one dial.
func (r *Registry) connect(ctx context.Context, name string) (*conn, error) {
	r.mu.Lock()
	cfg, known := r.confs[name]
	if !known {
		r.mu.Unlock()
		return nil, fmt.Errorf("unknown node %q", name)
	}
	if c := r.live[name]; c != nil && c.alive() {
		r.mu.Unlock()
		return c, nil
	}
	delete(r.live, name)
	mcpDial := r.mcpDial
	r.mu.Unlock()

	c, err := dial(ctx, name, r.hub, cfg, mcpDial)
	if err != nil {
		r.remember(&Status{Name: name, Addr: cfg.Addr, LastError: err.Error()})
		return nil, fmt.Errorf("node %q at %s: %w", name, cfg.Addr, err)
	}

	r.mu.Lock()
	// Another goroutine may have won the race; keep whichever landed first
	// so a node never ends up with two connections.
	if existing := r.live[name]; existing != nil && existing.alive() {
		r.mu.Unlock()
		c.close()
		return existing, nil
	}
	r.live[name] = c
	r.mu.Unlock()

	r.remember(&Status{
		Name: name, Addr: cfg.Addr, Up: true, Since: time.Now(), Advert: c.advert,
	})
	go func() {
		<-c.mux.Done()
		log.Printf("node: %s disconnected", name)
		r.mu.Lock()
		if r.live[name] == c {
			delete(r.live, name)
		}
		r.mu.Unlock()
	}()
	return c, nil
}

func (r *Registry) remember(status *Status) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.last[status.Name] = status
}
