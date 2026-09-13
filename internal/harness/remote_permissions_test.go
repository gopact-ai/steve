package harness

import (
	"context"
	"testing"

	"github.com/gopact-ai/steve/internal/acphost"
	"github.com/gopact-ai/steve/internal/nodewire"
)

type permissionTransport struct{ requests []nodewire.SessionRequest }

func (*permissionTransport) Transport(string, string) acphost.Transport { return nil }
func (p *permissionTransport) NodeSession(_ context.Context, _ string, req nodewire.SessionRequest) (nodewire.SessionState, error) {
	p.requests = append(p.requests, req)
	return nodewire.SessionState{ID: "ns_fixture", State: nodewire.SessionIdle, Binding: req.Binding}, nil
}

func TestSharedPolicyReachesCommittedNodeOpenWithoutLocalCommand(t *testing.T) {
	manager, err := NewManager(nil)
	if err != nil {
		t.Fatal(err)
	}
	transport := &permissionTransport{}
	manager.remote = transport
	policies := map[string]string{"codex": "auto"}
	prepared, _ := NewManager(nil)
	if err := prepared.SetRemotePermissions(policies); err != nil {
		t.Fatal(err)
	}
	manager.Publish(prepared)
	policies["codex"] = "deny"
	if err := prepared.SetRemotePermissions(policies); err != nil {
		t.Fatal(err)
	}
	ctx := WithNodeSession(t.Context(), NodeSessionContext{Binding: nodewire.SessionBinding{NodeID: "worker", AttemptID: "a"}, CommandID: "input"})
	_, handled, err := manager.openNodeSession(ctx, Placement{Node: "worker", Harness: "codex"}, "", "/work", nil)
	if err != nil || !handled {
		t.Fatalf("open: %v %v", handled, err)
	}
	if len(transport.requests) != 1 || transport.requests[0].Permission != "auto" {
		t.Fatalf("node did not receive inherited policy: %+v", transport.requests)
	}
	if _, err := manager.host(Placement{Harness: "codex"}); err == nil {
		t.Fatal("remote policy registered a local command")
	}
	if err := manager.SetRemotePermissions(map[string]string{"codex": "invalid"}); err == nil {
		t.Fatal("invalid policy accepted")
	}
	if manager.remotePermission("codex") != "auto" || manager.remotePermission("new-tool") != "read" {
		t.Fatal("policy changed or unknown tool gained permission")
	}
}
