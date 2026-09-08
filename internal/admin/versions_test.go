package admin

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/gopact-ai/steve/internal/config"
	"github.com/gopact-ai/steve/internal/consoleapi"
)

type fixtureReleases struct{ calls int }

func (r *fixtureReleases) Latest(_ context.Context, component, os, arch string) (consoleapi.ReleaseManifest, error) {
	r.calls++
	return consoleapi.ReleaseManifest{Component: component, OS: os, Arch: arch, Version: "next"}, nil
}

func TestVersionsReportsIdentityWithoutCredentialsOrInstalling(t *testing.T) {
	a := &Service{Cfg: &config.Config{Gateway: config.Gateway{HubID: "stable-hub", Peers: map[string]config.HubPeer{"other": {URL: "https://hub.example", Token: "private-test-value"}}}}}
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
	a.Cfg = nil
	v, err = a.Versions(t.Context())
	if err != nil || v.HubID != "" {
		t.Fatalf("unavailable identity replaced with a machine name: %+v, %v", v, err)
	}
}
