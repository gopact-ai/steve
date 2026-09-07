// Package desktop owns the local desktop installation and its launcher.
package desktop

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/gopact-ai/steve/internal/config"
)

const profileName = "desktop.json"

type Options struct {
	// StateDir defaults to the per-user application support directory.
	StateDir string
}

type Paths struct {
	Root     string `json:"root"`
	Config   string `json:"config"`
	Profile  string `json:"profile"`
	Identity string `json:"identity"`
	Token    string `json:"-"`
	Endpoint string `json:"endpoint"`
	Process  string `json:"-"`
	Log      string `json:"log"`
}

type Installation struct {
	Paths  Paths  `json:"paths"`
	NodeID string `json:"node_id"`
	URL    string `json:"url"`
	Token  string `json:"-"`
}

type profile struct {
	Version int    `json:"version"`
	NodeID  string `json:"node_id"`
}

func DefaultStateDir() (string, error) {
	root, err := os.UserConfigDir()
	if err != nil {
		return "", fmt.Errorf("find application support directory: %w", err)
	}
	return filepath.Join(root, "Steve"), nil
}

func installationPaths(root string) Paths {
	return Paths{Root: root, Config: filepath.Join(root, "config.json"), Profile: filepath.Join(root, profileName),
		Identity: filepath.Join(root, "node-identity.json"), Token: filepath.Join(root, "loopback-token"),
		Endpoint: filepath.Join(root, "backend.json"), Process: filepath.Join(root, ".desktop-process.json"), Log: filepath.Join(root, "backend.log")}
}

// Bootstrap creates only the desktop's own files. Existing configurations are
// read and validated; no relaunch replaces them or imports agent state.
func Bootstrap(options Options) (*Installation, error) {
	root := options.StateDir
	var err error
	if root == "" {
		root, err = DefaultStateDir()
		if err != nil {
			return nil, err
		}
	}
	root, err = filepath.Abs(root)
	if err != nil {
		return nil, err
	}
	if err := privateDirectory(root); err != nil {
		return nil, err
	}
	unlock, err := lockFile(filepath.Join(root, ".desktop-init.lock"))
	if err != nil {
		return nil, err
	}
	defer unlock()
	paths := installationPaths(root)
	var saved profile
	if err := readJSON(paths.Profile, &saved); err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("read desktop profile: %w", err)
		}
		if _, err := os.Lstat(paths.Config); !errors.Is(err, os.ErrNotExist) {
			return nil, errors.New("desktop configuration already exists without a desktop profile; choose a new state directory")
		}
		id, err := randomID()
		if err != nil {
			return nil, err
		}
		saved = profile{Version: 1, NodeID: id}
		if err := createJSON(paths.Profile, saved); err != nil {
			return nil, err
		}
	}
	if err := saved.validate(); err != nil {
		return nil, err
	}
	var identity struct {
		ID string `json:"id"`
	}
	if err := readJSON(paths.Identity, &identity); errors.Is(err, os.ErrNotExist) {
		identity.ID = saved.NodeID
		if err := createJSON(paths.Identity, identity); err != nil {
			return nil, err
		}
	} else if err != nil {
		return nil, fmt.Errorf("read node identity: %w", err)
	}
	if identity.ID != saved.NodeID {
		return nil, errors.New("desktop profile and node identity disagree")
	}
	token, err := readPrivate(paths.Token)
	if errors.Is(err, os.ErrNotExist) {
		var bytes [32]byte
		if _, err := rand.Read(bytes[:]); err != nil {
			return nil, err
		}
		token = []byte(base64.RawURLEncoding.EncodeToString(bytes[:]))
		if err := createPrivate(paths.Token, token); err != nil {
			return nil, err
		}
	} else if err != nil {
		return nil, fmt.Errorf("read loopback token: %w", err)
	}
	if len(token) < 40 || strings.ContainsAny(string(token), "\r\n\t ") {
		return nil, errors.New("desktop loopback token is invalid")
	}
	if _, err := readPrivate(paths.Config); errors.Is(err, os.ErrNotExist) {
		enabled := false
		workspace := filepath.Join(root, "workspace")
		if err := privateDirectory(workspace); err != nil {
			return nil, err
		}
		cfg := config.Config{
			Agents: map[string]config.Agent{}, Harnesses: map[string]config.Harness{}, MCPServers: map[string]config.MCPServer{},
			Projects: map[string]config.Project{"workspace": {Home: config.ProjectHome{Path: workspace}}},
			Feishu:   config.Feishu{Enabled: &enabled},
			Gateway: config.Gateway{HubID: saved.NodeID, OwnerID: "owner-" + strings.TrimPrefix(saved.NodeID, "node-"),
				Locale: "zh", DefaultChannel: "console", PromptTimeout: config.Duration(10 * time.Minute),
				StatePath: filepath.Join(root, "state.json"), HomePath: filepath.Join(root, "home"),
				ReadModelAddr: "127.0.0.1:0", ReadModelToken: string(token)},
		}
		if err := createJSON(paths.Config, cfg); err != nil {
			return nil, err
		}
	} else if err != nil {
		return nil, fmt.Errorf("read desktop configuration: %w", err)
	}
	cfg, err := config.Load(paths.Config)
	if err != nil {
		return nil, fmt.Errorf("load desktop configuration: %w", err)
	}
	if err := cfg.ValidateChannels(); err != nil {
		return nil, err
	}
	if cfg.Gateway.HubID != saved.NodeID || filepath.Clean(filepath.Dir(cfg.Gateway.StatePath)) != root || cfg.Gateway.ReadModelToken != string(token) {
		return nil, errors.New("desktop configuration does not match its persisted local identity, state directory, or access token")
	}
	address := "http://" + cfg.Gateway.ReadModelAddr
	if err := localURL(address, true); err != nil {
		return nil, fmt.Errorf("desktop endpoint: %w", err)
	}
	return &Installation{Paths: paths, NodeID: saved.NodeID, URL: address, Token: string(token)}, nil
}

func (p profile) validate() error {
	if p.Version != 1 || !strings.HasPrefix(p.NodeID, "node-") || len(p.NodeID) != 37 {
		return errors.New("desktop profile contains an unsupported version or invalid node identity")
	}
	if _, err := hex.DecodeString(strings.TrimPrefix(p.NodeID, "node-")); err != nil {
		return errors.New("desktop node identity is invalid")
	}
	return nil
}

func IsManagedConfig(configPath string) bool {
	if filepath.Base(configPath) != "config.json" {
		return false
	}
	var p profile
	return readJSON(filepath.Join(filepath.Dir(configPath), profileName), &p) == nil && p.validate() == nil
}

func randomID() (string, error) {
	var value [16]byte
	if _, err := rand.Read(value[:]); err != nil {
		return "", err
	}
	return "node-" + hex.EncodeToString(value[:]), nil
}

func localURL(raw string, allowZero bool) error {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "http" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || u.Path != "" {
		return errors.New("expected a plain local HTTP endpoint")
	}
	ip := net.ParseIP(u.Hostname())
	if ip == nil || !ip.IsLoopback() || u.Port() == "" || (!allowZero && u.Port() == "0") {
		return errors.New("desktop requires an explicit loopback address and port")
	}
	return nil
}

func privateDirectory(path string) error {
	if err := os.MkdirAll(path, 0o700); err != nil {
		return fmt.Errorf("create desktop directory: %w", err)
	}
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm()&0o077 != 0 {
		return errors.New("desktop state directory must be a private directory, not a symbolic link")
	}
	return nil
}

func readPrivate(path string) ([]byte, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0o077 != 0 {
		return nil, fmt.Errorf("%s must be a private regular file", filepath.Base(path))
	}
	if info.Size() > 8<<20 {
		return nil, fmt.Errorf("%s exceeds the local metadata size limit", filepath.Base(path))
	}
	return os.ReadFile(path)
}

func readJSON(path string, into any) error {
	raw, err := readPrivate(path)
	if err != nil {
		return err
	}
	return json.Unmarshal(raw, into)
}

func createJSON(path string, value any) error {
	raw, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	return createPrivate(path, append(raw, '\n'))
}

// createPrivate publishes a fully synced file without replacing an existing one.
func createPrivate(path string, raw []byte) error {
	temp, err := os.CreateTemp(filepath.Dir(path), ".desktop-*")
	if err != nil {
		return err
	}
	defer os.Remove(temp.Name())
	if _, err := temp.Write(raw); err != nil {
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
	if err := os.Link(temp.Name(), path); err != nil {
		return fmt.Errorf("create %s: %w", filepath.Base(path), err)
	}
	return syncDirectory(filepath.Dir(path))
}

func syncDirectory(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	return f.Sync()
}
