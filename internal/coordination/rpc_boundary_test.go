package coordination

import (
	"bytes"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"
)

type rpcBodyProbe struct {
	io.Reader
	read, closed bool
}

func (b *rpcBodyProbe) Read(p []byte) (int, error) { b.read = true; return b.Reader.Read(p) }
func (b *rpcBodyProbe) Close() error               { b.closed = true; return nil }

// These overlapping failures pin the transport's precedence: identity,
// route, method, owner authorization, decoding, then caller authorization.
func TestRPCRequestBoundaryGolden(t *testing.T) {
	type response struct {
		Name         string
		Status       int
		Header       http.Header
		Body         string
		Read, Closed bool
		Authorized   []string
	}
	var got []response
	cases := []struct {
		name, action, method, body, owner string
		plain                             bool
		limit                             int64
	}{
		{name: "identity-before-route", action: "missing/nested", method: "GET", plain: true},
		{name: "nested-route", action: "missing/nested", method: "POST"},
		{name: "read-method", action: "state", method: "POST"},
		{name: "mutation-method-before-route", action: "missing", method: "GET"},
		{name: "unknown-mutation", action: "missing", method: "POST"},
		{name: "owner-required-before-body", action: "transfer", method: "POST", body: "{"},
		{name: "owner-denied", action: "policy", method: "POST", body: "{", owner: "deny"},
		{name: "owner-blank", action: "join", method: "POST", owner: "blank"},
		{name: "invalid-json", action: "writer", method: "POST", body: "{"},
		{name: "unknown-field", action: "app", method: "POST", body: `{"unknown":true}`},
		{name: "second-object", action: "writer", method: "POST", body: `{} {}`},
		{name: "body-limit", action: "app", method: "POST", body: `{"id":"oversized"}`, limit: 4},
		{name: "writer-spoof", action: "writer", method: "POST", body: `{"caller_node_id":"other"}`},
		{name: "app-spoof", action: "app", method: "POST", body: `{"caller_node_id":"other"}`},
	}
	for _, action := range []string{"transfer", "policy", "eligibility", "join", "remove", "address"} {
		cases = append(cases, struct {
			name, action, method, body, owner string
			plain                             bool
			limit                             int64
		}{name: action + "-decode", action: action, method: "POST", body: "{", owner: "allow"})
	}
	for _, tc := range cases {
		row := response{Name: tc.name}
		options := RPCOptions{MaxBodyBytes: tc.limit}
		if tc.owner != "" {
			options.AuthorizeControl = func(_ *http.Request, identity Identity, action string) (string, error) {
				row.Authorized = append(row.Authorized, identity.NodeID+":"+action)
				switch tc.owner {
				case "deny":
					return "", errors.New("denied")
				case "blank":
					return " ", nil
				}
				return "owner", nil
			}
		}
		body := &rpcBodyProbe{Reader: strings.NewReader(tc.body)}
		req := httptest.NewRequest(tc.method, RPCPath+tc.action, body)
		if !tc.plain {
			cert := &x509.Certificate{URIs: []*url.URL{IdentityURI("cluster", "peer")}}
			req.TLS = &tls.ConnectionState{PeerCertificates: []*x509.Certificate{cert}, VerifiedChains: [][]*x509.Certificate{{cert}}}
		}
		w := httptest.NewRecorder()
		NewRPCHandler(&Service{config: Config{ClusterID: "cluster"}}, options).ServeHTTP(w, req)
		row.Status, row.Header, row.Body, row.Read, row.Closed = w.Code, w.Header(), w.Body.String(), body.read, body.closed
		got = append(got, row)
	}
	raw, err := json.MarshalIndent(got, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	path := "testdata/rpc_boundary.json"
	if *updateGolden {
		if err := os.WriteFile(path, append(raw, '\n'), 0644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(bytes.TrimSpace(want), raw) {
		t.Fatalf("RPC boundary changed:\n%s", raw)
	}
}
