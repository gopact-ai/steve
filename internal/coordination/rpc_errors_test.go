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
