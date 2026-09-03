package node

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/gopact-ai/steve/internal/nodewire"
)

var idShape = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)

// settings is the editable part of this machine's configuration.
func (s *Server) settings() nodewire.Settings {
	cfg := s.conf()
	out := nodewire.Settings{Harnesses: map[string]nodewire.HarnessSetting{}, Tools: append([]string{}, cfg.Tools...),
		MCPServers: map[string]nodewire.MCPSetting{}, Declares: append([]string{}, cfg.Declares...), Capabilities: append([]string{}, cfg.Capabilities...)}
	for id, h := range cfg.Harnesses {
		out.Harnesses[id] = nodewire.HarnessSetting{Command: h.Command, Args: h.Args, Env: h.Env, ProcessDir: h.ProcessDir, Models: h.Models}
	}
	for id, m := range cfg.MCPServers {
		out.MCPServers[id] = nodewire.MCPSetting{Type: m.Type, Command: m.Command, Args: m.Args, Env: m.Env, URL: m.URL, Headers: m.Headers}
	}
	if cfg.MCPBroker != nil && cfg.MCPBroker.Socket != "" {
		out.ExternalBroker = true
	}
	return out
}

// applySettings validates and takes new settings: the file this node was
// started from is rewritten, the running configuration replaced whole,
// the launch probe woken, the broker told. The next advert carries the
// result; the hub asks for one as soon as this returns.
func (s *Server) applySettings(set nodewire.Settings) error {
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
		next.Harnesses[id] = HarnessSpec{Command: h.Command, Args: h.Args, Env: h.Env, ProcessDir: h.ProcessDir, Models: h.Models}
	}
	if len(next.Harnesses) == 0 {
		return errors.New("a node needs at least one AI tool")
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
	if next.Source != "" {
		if err := writeConfig(next); err != nil {
			return err
		}
	}
	s.cfg.Store(&next)
	s.launch.Wake()
	switch b := s.broker.(type) {
	case localBroker:
		b.b.SetServers(next.MCPServers)
	case nil:
		if len(next.MCPServers) > 0 {
			if err := s.startBroker(); err != nil {
				log.Printf("steve-node: %v", err)
			}
		}
	}
	log.Printf("steve-node: settings applied from the hub: %d harnesses, %d tools, %d mcp, %d declares, %d tags",
		len(next.Harnesses), len(next.Tools), len(next.MCPServers), len(next.Declares), len(next.Capabilities))
	return nil
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
		s.broker = localBroker{b}
		go func() {
			if err := b.Serve(s.ctx); err != nil {
				log.Printf("steve-node: %v", err)
			}
		}()
	}
	return nil
}

// writeConfig rewrites the node's file with the configuration in force:
// the whole document, so nothing the file had is lost, through a
// temporary file so a crash mid-write leaves the old one.
func writeConfig(cfg ServerConfig) error {
	raw, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	tmp := cfg.Source + ".tmp"
	if err := os.WriteFile(tmp, append(raw, '\n'), 0o600); err != nil {
		return fmt.Errorf("write %s: %w", cfg.Source, err)
	}
	if err := os.Rename(tmp, cfg.Source); err != nil {
		os.Remove(tmp)
		return fmt.Errorf("write %s: %w", cfg.Source, err)
	}
	return nil
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
		}
		_ = json.NewEncoder(stream).Encode(out)
	}
	switch verb {
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
		reply(fmt.Errorf("unknown config verb %q", verb))
	}
}
