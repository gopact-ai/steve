package coordination

import (
	"context"
	"net"
	"sync"
)

// Route is the way from this node to one other node when that is not the
// address the node advertises. An SSH tunnel is the usual case: the other
// node is reachable at a loopback port that exists only on this machine.
// Routes are this node's own knowledge; they never enter the shared state,
// and the TLS identity check on every connection is what proves the
// address answers for the node it was meant for.
type Route struct {
	// Raft is host:port reaching the node's Raft listener.
	Raft string `json:"raft"`
	// API is host:port reaching the node's HTTPS listener.
	API string `json:"api"`
}

// RouteTable holds this node's routes to other nodes by node ID. Dialers
// consult it on every connection, so a route can appear, change or go away
// while the node runs. A nil table has no routes.
type RouteTable struct {
	mu     sync.RWMutex
	routes map[string]Route
}

func NewRouteTable(routes map[string]Route) *RouteTable {
	t := &RouteTable{routes: map[string]Route{}}
	for id, route := range routes {
		t.routes[id] = route
	}
	return t
}

// Route answers where this node connects to nodeID, if not at what the
// node advertises.
func (t *RouteTable) Route(nodeID string) (Route, bool) {
	if t == nil {
		return Route{}, false
	}
	t.mu.RLock()
	defer t.mu.RUnlock()
	route, ok := t.routes[nodeID]
	return route, ok
}

func (t *RouteTable) Set(nodeID string, route Route) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.routes[nodeID] = route
}

func (t *RouteTable) Delete(nodeID string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	delete(t.routes, nodeID)
}

// All copies the table, for persisting or showing it.
func (t *RouteTable) All() map[string]Route {
	if t == nil {
		return map[string]Route{}
	}
	t.mu.RLock()
	defer t.mu.RUnlock()
	all := make(map[string]Route, len(t.routes))
	for id, route := range t.routes {
		all[id] = route
	}
	return all
}

// RaftDial wraps dial so a connection meant for nodeID's Raft listener goes
// through the route when there is one; the address raft asks for is what
// the node advertises and is ignored in that case.
func (t *RouteTable) RaftDial(nodeID string, dial DialFunc) DialFunc {
	return t.dial(nodeID, dial, func(route Route) string { return route.Raft })
}

// APIDial is RaftDial for the node's HTTPS listener.
func (t *RouteTable) APIDial(nodeID string, dial DialFunc) DialFunc {
	return t.dial(nodeID, dial, func(route Route) string { return route.API })
}

func (t *RouteTable) dial(nodeID string, dial DialFunc, pick func(Route) string) DialFunc {
	return func(ctx context.Context, network, address string) (net.Conn, error) {
		if route, ok := t.Route(nodeID); ok && pick(route) != "" {
			address = pick(route)
		}
		return dial(ctx, network, address)
	}
}
