package desktop

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestExistingBackendIsAuthenticatedAndReused(t *testing.T) {
	installed, err := Bootstrap(Options{StateDir: filepath.Join(t.TempDir(), "Steve")})
	if err != nil {
		t.Fatal(err)
	}
	var requests int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		if r.URL.Path != "/console/versions" || r.Header.Get("Authorization") != "Bearer "+installed.Token {
			t.Error("backend discovery did not authenticate its identity probe")
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]string{"hub_id": installed.NodeID})
	}))
	defer server.Close()
	cleanup, err := PublishEndpoint(installed.Paths.Config, server.URL)
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	result, err := EnsureRunning(ctx, installed, "/this/must/not/be/executed")
	if err != nil {
		t.Fatal(err)
	}
	if result.Started || result.URL != server.URL || requests == 0 {
		t.Fatalf("backend was not reused: %+v", result)
	}
	if result.PID != os.Getpid() {
		t.Fatal("backend process identity was not preserved")
	}
}

func TestWrongBackendIdentityIsNeverAccepted(t *testing.T) {
	installed, err := Bootstrap(Options{StateDir: filepath.Join(t.TempDir(), "Steve")})
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"hub_id":"another-installation"}`))
	}))
	defer server.Close()
	cleanup, err := PublishEndpoint(installed.Paths.Config, server.URL)
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	if _, err := EnsureRunning(ctx, installed, "/this/must/not/be/executed"); err == nil {
		t.Fatal("connected desktop to another installation")
	}
}

func TestPublishEndpointIsPrivateAndRejectsRemoteURLs(t *testing.T) {
	installed, err := Bootstrap(Options{StateDir: filepath.Join(t.TempDir(), "Steve")})
	if err != nil {
		t.Fatal(err)
	}
	for _, raw := range []string{"http://example.com:7710", "http://127.0.0.1:0", "http://127.0.0.1:7710/?token=secret", "https://127.0.0.1:7710"} {
		if _, err := PublishEndpoint(installed.Paths.Config, raw); err == nil {
			t.Fatalf("unsafe endpoint accepted: %s", raw)
		}
	}
	cleanup, err := PublishEndpoint(installed.Paths.Config, "http://127.0.0.1:7710")
	if err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(installed.Paths.Endpoint)
	if err != nil {
		t.Fatal(err)
	}
	info, _ := os.Stat(installed.Paths.Endpoint)
	if info.Mode().Perm() != 0o600 || strings.Contains(string(raw), installed.Token) {
		t.Fatal("backend descriptor disclosed local credentials")
	}
	cleanup()
	if _, err := os.Stat(installed.Paths.Endpoint); !os.IsNotExist(err) {
		t.Fatal("stopped backend kept its live endpoint")
	}
}
