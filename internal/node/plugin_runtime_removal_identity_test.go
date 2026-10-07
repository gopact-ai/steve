package node

import (
	"bytes"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/gopact-ai/steve/internal/harness"
	"github.com/gopact-ai/steve/internal/nodewire"
	"github.com/gopact-ai/steve/internal/plugins"
)

func TestNodePluginRuntimeRemovalRejectsSelectionDriftBeforeStoppingBroker(t *testing.T) {
	const body = "original runtime route"
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, body)
	}))
	t.Cleanup(upstream.Close)
	transport := &http.Transport{DisableKeepAlives: true}
	t.Cleanup(transport.CloseIdleConnections)
	client := &http.Client{
		Transport: transport,
		Timeout:   2 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	tests := []struct {
		name  string
		drift func(*plugins.Selection)
	}{
		{"project", func(s *plugins.Selection) { s.Project = "other-project" }},
		{"harness", func(s *plugins.Selection) { s.Harness = "other-harness" }},
		{"excluded_skill", func(s *plugins.Selection) { s.ExcludedSkills = []string{"skill"} }},
		{"capability_filter", func(s *plugins.Selection) {
			s.Filters = map[string]plugins.CapabilityFilter{
				"runtime": {Skills: []string{}, MCP: []string{"api"}},
			}
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			s := NewServer(ServerConfig{Name: "worker", StateDir: t.TempDir()})
			selection := nodeRuntimeFixture(t, s, upstream.URL)
			pool := s.pluginRuntimePool()
			t.Cleanup(func() {
				if err := pool.Close(); err != nil {
					t.Errorf("close runtime broker: %v", err)
				}
			})
			runtime, err := pool.Prepare(t.Context(), "runtime-removal", selection,
				harness.Config{Command: "not-executed", Permission: "read"})
			if err != nil {
				t.Fatal(err)
			}
			runtime, err = pool.Load(t.Context(), runtime.Ref)
			if err != nil {
				t.Fatal(err)
			}
			if len(runtime.Servers) != 1 || runtime.Servers[0].URL == "" {
				t.Fatal("runtime must expose exactly one HTTP descriptor")
			}
			route := runtime.Servers[0].URL
			checkRoute := func() {
				t.Helper()
				response, err := client.Get(route)
				if err != nil {
					t.Fatalf("original broker route stopped: %v", err)
				}
				defer response.Body.Close()
				raw, err := io.ReadAll(io.LimitReader(response.Body, 1024))
				if err != nil || response.StatusCode != http.StatusOK || string(raw) != body {
					t.Fatalf("original broker route changed: status=%d err=%v", response.StatusCode, err)
				}
			}
			checkRoute()
			store := s.pluginStore()
			if err := store.RetireRuntime(t.Context(), runtime.Ref); err != nil {
				t.Fatal(err)
			}
			checkRoute()
			type runtimeFile struct {
				info os.FileInfo
				data []byte
			}
			before := map[string]runtimeFile{}
			for _, path := range []string{
				filepath.Join(store.RuntimeDir(runtime.Ref.ID), "runtime.json"),
				filepath.Join(store.RuntimeDir(runtime.Ref.ID), "retired.json"),
				filepath.Join(store.Dir, "runtime-commands", "runtime-removal.json"),
			} {
				info, err := os.Lstat(path)
				if err != nil || !info.Mode().IsRegular() {
					t.Fatalf("runtime record must be a regular file: %v", err)
				}
				raw, err := os.ReadFile(path)
				if err != nil {
					t.Fatal(err)
				}
				before[path] = runtimeFile{info: info, data: raw}
			}
			drift := runtime.Ref.Clone()
			test.drift(&drift.Selection)
			if drift.ID != runtime.Ref.ID || drift.Selection.Node != "worker" {
				t.Fatal("selection drift must keep the original runtime ID and node")
			}
			if err := drift.Validate(); err != nil {
				t.Fatalf("drift must remain a valid runtime reference: %v", err)
			}
			originalHash, err := runtime.Ref.Selection.Hash()
			if err != nil {
				t.Fatal(err)
			}
			driftHash, err := drift.Selection.Hash()
			if err != nil || driftHash == originalHash {
				t.Fatalf("selection identity must actually differ: %v", err)
			}
			request := nodewire.PluginRequest{
				Action:    nodewire.PluginRuntimeRemove,
				Node:      "worker",
				Authority: nodewire.SessionAuthority{ClusterID: "hub"},
				Runtime:   drift,
				Selection: &drift.Selection,
			}
			if _, err := s.pluginOperation(t.Context(), "hub", request); !errors.Is(err, plugins.ErrIntegrity) {
				t.Fatalf("wrong selection must return ErrIntegrity: %v", err)
			}
			checkRoute()
			if _, err := store.Runtime(runtime.Ref); err != nil {
				t.Fatalf("rejected removal lost the original runtime: %v", err)
			}
			for path, original := range before {
				info, err := os.Lstat(path)
				if err != nil || !os.SameFile(original.info, info) || info.Mode() != original.info.Mode() {
					t.Fatalf("rejected removal changed runtime file identity or mode: %v", err)
				}
				raw, err := os.ReadFile(path)
				if err != nil || !bytes.Equal(raw, original.data) {
					t.Fatalf("rejected removal changed runtime file bytes: %v", err)
				}
			}
			request.Runtime = runtime.Ref.Clone()
			request.Selection = &request.Runtime.Selection
			if _, err := s.pluginOperation(t.Context(), "hub", request); err != nil {
				t.Fatalf("original runtime removal failed: %v", err)
			}
			if _, err := os.Lstat(store.RuntimeDir(runtime.Ref.ID)); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("original runtime directory must be removed: %v", err)
			}
			response, err := client.Get(route)
			if err != nil {
				if !errors.Is(err, syscall.ECONNREFUSED) {
					t.Fatalf("removed route must refuse connections, not fail ambiguously: %v", err)
				}
				return
			}
			defer response.Body.Close()
			if response.StatusCode != http.StatusForbidden && response.StatusCode != http.StatusNotFound {
				t.Fatalf("removed broker route remains admitted: status=%d", response.StatusCode)
			}
		})
	}
}
