package admin

import (
	"context"
	"encoding/json"
	"net"
	"strings"
	"testing"

	"github.com/gopact-ai/steve/internal/config"
	"github.com/gopact-ai/steve/internal/consoleapi"
	"github.com/gopact-ai/steve/internal/node"
	"github.com/gopact-ai/steve/internal/nodewire"
)

type fixtureReleases struct{ calls int }

func (r *fixtureReleases) Latest(_ context.Context, component, os, arch string) (consoleapi.ReleaseManifest, error) {
	r.calls++
	return consoleapi.ReleaseManifest{Component: component, OS: os, Arch: arch, Version: "next"}, nil
}

func TestVersionsReportsIdentityWithoutCredentialsOrInstalling(t *testing.T) {
	a := &Service{ConfigStore: NewConfigStore(&config.Config{Gateway: config.Gateway{HubID: "stable-hub", Peers: map[string]config.HubPeer{"other": {URL: "https://hub.example", Token: "private-test-value"}}}})}
	v, err := a.Versions(t.Context())
	if err != nil || v.HubID != "stable-hub" || v.Automatic || v.DiscoveryConfigured {
		t.Fatalf("versions = %+v, %v", v, err)
	}
	raw, _ := json.Marshal(v)
	if strings.Contains(string(raw), "private-test-value") {
		t.Fatal("version response exposed peer credentials")
	}
	r := &fixtureReleases{}
	a.releases = r
	v, err = a.Versions(t.Context())
	if err != nil || r.calls != 1 || !v.DiscoveryConfigured || v.Automatic || len(v.Latest) != 1 {
		t.Fatalf("release provider not reported independently of installation: %+v, %v", v, err)
	}
	a.ConfigStore = nil
	v, err = a.Versions(t.Context())
	if err != nil || v.HubID != "" {
		t.Fatalf("unavailable identity replaced with a machine name: %+v, %v", v, err)
	}
}

// A machine refused for its protocol version never sent an advert; the
// version it named in the refusal is the protocol it speaks.
func TestVersionsReportsTheProtocolARefusedNodeSpeaks(t *testing.T) {
	a := &Service{}
	a.Nodes = node.NewRegistry("hub-test", map[string]node.Config{"old": {Addr: "old.example:7701", Token: "t", DialContext: func(context.Context, string) (net.Conn, error) {
		hub, peer := net.Pipe()
		go func() {
			defer peer.Close()
			if _, err := nodewire.ReadFrame(peer); err != nil {
				return
			}
			payload, _ := json.Marshal(map[string]any{"version": 1, "refused": "hub speaks v2–v2, node speaks v1–v1"})
			_ = nodewire.WriteFrame(peer, nodewire.Frame{Kind: nodewire.KindOpen, Payload: payload})
		}()
		return hub, nil
	}}})
	t.Cleanup(a.Nodes.Close)
	a.Nodes.Probe(t.Context())
	v, err := a.Versions(t.Context())
	if err != nil || len(v.Nodes) != 1 || v.Nodes[0].Online || v.Nodes[0].Protocol != 1 {
		t.Fatalf("versions nodes = %+v, %v, want the refused node on protocol 1", v.Nodes, err)
	}
}
