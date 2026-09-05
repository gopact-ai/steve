// Package mcpscan finds the MCP servers a machine's coding agents were
// given outside Steve — in Codex's config.toml and Claude Code's
// ~/.claude.json — so the owner can see them from the hub and adopt
// one. What leaves the machine is the shape: transport, command or URL,
// and the names of environment variables and headers. Values stay in
// the file they came from until the machine itself copies them into its
// own settings.
package mcpscan

import (
	"encoding/json"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
)

// Found is one server as a coding agent has it configured.
type Found struct {
	Name   string `json:"name"`
	Source string `json:"source"`          // codex | claude-code
	Scope  string `json:"scope,omitempty"` // "" for the user's own config; a project directory for a project's
	Type   string `json:"type"`            // stdio | http | sse
	Command string   `json:"command,omitempty"`
	Args    []string `json:"args,omitempty"`
	URL     string   `json:"url,omitempty"`
	// EnvKeys and HeaderKeys are the names; the values are not carried.
	EnvKeys    []string `json:"env_keys,omitempty"`
	HeaderKeys []string `json:"header_keys,omitempty"`
}

// Full is a server with its values, for the machine's own use when it
// adopts one into its settings.
type Full struct {
	Found
	Env     map[string]string
	Headers map[string]string
}

// ScanLocal lists the servers under home, shapes only.
func ScanLocal(home string) []Found {
	full := scanFull(home)
	out := make([]Found, 0, len(full))
	for _, f := range full {
		out = append(out, f.Found)
	}
	return out
}

// Lookup finds one server with its values: what the machine copies into
// its own settings when the owner adopts it.
func Lookup(home, source, name string) (Full, bool) {
	for _, f := range scanFull(home) {
		if f.Source == source && f.Name == name && f.Scope == "" {
			return f, true
		}
	}
	for _, f := range scanFull(home) {
		if f.Source == source && f.Name == name {
			return f, true
		}
	}
	return Full{}, false
}

// CodexConfig and ClaudeConfig are where the two tools keep their
// configuration for the user this process runs as: CODEX_HOME and
// CLAUDE_CONFIG_DIR move them, else they sit under home. A harness run
// by another user or with another environment keeps its own; the scan
// says which user it looked as.
func CodexConfig(home string) string {
	if dir := os.Getenv("CODEX_HOME"); dir != "" {
		return filepath.Join(dir, "config.toml")
	}
	return filepath.Join(home, ".codex", "config.toml")
}

func ClaudeConfig(home string) string {
	if dir := os.Getenv("CLAUDE_CONFIG_DIR"); dir != "" {
		return filepath.Join(dir, ".claude.json")
	}
	return filepath.Join(home, ".claude.json")
}

func scanFull(home string) []Full {
	if home == "" {
		return nil
	}
	var out []Full
	out = append(out, scanCodex(CodexConfig(home))...)
	out = append(out, scanClaude(ClaudeConfig(home))...)
	for i := range out {
		out[i].Found = masked(out[i].Found)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Source != out[j].Source {
			return out[i].Source < out[j].Source
		}
		if out[i].Scope != out[j].Scope {
			return out[i].Scope < out[j].Scope
		}
		return out[i].Name < out[j].Name
	})
	return out
}

// ---------------------------------------------------------------- claude code

// scanClaude reads ~/.claude.json: mcpServers at the top for the user,
// and each project's own under projects.<dir>.mcpServers.
func scanClaude(path string) []Full {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var doc struct {
		MCPServers map[string]claudeServer `json:"mcpServers"`
		Projects   map[string]struct {
			MCPServers map[string]claudeServer `json:"mcpServers"`
		} `json:"projects"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		return nil
	}
	var out []Full
	for name, s := range doc.MCPServers {
		out = append(out, s.full(name, ""))
	}
	for dir, p := range doc.Projects {
		for name, s := range p.MCPServers {
			out = append(out, s.full(name, dir))
		}
	}
	return out
}

type claudeServer struct {
	Type    string            `json:"type"`
	Command string            `json:"command"`
	Args    []string          `json:"args"`
	Env     map[string]string `json:"env"`
	URL     string            `json:"url"`
	Headers map[string]string `json:"headers"`
}

func (s claudeServer) full(name, scope string) Full {
	typ := s.Type
	if typ == "" {
		if s.URL != "" {
			typ = "http"
		} else {
			typ = "stdio"
		}
	}
	return Full{Found: Found{Name: name, Source: "claude-code", Scope: scope, Type: typ, Command: s.Command, Args: s.Args, URL: s.URL, EnvKeys: keys(s.Env), HeaderKeys: keys(s.Headers)}, Env: s.Env, Headers: s.Headers}
}

// ---------------------------------------------------------------- codex

// scanCodex reads the [mcp_servers.<name>] tables of config.toml. Codex
// writes plain TOML: strings, string arrays, and env as an inline table
// or a [mcp_servers.<name>.env] sub-table; env_vars names variables to
// pass through from the environment. That is the subset read here.
func scanCodex(path string) []Full {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	servers := map[string]*Full{}
	order := []string{}
	get := func(name string) *Full {
		if f, ok := servers[name]; ok {
			return f
		}
		f := &Full{Found: Found{Name: name, Source: "codex"}, Env: map[string]string{}, Headers: map[string]string{}}
		servers[name] = f
		order = append(order, name)
		return f
	}
	var cur *Full
	sub := "" // "env" or "http_headers" when inside a sub-table
	for _, line := range joinContinuations(strings.Split(string(raw), "\n")) {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if strings.HasPrefix(line, "[") {
			cur, sub = nil, ""
			header := strings.Trim(line, "[]")
			header = strings.TrimSpace(header)
			if !strings.HasPrefix(header, "mcp_servers.") {
				continue
			}
			rest := strings.TrimPrefix(header, "mcp_servers.")
			name, tail := splitKey(rest)
			cur = get(name)
			if tail == "env" || tail == "http_headers" {
				sub = tail
			}
			continue
		}
		if cur == nil {
			continue
		}
		key, value, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		key, value = strings.TrimSpace(key), strings.TrimSpace(value)
		if sub != "" {
			if v, ok := tomlString(value); ok {
				if sub == "env" {
					cur.Env[unquoteKey(key)] = v
				} else {
					cur.Headers[unquoteKey(key)] = v
				}
			}
			continue
		}
		switch key {
		case "command":
			cur.Command, _ = tomlString(value)
		case "url":
			cur.URL, _ = tomlString(value)
		case "args":
			cur.Args = tomlStrings(value)
		case "env":
			for k, v := range tomlInlineTable(value) {
				cur.Env[k] = v
			}
		case "http_headers":
			for k, v := range tomlInlineTable(value) {
				cur.Headers[k] = v
			}
		case "env_vars":
			// Names passed through from the machine's environment: the
			// values are the machine's, so they are listed as keys only.
			for _, k := range tomlStrings(value) {
				if _, has := cur.Env[k]; !has {
					cur.Env[k] = ""
				}
			}
		case "bearer_token_env_var":
			if v, ok := tomlString(value); ok && v != "" {
				cur.Headers["Authorization"] = "$" + v
			}
		}
	}
	out := make([]Full, 0, len(order))
	for _, name := range order {
		f := servers[name]
		if f.URL != "" {
			f.Type = "http"
		} else if f.Command != "" {
			f.Type = "stdio"
		} else {
			continue
		}
		f.EnvKeys, f.HeaderKeys = keys(f.Env), keys(f.Headers)
		out = append(out, *f)
	}
	return out
}

// joinContinuations folds an array or inline table that spans lines
// into one line, so the value parsers see it whole. Brackets inside
// strings do not count.
func joinContinuations(lines []string) []string {
	var out []string
	var cur strings.Builder
	depth := 0
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if depth == 0 && (trimmed == "" || strings.HasPrefix(trimmed, "#")) {
			continue
		}
		if depth > 0 {
			cur.WriteByte(' ')
		}
		cur.WriteString(trimmed)
		depth += bracketDelta(trimmed)
		if depth <= 0 {
			out = append(out, cur.String())
			cur.Reset()
			depth = 0
		}
	}
	if cur.Len() > 0 {
		out = append(out, cur.String())
	}
	return out
}

func bracketDelta(s string) int {
	depth := 0
	quote := byte(0)
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case quote != 0:
			if c == '\\' {
				i++
			} else if c == quote {
				quote = 0
			}
		case c == '"' || c == '\'':
			quote = c
		case c == '#':
			return depth
		case c == '[' || c == '{':
			depth++
		case c == ']' || c == '}':
			depth--
		}
	}
	return depth
}

// splitKey separates "name.env" into ("name", "env"), honouring a quoted
// name that itself holds dots.
func splitKey(s string) (string, string) {
	if strings.HasPrefix(s, `"`) {
		if end := strings.Index(s[1:], `"`); end >= 0 {
			name := s[1 : end+1]
			tail := strings.TrimPrefix(s[end+2:], ".")
			return name, tail
		}
	}
	name, tail, _ := strings.Cut(s, ".")
	return name, tail
}

func unquoteKey(k string) string { return strings.Trim(strings.TrimSpace(k), `"'`) }

func tomlString(v string) (string, bool) {
	v = strings.TrimSpace(v)
	if i := strings.Index(v, " #"); i > 0 && !strings.HasPrefix(v, `"`) && !strings.HasPrefix(v, `'`) {
		v = strings.TrimSpace(v[:i])
	}
	if len(v) >= 2 && (v[0] == '"' && v[len(v)-1] == '"') {
		var s string
		if err := json.Unmarshal([]byte(v), &s); err == nil {
			return s, true
		}
		return v[1 : len(v)-1], true
	}
	if len(v) >= 2 && v[0] == '\'' && v[len(v)-1] == '\'' {
		return v[1 : len(v)-1], true
	}
	return "", false
}

// tomlStrings reads ["a", "b"], possibly spanning what was one line.
func tomlStrings(v string) []string {
	v = strings.TrimSpace(v)
	v = strings.TrimPrefix(v, "[")
	v = strings.TrimSuffix(v, "]")
	var out []string
	for _, part := range splitTop(v) {
		if s, ok := tomlString(part); ok {
			out = append(out, s)
		}
	}
	return out
}

// tomlInlineTable reads { K = "v", K2 = "w" }.
func tomlInlineTable(v string) map[string]string {
	v = strings.TrimSpace(v)
	v = strings.TrimPrefix(v, "{")
	v = strings.TrimSuffix(v, "}")
	out := map[string]string{}
	for _, part := range splitTop(v) {
		k, val, ok := strings.Cut(part, "=")
		if !ok {
			continue
		}
		if s, ok := tomlString(val); ok {
			out[unquoteKey(k)] = s
		}
	}
	return out
}

// splitTop splits on commas outside quotes.
func splitTop(v string) []string {
	var out []string
	var cur strings.Builder
	quote := byte(0)
	for i := 0; i < len(v); i++ {
		c := v[i]
		switch {
		case quote != 0:
			cur.WriteByte(c)
			if c == '\\' && i+1 < len(v) {
				i++
				cur.WriteByte(v[i])
			} else if c == quote {
				quote = 0
			}
		case c == '"' || c == '\'':
			quote = c
			cur.WriteByte(c)
		case c == ',':
			out = append(out, strings.TrimSpace(cur.String()))
			cur.Reset()
		default:
			cur.WriteByte(c)
		}
	}
	if s := strings.TrimSpace(cur.String()); s != "" {
		out = append(out, s)
	}
	return out
}

var secretArg = regexp.MustCompile(`(?i)^(--?[\w-]*(token|secret|password|passwd|key|auth|credential)[\w-]*)=(.*)$`)

// masked is the shape that may leave the machine: a URL without its
// userinfo or query, and arguments that look like they carry a secret
// with the value blanked.
func masked(f Found) Found {
	if u, err := url.Parse(f.URL); err == nil && f.URL != "" {
		u.User = nil
		u.RawQuery = ""
		u.Fragment = ""
		f.URL = u.String()
	}
	if len(f.Args) > 0 {
		args := make([]string, len(f.Args))
		for i, a := range f.Args {
			if m := secretArg.FindStringSubmatch(a); m != nil {
				args[i] = m[1] + "=…"
			} else {
				args[i] = a
			}
		}
		f.Args = args
	}
	return f
}

// Local is a cached scan of this machine's own MCP servers: what the
// advert carries, redone when older than the time given.
type Local struct {
	mu   sync.Mutex
	at   time.Time
	list []Found
}

// Get is the last scan, redone when older than maxAge.
func (l *Local) Get(maxAge time.Duration) []Found {
	l.mu.Lock()
	defer l.mu.Unlock()
	if time.Since(l.at) > maxAge {
		home, _ := os.UserHomeDir()
		l.list, l.at = ScanLocal(home), time.Now()
	}
	return append([]Found{}, l.list...)
}

func keys(m map[string]string) []string {
	if len(m) == 0 {
		return nil
	}
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
