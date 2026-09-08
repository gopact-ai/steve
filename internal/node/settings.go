package node

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"

	"github.com/gopact-ai/steve/internal/mcpscan"
	"github.com/gopact-ai/steve/internal/nodewire"
)

var idShape = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)

// settings is the editable part of this machine's configuration.
func (s *Server) settings() nodewire.Settings {
	cfg := s.conf()
	out := nodewire.Settings{Harnesses: map[string]nodewire.HarnessSetting{}, Tools: append([]string{}, cfg.Tools...),
		MCPServers: map[string]nodewire.MCPSetting{}, Declares: append([]string{}, cfg.Declares...), Capabilities: append([]string{}, cfg.Capabilities...)}
	for id, h := range cfg.Harnesses {
		out.Harnesses[id] = nodewire.HarnessSetting{Adapter: &h.Adapter, Slots: &h.Slots, Command: h.Command, Args: h.Args, Env: h.Env, ProcessDir: h.ProcessDir, Models: h.Models}
	}
	for id, m := range cfg.MCPServers {
		out.MCPServers[id] = nodewire.MCPSetting{Type: m.Type, Command: m.Command, Args: m.Args, Env: m.Env, URL: m.URL, Headers: m.Headers}
	}
	if cfg.MCPBroker != nil && cfg.MCPBroker.Socket != "" {
		out.ExternalBroker = true
	}
	out.Revision = nodewire.SettingsRevision(out)
	return nodewire.CloneSettings(out)
}

// applySettings validates and takes new settings: the file this node was
// started from is rewritten, the running configuration replaced whole,
// the launch probe woken, the broker told. The next advert carries the
// result; the hub asks for one as soon as this returns.
func (s *Server) applySettings(set nodewire.Settings) error {
	s.settingsMu.Lock()
	defer s.settingsMu.Unlock()
	if set.Revision == "" || set.Revision != s.settings().Revision {
		return nodewire.ErrSettingsRevisionConflict
	}
	set = nodewire.CloneSettings(set)
	cfg := s.conf()
	next := cfg
	next.Harnesses = make(map[string]HarnessSpec, len(set.Harnesses))
	for id, h := range set.Harnesses {
		if !idShape.MatchString(id) {
			return fmt.Errorf("harness id %q is not a plain name", id)
		}
		if strings.TrimSpace(h.Command) == "" {
			return fmt.Errorf("harness %q needs a command", id)
		}
		previous := cfg.Harnesses[id]
		if h.Adapter != nil && *h.Adapter != previous.Adapter {
			return fmt.Errorf("harness %s adapter changes require configuration and restart", id)
		}
		if previous.Adapter != "" && (h.Command != previous.Command || !slices.Equal(h.Args, previous.Args)) {
			return fmt.Errorf("harness %s has a pinned adapter; its generated command and arguments cannot be edited", id)
		}
		if h.Permission != nil && *h.Permission != "" {
			return fmt.Errorf("harness %s permissions are managed by the hub", id)
		}
		previous.Command, previous.Args, previous.ProcessDir = h.Command, h.Args, h.ProcessDir
		if h.Env != nil {
			previous.Env = h.Env
		}
		if h.Models != nil {
			previous.Models = h.Models
		}
		if h.Slots != nil {
			if *h.Slots < 0 {
				return fmt.Errorf("harness %s slots must be nonnegative", id)
			}
			previous.Slots = *h.Slots
		}
		next.Harnesses[id] = previous
	}
	next.Tools = cleanList(set.Tools)
	next.Declares = cleanList(set.Declares)
	for _, d := range next.Declares {
		if !strings.Contains(d, ":") {
			return fmt.Errorf("declaration %q must be kind:id, like network:office", d)
		}
	}
	next.Capabilities = cleanList(set.Capabilities)
	if cfg.MCPBroker != nil && cfg.MCPBroker.Socket != "" {
		if len(set.MCPServers) > 0 {
			return errors.New("this node's MCP servers belong to its broker process; edit the broker's config")
		}
	} else {
		next.MCPServers = make(map[string]MCPSpec, len(set.MCPServers))
		for id, m := range set.MCPServers {
			previous := cfg.MCPServers[id]
			if m.Env == nil {
				m.Env = previous.Env
			}
			if m.Headers == nil {
				m.Headers = previous.Headers
			}
			if !idShape.MatchString(id) {
				return fmt.Errorf("MCP server id %q is not a plain name", id)
			}
			switch m.Type {
			case "", "stdio":
				if strings.TrimSpace(m.Command) == "" {
					return fmt.Errorf("MCP server %q needs a command", id)
				}
				m.Type = "stdio"
			case "http", "sse":
				if !strings.HasPrefix(m.URL, "http://") && !strings.HasPrefix(m.URL, "https://") {
					return fmt.Errorf("MCP server %q needs an http(s) url", id)
				}
			default:
				return fmt.Errorf("MCP server %q: unknown type %q", id, m.Type)
			}
			next.MCPServers[id] = MCPSpec{Type: m.Type, Command: m.Command, Args: m.Args, Env: m.Env, URL: m.URL, Headers: m.Headers}
		}
	}
	var writeErr error
	if next.Source != "" {
		current, err := nodeSettingsFileRevision(next.Source)
		if err != nil {
			return fmt.Errorf("read node configuration revision: %w", err)
		}
		if current != s.settingsFileRevision {
			return fmt.Errorf("%w: node configuration was edited externally; restart before saving", nodewire.ErrSettingsRevisionConflict)
		}
		writeErr = writeConfig(next)
		if writeErr != nil && !settingsCommitted(writeErr) {
			return writeErr
		}
		s.settingsFileRevision, _ = nodeSettingsFileRevision(next.Source)
	}
	s.cfg.Store(&next)
	s.launch.Wake()
	switch b := s.broker.(type) {
	case localBroker:
		b.b.SetServers(next.MCPServers)
	case nil:
		if len(next.MCPServers) > 0 {
			if err := s.startBroker(); err != nil {
				slog.Error(fmt.Sprintf("steve-node: %v", err))
			}
		}
	}
	slog.Info(fmt.Sprintf("steve-node: settings applied from the hub: %d harnesses, %d tools, %d mcp, %d declares, %d tags",
		len(next.Harnesses), len(next.Tools), len(next.MCPServers), len(next.Declares), len(next.Capabilities)))
	return writeErr
}

// startBroker starts the MCP broker the settings call for, if any.
func (s *Server) startBroker() error {
	cfg := s.conf()
	switch {
	case cfg.MCPBroker != nil && cfg.MCPBroker.Socket != "":
		if len(cfg.MCPServers) > 0 {
			return errors.New("mcp_servers and mcp_broker are exclusive: the servers belong to the broker")
		}
		s.broker = remoteBroker{socket: cfg.MCPBroker.Socket, token: cfg.MCPBroker.Token}
	case len(cfg.MCPServers) > 0 && s.broker == nil && s.ctx != nil:
		b := NewBroker(BrokerConfig{Socket: s.SocketPath(), MCPServers: cfg.MCPServers, WorkspaceRoot: cfg.WorkspaceRoot,
			PortFile: filepath.Join(cfg.StateDir, "mcp-proxy.port")})
		b.work = s.beginWork
		s.broker = localBroker{b}
		s.backgroundWG.Go(func() {
			if err := b.Serve(s.ctx); err != nil {
				slog.Error(fmt.Sprintf("steve-node: %v", err))
			}
		})
	}
	return nil
}

// writeConfig rewrites the node's file with the configuration in force:
// the whole document, so nothing the file had is lost, through a
// temporary file so a crash mid-write leaves the old one.
func writeConfig(cfg ServerConfig) error {
	declared := cfg
	declared.Harnesses = make(map[string]HarnessSpec, len(cfg.Harnesses))
	for id, h := range cfg.Harnesses {
		if h.Adapter != "" {
			h.Command = ""
		}
		declared.Harnesses[id] = h
	}
	raw, err := json.MarshalIndent(declared, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(cfg.Source), ".node-config-*")
	if err != nil {
		return fmt.Errorf("write node configuration: %w", err)
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(append(raw, '\n')); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmp.Name(), cfg.Source); err != nil {
		return fmt.Errorf("write %s: %w", cfg.Source, err)
	}
	dir, err := os.Open(filepath.Dir(cfg.Source))
	if err != nil {
		return &settingsCommittedError{err: err}
	}
	defer dir.Close()
	if err := dir.Sync(); err != nil {
		return &settingsCommittedError{err: err}
	}
	return nil
}

type settingsCommittedError struct{ err error }

func (e *settingsCommittedError) Error() string {
	return "node settings applied; directory sync failed: " + e.err.Error()
}
func (e *settingsCommittedError) Unwrap() error { return e.err }
func settingsCommitted(err error) bool {
	var committed *settingsCommittedError
	return errors.As(err, &committed)
}

func cleanList(in []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, x := range in {
		x = strings.TrimSpace(x)
		if x == "" || seen[x] {
			continue
		}
		seen[x] = true
		out = append(out, x)
	}
	return out
}

// configure serves StreamConfig: "get" answers the settings, "set" takes
// new ones and answers what is in force afterwards.
func (s *Server) configure(stream *nodewire.Stream) {
	defer stream.Close()
	verb := strings.TrimSpace(stream.Request().Command)
	reply := func(err error) {
		out := nodewire.ConfigReply{Settings: s.settings()}
		if err != nil {
			out.Error = err.Error()
			if errors.Is(err, nodewire.ErrSettingsRevisionConflict) {
				out.ErrorCode = nodewire.SettingsRevisionConflictCode
			} else if settingsCommitted(err) {
				out.ErrorCode = "settings_committed"
			}
		}
		if err := json.NewEncoder(stream).Encode(out); err != nil {
			slog.Error(fmt.Sprintf("steve-node: config reply: %v", err))
		}
	}
	switch verb {
	case "discover-agents", "enroll-agent":
		s.configureAgentTools(stream)
	case "get":
		reply(nil)
	case "set":
		var set nodewire.Settings
		if err := json.NewDecoder(stream).Decode(&set); err != nil {
			reply(fmt.Errorf("read settings: %w", err))
			return
		}
		reply(s.applySettings(set))
	default:
		// "adopt <source> <name>": copy one of the coding agents' own
		// MCP servers into this machine's settings. The values come from
		// the file on this machine and go into node.json here; nothing
		// but the shape has ever left.
		fields := strings.Fields(verb)
		if len(fields) == 3 && fields[0] == "adopt" {
			reply(s.adoptMCP(fields[1], fields[2]))
			return
		}
		reply(fmt.Errorf("unknown config verb %q", verb))
	}
}

func nodeSettingsFileRevision(path string) (string, error) {
	if path == "" {
		return "", nil
	}
	raw, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return "missing", nil
	}
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:]), nil
}

// reservedMCP are names the platform gives its own session servers.
func reservedMCP(name string) bool {
	return name == "feishu" || strings.HasPrefix(name, "steve")
}

func (s *Server) adoptMCP(source, name string) error {
	if reservedMCP(name) {
		return fmt.Errorf("%q is a name the platform uses; adopt it under another name", name)
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	full, ok := mcpscan.Lookup(home, source, name)
	if !ok {
		return fmt.Errorf("no %s server %q in this user's configuration", source, name)
	}
	set := s.settings()
	if _, exists := set.MCPServers[name]; exists {
		return fmt.Errorf("this machine already has an MCP server named %q; remove it first or adopt under another name", name)
	}
	if set.MCPServers == nil {
		set.MCPServers = map[string]nodewire.MCPSetting{}
	}
	set.MCPServers[name] = nodewire.MCPSetting{Type: full.Type, Command: full.Command, Args: full.Args, Env: full.Env, URL: full.URL, Headers: full.Headers}
	if err := s.applySettings(set); err != nil {
		return err
	}
	ownMCP.Get(0)
	return nil
}
