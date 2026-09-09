package plugins

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func example(t *testing.T, name string) Bundle {
	t.Helper()
	b, err := ReadDirectory(t.Context(), filepath.Join("..", "..", "examples", "plugins", name))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestExamplesShareManifestContract(t *testing.T) {
	github, team := example(t, "github"), example(t, "team-tools")
	for _, b := range []Bundle{github, team} {
		if b.Manifest.Validate() != nil || len(b.Manifest.Skills) != 1 || len(b.Manifest.MCP) != 1 || len(b.Manifest.Agents) != 1 {
			t.Fatalf("incomplete example: %+v", b.Manifest)
		}
		decoded, err := DecodeBundle(b.Data)
		if err != nil || decoded.Digest != b.Digest {
			t.Fatalf("bundle: %v", err)
		}
	}
	a, err := NativeName(github.Manifest.ID, "mcp", "service")
	if err != nil {
		t.Fatal(err)
	}
	b, err := NativeName(team.Manifest.ID, "mcp", "service")
	if err != nil || a == b || len(a) > 64 {
		t.Fatalf("names: %s %s %v", a, b, err)
	}
}

func TestManifestRejectsAmbiguousAndUnsupportedDeclarations(t *testing.T) {
	good, _ := json.Marshal(example(t, "github").Manifest)
	cases := []struct {
		name string
		raw  string
	}{
		{"unknown", strings.Replace(string(good), `"schema":1`, `"hooks":{},"schema":1`, 1)},
		{"case-alias", strings.Replace(string(good), `"schema":1`, `"schema":1,"Schema":1`, 1)},
		{"nested-case-alias", strings.Replace(string(good), `"secret":true`, `"secret":true,"Secret":true`, 1)},
		{"duplicate", strings.Replace(string(good), `"schema":1`, `"schema":2,"schema":1`, 1)},
		{"nested-duplicate", strings.Replace(string(good), `"secret":true`, `"secret":false,"secret":true`, 1)},
		{"extra-object", string(good) + `{}`},
		{"null", `null`},
		{"array", `[]`},
		{"large", strings.Repeat(" ", MaxManifestBytes+1)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := ParseManifest([]byte(tc.raw)); err == nil {
				t.Fatal("accepted invalid manifest")
			}
		})
	}
	mutations := []struct {
		name string
		edit func(*Manifest)
	}{
		{"schema", func(m *Manifest) { m.Schema++ }},
		{"api", func(m *Manifest) { m.API = "steve.plugins.v2" }},
		{"id", func(m *Manifest) { m.ID = "../outside" }},
		{"version", func(m *Manifest) { m.Version = "latest" }},
		{"platform", func(m *Manifest) { m.Platforms = []Platform{{OS: "unknown", Arch: "arm64"}} }},
		{"secret-default", func(m *Manifest) {
			s := m.Settings["token"]
			value := "secret"
			s.Default = &value
			m.Settings["token"] = s
		}},
		{"missing-secret", func(m *Manifest) { delete(m.Settings, "token") }},
		{"wrong-secret-kind", func(m *Manifest) { s := m.Settings["token"]; s.Secret = false; m.Settings["token"] = s }},
		{"path", func(m *Manifest) { m.Skills["review"] = "../review" }},
		{"missing-skill", func(m *Manifest) { p := m.Agents["reviewer"]; p.Skills = []string{"missing"}; m.Agents["reviewer"] = p }},
		{"unknown-transport", func(m *Manifest) { s := m.MCP["github"]; s.Transport = "shell"; m.MCP["github"] = s }},
		{"credential-url", func(m *Manifest) {
			s := m.MCP["github"]
			s.URL.Text = "https://user:secret@example.com"
			m.MCP["github"] = s
		}},
		{"newline-header", func(m *Manifest) {
			s := m.MCP["github"]
			s.Headers["Authorization"] = Value{Text: "value\r\nInjected:yes"}
			m.MCP["github"] = s
		}},
	}
	for _, tc := range mutations {
		t.Run(tc.name, func(t *testing.T) {
			var m Manifest
			if err := json.Unmarshal(good, &m); err != nil {
				t.Fatal(err)
			}
			tc.edit(&m)
			if err := m.Validate(); err == nil {
				t.Fatal("accepted invalid declaration")
			}
		})
	}
}

func TestBundleSnapshotDoesNotFollowLinksOrIgnoreMissingCapabilities(t *testing.T) {
	for _, tc := range []string{"symlink", "traversal", "missing-skill", "missing-program"} {
		t.Run(tc, func(t *testing.T) {
			dir := fixtureDirectory(t)
			switch tc {
			case "symlink":
				if err := os.Symlink(filepath.Join(t.TempDir(), "outside"), filepath.Join(dir, "link")); err != nil {
					t.Fatal(err)
				}
			case "traversal":
				m := example(t, "github").Manifest
				m.Skills["review"] = "../outside"
				writeManifest(t, dir, m)
			case "missing-skill":
				if err := os.Remove(filepath.Join(dir, "skills/review/SKILL.md")); err != nil {
					t.Fatal(err)
				}
			case "missing-program":
				m := example(t, "github").Manifest
				m.MCP["github"] = MCPServer{Transport: "stdio", Program: &Program{Path: "bin/missing"}}
				writeManifest(t, dir, m)
			}
			if _, err := ReadDirectory(t.Context(), dir); !errors.Is(err, ErrInvalid) {
				t.Fatalf("error = %v", err)
			}
		})
	}
}

func fixtureDirectory(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	b := example(t, "github")
	files, err := archiveFiles(b.Data)
	if err != nil {
		t.Fatal(err)
	}
	for name, file := range files {
		p := filepath.Join(dir, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(p), 0700); err != nil {
			t.Fatal(err)
		}
		mode := os.FileMode(0600)
		if file.executable {
			mode = 0700
		}
		if err := os.WriteFile(p, file.data, mode); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func writeManifest(t *testing.T, dir string, m Manifest) {
	t.Helper()
	raw, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ManifestFile), raw, 0600); err != nil {
		t.Fatal(err)
	}
}

func TestManifestVersionsAndStdioConfiguration(t *testing.T) {
	for _, version := range []string{"0.1.0", "1.2.3-alpha.1", "1.0.0-rc-1"} {
		m := example(t, "github").Manifest
		m.Version = version
		m.MCP["github"] = MCPServer{Transport: "stdio", Program: &Program{Runtime: "node", Path: "server.js", Args: []Value{{Config: "endpoint"}}}, Env: map[string]Value{"TOKEN": {Secret: "token"}}}
		m.Settings["endpoint"] = Setting{Description: "API endpoint", Required: true}
		if err := m.Validate(); err != nil {
			t.Fatal(err)
		}
		m.MCP["github"].Env["BAD=NAME"] = Value{Text: "wrong"}
		if err := m.Validate(); err == nil {
			t.Fatal("invalid environment name accepted")
		}
	}
	for _, version := range []string{"01.2.3", "1.2.3-01", "1.2.3-alpha..1", "1.2.3+build", "latest"} {
		m := example(t, "github").Manifest
		m.Version = version
		if err := m.Validate(); err == nil {
			t.Fatalf("accepted version %s", version)
		}
	}
}
