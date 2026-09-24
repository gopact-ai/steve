package node

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/gopact-ai/acp"
	"github.com/gopact-ai/steve/internal/nativehistory"
	"github.com/gopact-ai/steve/internal/nodewire"
	"github.com/gopact-ai/steve/internal/plugins"
)

func mcpRefreshFixture(t *testing.T, command string) (ServerConfig, nodewire.SessionRequest, sessionRecord) {
	t.Helper()
	cfg, req, old := resumedFixture(t, command)
	req.MCPServers = []acp.MCPServer{acp.HTTPMCPServer("steve", "http://127.0.0.1:1/mcp", []acp.HTTPHeader{{Name: "aUtHoRiZaTiOn", Value: "Bearer revoked-test-token"}, {Name: "X-Tools", Value: "unchanged"}})}
	old.ConfigHash = sessionConfigHash(req)
	req.MCPServers[0].Headers[0].Value = "Bearer fresh-test-token"
	req.MCPAuthorizationRefresh = &nodewire.MCPAuthorizationRefresh{PreviousAuthorization: "Bearer revoked-test-token"}
	return cfg, req, old
}

func TestMCPAuthorizationRefreshColdResumeAndReplay(t *testing.T) {
	var accepted, rejected atomic.Int32
	endpoint := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer fresh-test-token" {
			rejected.Add(1)
			http.Error(w, "revoked", http.StatusUnauthorized)
			return
		}
		accepted.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":{"content":[{"type":"text","text":"sent message_id=test"}]}}`))
	}))
	defer endpoint.Close()
	cfg, req, old := mcpRefreshFixture(t, buildMockAgent(t))
	req.MCPServers[0].URL = endpoint.URL
	req.MCPServers[0].Headers[0].Value = req.MCPAuthorizationRefresh.PreviousAuthorization
	old.ConfigHash = sessionConfigHash(req)
	req.MCPServers[0].Headers[0].Value = "Bearer fresh-test-token"
	saveResumeFixture(t, cfg, old)
	s := NewServer(cfg)
	if err := s.startSessions(t.Context()); err != nil {
		t.Fatal(err)
	}
	defer func() { s.sessions.Close() }()
	state, err := s.sessions.Do(t.Context(), "cluster-1", req)
	if err != nil {
		t.Fatalf("rotated credential prevented cold resume: %v", err)
	}
	current, _, err := s.sessions.readRecord(state.ID)
	if err != nil || current.UpstreamID != old.UpstreamID || state.ContextID != old.State.ID || current.ConfigHash != sessionConfigHash(req) || current.ResumedFrom != old.State.ID {
		t.Fatalf("refresh lost native context or new config: %+v %v", current, err)
	}
	if req.MCPServers[0].Headers[0].Value != "Bearer fresh-test-token" {
		t.Fatal("proof verification mutated request")
	}
	archived, _, _ := s.sessions.readRecord(old.State.ID)
	if archived.ConfigHash != old.ConfigHash || !reflect.DeepEqual(archived.Commands, old.Commands) || archived.State.Binding != old.State.Binding {
		t.Fatal("refresh rewrote original history")
	}
	input := req
	input.MCPAuthorizationRefresh = nil
	input.ID, input.Action, input.CommandID, input.InputSequence, input.Text = state.ID, nodewire.SessionActionPrompt, "fresh-input", 1, "mcpfull"
	state, err = s.sessions.Do(t.Context(), "cluster-1", input)
	if err != nil {
		t.Fatal(err)
	}
	for range 100 {
		if state.Command != nil && state.Command.Settled {
			break
		}
		input.Action, input.After, input.WaitMS = nodewire.SessionActionPoll, state.Sequence, 50
		state, err = s.sessions.Do(t.Context(), "cluster-1", input)
		if err != nil {
			t.Fatal(err)
		}
	}
	if state.Command == nil || !state.Command.Settled || !strings.Contains(state.Command.Output, "recalled_ok=true") || accepted.Load() == 0 || rejected.Load() != 0 {
		t.Fatalf("native load did not receive fresh header: %+v accepted=%d rejected=%d", state, accepted.Load(), rejected.Load())
	}
	for _, restart := range []bool{false, true} {
		if restart {
			stop := input
			stop.Action, stop.CommandID = nodewire.SessionActionClose, ""
			if _, err := s.sessions.Do(t.Context(), "cluster-1", stop); err != nil {
				t.Fatal(err)
			}
			s.sessions.Close()
			s = NewServer(cfg)
			if err := s.startSessions(t.Context()); err != nil {
				t.Fatal(err)
			}
		}
		replay, err := s.sessions.Do(t.Context(), "cluster-1", req)
		if err != nil || replay.ID != current.State.ID || replay.ProcessStopped != restart {
			t.Fatalf("replay restart=%v: %+v %v", restart, replay, err)
		}
		changed := req
		changed.MCPAuthorizationRefresh = &nodewire.MCPAuthorizationRefresh{PreviousAuthorization: "Bearer unrelated-test-token"}
		if _, err := s.sessions.Do(t.Context(), "cluster-1", changed); err == nil {
			t.Fatal("changed proof altered reserved open")
		}
	}
}

func TestMCPAuthorizationRefreshRefusalsBeforeReservation(t *testing.T) {
	cases := []string{"nil-proof", "wrong-proof", "wrong-server", "sse", "duplicate-server", "duplicate-authorization", "missing-authorization", "empty-bearer", "invalid-bearer", "same-bearer", "endpoint", "tools", "other-server", "header-name", "workdir", "permission", "plugin", "import", "process", "receipt", "question", "stale-authority", "stale-writer", "unauthenticated", "start", "same-execution", "fresh-open"}
	for _, mode := range cases {
		t.Run(mode, func(t *testing.T) {
			cfg, req, old := mcpRefreshFixture(t, "/must-not-start")
			principal := "cluster-1"
			switch mode {
			case "nil-proof":
				req.MCPAuthorizationRefresh = nil
			case "wrong-proof":
				req.MCPAuthorizationRefresh.PreviousAuthorization = "Bearer unrelated-test-token"
			case "wrong-server":
				req.MCPServers[0].Name = "other"
			case "sse":
				req.MCPServers[0].Type = acp.MCPServerTypeSSE
			case "duplicate-server":
				req.MCPServers = append(req.MCPServers, req.MCPServers[0])
			case "duplicate-authorization":
				req.MCPServers[0].Headers = append(req.MCPServers[0].Headers, acp.HTTPHeader{Name: "Authorization", Value: "Bearer fresh-test-token"})
			case "missing-authorization":
				req.MCPServers[0].Headers = req.MCPServers[0].Headers[1:]
			case "empty-bearer":
				req.MCPServers[0].Headers[0].Value = "Bearer "
			case "invalid-bearer":
				req.MCPServers[0].Headers[0].Value = "Bearer a\r\nX: b"
			case "same-bearer":
				req.MCPServers[0].Headers[0].Value = req.MCPAuthorizationRefresh.PreviousAuthorization
			case "endpoint":
				req.MCPServers[0].URL += "/changed"
			case "tools":
				req.MCPServers[0].Headers[1].Value = "changed"
			case "other-server":
				req.MCPServers = append(req.MCPServers, acp.HTTPMCPServer("other", "http://127.0.0.1:2", nil))
			case "header-name":
				req.MCPServers[0].Headers[0].Name = "Authorization"
			case "workdir":
				req.Workdir = t.TempDir()
			case "permission":
				req.Permission = "auto"
			case "plugin":
				req.Plugin = &plugins.RuntimeRef{ID: "changed"}
			case "import":
				req.NativeImport = &nativehistory.Reference{ID: "changed"}
			case "process":
				old.State.ProcessStopped = false
			case "receipt":
				c := old.Commands["old-input"]
				c.Settled = false
				old.Commands["old-input"] = c
			case "question":
				old.State.Questions = []nodewire.SessionQuestion{{State: "unknown"}}
			case "stale-authority":
				old.Authority.CoordinatorEpoch++
			case "stale-writer":
				old.Authority.WriterGeneration++
			case "unauthenticated":
				principal = "other"
			case "start":
				cfg.SessionAuthorizer.(*sessionAuthorityTest).denyStart = true
			case "same-execution":
				req.Binding, req.CommandID = old.State.Binding, old.OpenID
			case "fresh-open":
				req.ID = ""
			}
			saveResumeFixture(t, cfg, old)
			s := NewServer(cfg)
			if err := s.startSessions(t.Context()); err != nil {
				t.Fatal(err)
			}
			defer s.sessions.Close()
			_, err := s.sessions.Do(t.Context(), principal, req)
			if err == nil {
				t.Fatal("unsafe refresh accepted")
			}
			if strings.Contains(err.Error(), "test-token") {
				t.Fatal("refusal disclosed credential")
			}
			id := nodewire.SessionOpenID(req.Authority.ClusterID, req.Binding.NodeID, req.Binding.AttemptID, req.CommandID, req.Harness)
			if _, exists, readErr := s.sessions.readRecord(id); readErr != nil || exists || len(s.sessions.sessions) != 0 {
				t.Fatalf("refusal reserved a process: exists=%v err=%v", exists, readErr)
			}
			archived, _, _ := s.sessions.readRecord(old.State.ID)
			if archived.ResumeTarget != "" {
				t.Fatal("refusal consumed source")
			}
		})
	}
}

func TestMCPAuthorizationRefreshCannotReuseLiveSession(t *testing.T) {
	cfg, req, old := mcpRefreshFixture(t, buildMockAgent(t))
	// First open uses the original credential; only the next cold open may rotate.
	original := req
	original.MCPAuthorizationRefresh = nil
	original.MCPServers = append([]acp.MCPServer(nil), req.MCPServers...)
	original.MCPServers[0].Headers = append([]acp.HTTPHeader(nil), req.MCPServers[0].Headers...)
	original.MCPServers[0].Headers[0].Value = req.MCPAuthorizationRefresh.PreviousAuthorization
	saveResumeFixture(t, cfg, old)
	s := NewServer(cfg)
	if err := s.startSessions(t.Context()); err != nil {
		t.Fatal(err)
	}
	defer s.sessions.Close()
	first, err := s.sessions.Do(t.Context(), "cluster-1", original)
	if err != nil {
		t.Fatal(err)
	}
	req.ID, req.CommandID, req.Binding.TaskID = first.ID, "rotated-open", "task-3"
	if _, err := s.sessions.Do(t.Context(), "cluster-1", req); err == nil {
		t.Fatal("refresh rebound live process")
	}
	// A proof cannot turn an exact-config warm rebind into a credential refresh.
	same := original
	same.ID, same.CommandID, same.Binding.TaskID = first.ID, req.CommandID, req.Binding.TaskID
	same.MCPAuthorizationRefresh = &nodewire.MCPAuthorizationRefresh{PreviousAuthorization: "Bearer unrelated-test-token"}
	if _, err := s.sessions.Do(t.Context(), "cluster-1", same); err == nil {
		t.Fatal("proof permitted live exact-config reuse")
	}
	before, _, _ := s.sessions.readRecord(first.ID)
	if before.State.Binding != first.Binding {
		t.Fatal("refused refresh changed live binding")
	}
	s.sessions.sessions[first.ID].host.Close()
	resumed, err := s.sessions.Do(t.Context(), "cluster-1", req)
	if err != nil || resumed.ID == first.ID || resumed.ContextID != old.State.ID {
		t.Fatalf("observed process exit did not allow cold refresh: %+v %v", resumed, err)
	}
}

func TestMCPAuthorizationRefreshDoesNotWeakenConfigHash(t *testing.T) {
	_, req, old := mcpRefreshFixture(t, "/must-not-start")
	// Imported history and plugin selection are bound even in hash-only records.
	req.NativeImport = &nativehistory.Reference{ID: "import", Revision: "revision"}
	req.Plugin = &plugins.RuntimeRef{ID: "runtime", Selection: plugins.Selection{Deployments: []string{"one"}}}
	req.MCPServers[0].Headers[0].Value = req.MCPAuthorizationRefresh.PreviousAuthorization
	old.ConfigHash = sessionConfigHash(req)
	req.MCPServers[0].Headers[0].Value = "Bearer fresh-test-token"
	if err := validateResumeSource(req, old); err != nil {
		t.Fatal(err)
	}
	for _, mode := range []string{"plugin", "import"} {
		t.Run(mode, func(t *testing.T) {
			changed := req
			changed.Plugin = req.Plugin.Clone()
			changed.NativeImport = req.NativeImport.Clone()
			if mode == "plugin" {
				changed.Plugin.Selection.Deployments[0] = "two"
			} else {
				changed.NativeImport.Revision = "changed"
			}
			if err := validateResumeSource(changed, old); err == nil {
				t.Fatal("refresh authorized unrelated configuration change")
			}
		})
	}
	// Without the proof the fresh header stays in the full hash, which then
	// no longer matches the source: the resume fails closed.
	raw, _ := json.Marshal(req)
	var withoutProof map[string]json.RawMessage
	_ = json.Unmarshal(raw, &withoutProof)
	delete(withoutProof, "mcp_authorization_refresh")
	raw, _ = json.Marshal(withoutProof)
	var decoded nodewire.SessionRequest
	_ = json.Unmarshal(raw, &decoded)
	if decoded.MCPAuthorizationRefresh != nil || sessionConfigHash(decoded) == old.ConfigHash {
		t.Fatal("a request without the proof would not fail closed")
	}
}

// Even when the ordinary full config hash matches (e.g. a later handoff in a
// chain), the proof is part of the immutable open command identity.
func TestMCPAuthorizationRefreshProofIsBoundToReservedOpen(t *testing.T) {
	cfg, req, old := mcpRefreshFixture(t, buildMockAgent(t))
	old.ConfigHash = sessionConfigHash(req)
	saveResumeFixture(t, cfg, old)
	s := NewServer(cfg)
	if err := s.startSessions(t.Context()); err != nil {
		t.Fatal(err)
	}
	defer func() { s.sessions.Close() }()
	first, err := s.sessions.Do(t.Context(), "cluster-1", req)
	if err != nil {
		t.Fatal(err)
	}
	for _, restart := range []bool{false, true} {
		if restart {
			stop := req
			stop.ID, stop.Action, stop.CommandID, stop.MCPAuthorizationRefresh = first.ID, nodewire.SessionActionClose, "", nil
			if _, err := s.sessions.Do(t.Context(), "cluster-1", stop); err != nil {
				t.Fatal(err)
			}
			s.sessions.Close()
			s = NewServer(cfg)
			if err := s.startSessions(t.Context()); err != nil {
				t.Fatal(err)
			}
		}
		changed := req
		changed.MCPAuthorizationRefresh = &nodewire.MCPAuthorizationRefresh{PreviousAuthorization: "Bearer different-proof"}
		if _, err := s.sessions.Do(t.Context(), "cluster-1", changed); err == nil || !strings.Contains(err.Error(), "open command already used") {
			t.Fatalf("proof not bound to open restart=%v: %v", restart, err)
		}
		changed.MCPAuthorizationRefresh = nil
		if _, err := s.sessions.Do(t.Context(), "cluster-1", changed); err == nil || !strings.Contains(err.Error(), "open command already used") {
			t.Fatalf("removed proof not bound to open restart=%v: %v", restart, err)
		}
		if state, err := s.sessions.Do(t.Context(), "cluster-1", req); err != nil || state.ID != first.ID {
			t.Fatalf("identical replay failed: %+v %v", state, err)
		}
	}
}

func TestMCPAuthorizationRefreshRejectsAmbiguousConfiguration(t *testing.T) {
	for _, mode := range []string{"wrong-server", "sse", "duplicate-server", "duplicate-authorization", "missing-authorization"} {
		t.Run(mode, func(t *testing.T) {
			_, req, old := mcpRefreshFixture(t, "/must-not-start")
			switch mode {
			case "wrong-server":
				req.MCPServers[0].Name = "plugin"
			case "sse":
				req.MCPServers[0].Type = acp.MCPServerTypeSSE
			case "duplicate-server":
				req.MCPServers = append(req.MCPServers, acp.HTTPMCPServer("steve", "http://127.0.0.1:2", nil))
			case "duplicate-authorization":
				req.MCPServers[0].Headers = append(req.MCPServers[0].Headers, acp.HTTPHeader{Name: "Authorization", Value: "Bearer fresh-test-token"})
			case "missing-authorization":
				req.MCPServers[0].Headers[0].Name = "X-Other"
			}
			req.MCPServers[0].Headers[0].Value = req.MCPAuthorizationRefresh.PreviousAuthorization
			old.ConfigHash = sessionConfigHash(req)
			req.MCPServers[0].Headers[0].Value = "Bearer fresh-test-token"
			if err := validateResumeSource(req, old); err == nil {
				t.Fatal("proof matched hash but authorized an ambiguous or non-built-in credential")
			}
		})
	}
}

func TestMCPAuthorizationRefreshBearerValidation(t *testing.T) {
	for _, value := range []string{"", "Bearer ", "Basic abc", "Bearer a b", "Bearer a\t", "Bearer a\r\n", " Bearer abc", "Bearer abc,def", "Bearer =", "Bearer a=b", "Bearer 中文"} {
		t.Run(value, func(t *testing.T) {
			_, req, old := mcpRefreshFixture(t, "/must-not-start")
			req.MCPServers[0].Headers[0].Value = value
			if err := validateResumeSource(req, old); err == nil {
				t.Fatal("invalid new Bearer accepted")
			}
			req.MCPAuthorizationRefresh.PreviousAuthorization = value
			old.ConfigHash = sessionConfigHash(req)
			req.MCPServers[0].Headers[0].Value = "Bearer fresh-test-token"
			if err := validateResumeSource(req, old); err == nil {
				t.Fatal("invalid previous Bearer accepted")
			}
		})
	}
	for _, value := range []string{"Bearer a", "bearer a-z_A.0~+/=="} {
		_, req, old := mcpRefreshFixture(t, "/must-not-start")
		req.MCPServers[0].Headers[0].Value = value
		if err := validateResumeSource(req, old); err != nil {
			t.Fatalf("valid new Bearer refused: %v", err)
		}
	}
}

func TestMCPAuthorizationRefreshRestartRequiresReconciliation(t *testing.T) {
	cfg, req, old := mcpRefreshFixture(t, buildMockAgent(t))
	saveResumeFixture(t, cfg, old)
	s := NewServer(cfg)
	if err := s.startSessions(t.Context()); err != nil {
		t.Fatal(err)
	}
	first, err := s.sessions.Do(t.Context(), "cluster-1", req)
	if err != nil {
		s.sessions.Close()
		t.Fatal(err)
	}
	s.sessions.Close() // Native process stopped, but no coordinator close receipt.
	s = NewServer(cfg)
	if err := s.startSessions(t.Context()); err != nil {
		t.Fatal(err)
	}
	defer s.sessions.Close()
	if _, err := s.sessions.Do(t.Context(), "cluster-1", req); err == nil {
		t.Fatal("restart silently reopened an interrupted reserved execution")
	}
	if len(s.sessions.sessions) != 0 {
		t.Fatal("restart replay started a process")
	}
	reconcile := req
	reconcile.ID, reconcile.Action, reconcile.MCPAuthorizationRefresh = "", nodewire.SessionActionCancelOpen, nil
	stopped, err := s.sessions.Do(t.Context(), "cluster-1", reconcile)
	if err != nil || stopped.ID != first.ID || !stopped.ProcessStopped {
		t.Fatalf("reserved refresh could not be reconciled: %+v %v", stopped, err)
	}
	replay, err := s.sessions.Do(t.Context(), "cluster-1", req)
	if err != nil || replay.ID != first.ID || !replay.ProcessStopped {
		t.Fatalf("reconciled replay lost reserved identity: %+v %v", replay, err)
	}
	if len(s.sessions.sessions) != 0 {
		t.Fatal("reconciled replay started a process")
	}
}
