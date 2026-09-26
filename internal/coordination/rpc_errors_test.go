package coordination

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// overRPC answers a call with err as the RPC handler does and reads the reply
// back as the client does, returning the status and the error the caller sees.
func overRPC(t *testing.T, err error) (int, error) {
	t.Helper()
	w := httptest.NewRecorder()
	(&rpcHandler{service: &Service{}}).reply(w, nil, err)
	var failure rpcFailure
	if err := json.Unmarshal(w.Body.Bytes(), &failure); err != nil {
		t.Fatalf("reply to %v is not a coordination error: %q", err, w.Body.String())
	}
	return w.Code, failure.err()
}

// Only errors the service classified as the caller's input travel as invalid
// requests. An error nobody classified, such as a Raft outcome reported only
// as text, says nothing about the request, so a caller that got it over RPC
// must not treat it as a bad request that retrying cannot fix.
func TestRPCReportsOnlyClassifiedCallerErrorsAsInvalid(t *testing.T) {
	for _, cause := range []error{
		unclassifiedRaftError{errors.New("leadership transfer timeout")},
		errors.New("encode command: unsupported value"),
	} {
		status, err := overRPC(t, cause)
		if status < 500 {
			t.Errorf("%v was answered with HTTP %d, a caller error", cause, status)
		}
		if errors.Is(err, ErrInvalid) {
			t.Errorf("%v reached the caller as an invalid request: %v", cause, err)
		}
		if !strings.Contains(err.Error(), cause.Error()) {
			t.Errorf("%v reached the caller without its text: %v", cause, err)
		}
	}
	status, err := overRPC(t, fmt.Errorf("%w: member identity, address, command ID and actor are required", ErrInvalid))
	if status != http.StatusBadRequest || !errors.Is(err, ErrInvalid) {
		t.Errorf("an invalid request was answered with HTTP %d and reached the caller as %v", status, err)
	}
	// A peer running a later build can name an error this build does not know.
	if err := (rpcFailure{Code: "added_later", Message: "not known here"}).err(); errors.Is(err, ErrInvalid) {
		t.Errorf("an error code this build does not know reached the caller as %v", err)
	}
}

// A reply that is not a coordination error, such as a proxy's or a stopping
// server's plain-text page, or a reply the client cannot read, says nothing
// about the request either. A 5xx page is a transient transport failure the
// client may retry.
func TestClientDoesNotReportUnreadablePeerRepliesAsInvalid(t *testing.T) {
	authority := newTestAuthority(t)
	server := authority.node(t, "cluster", "peer")
	caller := authority.node(t, "cluster", "caller")
	tests := []struct {
		name      string
		status    int
		body      string
		transient bool
	}{
		{"bad-gateway", http.StatusBadGateway, "bad gateway\n", true},
		{"stopping", http.StatusServiceUnavailable, "replica stopped\n", true},
		{"unknown-route", http.StatusNotFound, "404 page not found\n", false},
		{"unreadable-success", http.StatusOK, "<html>", false},
		{"oversized-success", http.StatusOK, `{"node_id":"peer","cluster_id":"cluster","padding":"` + strings.Repeat("x", 256) + `"}`, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			peer := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(tc.status)
				w.Write([]byte(tc.body))
			}))
			config, err := server.ServerConfig()
			if err != nil {
				t.Fatal(err)
			}
			peer.TLS = config
			peer.StartTLS()
			defer peer.Close()
			client, err := NewClient(ClientConfig{TLS: caller, Timeout: time.Second, MaxResponseBytes: 128})
			if err != nil {
				t.Fatal(err)
			}
			defer client.Close()
			_, err = client.Status(context.Background(), Member{NodeID: "peer", APIAddress: peer.URL})
			if err == nil || errors.Is(err, ErrInvalid) {
				t.Fatalf("HTTP %d %q reached the caller as %v", tc.status, tc.body, err)
			}
			if errors.Is(err, ErrUnavailable) != tc.transient {
				t.Fatalf("HTTP %d %q reached the caller as %v; transient: %v", tc.status, tc.body, err, tc.transient)
			}
		})
	}
}

// A peer running a build without an action answers it as it answers any
// action it serves only by POST: HTTP 405 and an invalid request. The
// caller learns that the peer does not serve the action, so an operator
// can tell a cluster running mixed builds from a request this build got
// wrong; it is still not a transient failure to retry.
func TestClientNamesAnActionAPeerDoesNotServe(t *testing.T) {
	authority := newTestAuthority(t)
	server := authority.node(t, "cluster", "old")
	caller := authority.node(t, "cluster", "caller")
	peer := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			t.Errorf("the read index request was sent by POST")
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Allow", http.MethodPost)
		w.WriteHeader(http.StatusMethodNotAllowed)
		json.NewEncoder(w).Encode(rpcFailure{Code: "invalid", Message: ErrInvalid.Error()})
	}))
	config, err := server.ServerConfig()
	if err != nil {
		t.Fatal(err)
	}
	peer.TLS = config
	peer.StartTLS()
	defer peer.Close()
	client, err := NewClient(ClientConfig{TLS: caller, Timeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	client.RememberMembers([]Member{{NodeID: "old", APIAddress: peer.URL}})
	_, err = client.ReadIndex(context.Background())
	if err == nil {
		t.Fatal("a peer that does not serve read index requests answered one")
	}
	if errors.Is(err, ErrUnavailable) || errors.Is(err, ErrNotLeader) {
		t.Errorf("a peer that does not serve read index requests reached the caller as a transient failure: %v", err)
	}
	if !strings.Contains(err.Error(), "peer old does not serve readindex requests") {
		t.Errorf("a peer that does not serve read index requests reached the caller as %q, which does not name the action", err)
	}
}
