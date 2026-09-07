package desktop

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/gopact-ai/steve/internal/config"
)

type endpoint struct {
	NodeID   string `json:"node_id"`
	URL      string `json:"url"`
	PID      int    `json:"pid"`
	Instance string `json:"instance"`
}

type LaunchResult struct {
	URL     string `json:"url"`
	PID     int    `json:"pid"`
	NodeID  string `json:"node_id"`
	Started bool   `json:"started"`
}

// PinAddress remembers the OS-selected port once so subsequent service
// restarts retain the web view's origin, including local drafts and preferences.
// Call after the initial listener binds, before publishing configuration users.
func PinAddress(configPath string, cfg *config.Config, address string) error {
	if !IsManagedConfig(configPath) {
		return nil
	}
	if err := localURL(address, false); err != nil {
		return err
	}
	u, _ := url.Parse(address)
	if cfg.Gateway.ReadModelAddr == u.Host {
		return nil
	}
	configured, err := url.Parse("http://" + cfg.Gateway.ReadModelAddr)
	if err != nil || configured.Port() != "0" {
		return errors.New("desktop listener differs from its saved address")
	}
	before := cfg.Gateway.ReadModelAddr
	cfg.Gateway.ReadModelAddr = u.Host
	if err := config.Save(configPath, cfg); err != nil {
		if !config.Committed(err) {
			cfg.Gateway.ReadModelAddr = before
		}
		return err
	}
	return nil
}

// PublishEndpoint is called after the console listener binds. Its descriptor
// has no credentials; the launcher authenticates the backend before using it.
// Non-desktop deployments do not publish a desktop endpoint.
func PublishEndpoint(configPath, address string) (func(), error) {
	if !IsManagedConfig(configPath) {
		return func() {}, nil
	}
	if err := localURL(address, false); err != nil {
		return nil, err
	}
	paths := installationPaths(filepath.Dir(configPath))
	var p profile
	if err := readJSON(paths.Profile, &p); err != nil {
		return nil, err
	}
	instance, err := randomID()
	if err != nil {
		return nil, err
	}
	value := endpoint{NodeID: p.NodeID, URL: address, PID: os.Getpid(), Instance: instance}
	if err := replaceJSON(paths.Endpoint, value); err != nil {
		return nil, err
	}
	return func() {
		var current endpoint
		if readJSON(paths.Endpoint, &current) == nil && current.Instance == instance {
			_ = os.Remove(paths.Endpoint)
			_ = syncDirectory(paths.Root)
		}
	}, nil
}

// EnsureRunning serializes launches, reuses an authenticated live process, and
// starts a detached backend only when the previous process is no longer alive.
// Closing the desktop window has no relationship to this process's lifetime.
func EnsureRunning(ctx context.Context, installed *Installation, executablePath string) (LaunchResult, error) {
	unlock, err := lockFileContext(ctx, filepath.Join(installed.Paths.Root, ".desktop-start.lock"))
	if err != nil {
		return LaunchResult{}, err
	}
	defer unlock()
	var current endpoint
	if err := readJSON(installed.Paths.Endpoint, &current); err == nil {
		if current.NodeID != installed.NodeID {
			return LaunchResult{}, errors.New("desktop backend belongs to another node")
		}
		if processAlive(current.PID) {
			return awaitBackend(ctx, installed, nil)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return LaunchResult{}, fmt.Errorf("read desktop backend endpoint: %w", err)
	}
	// An earlier window can time out while the backend is still preparing
	// its listener. Retain its PID separately from the ready descriptor so
	// retrying during startup cannot create a second process.
	var pending endpoint
	if err := readJSON(installed.Paths.Process, &pending); err == nil {
		if pending.NodeID != installed.NodeID {
			return LaunchResult{}, errors.New("desktop process belongs to another node")
		}
		if processAlive(pending.PID) {
			return awaitBackend(ctx, installed, nil)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return LaunchResult{}, fmt.Errorf("read desktop process: %w", err)
	}
	if err := ctx.Err(); err != nil {
		return LaunchResult{}, err
	}
	log, err := openPrivateLog(installed.Paths.Log)
	if err != nil {
		return LaunchResult{}, err
	}
	defer log.Close()
	command := exec.Command(executablePath, "run", "--config", installed.Paths.Config)
	command.Dir = installed.Paths.Root
	command.Env = os.Environ()
	for i := 0; i < len(command.Env); i++ {
		if strings.HasPrefix(command.Env[i], "PATH=") {
			command.Env = append(command.Env[:i], command.Env[i+1:]...)
			i--
		}
	}
	command.Env = append(command.Env, "PATH="+ExecutablePath(""))
	command.Stdout, command.Stderr = log, log
	detach(command)
	if err := command.Start(); err != nil {
		return LaunchResult{}, fmt.Errorf("start desktop backend: %w", err)
	}
	finished := make(chan error, 1)
	go func() { finished <- command.Wait() }()
	if err := replaceJSON(installed.Paths.Process, endpoint{NodeID: installed.NodeID, PID: command.Process.Pid}); err != nil {
		return LaunchResult{}, fmt.Errorf("remember starting desktop backend: %w", err)
	}
	result, err := awaitBackend(ctx, installed, finished)
	if err != nil {
		// A slow or restarting backend remains independently owned. A UI
		// timeout must not terminate work that the service has accepted.
		return LaunchResult{}, fmt.Errorf("desktop backend did not become ready; inspect %s: %w", filepath.Base(installed.Paths.Log), err)
	}
	result.Started = true
	return result, nil
}

func awaitBackend(ctx context.Context, installed *Installation, finished <-chan error) (LaunchResult, error) {
	client := &http.Client{Timeout: time.Second, Transport: &http.Transport{Proxy: nil}, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	defer client.CloseIdleConnections()
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	var last error
	for {
		var current endpoint
		if err := readJSON(installed.Paths.Endpoint, &current); err == nil {
			if current.NodeID != installed.NodeID || !processAlive(current.PID) {
				last = errors.New("backend identity is stale")
			} else if err := probeBackend(ctx, client, installed, current); err != nil {
				last = err
			} else {
				return LaunchResult{URL: current.URL, PID: current.PID, NodeID: current.NodeID}, nil
			}
		} else {
			last = err
		}
		select {
		case <-ctx.Done():
			return LaunchResult{}, errors.Join(ctx.Err(), last)
		case err := <-finished:
			if err == nil {
				err = errors.New("backend exited before it was ready")
			}
			return LaunchResult{}, err
		case <-ticker.C:
		}
	}
}

func probeBackend(ctx context.Context, client *http.Client, installed *Installation, current endpoint) error {
	if err := localURL(current.URL, false); err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, current.URL+"/console/versions", nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+installed.Token)
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("backend identity probe returned HTTP %d", resp.StatusCode)
	}
	var identity struct {
		HubID string `json:"hub_id"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&identity); err != nil {
		return fmt.Errorf("read backend identity: %w", err)
	}
	if identity.HubID != installed.NodeID {
		return errors.New("the responding backend has a different node identity")
	}
	return nil
}

// AuthenticatedURL is for delivery directly to the local web view. It must not
// be logged, put in process arguments, or included in error messages.
func (i *Installation) AuthenticatedURL(address string) (string, error) {
	if err := localURL(address, false); err != nil {
		return "", err
	}
	u, _ := url.Parse(address)
	u.Path = "/"
	u.RawQuery = url.Values{"token": {i.Token}}.Encode()
	return u.String(), nil
}

func replaceJSON(path string, value any) error {
	if _, err := readPrivate(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	raw, err := json.Marshal(value)
	if err != nil {
		return err
	}
	temp, err := os.CreateTemp(filepath.Dir(path), ".desktop-*")
	if err != nil {
		return err
	}
	defer os.Remove(temp.Name())
	if _, err := temp.Write(append(raw, '\n')); err != nil {
		temp.Close()
		return err
	}
	if err := temp.Sync(); err != nil {
		temp.Close()
		return err
	}
	if err := temp.Close(); err != nil {
		return err
	}
	if err := os.Rename(temp.Name(), path); err != nil {
		return err
	}
	return syncDirectory(filepath.Dir(path))
}
