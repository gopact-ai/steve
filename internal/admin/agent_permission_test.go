package admin

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"os"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gopact-ai/steve/internal/config"
	"github.com/gopact-ai/steve/internal/consoleapi"
	"github.com/gopact-ai/steve/internal/harness"
	"github.com/gopact-ai/steve/internal/node"
	"github.com/gopact-ai/steve/internal/nodewire"
)

func TestExpectedAgentPermissionRejectsMismatchWithoutMutation(t *testing.T) {
	a := agentAdminFixture(t)
	if err := a.ConfigStore.Update(func(c *config.Config) error {
		h := c.Harnesses["mock"]
		h.Permission = "auto"
		c.Harnesses["mock"] = h
		return nil
	}, func(c *config.Config) error { return config.Save(a.Path, c) }); err != nil {
		t.Fatal(err)
	}
	before := a.cfg().Clone()
	catalog := a.Catalog.List()
	disk, err := os.ReadFile(a.Path)
	if err != nil {
		t.Fatal(err)
	}
	writes := 0
	a.WriteConfig = func(path string, c *config.Config) error {
		writes++
		return config.Save(path, c)
	}
	// Decode the actual request to verify that the assertion reaches the
	// administration boundary, rather than only testing a struct literal.
	var req consoleapi.AddAgentRequest
	if err := json.Unmarshal([]byte(`{"id":"new","harness":"mock","expected_permission":"read","expected_permission_revision":"observed-read"}`), &req); err != nil {
		t.Fatal(err)
	}
	if err := a.AddAgent(t.Context(), req); err == nil {
		t.Error("a read confirmation accepted the now-auto permission")
	}
	if writes != 0 || !reflect.DeepEqual(before, a.cfg()) || !reflect.DeepEqual(catalog, a.Catalog.List()) {
		t.Error("permission conflict changed config/catalog or reached persistence")
	}
	after, err := os.ReadFile(a.Path)
	if err != nil || string(after) != string(disk) {
		t.Errorf("permission conflict changed the file: %v", err)
	}
}

// permissionNode speaks the real node protocol over net.Pipe. It only answers
// settings reads and the advert needed by the existing AddAgent preflight.
// No node server, agent process, probe, configuration write or listener exists.
type permissionNode struct {
	mu       sync.Mutex
	settings nodewire.Settings
	advert   nodewire.Advert
	requests []nodewire.OpenRequest
	onRead   func()
}

func remotePermissionFixture(t *testing.T, name string) (*Service, *permissionNode) {
	t.Helper()
	a := agentAdminFixture(t)
	f := &permissionNode{
		settings: nodewire.Settings{Revision: "node-settings-r1", Harnesses: map[string]nodewire.HarnessSetting{
			"mock":        {Command: "/fixture/unknown-must-not-run"},
			"remote-only": {Command: "/fixture/remote-only"},
		}},
		advert: nodewire.Advert{Node: name, OS: "fixture", Arch: "fixture", Harnesses: []nodewire.Harness{
			{ID: "mock", Command: "/fixture/unknown-must-not-run"},
			{ID: "remote-only", Command: "/fixture/remote-only"},
		}},
	}
	publishUnsaved(t, a, func(c *config.Config) {
		c.Nodes = map[string]config.Node{name: {Addr: "fixture-address", Token: "fixture-token"}}
	})
	a.Nodes = node.NewRegistry(a.NodeName, map[string]node.Config{name: {
		Addr: "fixture-address", Token: "fixture-token",
		DialContext: func(context.Context, string) (net.Conn, error) {
			client, server := net.Pipe()
			t.Cleanup(func() { _ = client.Close(); _ = server.Close() })
			go func() {
				if _, err := nodewire.Accept(server, "fixture-token", f.advert); err != nil {
					t.Errorf("fixture handshake: %v", err)
					return
				}
				mux := nodewire.NewMux(server, false)
				defer mux.Close()
				for {
					stream, err := mux.Accept(t.Context())
					if err != nil {
						return
					}
					request := stream.Request()
					f.mu.Lock()
					f.requests = append(f.requests, request)
					settings, onRead := nodewire.CloneSettings(f.settings), f.onRead
					f.mu.Unlock()
					var reply any
					switch request.Kind {
					case nodewire.StreamConfig:
						if request.Command != "get" {
							t.Errorf("permission fixture received configuration mutation: %+v", request)
							stream.Close()
							continue
						}
						if onRead != nil {
							onRead()
						}
						reply = nodewire.ConfigReply{Settings: settings}
					case nodewire.StreamAdvert:
						reply = f.advert
					default:
						t.Errorf("unexpected executable/probe stream: %+v", request)
						stream.Close()
						continue
					}
					if err := json.NewEncoder(stream).Encode(reply); err != nil {
						t.Errorf("fixture reply: %v", err)
					}
					stream.Close()
				}
			}()
			return client, nil
		},
	}})
	t.Cleanup(a.Nodes.Close)
	return a, f
}

func TestAgentPermissionReadsCoordinatorPolicyAndSource(t *testing.T) {
	for _, tc := range []struct {
		name, node, harness, hubPolicy, sharedPolicy, permission string
		source                                                   consoleapi.AgentPermissionSource
	}{
		{"remote-same-name-hub-auto", "worker", "mock", "auto", "", "auto", consoleapi.AgentPermissionHubHarness},
		{"remote-shared-override", "worker", "mock", "auto", "deny", "deny", consoleapi.AgentPermissionSharedRemote},
		{"remote-only-read", "worker", "remote-only", "", "", "read", consoleapi.AgentPermissionDefaultRead},
		{"local-new", "", "mock", "read", "", "read", consoleapi.AgentPermissionHubHarness},
		{"local-default-read", "", "mock", "", "", "read", consoleapi.AgentPermissionDefaultRead},
		{"local-host-does-not-use-remote-override", "", "mock", "read", "auto", "read", consoleapi.AgentPermissionHubHarness},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var a *Service
			var remote *permissionNode
			if tc.node == "" {
				a = agentAdminFixture(t)
			} else {
				a, remote = remotePermissionFixture(t, tc.node)
			}
			publishUnsaved(t, a, func(c *config.Config) {
				h := c.Harnesses["mock"]
				h.Permission = tc.hubPolicy
				c.Harnesses["mock"] = h
				if tc.sharedPolicy != "" {
					c.RuntimePermissions = map[string]string{tc.harness: tc.sharedPolicy}
				}
			})
			// Deliberately stale manager cache must not supply the observation.
			manager, err := harness.NewManager(map[string]harness.Config{"mock": {Command: "/fixture", Permission: "always_allow"}})
			if err != nil {
				t.Fatal(err)
			}
			a.Manager = manager
			t.Cleanup(manager.Stop)
			fact, err := a.AgentPermission(t.Context(), tc.node, tc.harness)
			if err != nil {
				t.Fatal(err)
			}
			if fact.Node != a.place(tc.node) || fact.Harness != tc.harness || fact.Permission != tc.permission || fact.Source != tc.source || len(fact.Revision) != 64 {
				t.Fatalf("coordinator fact = %+v", fact)
			}
			if remote != nil {
				remote.mu.Lock()
				defer remote.mu.Unlock()
				if len(remote.requests) != 1 || remote.requests[0].Kind != nodewire.StreamConfig || remote.requests[0].Command != "get" {
					t.Fatalf("read-only discovery refreshed or executed: %+v", remote.requests)
				}
			}
		})
	}
}

func TestAgentPermissionAcceptsAllSupportedPolicies(t *testing.T) {
	for _, policy := range []string{"read", "write", "deny", "auto", "always_allow"} {
		t.Run(policy, func(t *testing.T) {
			a, _ := remotePermissionFixture(t, "worker")
			publishUnsaved(t, a, func(c *config.Config) { c.RuntimePermissions = map[string]string{"mock": policy} })
			fact, err := a.AgentPermission(t.Context(), "worker", "mock")
			if err != nil || fact.Permission != policy || fact.Source != consoleapi.AgentPermissionSharedRemote {
				t.Fatalf("fact=%+v err=%v", fact, err)
			}
			req := consoleapi.AddAgentRequest{ID: "new", Node: "worker", Harness: "mock", ExpectedPermission: &fact.Permission, ExpectedPermissionRevision: fact.Revision}
			if err := a.AddAgent(t.Context(), req); err != nil {
				t.Fatal(err)
			}
			if a.cfg().RuntimePermissions["mock"] != policy || a.cfg().Harnesses["mock"].Permission != "" {
				t.Fatal("assertion rewrote execution policy")
			}
		})
	}
}

func TestAgentPermissionNormalizesHubAliases(t *testing.T) {
	a := agentAdminFixture(t)
	for _, name := range []string{"", "hub", a.NodeName} {
		fact, err := a.AgentPermission(t.Context(), name, "mock")
		if err != nil || fact.Node != a.NodeName {
			t.Fatalf("local alias %q: %+v %v", name, fact, err)
		}
	}
	cluster, _ := remotePermissionFixture(t, "node-hub")
	cluster.ClusterMode = true
	publishUnsaved(t, cluster, func(c *config.Config) { c.RuntimePermissions = map[string]string{"mock": "deny"} })
	var previous consoleapi.AgentPermission
	for _, name := range []string{"", "hub", cluster.NodeName} {
		fact, err := cluster.AgentPermission(t.Context(), name, "mock")
		if err != nil || fact.Node != cluster.NodeName || fact.Permission != "deny" {
			t.Fatalf("cluster alias %q: %+v %v", name, fact, err)
		}
		if previous.Revision != "" && previous.Revision != fact.Revision {
			t.Fatal("aliases acquired different confirmation scopes")
		}
		previous = fact
	}
	req := consoleapi.AddAgentRequest{ID: "new", Node: "hub", Harness: "mock", ExpectedPermission: &previous.Permission, ExpectedPermissionRevision: previous.Revision}
	if err := cluster.AddAgent(t.Context(), req); err != nil || cluster.cfg().Agents["new"].Node != cluster.NodeName {
		t.Fatalf("cluster alias bind = %v", err)
	}
}

func TestAgentPermissionUnknownTargetNeverInventsPolicy(t *testing.T) {
	a, f := remotePermissionFixture(t, "worker")
	publishUnsaved(t, a, func(c *config.Config) {
		c.RuntimePermissions = map[string]string{"missing": "auto", "mock": "invalid-policy"}
	})
	for _, target := range []struct{ node, harness string }{{"missing", "mock"}, {"worker", "missing"}, {"", "missing"}, {"worker", "mock"}} {
		if _, err := a.AgentPermission(t.Context(), target.node, target.harness); err == nil {
			t.Fatalf("invented permission for %+v", target)
		}
	}
	f.mu.Lock()
	for _, request := range f.requests {
		if request.Kind != nodewire.StreamConfig || request.Command != "get" {
			t.Errorf("invalid target caused executable/probe stream: %+v", request)
		}
	}
	f.mu.Unlock()
	if _, err := (&Service{}).AgentPermission(t.Context(), "worker", "mock"); err == nil {
		t.Fatal("missing configuration invented policy")
	}
}

func TestExpectedAgentPermissionIsCheckedAtAtomicWrite(t *testing.T) {
	a, _ := remotePermissionFixture(t, "worker")
	fact, err := a.AgentPermission(t.Context(), "worker", "mock")
	if err != nil {
		t.Fatal(err)
	}
	req := consoleapi.AddAgentRequest{ID: "new", Node: "worker", Harness: "mock", ExpectedPermission: &fact.Permission, ExpectedPermissionRevision: fact.Revision}
	a.Mu.Lock()
	var once sync.Once
	unlock := func() { once.Do(a.Mu.Unlock) }
	defer unlock()
	result := make(chan error, 1)
	go func() { result <- a.AddAgent(t.Context(), req) }()
	waitForAgentAdminCommit(t, "changeAgents")
	// The remote preflight has finished; another coordinator writer changes
	// policy before this registration's ConfigStore callback gets to run.
	if err := a.ConfigStore.Update(func(c *config.Config) error {
		c.RuntimePermissions = map[string]string{"mock": "auto"}
		return nil
	}, func(c *config.Config) error { return config.Save(a.Path, c) }); err != nil {
		t.Fatal(err)
	}
	current, err := a.AgentPermission(t.Context(), "worker", "mock")
	if err != nil {
		t.Fatal(err)
	}
	before, catalog := a.cfg().Clone(), a.Catalog.List()
	disk, err := os.ReadFile(a.Path)
	if err != nil {
		t.Fatal(err)
	}
	a.WriteConfig = func(string, *config.Config) error {
		t.Error("stale confirmation reached persistence")
		return nil
	}
	unlock()
	select {
	case err := <-result:
		if !errors.Is(err, consoleapi.ErrAgentPermissionConflict) {
			t.Fatalf("stale confirmation = %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("registration did not complete")
	}
	after, _ := os.ReadFile(a.Path)
	observed, err := a.AgentPermission(t.Context(), "worker", "mock")
	if err != nil || !reflect.DeepEqual(before, a.cfg()) || !reflect.DeepEqual(catalog, a.Catalog.List()) || string(after) != string(disk) || observed.Revision != current.Revision {
		t.Fatalf("conflict changed file/live/catalog/revision: %+v %v", observed, err)
	}
}

func TestExpectedAgentPermissionRevisionChecksSourceAndPlacement(t *testing.T) {
	for _, change := range []string{"source", "placement"} {
		t.Run(change, func(t *testing.T) {
			a, _ := remotePermissionFixture(t, "worker")
			fact, err := a.AgentPermission(t.Context(), "worker", "mock")
			if err != nil {
				t.Fatal(err)
			}
			publishUnsaved(t, a, func(c *config.Config) {
				if change == "source" {
					c.RuntimePermissions = map[string]string{"mock": "read"}
				} else {
					n := c.Nodes["worker"]
					n.Addr = "replacement-fixture"
					c.Nodes["worker"] = n
				}
			})
			req := consoleapi.AddAgentRequest{ID: "new", Node: "worker", Harness: "mock", ExpectedPermission: &fact.Permission, ExpectedPermissionRevision: fact.Revision}
			if err := a.AddAgent(t.Context(), req); !errors.Is(err, consoleapi.ErrAgentPermissionConflict) {
				t.Fatalf("changed confirmation scope = %v", err)
			}
		})
	}
}

func TestAgentPermissionRechecksNodeIdentityAfterRead(t *testing.T) {
	a, f := remotePermissionFixture(t, "worker")
	f.onRead = func() {
		publishUnsaved(t, a, func(c *config.Config) {
			n := c.Nodes["worker"]
			n.Token = "replacement-fixture-token"
			c.Nodes["worker"] = n
		})
	}
	if _, err := a.AgentPermission(t.Context(), "worker", "mock"); !errors.Is(err, nodewire.ErrSettingsRevisionConflict) {
		t.Fatalf("reused node name accepted old settings response: %v", err)
	}
}

func TestAgentPermissionPolicyAndRevisionUseOneSnapshot(t *testing.T) {
	a, f := remotePermissionFixture(t, "worker")
	var once sync.Once
	f.onRead = func() {
		once.Do(func() {
			publishUnsaved(t, a, func(c *config.Config) { c.RuntimePermissions = map[string]string{"mock": "auto"} })
		})
	}
	fact, err := a.AgentPermission(t.Context(), "worker", "mock")
	if err != nil || fact.Permission != "auto" || fact.Source != consoleapi.AgentPermissionSharedRemote {
		t.Fatalf("fact from mixed snapshots: %+v %v", fact, err)
	}
	current, err := a.AgentPermission(t.Context(), "worker", "mock")
	if err != nil || current.Revision != fact.Revision {
		t.Fatalf("policy and revision belonged to different versions: %+v / %+v %v", fact, current, err)
	}
}

func TestExpectedAgentPermissionLocalConfirmationKeepsDefaultPolicy(t *testing.T) {
	a := agentAdminFixture(t)
	fact, err := a.AgentPermission(t.Context(), a.NodeName, "mock")
	if err != nil {
		t.Fatal(err)
	}
	req := consoleapi.AddAgentRequest{ID: "new", Node: a.NodeName, Harness: "mock", ExpectedPermission: &fact.Permission, ExpectedPermissionRevision: fact.Revision}
	if err := a.AddAgent(t.Context(), req); err != nil || a.cfg().Agents["new"].Node != "" || a.cfg().Harnesses["mock"].Permission != "" {
		t.Fatalf("local assertion changed placement/default policy: %v", err)
	}
}

func TestExpectedAgentPermissionCannotReuseAnotherTargetScope(t *testing.T) {
	a, _ := remotePermissionFixture(t, "worker")
	fact, err := a.AgentPermission(t.Context(), "worker", "mock")
	if err != nil {
		t.Fatal(err)
	}
	for _, req := range []consoleapi.AddAgentRequest{
		{ID: "new", Node: "", Harness: "mock", ExpectedPermission: &fact.Permission, ExpectedPermissionRevision: fact.Revision},
		{ID: "new", Node: "worker", Harness: "remote-only", ExpectedPermission: &fact.Permission, ExpectedPermissionRevision: fact.Revision},
	} {
		if err := a.AddAgent(t.Context(), req); !errors.Is(err, consoleapi.ErrAgentPermissionConflict) {
			t.Fatalf("another placement reused confirmation: %+v %v", req, err)
		}
	}
}

func TestExpectedAgentPermissionLegacyOmissionDoesNotRewritePolicy(t *testing.T) {
	for _, raw := range []string{`{"id":"new","node":"worker","harness":"mock"}`, `{"id":"new","node":"worker","harness":"mock","expected_permission":null}`} {
		a, _ := remotePermissionFixture(t, "worker")
		publishUnsaved(t, a, func(c *config.Config) { c.RuntimePermissions = map[string]string{"mock": "deny"} })
		var req consoleapi.AddAgentRequest
		if err := json.Unmarshal([]byte(raw), &req); err != nil {
			t.Fatal(err)
		}
		if err := a.AddAgent(t.Context(), req); err != nil {
			t.Fatal(err)
		}
		if a.cfg().RuntimePermissions["mock"] != "deny" || a.cfg().Harnesses["mock"].Permission != "" {
			t.Fatal("legacy registration weakened policy")
		}
	}
}

func TestExpectedAgentPermissionRejectsIncompleteOrInvalidAssertion(t *testing.T) {
	for _, fields := range []string{
		`"expected_permission":"read"`, `"expected_permission_revision":"r1"`,
		`"expected_permission":"","expected_permission_revision":"r1"`,
		`"expected_permission":"full","expected_permission_revision":"r1"`,
	} {
		a := agentAdminFixture(t)
		a.WriteConfig = func(string, *config.Config) error {
			t.Error("invalid confirmation reached persistence")
			return nil
		}
		var req consoleapi.AddAgentRequest
		if err := json.Unmarshal([]byte(`{"id":"new","harness":"mock",`+fields+`}`), &req); err != nil {
			t.Fatal(err)
		}
		if err := a.AddAgent(t.Context(), req); !errors.Is(err, consoleapi.ErrAgentPermissionConfirmationInvalid) {
			t.Fatalf("invalid assertion = %v", err)
		}
	}
}

func TestAgentPermissionDoesNotExposeLaunchOrCredentials(t *testing.T) {
	a, _ := remotePermissionFixture(t, "worker")
	publishUnsaved(t, a, func(c *config.Config) {
		h := c.Harnesses["mock"]
		h.Env = []string{"PRIVATE=fixture-private-value"}
		h.Permission = "auto"
		c.Harnesses["mock"] = h
	})
	fact, err := a.AgentPermission(t.Context(), "worker", "mock")
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(fact)
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{"fixture-private-value", "fixture-token", "fixture-address", "unknown-must-not-run"} {
		if strings.Contains(string(raw), forbidden) {
			t.Fatalf("fact exposed config: %s", raw)
		}
	}
}
