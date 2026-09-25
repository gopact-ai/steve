package coordination

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

// electionPeers serves the coordination RPC of three members through a
// leader election. Until elect is called no member leads and each refuses
// every call as not the leader; afterwards node-2 leads and takes calls, and
// the others name it.
type electionPeers struct {
	members  []Member
	elected  atomic.Bool
	answer   func(w http.ResponseWriter) // node-2's answer once elected
	requests atomic.Int32
}

func newElectionPeers(t *testing.T, authority *testAuthority) *electionPeers {
	t.Helper()
	p := &electionPeers{answer: func(w http.ResponseWriter) { json.NewEncoder(w).Encode(Result{Revision: 7}) }}
	for i := 1; i <= 3; i++ {
		id := fmt.Sprintf("node-%d", i)
		server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			p.requests.Add(1)
			w.Header().Set("Content-Type", "application/json")
			switch {
			case !p.elected.Load():
				w.WriteHeader(http.StatusServiceUnavailable)
				json.NewEncoder(w).Encode(rpcFailure{Code: "not_leader", Message: "coordination: not consensus leader: leader is "})
			case id == "node-2":
				p.answer(w)
			default:
				w.WriteHeader(http.StatusServiceUnavailable)
				json.NewEncoder(w).Encode(rpcFailure{Code: "not_leader", Message: "coordination: not consensus leader: leader is node-2", LeaderID: "node-2"})
			}
		}))
		config, err := authority.node(t, "cluster", id).ServerConfig()
		if err != nil {
			t.Fatal(err)
		}
		server.TLS = config
		server.StartTLS()
		t.Cleanup(server.Close)
		p.members = append(p.members, Member{NodeID: id, APIAddress: server.URL})
	}
	return p
}

func newElectionClient(t *testing.T, authority *testAuthority, peers *electionPeers, config ClientConfig) *Client {
	t.Helper()
	config.TLS = authority.node(t, "cluster", "caller")
	config.Members = peers.members
	config.Timeout = time.Second
	config.ControlHeaders = func(context.Context, string) (http.Header, error) { return http.Header{}, nil }
	client, err := NewClient(config)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(client.Close)
	return client
}

// Raft elects a leader within a few election timeouts, and until then every
// member refuses calls as not the leader. A routed call, such as a Join, has
// to outlast the election instead of giving up after a few refusals.
func TestClientRoutesACallThroughALeaderElection(t *testing.T) {
	authority := newTestAuthority(t)
	peers := newElectionPeers(t, authority)
	client := newElectionClient(t, authority, peers, ClientConfig{})
	const election = 600 * time.Millisecond
	time.AfterFunc(election, func() { peers.elected.Store(true) })
	started := time.Now()
	result, err := client.Join(t.Context(), JoinRequest{ID: "join-during-election", Actor: "owner", Member: Member{NodeID: "node-4"}})
	if err != nil {
		t.Fatalf("a join sent during a %s leader election failed after %s: %v", election, time.Since(started).Round(time.Millisecond), err)
	}
	if result.Revision != 7 {
		t.Fatalf("the join was answered with %+v, not by the elected leader", result)
	}
}

// Only a refusal that ends once the cluster settles is retried: an answer
// about the request itself comes back after one call.
func TestClientDoesNotRetryAnAnswerAboutTheRequest(t *testing.T) {
	authority := newTestAuthority(t)
	peers := newElectionPeers(t, authority)
	peers.answer = func(w http.ResponseWriter) {
		w.WriteHeader(http.StatusConflict)
		json.NewEncoder(w).Encode(rpcFailure{Code: "conflict", Message: "coordination: state changed: membership revision changed"})
	}
	peers.elected.Store(true)
	client := newElectionClient(t, authority, peers, ClientConfig{})
	client.mu.Lock()
	client.leader = "node-2"
	client.mu.Unlock()
	_, err := client.Join(t.Context(), JoinRequest{ID: "join-conflict", Actor: "owner", Member: Member{NodeID: "node-4"}})
	if !errors.Is(err, ErrConflict) {
		t.Fatalf("a conflicting join returned %v", err)
	}
	if n := peers.requests.Load(); n != 1 {
		t.Fatalf("a conflicting join was sent %d times", n)
	}
}
