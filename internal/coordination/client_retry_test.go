package coordination

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
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
	refusal  time.Duration               // how long the others take to name node-2 once elected
	requests atomic.Int32
	latest   atomic.Int64 // when the latest request arrived, in Unix nanoseconds
}

func newElectionPeers(t *testing.T, authority *testAuthority) *electionPeers {
	t.Helper()
	p := &electionPeers{answer: func(w http.ResponseWriter) { json.NewEncoder(w).Encode(Result{Revision: 7}) }}
	for i := 1; i <= 3; i++ {
		id := fmt.Sprintf("node-%d", i)
		server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			p.requests.Add(1)
			p.latest.Store(time.Now().UnixNano())
			w.Header().Set("Content-Type", "application/json")
			switch {
			case !p.elected.Load():
				w.WriteHeader(http.StatusServiceUnavailable)
				json.NewEncoder(w).Encode(rpcFailure{Code: "not_leader", Message: "coordination: not consensus leader: leader is "})
			case id == "node-2":
				p.answer(w)
			default:
				time.Sleep(p.refusal)
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

// A call sent before the retry window ends may take its own timeout, but no
// call starts once the window has ended: the pause after a round in which
// every member refused ends with the window, and the client then gives up
// instead of asking one more member.
func TestClientStartsNoCallAfterItsRetryWindowEnds(t *testing.T) {
	authority := newTestAuthority(t)
	peers := newElectionPeers(t, authority)
	// The first round and the first pause end well inside the window, and
	// the second pause, 100ms, would end after it.
	const window = 150 * time.Millisecond
	client := newElectionClient(t, authority, peers, ClientConfig{RetryWindow: window})
	deadline := time.Now().Add(window)
	_, err := client.Join(t.Context(), JoinRequest{ID: "join-past-window", Actor: "owner", Member: Member{NodeID: "node-4"}})
	if !errors.Is(err, ErrUnavailable) || !errors.Is(err, ErrNotLeader) {
		t.Fatalf("a join without a leader returned %v", err)
	}
	if latest := time.Unix(0, peers.latest.Load()); latest.After(deadline) {
		t.Fatalf("a join started a call %s after its %s retry window ended (%d calls in all)", latest.Sub(deadline).Round(time.Microsecond), window, peers.requests.Load())
	}
}

// A caller that has already given up gets its own context error back even
// when no member address is known, not a report that the cluster is
// unavailable.
func TestClientWithoutPeersReturnsTheCallersOwnContextError(t *testing.T) {
	authority := newTestAuthority(t)
	client, err := NewClient(ClientConfig{
		TLS:            authority.node(t, "cluster", "caller"),
		ControlHeaders: func(context.Context, string) (http.Header, error) { return http.Header{}, nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(client.Close)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err = client.Join(ctx, JoinRequest{ID: "join-canceled", Actor: "owner", Member: Member{NodeID: "node-4"}})
	if err != context.Canceled {
		t.Fatalf("a join whose caller had given up, with no peer address known, returned %v", err)
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

// holdLeader makes node-2 take every call and answer none until the test
// ends.
func holdLeader(t *testing.T, peers *electionPeers) {
	t.Helper()
	release := make(chan struct{})
	// Registered after the servers, so it runs before they close and wait
	// for their handlers.
	t.Cleanup(func() { close(release) })
	peers.answer = func(http.ResponseWriter) { <-release }
}

// saysUnanswered reports whether err says that node-2 was sent action and
// did not answer within the client's 1s timeout, rather than that no member
// took it.
func saysUnanswered(err error, action string) bool {
	return errors.Is(err, ErrUnavailable) && !strings.Contains(err.Error(), "no member took") &&
		strings.Contains(err.Error(), "peer node-2 was sent "+action+" and did not answer within 1s") &&
		strings.Contains(err.Error(), "retrying it with the same ID continues it")
}

// A leader runs a control command in steps, each bounded by its apply
// timeout, so the command can take longer than one call's timeout, and a
// member that forwarded it then stops waiting. The call fails as
// unavailable, as before, but the leader was sent the command and may still
// be working on it, so the error says that and that a retry with the same
// ID continues it, not that no member took it.
func TestClientSaysTheLeaderWasSentACommandItDidNotAnswer(t *testing.T) {
	calls := map[string]func(context.Context, *Client) error{
		"join": func(ctx context.Context, client *Client) error {
			_, err := client.Join(ctx, JoinRequest{ID: "join-unanswered", Actor: "owner", Member: Member{NodeID: "node-4"}})
			return err
		},
		"voting": func(ctx context.Context, client *Client) error {
			_, err := client.SetVoting(ctx, VotingRequest{ID: "voting-unanswered", Actor: "owner", NodeID: "node-4", Voting: true})
			return err
		},
		"transfer": func(ctx context.Context, client *Client) error {
			_, err := client.Transfer(ctx, TransferRequest{ID: "transfer-unanswered", Actor: "owner", ExpectedEpoch: 1, TargetNodeID: "node-3"})
			return err
		},
	}
	for action, call := range calls {
		t.Run(action, func(t *testing.T) {
			authority := newTestAuthority(t)
			peers := newElectionPeers(t, authority)
			holdLeader(t, peers)
			peers.elected.Store(true)
			client := newElectionClient(t, authority, peers, ClientConfig{RetryWindow: time.Second})
			client.mu.Lock()
			client.leader = "node-2"
			client.mu.Unlock()
			if err := call(t.Context(), client); !saysUnanswered(err, action) {
				t.Fatalf("a %s call the leader was sent and did not answer returned %v", action, err)
			}
		})
	}
}

// A member the command never reached is not working on it. A call whose
// connection timed out before the request went out, here in a TLS handshake
// the member never answers, still says that no member took the command.
func TestClientSaysNoMemberTookACommandItNeverSent(t *testing.T) {
	authority := newTestAuthority(t)
	peers := newElectionPeers(t, authority)
	peers.elected.Store(true)
	// The kernel completes connections to a listener that accepts none, and
	// nothing on them answers a TLS handshake.
	silent, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { silent.Close() })
	peers.members[1].APIAddress = "https://" + silent.Addr().String()
	client := newElectionClient(t, authority, peers, ClientConfig{RetryWindow: time.Second})
	client.mu.Lock()
	client.leader = "node-2"
	client.mu.Unlock()
	started := time.Now()
	_, err = client.Join(t.Context(), JoinRequest{ID: "join-never-sent", Actor: "owner", Member: Member{NodeID: "node-4"}})
	if elapsed := time.Since(started); elapsed < time.Second {
		t.Fatalf("a join whose connection to the leader was never set up returned after %s, before its 1s timeout: %v", elapsed.Round(time.Millisecond), err)
	}
	if !errors.Is(err, ErrUnavailable) || !strings.Contains(err.Error(), "no member took join within 1s") || strings.Contains(err.Error(), "was sent") {
		t.Fatalf("a join that timed out before it was sent to the leader returned %v", err)
	}
}

// Only a call that runs out of time is one the member did not answer. A
// member that was sent the command in full and closes the connection
// without answering failed at once, as when its process stops, so the call
// still says that no member took the command.
func TestClientSaysNoMemberTookACommandTheLeaderDroppedWithoutAnswering(t *testing.T) {
	authority := newTestAuthority(t)
	peers := newElectionPeers(t, authority)
	var dropped atomic.Int32
	peers.answer = func(w http.ResponseWriter) {
		conn, _, err := w.(http.Hijacker).Hijack()
		if err != nil {
			t.Error(err)
			return
		}
		dropped.Add(1)
		conn.Close()
	}
	peers.elected.Store(true)
	client := newElectionClient(t, authority, peers, ClientConfig{RetryWindow: time.Second})
	client.mu.Lock()
	client.leader = "node-2"
	client.mu.Unlock()
	_, err := client.Join(t.Context(), JoinRequest{ID: "join-dropped", Actor: "owner", Member: Member{NodeID: "node-4"}})
	if dropped.Load() == 0 {
		t.Fatalf("the leader never got the join: %v", err)
	}
	if !errors.Is(err, ErrUnavailable) || !strings.Contains(err.Error(), "no member took join within 1s") || strings.Contains(err.Error(), "was sent") {
		t.Fatalf("a join the leader dropped without answering returned %v", err)
	}
}

// A member that refuses the command later in the window names the leader,
// which may still be working on it, so that refusal does not hide that the
// leader was sent the command and did not answer.
func TestClientReportsTheUnansweredLeaderAfterAnotherMemberRefuses(t *testing.T) {
	authority := newTestAuthority(t)
	peers := newElectionPeers(t, authority)
	holdLeader(t, peers)
	// node-2 holds the first call until the client's 1s timeout; node-1
	// takes the second inside the window and refuses it after the window.
	peers.refusal = 800 * time.Millisecond
	peers.elected.Store(true)
	client := newElectionClient(t, authority, peers, ClientConfig{RetryWindow: 1500 * time.Millisecond})
	client.mu.Lock()
	client.leader = "node-2"
	client.mu.Unlock()
	_, err := client.Join(t.Context(), JoinRequest{ID: "join-unanswered-then-refused", Actor: "owner", Member: Member{NodeID: "node-4"}})
	if n := peers.requests.Load(); n != 2 {
		t.Fatalf("the join was sent %d times, not to the leader and then to one member that refused it: %v", n, err)
	}
	if !saysUnanswered(err, "join") {
		t.Fatalf("a join the leader did not answer and another member then refused returned %v", err)
	}
}

// A leader that refuses the command after it did not answer an earlier call
// is no longer working on it, as when it has lost its leadership, so the
// call says again that no member took the command.
func TestClientSaysNoMemberTookACommandTheLeaderRefusedAfterNotAnswering(t *testing.T) {
	authority := newTestAuthority(t)
	peers := newElectionPeers(t, authority)
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	var calls atomic.Int32
	peers.answer = func(w http.ResponseWriter) {
		if calls.Add(1) == 1 {
			<-release
			return
		}
		w.WriteHeader(http.StatusServiceUnavailable)
		json.NewEncoder(w).Encode(rpcFailure{Code: "not_leader", Message: "coordination: not consensus leader: leader is "})
	}
	peers.elected.Store(true)
	client := newElectionClient(t, authority, peers, ClientConfig{RetryWindow: 1500 * time.Millisecond})
	client.mu.Lock()
	client.leader = "node-2"
	client.mu.Unlock()
	_, err := client.Join(t.Context(), JoinRequest{ID: "join-unanswered-then-lost", Actor: "owner", Member: Member{NodeID: "node-4"}})
	if n := calls.Load(); n < 2 {
		t.Fatalf("the leader was sent the join %d times, so it never refused it: %v", n, err)
	}
	if !errors.Is(err, ErrNotLeader) || !strings.Contains(err.Error(), "no member took join within 1.5s") || strings.Contains(err.Error(), "was sent") {
		t.Fatalf("a join the leader refused after it did not answer an earlier call returned %v", err)
	}
}

// A read the leader did not answer leaves nothing in progress there to
// continue, so it still says that no member took it.
func TestClientSaysNoMemberTookAReadTheLeaderDidNotAnswer(t *testing.T) {
	authority := newTestAuthority(t)
	peers := newElectionPeers(t, authority)
	holdLeader(t, peers)
	peers.elected.Store(true)
	client := newElectionClient(t, authority, peers, ClientConfig{RetryWindow: time.Second})
	client.mu.Lock()
	client.leader = "node-2"
	client.mu.Unlock()
	_, err := client.ReadState(t.Context())
	if !errors.Is(err, ErrUnavailable) || !strings.Contains(err.Error(), "no member took state within 1s") || strings.Contains(err.Error(), "was sent") {
		t.Fatalf("a read the leader did not answer returned %v", err)
	}
}
