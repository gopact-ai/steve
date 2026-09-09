package plugins

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"time"
)

// ResolvedServer is node-local runtime input. Never serialize it across the
// control plane: URL headers and environment may contain resolved credentials.
type ResolvedServer struct {
	Type    string
	Command string
	Args    []string
	Env     map[string]string
	URL     string
	Headers map[string]string
}

// MarshalJSON refuses to turn resolved credentials into a control-plane response.
func (ResolvedServer) MarshalJSON() ([]byte, error) {
	return nil, fmt.Errorf("resolved plugin server contains node-local configuration")
}

type DeploymentReceipt struct {
	Schema     int        `json:"schema"`
	Hash       string     `json:"hash"`
	Deployment Deployment `json:"deployment"`
	PreparedAt time.Time  `json:"prepared_at"`
}

type Environment struct {
	OS, Arch string
	LookPath func(string) (string, error)
}

func (s *Store) PrepareDeployment(ctx context.Context, d Deployment, environment Environment) (DeploymentReceipt, error) {
	hash, err := d.Hash()
	if err != nil {
		return DeploymentReceipt{}, err
	}
	bundle, err := s.Read(d.Digest)
	if err != nil {
		return DeploymentReceipt{}, err
	}
	if bundle.Manifest.ID != d.PackageID {
		return DeploymentReceipt{}, ErrIntegrity
	}
	if err := bundle.Manifest.CheckConfiguration(d.Configuration); err != nil {
		return DeploymentReceipt{}, err
	}
	if err := checkPlatform(bundle.Manifest, environment); err != nil {
		return DeploymentReceipt{}, err
	}
	if _, err := s.ResolveServers(d, environment); err != nil {
		return DeploymentReceipt{}, err
	}
	if err := ctx.Err(); err != nil {
		return DeploymentReceipt{}, err
	}
	if err := s.ensure(); err != nil {
		return DeploymentReceipt{}, err
	}
	unlock, err := s.lock(ctx)
	if err != nil {
		return DeploymentReceipt{}, err
	}
	defer unlock()
	if receipt, err := s.Deployment(hash); err == nil {
		return receipt, s.sync(filepath.Join(s.Dir, "deployments"))
	} else if !os.IsNotExist(err) {
		return DeploymentReceipt{}, err
	}
	receipt := DeploymentReceipt{Schema: Schema, Hash: hash, Deployment: d, PreparedAt: time.Now().UTC()}
	raw, err := json.Marshal(receipt)
	if err != nil {
		return DeploymentReceipt{}, err
	}
	if err := s.writeRecord(filepath.Join(s.Dir, "deployments", hash+".json"), raw); err != nil {
		return receipt, err
	}
	return receipt, nil
}

func (s *Store) Deployment(hash string) (DeploymentReceipt, error) {
	if !digestShape.MatchString(hash) {
		return DeploymentReceipt{}, ErrInvalid
	}
	root, err := os.OpenRoot(s.Dir)
	if err != nil {
		return DeploymentReceipt{}, err
	}
	defer root.Close()
	raw, err := readRegular(root, "deployments/"+hash+".json", MaxManifestBytes)
	if err != nil {
		return DeploymentReceipt{}, err
	}
	var receipt DeploymentReceipt
	if err := decodeStrict(raw, &receipt); err != nil {
		return receipt, err
	}
	actual, err := receipt.Deployment.Hash()
	if err != nil || receipt.Schema != Schema || receipt.Hash != hash || actual != hash || receipt.PreparedAt.IsZero() {
		return receipt, ErrIntegrity
	}
	return receipt, nil
}

func checkPlatform(m Manifest, e Environment) error {
	if e.OS == "" {
		e.OS = runtime.GOOS
	}
	if e.Arch == "" {
		e.Arch = runtime.GOARCH
	}
	if len(m.Platforms) > 0 && !slices.Contains(m.Platforms, Platform{OS: e.OS, Arch: e.Arch}) {
		return fmt.Errorf("%w: package does not support %s/%s", ErrIncompatible, e.OS, e.Arch)
	}
	return nil
}

func (s *Store) ResolveServers(d Deployment, e Environment) (map[string]ResolvedServer, error) {
	if err := d.Validate(); err != nil {
		return nil, err
	}
	bundle, err := s.Read(d.Digest)
	if err != nil {
		return nil, err
	}
	if bundle.Manifest.ID != d.PackageID {
		return nil, ErrIntegrity
	}
	if err := bundle.Manifest.CheckConfiguration(d.Configuration); err != nil {
		return nil, err
	}
	if err := checkPlatform(bundle.Manifest, e); err != nil {
		return nil, err
	}
	// Validate required credentials even when no current MCP field consumes them.
	for _, key := range sortedKeys(d.Configuration.Secrets) {
		if _, err := s.Secret(d.Configuration.Secrets[key]); err != nil {
			return nil, fmt.Errorf("%w: secret %s is missing on this node", ErrUnavailable, key)
		}
	}
	out := map[string]ResolvedServer{}
	for _, name := range sortedKeys(bundle.Manifest.MCP) {
		server, err := s.resolveServer(bundle, d.Configuration, bundle.Manifest.MCP[name], e)
		if err != nil {
			return nil, fmt.Errorf("MCP %s: %w", name, err)
		}
		out[name] = server
	}
	return out, nil
}

func (s *Store) resolveServer(bundle Bundle, c Configuration, mcp MCPServer, e Environment) (ResolvedServer, error) {
	server := ResolvedServer{Type: mcp.Transport, Env: map[string]string{}, Headers: map[string]string{}}
	resolve := func(value Value) (string, error) { return c.value(bundle.Manifest, value, s.Secret) }
	if mcp.Program == nil {
		url, err := resolve(mcp.URL)
		if err != nil {
			return server, err
		}
		if err := validateEndpoint(url); err != nil {
			return server, err
		}
		server.URL = url
		for _, name := range sortedKeys(mcp.Headers) {
			value, err := resolve(mcp.Headers[name])
			if err != nil {
				return server, err
			}
			if strings.ContainsAny(value, "\r\n\x00") {
				return server, fmt.Errorf("%w: invalid resolved header", ErrInvalid)
			}
			server.Headers[name] = value
		}
		return server, nil
	}
	command := filepath.Join(s.Dir, "packages", bundle.Digest, "content", filepath.FromSlash(mcp.Program.Path))
	if mcp.Program.Runtime != "" {
		lookup := e.LookPath
		if lookup == nil {
			lookup = exec.LookPath
		}
		executable, err := lookup(mcp.Program.Runtime)
		if err != nil {
			return server, fmt.Errorf("%w: interpreter %s is not available", ErrUnavailable, mcp.Program.Runtime)
		}
		server.Command = executable
		server.Args = append(server.Args, command)
	} else {
		server.Command = command
	}
	for _, arg := range mcp.Program.Args {
		value, err := resolve(arg)
		if err != nil {
			return server, err
		}
		if strings.ContainsRune(value, '\x00') {
			return server, ErrInvalid
		}
		server.Args = append(server.Args, value)
	}
	for _, name := range sortedKeys(mcp.Env) {
		value, err := resolve(mcp.Env[name])
		if err != nil {
			return server, err
		}
		if strings.ContainsRune(value, '\x00') {
			return server, ErrInvalid
		}
		server.Env[name] = value
	}
	return server, nil
}
