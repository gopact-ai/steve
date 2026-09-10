package fleetlab

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"sync"
	"time"
)

// Hub is an isolated application process, its project and its Docker fleet.
// OpenHub never attaches to operator-supplied machines or reads their config.
// Close must be called even when the gate fails; it joins the hub before
// deleting the machines and directories it owns.
type Hub struct {
	URL, Token, ProjectDir, Dir string
	lab                         *docker
	process                     *ownedProcess
	barrier                     *rendezvous
	cancel                      context.CancelFunc
	closeOnce                   sync.Once
	closeErr                    error
}

// OpenHub requires a local Linux Docker engine and a matching Linux Go
// toolchain. The builder receives a copy of that toolchain, not a prebuilt
// artifact. Missing tools are errors, including on explicit acceptance runs.
func OpenHub(ctx context.Context) (_ *Hub, err error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	goroot, err := hubPrerequisites(ctx)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithCancel(ctx)
	h := &Hub{Token: "hub-" + token(16), cancel: cancel}
	h.Dir, err = os.MkdirTemp("", "steve-hublab-")
	if err != nil {
		cancel()
		return nil, err
	}
	defer func() {
		if err != nil {
			err = errors.Join(err, h.Close())
		}
	}()
	h.ProjectDir = filepath.Join(h.Dir, "scratch")
	if err = h.prepare(ctx); err != nil {
		return nil, err
	}
	h.lab, err = startDockerContext(ctx, []Spec{
		{Name: "node-a", Capabilities: []string{"docs"}},
		{Name: "node-b", Capabilities: []string{"build"}, Tools: []string{"go"}},
	}, "./e2e/fleetlab/agent")
	if err != nil {
		return nil, err
	}
	if err = h.installToolchain(ctx, goroot); err != nil {
		return nil, err
	}
	h.barrier, err = newRendezvous(ctx, h.lab.gateway)
	if err != nil {
		return nil, err
	}
	config, err := h.config()
	if err != nil {
		return nil, err
	}
	cmd := exec.Command(filepath.Join(h.Dir, "steve"), "run", "-config", config)
	cmd.Dir = h.Dir
	cmd.Env = h.environment()
	h.process, err = startOwned(cmd, filepath.Join(h.Dir, "hub.log"))
	if err != nil {
		return nil, err
	}
	if err = h.ready(ctx); err != nil {
		return nil, fmt.Errorf("hub readiness: %w\n%s", err, h.Logs())
	}
	return h, nil
}

func hubPrerequisites(ctx context.Context) (string, error) {
	if runtime.GOOS != "linux" {
		return "", errors.New("hub lab requires a Linux host and local Linux Docker engine")
	}
	for _, tool := range []string{"go", "git", "docker", "file", "sha256sum"} {
		if _, err := exec.LookPath(tool); err != nil {
			return "", fmt.Errorf("hub lab requires %s: %w", tool, err)
		}
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if reason := dockerUnavailable(); reason != "" {
		return "", fmt.Errorf("hub lab: %s", reason)
	}
	endpoint, err := runContext(ctx, 30*time.Second, "docker", "context", "inspect", "--format", "{{.Endpoints.docker.Host}}")
	if host := os.Getenv("DOCKER_HOST"); host != "" && os.Getenv("DOCKER_CONTEXT") == "" {
		endpoint = host
	}
	if err != nil || !strings.HasPrefix(strings.TrimSpace(endpoint), "unix://") {
		return "", errors.New("hub lab requires a local Docker Unix socket (remote engines are unsupported)")
	}
	security, err := runContext(ctx, 30*time.Second, "docker", "info", "--format", "{{json .SecurityOptions}}")
	if err != nil {
		return "", fmt.Errorf("Docker security options: %w", err)
	}
	if strings.Contains(security, "rootless") {
		return "", errors.New("hub lab requires rootful Docker bridge networking")
	}
	out, err := runContext(ctx, 30*time.Second, "go", "env", "-json", "GOROOT", "GOHOSTOS", "GOHOSTARCH")
	if err != nil {
		return "", err
	}
	var env struct{ GOROOT, GOHOSTOS, GOHOSTARCH string }
	if err = json.Unmarshal([]byte(out), &env); err != nil {
		return "", err
	}
	arch, err := serverArchContext(ctx)
	if err != nil {
		return "", err
	}
	if env.GOHOSTOS != "linux" || env.GOHOSTARCH != runtime.GOARCH || arch != env.GOHOSTARCH {
		return "", errors.New("hub lab requires matching Linux host, Go toolchain and Docker server architectures")
	}
	for _, name := range []string{"bin/go", "pkg/tool/linux_" + env.GOHOSTARCH + "/compile", "src/runtime/runtime.go"} {
		if _, err = os.Stat(filepath.Join(env.GOROOT, name)); err != nil {
			return "", fmt.Errorf("incomplete Go toolchain: %w", err)
		}
	}
	return env.GOROOT, nil
}

func (h *Hub) installToolchain(ctx context.Context, goroot string) error {
	container := h.lab.containers["node-b"]
	if out, err := runContext(ctx, 2*time.Minute, "docker", "cp", goroot, container+":/opt/go"); err != nil {
		return fmt.Errorf("copy host Go toolchain: %w\n%s", err, out)
	}
	if out, err := h.lab.exec("node-b", "/opt/go/bin/go version"); err != nil {
		return fmt.Errorf("run copied Go toolchain: %w\n%s", err, out)
	}
	if out, err := runContext(ctx, time.Minute, "docker", "exec", "--user", "root", container, "ln", "-s", "/opt/go/bin/go", "/usr/local/bin/go"); err != nil {
		return fmt.Errorf("expose copied Go: %w\n%s", err, out)
	}
	if err := h.lab.stop("node-b"); err != nil {
		return err
	}
	return h.lab.start("node-b")
}

func (h *Hub) environment() []string {
	env := h.baseEnvironment()
	return append(env, "STEVE_LAB_ROLE=coordinator", "STEVE_LAB_BARRIER="+h.barrier.URL, "STEVE_LAB_BARRIER_TOKEN="+h.barrier.token)
}

func (h *Hub) baseEnvironment() []string {
	// Do not discover personal agents, credentials, Git hooks or XDG config.
	return []string{"PATH=" + os.Getenv("PATH"), "HOME=" + filepath.Join(h.Dir, "account"),
		"TMPDIR=" + h.Dir, "XDG_CONFIG_HOME=" + filepath.Join(h.Dir, "xdg-config"),
		"XDG_CACHE_HOME=" + filepath.Join(h.Dir, "xdg-cache"), "XDG_DATA_HOME=" + filepath.Join(h.Dir, "xdg-data"),
		"GIT_CONFIG_NOSYSTEM=1", "GIT_AUTHOR_NAME=Steve Lab", "GIT_AUTHOR_EMAIL=lab@steve.invalid",
		"GIT_COMMITTER_NAME=Steve Lab", "GIT_COMMITTER_EMAIL=lab@steve.invalid"}
}

var dashboardLine = regexp.MustCompile(`steve: dashboard on (http://[^\s]+)`)

func (h *Hub) ready(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	client := &http.Client{Timeout: time.Second}
	defer client.CloseIdleConnections()
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-h.process.done:
			return fmt.Errorf("process exited: %v", h.process.err)
		case <-ticker.C:
		}
		if h.URL == "" {
			raw, _ := os.ReadFile(filepath.Join(h.Dir, "hub.log"))
			if match := dashboardLine.FindSubmatch(raw); len(match) == 2 {
				h.URL = string(match[1])
			}
		}
		if h.URL == "" {
			continue
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, h.URL+"/state", nil)
		if err != nil {
			return err
		}
		req.Header.Set("Authorization", "Bearer "+h.Token)
		resp, err := client.Do(req)
		if err != nil {
			continue
		}
		var state struct {
			Nodes []struct {
				Name string
				Up   bool
			}
			Agents []struct {
				ID       string
				Eligible bool
			}
		}
		err = json.NewDecoder(io.LimitReader(resp.Body, 4<<20)).Decode(&state)
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK || err != nil {
			continue
		}
		up, agents := map[string]bool{}, map[string]bool{}
		for _, n := range state.Nodes {
			up[n.Name] = n.Up
		}
		for _, a := range state.Agents {
			agents[a.ID] = a.Eligible
		}
		if up["node-a"] && up["node-b"] && agents["coordinator"] && agents["writer"] && agents["builder"] {
			return nil
		}
	}
}

// Logs captures diagnostics before Close removes the lab's private files.
func (h *Hub) Logs() string {
	raw, _ := os.ReadFile(filepath.Join(h.Dir, "hub.log"))
	if len(raw) > 24000 {
		raw = raw[len(raw)-24000:]
	}
	return string(raw)
}

// RunGate runs the unchanged acceptance executable against this hub.
func (h *Hub) RunGate(ctx context.Context, scenario string, out io.Writer) error {
	if scenario != "delegate" && scenario != "autonomous" {
		return fmt.Errorf("unknown gate scenario %q", scenario)
	}
	cmd := exec.CommandContext(ctx, filepath.Join(h.Dir, "fleet"), "-hub", h.URL, "-token", h.Token,
		"-config", filepath.Join(h.Dir, "hub.json"), "-project", "scratch", "-agent", "coordinator",
		"-target-node", "node-b", "-target-agent", "builder", "-scenario", scenario)
	cmd.Dir = h.Dir
	cmd.Env = h.environment()
	cmd.Stdout = out
	cmd.Stderr = out
	cmd.WaitDelay = time.Second
	return cmd.Run()
}

func (h *Hub) Close() error {
	h.closeOnce.Do(func() {
		if h.cancel != nil {
			h.cancel()
		}
		if h.process != nil {
			h.closeErr = errors.Join(h.closeErr, h.process.stop())
		}
		if h.barrier != nil {
			h.closeErr = errors.Join(h.closeErr, h.barrier.close())
		}
		if h.lab != nil {
			h.closeErr = errors.Join(h.closeErr, h.lab.remove())
		}
		if h.Dir != "" {
			h.closeErr = errors.Join(h.closeErr, os.RemoveAll(h.Dir))
		}
	})
	return h.closeErr
}
