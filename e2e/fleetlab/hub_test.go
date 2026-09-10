package fleetlab

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestHubMissingToolsAreErrors(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	h, err := OpenHub(t.Context())
	if h != nil || err == nil || errors.Is(err, ErrUnavailable) {
		t.Fatalf("missing tools must fail, got hub=%v err=%v", h, err)
	}
	if !strings.Contains(err.Error(), "requires") {
		t.Fatal(err)
	}
}
func TestHubCancelledBeforeStartupCreatesNothing(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	h, err := OpenHub(ctx)
	if h != nil || !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled startup = %v, %v", h, err)
	}
}
func requireHubLab(t *testing.T) {
	t.Helper()
	if os.Getenv("STEVE_HUBLAB_E2E") == "" {
		t.Skip("set STEVE_HUBLAB_E2E=1 to exercise disposable hubs")
	}
	// Once requested, missing Docker or other prerequisites is a failure.
	if _, err := hubPrerequisites(t.Context()); err != nil {
		t.Fatal(err)
	}
}
func TestHubGatesUseIndependentLabs(t *testing.T) {
	requireHubLab(t)
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Minute)
	defer cancel()
	type opened struct {
		h   *Hub
		err error
	}
	pending := make(chan opened, 2)
	for range 2 {
		go func() { h, err := OpenHub(ctx); pending <- opened{h, err} }()
	}
	var labs []*Hub
	for range 2 {
		r := <-pending
		if r.err != nil {
			t.Error(r.err)
		}
		if r.h != nil {
			labs = append(labs, r.h)
			t.Cleanup(func() {
				if err := r.h.Close(); err != nil {
					t.Error(err)
				}
			})
		}
	}
	if len(labs) != 2 {
		t.FailNow()
	}
	a, b := labs[0], labs[1]
	if a.Dir == b.Dir || a.ProjectDir == b.ProjectDir || a.URL == b.URL || a.Token == b.Token || a.lab.network == b.lab.network {
		t.Fatal("labs share state, project, endpoint, token or network")
	}
	for _, name := range []string{"node-a", "node-b"} {
		if a.lab.nodes[name].Token == b.lab.nodes[name].Token || a.lab.nodes[name].Addr == b.lab.nodes[name].Addr {
			t.Fatal("labs share node identity")
		}
	}
	results := make(chan error, 2)
	for i, scenario := range []string{"delegate", "autonomous"} {
		go func() {
			var out bytes.Buffer
			err := labs[i].RunGate(ctx, scenario, &out)
			t.Log(out.String())
			results <- err
		}()
	}
	for range 2 {
		if err := <-results; err != nil {
			t.Error(err)
		}
	}
	for _, h := range labs {
		if err := h.Close(); err != nil {
			t.Error(err)
		}
		assertHubRemoved(t, h)
	}
}
func TestHubGateFailureAndCancellationReleaseResources(t *testing.T) {
	requireHubLab(t)
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Minute)
	defer cancel()
	h, err := OpenHub(ctx)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = h.Close() })
	// The unchanged gate rejects a missing required project file.
	if err = os.Remove(filepath.Join(h.ProjectDir, "kvtool", "main.go")); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err = h.RunGate(ctx, "autonomous", &out); err == nil || !strings.Contains(out.String(), "must exist") {
		t.Fatalf("preflight failure was hidden: %v\n%s", err, out.String())
	}
	cancel()
	if err = h.RunGate(ctx, "delegate", &out); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled gate = %v", err)
	}
	if err = h.Close(); err != nil {
		t.Fatal(err)
	}
	assertHubRemoved(t, h)
}
func assertHubRemoved(t *testing.T, h *Hub) {
	t.Helper()
	if _, err := os.Stat(h.Dir); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("hub directory survived: %v", err)
	}
	if _, err := os.Stat(h.lab.work); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("node build directory survived: %v", err)
	}
	select {
	case <-h.process.done:
	default:
		t.Error("hub process was not joined")
	}
	// ACP agents use their own process groups. Closing the hub's pipes must
	// also terminate those children, including when the hub was killed.
	deadline := time.Now().Add(3 * time.Second)
	for {
		var alive []string
		paths, _ := filepath.Glob("/proc/[0-9]*/cmdline")
		for _, path := range paths {
			raw, _ := os.ReadFile(path)
			exe, _, _ := strings.Cut(string(raw), "\x00")
			if strings.HasPrefix(exe, h.Dir+"/") {
				alive = append(alive, path)
			}
		}
		if len(alive) == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Errorf("hub processes survived: %v", alive)
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	for _, name := range h.lab.containers {
		if out, err := run(10*time.Second, "docker", "container", "inspect", name); err == nil || !strings.Contains(out, "No such") {
			t.Errorf("container %s cleanup: %v %s", name, err, out)
		}
	}
	if out, err := run(10*time.Second, "docker", "network", "inspect", h.lab.network); err == nil || (!strings.Contains(out, "No such network:") && !strings.Contains(out, "not found")) {
		t.Errorf("network cleanup: %v %s", err, out)
	}
}

// The Docker shim reports failure after creating the first container. This
// exercises the ambiguous outcome of an interrupted docker run, not a failure
// before there was anything to release. Only this test's resource is inspected.
func TestHubPartialDockerStartupReleasesResources(t *testing.T) {
	requireHubLab(t)
	realDocker, err := exec.LookPath("docker")
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	record := filepath.Join(dir, "network")
	script := "#!/bin/sh\nif [ \"$1 $2\" = 'network create' ]; then printf '%s' \"$3\" > '" + record + "'; fi\n'" + realDocker + "' \"$@\"\nstatus=$?\nif [ \"$1\" = run ] && [ \"$status\" = 0 ]; then echo injected-startup-failure >&2; exit 42; fi\nexit \"$status\"\n"
	if err = os.WriteFile(filepath.Join(dir, "docker"), []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Minute)
	defer cancel()
	h, err := OpenHub(ctx)
	if h != nil || err == nil || !strings.Contains(err.Error(), "injected-startup-failure") {
		t.Fatalf("injected startup = %v %v", h, err)
	}
	raw, err := os.ReadFile(record)
	if err != nil {
		t.Fatal(err)
	}
	network := string(raw)
	if out, err := run(10*time.Second, "docker", "network", "inspect", network); err == nil || (!strings.Contains(out, "No such network:") && !strings.Contains(out, "not found")) {
		t.Fatalf("partial startup leaked network: %v %s", err, out)
	}
	if out, err := run(10*time.Second, "docker", "ps", "-a", "--filter", "name="+network, "--format", "{{.Names}}"); err != nil || strings.TrimSpace(out) != "" {
		t.Fatalf("partial startup leaked containers: %v %s", err, out)
	}
}

func TestHubCancellationDuringActiveDelegation(t *testing.T) {
	requireHubLab(t)
	bounded, end := context.WithTimeout(t.Context(), 3*time.Minute)
	defer end()
	ctx, cancel := context.WithCancel(bounded)
	defer cancel()
	h, err := OpenHub(ctx)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = h.Close() })
	done := make(chan error, 1)
	var out bytes.Buffer
	go func() { done <- h.RunGate(ctx, "autonomous", &out) }()
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		h.barrier.mu.Lock()
		active := false
		for _, g := range h.barrier.groups {
			active = active || len(g.roles) > 0
		}
		h.barrier.mu.Unlock()
		if active {
			break
		}
		select {
		case err := <-done:
			t.Fatalf("gate ended before a worker arrived: %v\n%s", err, out.String())
		case <-bounded.Done():
			t.Fatal(bounded.Err())
		case <-ticker.C:
		}
	}
	cancel()
	if err := <-done; err == nil {
		t.Fatal("cancelled active gate succeeded")
	}
	if err := h.Close(); err != nil {
		t.Fatal(err)
	}
	assertHubRemoved(t, h)
}

func TestHubCancelledDuringDockerCreationWaitsThenCleans(t *testing.T) {
	requireHubLab(t)
	realDocker, err := exec.LookPath("docker")
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	record := filepath.Join(dir, "network")
	created := filepath.Join(dir, "created")
	release := filepath.Join(dir, "release")
	script := "#!/bin/sh\nif [ \"$1 $2\" = 'network create' ]; then printf '%s' \"$3\" > '" + record + "'; fi\n'" + realDocker + "' \"$@\"\nstatus=$?\nif [ \"$1\" = run ] && [ \"$status\" = 0 ]; then touch '" + created + "'; while [ ! -e '" + release + "' ]; do sleep 0.1; done; fi\nexit \"$status\"\n"
	if err = os.WriteFile(filepath.Join(dir, "docker"), []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("TMPDIR", dir)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	finished := make(chan error, 1)
	go func() {
		h, err := OpenHub(ctx)
		if h != nil {
			err = errors.Join(err, h.Close())
		}
		finished <- err
	}()
	// Release the shim even if an assertion fails, so no helper is stranded.
	t.Cleanup(func() { _ = os.WriteFile(release, nil, 0600) })
	deadline := time.NewTimer(2 * time.Minute)
	defer deadline.Stop()
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		if _, err := os.Stat(created); err == nil {
			break
		}
		select {
		case err := <-finished:
			t.Fatalf("startup ended before creation: %v", err)
		case <-deadline.C:
			t.Fatal("container was not created")
		case <-ticker.C:
		}
	}
	cancel()
	if err = os.WriteFile(release, nil, 0600); err != nil {
		t.Fatal(err)
	}
	if err = <-finished; !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled creation = %v", err)
	}
	raw, err := os.ReadFile(record)
	if err != nil {
		t.Fatal(err)
	}
	if out, err := run(10*time.Second, "docker", "network", "inspect", string(raw)); err == nil || (!strings.Contains(out, "No such network:") && !strings.Contains(out, "not found")) {
		t.Fatalf("cancelled startup leaked network: %v %s", err, out)
	}
	if out, err := run(10*time.Second, "docker", "ps", "-a", "--filter", "name="+string(raw), "--format", "{{.Names}}"); err != nil || strings.TrimSpace(out) != "" {
		t.Fatalf("cancelled startup leaked containers: %v %s", err, out)
	}
	for _, pattern := range []string{"steve-hublab-*", "steve-fleetlab-*"} {
		paths, _ := filepath.Glob(filepath.Join(dir, pattern))
		if len(paths) != 0 {
			t.Errorf("cancelled startup leaked directories: %v", paths)
		}
	}
}
