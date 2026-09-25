package app

import (
	"encoding/json"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gopact-ai/steve/internal/config"
	"github.com/gopact-ai/steve/internal/httpapi"
	"github.com/gopact-ai/steve/internal/localtoken"
	"github.com/gopact-ai/steve/internal/readmodel"
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

// loadConsoleConfig loads a configuration whose console listens on addr with
// token, for a Hub that keeps its state in dir.
func loadConsoleConfig(t *testing.T, dir, addr, token string) *config.Config {
	t.Helper()
	data, err := json.Marshal(map[string]any{
		"gateway":  map[string]string{"read_model_addr": addr, "read_model_token": token, "state_path": filepath.Join(dir, "state.json")},
		"projects": map[string]any{"p": map[string]any{"home": map[string]string{"path": filepath.Join(dir, "home")}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "config.json")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	return cfg
}

// A loopback console gets the generated token however the configuration
// spells its listener, blanks around it included, and when its token is
// only blanks. It listens where the configuration says, or on the default
// address when that is blank.
func TestLoopbackConsoleSpellingsStillGetAGeneratedToken(t *testing.T) {
	for _, c := range []struct{ addr, token, wantAddr string }{
		{"LOCALHOST:7710", "", "LOCALHOST:7710"},
		{" 127.0.0.1:7710 ", "", "127.0.0.1:7710"},
		{"   ", "", "127.0.0.1:7710"},
		{"127.0.0.1:7710", "   ", "127.0.0.1:7710"},
	} {
		dir := t.TempDir()
		served, err := consoleServerConfig(nil, loadConsoleConfig(t, dir, c.addr, c.token))
		stored, readErr := localtoken.Read(dir)
		if err != nil || readErr != nil || served.Token != stored || served.Addr != c.wantAddr {
			t.Errorf("read_model_addr %q, read_model_token %q: served on %q (want %q), generated token served: %v, err: %v", c.addr, c.token, served.Addr, c.wantAddr, readErr == nil && served.Token == stored, err)
		}
	}
}

// A token of blanks on a network console is a missing one, and the refusal
// names the setting to fill in.
func TestBlankTokenOnANetworkConsoleNamesTheSetting(t *testing.T) {
	served, err := consoleServerConfig(nil, loadConsoleConfig(t, t.TempDir(), "0.0.0.0:7710", "   "))
	if err == nil || !strings.Contains(err.Error(), "gateway.read_model_token") {
		t.Fatalf("network console with a blank token = %+v, %v; want a refusal naming gateway.read_model_token", served, err)
	}
}

// The console serves a configured token without the blanks around it, so the
// token signs in alike through the Authorization header, whose value arrives
// without its outer blanks, and through ?token=.
func TestConfiguredTokenSignsInAlikeThroughTheHeaderAndTheQuery(t *testing.T) {
	served, err := consoleServerConfig(nil, loadConsoleConfig(t, t.TempDir(), "127.0.0.1:0", "padded-owner-token\t"))
	if err != nil {
		t.Fatal(err)
	}
	if served.Token != "padded-owner-token" {
		t.Errorf("served token %q, want %q", served.Token, "padded-owner-token")
	}
	server, err := httpapi.NewServer(readmodel.New(readmodel.Sources{}), served)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = server.Close() })
	go func() { _ = server.Serve() }()
	header, _ := http.NewRequest(http.MethodGet, server.URL()+"/state", nil)
	header.Header.Set("Authorization", "Bearer "+served.Token)
	query, _ := http.NewRequest(http.MethodGet, server.URL()+"/state?token="+url.QueryEscape(served.Token), nil)
	for name, req := range map[string]*http.Request{"header": header, "query": query} {
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		res.Body.Close()
		if res.StatusCode != http.StatusOK {
			t.Errorf("served token in the %s = %d, want 200", name, res.StatusCode)
		}
	}
}
