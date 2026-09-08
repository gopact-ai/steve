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
	"math/rand/v2"
	"net"
	"sort"
	"sync"
	"time"

	"github.com/gopact-ai/steve/internal/ability"
	"github.com/gopact-ai/steve/internal/nodewire"
)

// Config is one remote node as the operator declared it. Everything about
// what the node can *run* comes from its advert, never from here.
type Config struct {
	Addr  string
	Token string
	// DialContext may supply an authenticated transport for this node. The
	// ordinary token handshake still runs inside that connection.
	DialContext func(context.Context, string) (net.Conn, error)
	// DialTimeout bounds one connection attempt; zero takes the default.
	DialTimeout time.Duration
	// Level is the data level the hub assigns this node: what it may hold.
	// Empty is internal.
	Level string
	// Region is whose leases the node's resources carry; empty is the
	// hub's own region.
	Region string
	// PeerAddr is where other nodes reach this one directly; empty means
	// Addr.
	PeerAddr string
}

// Status is a node as the registry currently knows it — the roster's raw
// material and what `/status` and `steve doctor` report.
type Status struct {
	Name       string
	Addr       string
	Generation int64
	// Level is the data level the hub assigned this node.
	Level string
	// Region is whose leases the node\'s resources carry.
	Region    string
	Up        bool
	Since     time.Time
	Advert    nodewire.Advert
	LastError string
}

// Registry keeps one connection per node, dialing lazily and redialing after
// a drop. It never blocks a turn on a node it cannot reach: the dial fails
// fast and the error names the node.
type Registry struct {
	configRevisions map[string]uint64
	configSerial    uint64
	hub             string
	// mcpDial connects to whatever the reverse MCP streams should reach —
	// the hub's loopback agentmcp listener. Nil disables the reverse channel.
	mcpDial          func(ctx context.Context) (net.Conn, error)
	sessionAuthority func(context.Context, string, nodewire.SessionAuthority, nodewire.SessionBinding, nodewire.SessionAction) error

	eventMu   sync.Mutex
	closed    bool
	dialing   map[string]chan struct{}
	changed   map[string]chan struct{}
	idleHooks map[string]map[*idleHook]struct{}
	mu        sync.Mutex
	confs     map[string]Config
	live      map[string]*conn
	last      map[string]*Status
	// hubLvl is the hub machine's own data level.
	hubLvl string
	// gens counts connections per node: the node's generation.
	gens map[string]int64
	// observe hears every change of a node's standing: up with an advert,
	// or down with a reason. History is made of these. drift hears what
	// changed in a node's manifest between two adverts.
	observe func(Status)
	drift   func(node string, changes []string)
}

// SetDriftObserver installs where manifest changes are reported.
func (r *Registry) SetDriftObserver(drift func(node string, changes []string)) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.drift = drift
}

// accept validates a node's snapshot on receipt — whole, fail closed —
// stamps when it arrived by the hub's clock, and refuses one that is
// older than what is already on record. A refused snapshot leaves the
// previous one in force and is said so in the status.
func (r *Registry) accept(name string, adv *nodewire.Advert) {
	if adv.Snapshot == nil {
		return
	}
	now := time.Now().UTC()
	snap := *adv.Snapshot
	snap.ReceivedAt = now
	if err := ability.Validate(&snap); err != nil {
		log.Printf("node: %s: snapshot rejected: %v", name, err)
		adv.Snapshot = nil
		adv.Capabilities = append(adv.Capabilities, "snapshot-rejected")
		return
	}
	r.mu.Lock()
	if last := r.last[name]; last != nil && last.Advert.Snapshot != nil {
		prev := last.Advert.Snapshot
		if prev.Generation == snap.Generation && prev.Sequence > snap.Sequence {
			r.mu.Unlock()
			log.Printf("node: %s: snapshot %d/%d is older than %d/%d on record; ignored", name, snap.Generation, snap.Sequence, prev.Generation, prev.Sequence)
			adv.Snapshot = prev
			return
		}
	}
	r.mu.Unlock()
	adv.Snapshot = &snap
}

// noteDrift compares a fresh advert with the last one on record and
// reports the difference, if any, as structured changes.
func (r *Registry) noteDrift(name string, adv nodewire.Advert) {
	r.mu.Lock()
	var before *ability.Snapshot
	if last := r.last[name]; last != nil {
		before = nodewire.Synthesize(last.Advert, time.Now())
	}
	drift := r.drift
	r.mu.Unlock()
	if drift == nil || before == nil {
		return
	}
	changes := ability.Diff(before, nodewire.Synthesize(adv, time.Now()))
	if len(changes) == 0 {
		return
	}
	lines := make([]string, 0, len(changes))
	for _, c := range changes {
		lines = append(lines, c.String())
	}
	drift(name, lines)
}

// SetObserver installs where connectivity changes are reported.
func (r *Registry) SetObserver(observe func(Status)) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.observe = observe
}

// Generation is how many times the node has connected: it moves on every
// reconnect, so a replica recorded under an older generation is suspect.
func (r *Registry) Generation(_ context.Context, name string) (int64, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.confs[name]; !ok {
		return 0, fmt.Errorf("node %q is not configured", name)
	}
	return r.gens[name], nil
}

// SetHubLevel declares the hub machine's data level (default internal).
func (r *Registry) SetHubLevel(level string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.hubLvl = level
}

func (r *Registry) hubLevel() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.hubLvl == "" {
		return "internal"
	}
	return r.hubLvl
}

func (r *Registry) config(name string) (Config, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	cfg, ok := r.confs[name]
	return cfg, ok
}

const defaultDialTimeout = 10 * time.Second

func NewRegistry(hub string, configs map[string]Config) *Registry {
	confs := make(map[string]Config, len(configs))
	revisions := make(map[string]uint64, len(configs))
	for name, cfg := range configs {
		confs[name] = cfg
		revisions[name] = 1
	}
	return &Registry{
		configRevisions: revisions, configSerial: 1,
		hub: hub, confs: confs,
		live: map[string]*conn{}, last: map[string]*Status{},
		dialing: map[string]chan struct{}{}, changed: map[string]chan struct{}{},
		idleHooks: map[string]map[*idleHook]struct{}{},
	}
}

// SetMCPDialer wires the reverse channel. Remote agents reach the hub's
// messaging server through a loopback listener on their own machine, which
// this registry forwards; without a dialer the node simply gets no such
// listener and its agents lose the send primitive rather than the session.
func (r *Registry) SetMCPDialer(dial func(ctx context.Context) (net.Conn, error)) {
	r.mu.Lock()
	r.mcpDial = dial
	live := make([]*conn, 0, len(r.live))
	for _, c := range r.live {
		live = append(live, c)
	}
	r.mu.Unlock()
	// Connections already up were dialed before the server existed;
	// they serve the channel from now on rather than after a redial.
	for _, c := range live {
		if c.alive() {
			c.startReverse(dial)
		}
	}
}

// Add registers a machine at runtime — the page adding one, not a
// restart — and dials it. A known name has its config replaced.
func (r *Registry) Add(name string, cfg Config) {
	r.mu.Lock()
	r.confs[name] = cfg
	if r.configRevisions == nil {
		r.configRevisions = map[string]uint64{}
	}
	r.configSerial++
	r.configRevisions[name] = r.configSerial
	r.mu.Unlock()
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), defaultDialTimeout)
		defer cancel()
		if _, err := r.connect(ctx, name); err != nil {
			log.Printf("node: %s added; not reachable yet: %v", name, err)
		}
	}()
}

// Remove forgets a machine: its connection is closed, and it is no
// longer dialed, refreshed or listed. Sessions that were on it end with
// the connection.
func (r *Registry) Remove(name string) {
	r.mu.Lock()
	c := r.live[name]
	delete(r.confs, name)
	delete(r.live, name)
	delete(r.last, name)
	if c != nil {
		c.released.Store(true)
	}
	r.signalLocked(name)
	r.mu.Unlock()
	if c != nil {
		c.close()
	}
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
// than at whatever the registry last happened to learn. Named nodes restrict
// that check to an already selected destination.
func (r *Registry) EnsureConnected(ctx context.Context, names ...string) {
	if len(names) == 0 {
		names = r.Names()
	}
	for _, name := range names {
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

// RefreshEvery is how often a connected node is asked to look at itself
// again. Evidence has a TTL — a tool seen fifteen minutes ago is a tool
// nobody has checked since — so a hub that only read the handshake advert
// would, a quarter of an hour later, be unable to place anything that
// needs a tool. Each node is asked on its own jittered clock.
var RefreshEvery = time.Minute

// Start dials every node and keeps redialing the ones that are down until the
// context ends, and keeps every connected node's snapshot fresh.
//
// Without this the registry only connects when something asks it to, so a hub
// that has just started reports every node as down and refuses every
// placement — the roster would be describing the registry's ignorance rather
// than the fleet.
func (r *Registry) Start(ctx context.Context) {
	// Capture startup configuration before background work begins. Tests and
	// later registries can change the default without racing these goroutines.
	refreshEvery := RefreshEvery
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
	go func() {
		ticker := time.NewTicker(refreshEvery)
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
					if live == nil || !live.alive() {
						continue
					}
					go func(name string) {
						// Jitter, so a fleet does not check itself in lockstep.
						select {
						case <-ctx.Done():
							return
						case <-time.After(time.Duration(rand.Int64N(max(1, int64(refreshEvery/5))))):
						}
						refreshCtx, cancel := context.WithTimeout(ctx, defaultDialTimeout)
						defer cancel()
						if _, err := r.Refresh(refreshCtx, name); err != nil {
							log.Printf("node: refresh %s: %v", name, err)
						}
					}(name)
				}
			}
		}
	}()
}

// Close explicitly releases every connection. This is shutdown, not an
// outage: transports must not redial it or keep its processes alive.
func (r *Registry) Close() {
	r.mu.Lock()
	r.closed = true
	for name := range r.confs {
		r.signalLocked(name)
	}
	live := make([]*conn, 0, len(r.live))
	for _, c := range r.live {
		c.released.Store(true)
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
	for {
		r.mu.Lock()
		cfg, known := r.confs[name]
		configurationRevision := r.configRevisions[name]
		if !known || r.closed {
			r.mu.Unlock()
			return nil, fmt.Errorf("node %q released", name)
		}
		if c := r.live[name]; c != nil && c.alive() {
			r.mu.Unlock()
			return c, nil
		}
		if wait := r.dialing[name]; wait != nil {
			r.mu.Unlock()
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-wait:
				continue
			}
		}
		wait := make(chan struct{})
		r.dialing[name] = wait
		mcpDial, previous := r.mcpDial, r.live[name]
		r.mu.Unlock()
		if previous != nil {
			r.down(previous)
		}
		c, err := dial(ctx, name, r.hub, cfg, mcpDial)
		if err != nil {
			r.remember(&Status{Name: name, Addr: cfg.Addr, LastError: err.Error()})
		} else {
			adv := c.getAdvert()
			r.accept(name, &adv)
			c.setAdvert(adv)
			r.noteDrift(name, adv)
			r.eventMu.Lock()
			r.mu.Lock()
			if _, ok := r.confs[name]; !ok || r.configRevisions[name] != configurationRevision || r.closed {
				err = fmt.Errorf("node %q released while dialing", name)
				r.mu.Unlock()
				_ = c.mux.Close()
			} else {
				if r.gens == nil {
					r.gens = map[string]int64{}
				}
				r.gens[name]++
				c.generation = r.gens[name]
				r.live[name] = c
				up := Status{Name: name, Addr: cfg.Addr, Generation: c.generation, Level: levelOr(cfg.Level), Region: cfg.Region, Up: true, Since: time.Now(), Advert: adv}
				r.last[name] = &up
				r.signalLocked(name)
				r.clocksLocked(name, true)
				observe := r.observe
				r.mu.Unlock()
				if observe != nil {
					observe(up)
				}
			}
			r.eventMu.Unlock()
		}
		r.mu.Lock()
		delete(r.dialing, name)
		close(wait)
		r.mu.Unlock()
		if err != nil {
			return nil, fmt.Errorf("node %q at %s: %w", name, cfg.Addr, err)
		}
		go func() { <-c.mux.Done(); r.down(c) }()
		return c, nil
	}
}

func (r *Registry) remember(status *Status) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.last[status.Name] = status
}

func levelOr(level string) string {
	if level == "" {
		return "internal"
	}
	return level
}
