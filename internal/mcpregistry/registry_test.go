package mcpregistry

import (
	"strings"
	"testing"
)

const listing = `{"servers":[{"server":{"name":"com.pulsemcp/remote-filesystem","description":"MCP server for remote filesystem operations","repository":{"url":"https://github.com/pulsemcp/mcp-servers"},"version":"0.1.2","packages":[{"registryType":"npm","identifier":"remote-filesystem-mcp-server","version":"0.1.2","runtimeHint":"npx","transport":{"type":"stdio"},"runtimeArguments":[{"value":"-y","type":"positional"}],"packageArguments":[{"type":"named","name":"--root","value":"/data"}],"environmentVariables":[{"description":"bucket","isRequired":true,"name":"GCS_BUCKET"},{"description":"key","isSecret":true,"name":"GCS_PRIVATE_KEY"}]},{"registryType":"oci","identifier":"ghcr.io/x/fs:1","transport":{"type":"stdio"},"environmentVariables":[{"name":"TOKEN","isSecret":true}]}],"remotes":[{"type":"streamable-http","url":"https://mcp.example.com/mcp","headers":[{"name":"Authorization","isSecret":true,"isRequired":true}]}]}}],"metadata":{"count":1}}`

func TestParseAndPlan(t *testing.T) {
	entries, err := Parse(strings.NewReader(listing))
	if err != nil || len(entries) != 1 {
		t.Fatalf("parse: %v %+v", err, entries)
	}
	e := entries[0]
	if e.Name != "com.pulsemcp/remote-filesystem" || len(e.Packages) != 2 || len(e.Remotes) != 1 {
		t.Fatalf("entry = %+v", e)
	}
	npm := e.Packages[0]
	if npm.Needs != "npx" || len(npm.Env) != 2 || !npm.Env[0].Required || !npm.Env[1].Secret {
		t.Fatalf("npm = %+v", npm)
	}
	s, err := Plan(npm)
	if err != nil || s.Command != "npx" || strings.Join(s.Args, " ") != "-y remote-filesystem-mcp-server@0.1.2 --root /data" {
		t.Fatalf("plan npm = %+v err=%v", s, err)
	}
	oci := e.Packages[1]
	if oci.Needs != "docker" {
		t.Fatalf("oci needs %q", oci.Needs)
	}
	s, err = Plan(oci)
	if err != nil || s.Command != "docker" || strings.Join(s.Args, " ") != "run -i --rm -e TOKEN ghcr.io/x/fs:1" {
		t.Fatalf("plan oci = %+v err=%v", s, err)
	}
	r, err := PlanRemote(e.Remotes[0])
	if err != nil || r.Type != "http" || r.URL != "https://mcp.example.com/mcp" {
		t.Fatalf("plan remote = %+v err=%v", r, err)
	}
	if _, err := Plan(Package{RegistryType: "mcpb", Identifier: "x"}); err == nil {
		t.Fatal("unknown runtime planned")
	}
}
