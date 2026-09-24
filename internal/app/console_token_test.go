package app

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/gopact-ai/steve/internal/config"
	"github.com/gopact-ai/steve/internal/localtoken"
)

// A Hub is never served without a token: an unconfigured one gets its own,
// kept in the state directory where local clients can find it.
func TestConsoleIsNeverServedWithoutAToken(t *testing.T) {
	state := t.TempDir()
	cfg := &config.Config{Gateway: config.Gateway{StatePath: filepath.Join(state, "state.json"), ReadModelAddr: "127.0.0.1:0"}}
	served, err := consoleServerConfig(nil, cfg)
	if err != nil {
		t.Fatal(err)
	}
	stored, err := localtoken.Read(state)
	if err != nil || served.Token == "" || served.Token != stored {
		t.Fatalf("served token %q, stored %q (%v)", served.Token, stored, err)
	}
	if cfg.Gateway.ReadModelToken != "" {
		t.Fatal("the generated token was written into the configuration")
	}

	cfg.Gateway.ReadModelToken = "configured-token"
	served, err = consoleServerConfig(nil, cfg)
	if err != nil || served.Token != "configured-token" || served.Addr != "127.0.0.1:0" {
		t.Fatalf("configured token not served: %+v %v", served, err)
	}
}

// Exposing the console to the network is the owner's explicit decision: a
// token is generated for loopback only, never to paper over a missing one,
// and the refusal names the setting to fill in.
func TestNetworkConsoleStillNeedsAConfiguredToken(t *testing.T) {
	state := t.TempDir()
	cfg := &config.Config{Gateway: config.Gateway{StatePath: filepath.Join(state, "state.json"), ReadModelAddr: "0.0.0.0:7710"}}
	served, err := consoleServerConfig(nil, cfg)
	if err == nil || served.Token != "" {
		t.Fatalf("network bind without a token = %+v, %v; want a refusal", served, err)
	}
	for _, want := range []string{"0.0.0.0:7710", "gateway.read_model_token"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("refusal %q does not mention %s", err, want)
		}
	}
	if _, err := localtoken.Read(state); err == nil {
		t.Fatal("a token was generated for a network bind")
	}
}
