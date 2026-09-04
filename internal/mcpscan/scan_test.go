package mcpscan

import (
	"os"
	"path/filepath"
	"testing"
)

func TestScanCodexAndClaude(t *testing.T) {
	home := t.TempDir()
	_ = os.MkdirAll(filepath.Join(home, ".codex"), 0o755)
	_ = os.WriteFile(filepath.Join(home, ".codex", "config.toml"), []byte(`
model = "gpt-5"

[mcp_servers.docs]
url = "https://developers.example.com/mcp"
bearer_token_env_var = "DOCS_TOKEN"

[mcp_servers.botmux]
command = "/opt/botmux/bin/botmux"
args = [ "mcp", "serve" ]
env_vars = [
  "BOTMUX_SESSION_ID",
  "SESSION_DATA_DIR",
]
env = { LOG_LEVEL = "info", TOKEN = "s3cret" }

[mcp_servers."dotted.name"]
command = "x"

[mcp_servers.tabled]
command = "y"
[mcp_servers.tabled.env]
API_KEY = "k"
`), 0o644)
	_ = os.WriteFile(filepath.Join(home, ".claude.json"), []byte(`{
  "mcpServers": {"github": {"command": "npx", "args": ["-y", "@modelcontextprotocol/server-github"], "env": {"GITHUB_TOKEN": "t"}},
                 "remote": {"type": "http", "url": "https://mcp.example.com", "headers": {"Authorization": "Bearer x"}}},
  "projects": {"/home/me/proj": {"mcpServers": {"local": {"command": "./mcp.sh"}}}}
}`), 0o644)
	found := ScanLocal(home)
	byName := map[string]Found{}
	for _, f := range found {
		byName[f.Source+"/"+f.Name] = f
	}
	if len(found) != 7 {
		t.Fatalf("found %d: %+v", len(found), found)
	}
	if b := byName["codex/botmux"]; b.Type != "stdio" || b.Command != "/opt/botmux/bin/botmux" || len(b.Args) != 2 || len(b.EnvKeys) != 4 {
		t.Fatalf("botmux = %+v", b)
	}
	if d := byName["codex/docs"]; d.Type != "http" || d.URL == "" || len(d.HeaderKeys) != 1 {
		t.Fatalf("docs = %+v", d)
	}
	if _, ok := byName["codex/dotted.name"]; !ok {
		t.Fatalf("quoted name lost: %+v", found)
	}
	if tb := byName["codex/tabled"]; len(tb.EnvKeys) != 1 || tb.EnvKeys[0] != "API_KEY" {
		t.Fatalf("sub-table env = %+v", tb)
	}
	if g := byName["claude-code/github"]; g.Type != "stdio" || g.Command != "npx" || len(g.EnvKeys) != 1 {
		t.Fatalf("github = %+v", g)
	}
	if r := byName["claude-code/remote"]; r.Type != "http" || len(r.HeaderKeys) != 1 {
		t.Fatalf("remote = %+v", r)
	}
	if l := byName["claude-code/local"]; l.Scope != "/home/me/proj" {
		t.Fatalf("project scope = %+v", l)
	}
	// Values never ride along in Found; Lookup has them for the machine.
	for _, f := range found {
		for _, k := range f.EnvKeys {
			if k == "s3cret" || k == "t" {
				t.Fatal("a value leaked as a key")
			}
		}
	}
	full, ok := Lookup(home, "codex", "botmux")
	if !ok || full.Env["TOKEN"] != "s3cret" || full.Env["LOG_LEVEL"] != "info" {
		t.Fatalf("lookup = %+v %v", full, ok)
	}
	if _, ok := Lookup(home, "codex", "nope"); ok {
		t.Fatal("found a server that is not there")
	}
}
