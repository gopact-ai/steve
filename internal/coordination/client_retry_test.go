package coordination

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
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

// Without an elected leader a routed call gives up once its retry window
// ends, as unavailable, and says what it last heard.
func TestClientGivesUpWhenNoLeaderIsElectedWithinItsRetryWindow(t *testing.T) {
	authority := newTestAuthority(t)
	peers := newElectionPeers(t, authority)
	const window = 300 * time.Millisecond
	client := newElectionClient(t, authority, peers, ClientConfig{RetryWindow: window})
	started := time.Now()
	_, err := client.Join(t.Context(), JoinRequest{ID: "join-without-leader", Actor: "owner", Member: Member{NodeID: "node-4"}})
	elapsed := time.Since(started)
	if !errors.Is(err, ErrUnavailable) || !errors.Is(err, ErrNotLeader) {
		t.Fatalf("a join without a leader returned %v", err)
	}
	if elapsed < window || elapsed > window+time.Second {
		t.Fatalf("a join without a leader gave up after %s; its retry window is %s", elapsed, window)
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

// An error the leader could not classify says nothing about whether another
// attempt would succeed, so the call is not repeated; it reaches the caller
// with the leader's text and as neither unavailable nor an invalid request.
func TestClientDoesNotRetryAnErrorTheLeaderCouldNotClassify(t *testing.T) {
	authority := newTestAuthority(t)
	peers := newElectionPeers(t, authority)
	peers.answer = func(w http.ResponseWriter) {
		w.WriteHeader(http.StatusInternalServerError)
		json.NewEncoder(w).Encode(rpcFailure{Code: unclassifiedCode, Message: "leadership transfer timeout"})
	}
	peers.elected.Store(true)
	client := newElectionClient(t, authority, peers, ClientConfig{})
	client.mu.Lock()
	client.leader = "node-2"
	client.mu.Unlock()
	_, err := client.Join(t.Context(), JoinRequest{ID: "join-unclassified", Actor: "owner", Member: Member{NodeID: "node-4"}})
	if err == nil || errors.Is(err, ErrUnavailable) || errors.Is(err, ErrInvalid) || !strings.Contains(err.Error(), "leadership transfer timeout") {
		t.Fatalf("a join the leader failed with an unclassified error returned %v", err)
	}
	if n := peers.requests.Load(); n != 1 {
		t.Fatalf("a join the leader failed with an unclassified error was sent %d times", n)
	}
}

// A caller that gives up while the client waits for a leader gets its own
// cancellation or deadline back, not a report that the cluster is
// unavailable, and gets it when it gives up, not when the retry window ends.
func TestClientReturnsTheCallersOwnContextError(t *testing.T) {
	for name, cause := range map[string]error{"canceled": context.Canceled, "deadline": context.DeadlineExceeded} {
		t.Run(name, func(t *testing.T) {
			authority := newTestAuthority(t)
			peers := newElectionPeers(t, authority)
			client := newElectionClient(t, authority, peers, ClientConfig{})
			const after = 300 * time.Millisecond
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			if cause == context.DeadlineExceeded {
				ctx, cancel = context.WithTimeout(ctx, after)
				defer cancel()
			} else {
				time.AfterFunc(after, cancel)
			}
			started := time.Now()
			_, err := client.Join(ctx, JoinRequest{ID: "join-" + name, Actor: "owner", Member: Member{NodeID: "node-4"}})
			elapsed := time.Since(started)
			if err != cause {
				t.Fatalf("a join whose caller gave up without a leader returned %v, not %v", err, cause)
			}
			if elapsed > after+time.Second {
				t.Fatalf("a join whose caller gave up after %s returned after %s", after, elapsed)
			}
		})
	}
}
