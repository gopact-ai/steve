package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gopact-ai/steve/internal/config"
	"github.com/gopact-ai/steve/internal/consoleapi"
	"github.com/gopact-ai/steve/internal/sshconnect"
)

func sshCheckFixture() sshconnect.CheckResult {
	check := sshconnect.CheckResult{OS: "linux", Arch: "amd64", Reachable: true}
	for _, name := range []string{"curl", "sha256sum", "bash", "nohup", "node", "npm"} {
		check.Tools = append(check.Tools, sshconnect.Tool{Name: name, Available: true})
	}
	return check
}

func installBinaryFixture(t *testing.T) string {
	t.Helper()
	raw := make([]byte, 64)
	copy(raw, []byte{0x7f, 'E', 'L', 'F', 2, 1, 1})
	raw[16], raw[18], raw[20], raw[52] = 2, 62, 1, 64
	path := filepath.Join(t.TempDir(), "node")
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestSSHPreviewMatchesCommittedBootstrapWithoutCopyingLocalAgents(t *testing.T) {
	admin := nodeAdminFixture(t)
	admin.cfg.Gateway.NodeBinary = installBinaryFixture(t)
	admin.cfg.Harnesses = map[string]config.Harness{"codex": {Adapter: "codex-acp", Command: "/coordinator-only/adapter"}}
	if err := config.Save(admin.path, admin.cfg); err != nil {
		t.Fatal(err)
	}
	before, _ := os.ReadFile(admin.path)
	backend := sshNodeBackend{admin: admin}
	req := sshconnect.InstallRequest{Alias: "example", Name: "remote", Addr: "127.0.0.1:1", HubURL: "https://coordinator.example"}
	plan, err := backend.Preview(t.Context(), req, sshCheckFixture())
	if err != nil || plan.Script == "" {
		t.Fatalf("preview = %#v, %v", plan, err)
	}
	for _, step := range plan.Steps {
		if step.Status == "blocked" {
			t.Fatalf("unexpected preview blocker: %#v", step)
		}
	}
	if strings.Contains(plan.Script, "/coordinator-only/adapter") || strings.Contains(plan.Script, `"adapter": "codex-acp"`) || !strings.Contains(plan.Script, `"harnesses": {}`) {
		t.Fatal("SSH installation should enroll an empty node without local Agent configuration")
	}
	after, _ := os.ReadFile(admin.path)
	if string(before) != string(after) {
		t.Fatal("preview changed persistent configuration")
	}
	id := strings.Repeat("a", 48)
	registration, err := backend.Register(t.Context(), req, sshCheckFixture(), id)
	if err != nil {
		t.Fatal(err)
	}
	normalized := strings.ReplaceAll(strings.ReplaceAll(registration.Script, registration.Token, sshconnect.PreviewToken), id, "pending-node-upload")
	if registration.Name != "remote" || registration.Token == "" || normalized != plan.Script {
		t.Fatal("committed script differs from reviewed script")
	}
}

func TestSSHPreviewExplainsPackageAndReachabilityRequirements(t *testing.T) {
	admin := nodeAdminFixture(t)
	backend := sshNodeBackend{admin: admin}
	req := sshconnect.InstallRequest{Name: "remote", Addr: "127.0.0.1:1", HubURL: "http://localhost:7700"}
	plan, err := backend.Preview(t.Context(), req, sshCheckFixture())
	if err != nil || !hasSSHBlocker(plan, "binary") {
		t.Fatalf("missing package blocker: %#v, %v", plan, err)
	}
	admin.cfg.Gateway.NodeBinary = installBinaryFixture(t)
	check := sshCheckFixture()
	check.Arch = "arm64"
	plan, err = backend.Preview(t.Context(), req, check)
	if err != nil || !hasSSHBlocker(plan, "binary") || hasSSHBlocker(plan, "download_address") {
		t.Fatalf("missing platform blocker or unwanted HTTP dependency: %#v, %v", plan, err)
	}
}

func TestSSHEmptyNodeNeedsNoAgentOrNPMLocallyOrRemotely(t *testing.T) {
	admin := nodeAdminFixture(t)
	admin.cfg.Harnesses = map[string]config.Harness{}
	admin.cfg.Gateway.NodeBinary = installBinaryFixture(t)
	check := sshCheckFixture()
	for index := range check.Tools {
		if check.Tools[index].Name == "node" || check.Tools[index].Name == "npm" {
			check.Tools[index].Available = false
		}
	}
	plan, err := (sshNodeBackend{admin: admin}).Preview(t.Context(), sshconnect.InstallRequest{Name: "empty-node", Addr: "127.0.0.1:1"}, check)
	if err != nil {
		t.Fatal(err)
	}
	for _, step := range plan.Steps {
		if step.Status == "blocked" {
			t.Fatalf("empty node blocked by %s: %s", step.ID, step.Message)
		}
	}
	if !strings.Contains(plan.Script, `"harnesses": {}`) {
		t.Fatal("missing explicit empty node harness config")
	}
}

func hasSSHBlocker(plan sshconnect.Template, id string) bool {
	for _, step := range plan.Steps {
		if step.ID == id && step.Status == "blocked" {
			return true
		}
	}
	return false
}

func TestBootstrapDoesNotReturnScriptWithInvalidBinaryOrToken(t *testing.T) {
	admin := nodeAdminFixture(t)
	registration, err := admin.AddNode(context.Background(), consoleapi.AddNodeRequest{Name: "remote", Addr: "127.0.0.1:1", HubURL: "https://coordinator.example"})
	if err != nil {
		t.Fatal(err)
	}
	admin.cfg.Gateway.NodeBinary = filepath.Join(t.TempDir(), "does-not-exist")
	if _, ok := admin.Bootstrap("remote", registration.Token); ok {
		t.Fatal("returned download script with unverified package")
	}
	if _, ok := admin.Bootstrap("remote", "incorrect-token"); ok {
		t.Fatal("bootstrap accepted another token")
	}
}

func TestManualBootstrapKeepsExplicitAdapterPortable(t *testing.T) {
	admin := nodeAdminFixture(t)
	admin.cfg.Harnesses = map[string]config.Harness{"codex": {Adapter: "codex-acp", Command: "/coordinator-only/adapter"}}
	registration, err := admin.AddNode(t.Context(), consoleapi.AddNodeRequest{Name: "remote", Addr: "127.0.0.1:1"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(registration.Note, "~/steve-bin/steve-node") || !strings.Contains(registration.Note, "先移除此未接入的机器登记") || !strings.Contains(registration.Note, "“通过 SSH 接入”重新添加") || strings.Contains(registration.Note, "hub") {
		t.Fatalf("manual enrollment lacks actionable installation guidance: %q", registration.Note)
	}
	script, ok := admin.Bootstrap("remote", registration.Token)
	if !ok || !strings.Contains(script, `"adapter": "codex-acp"`) || strings.Contains(script, "/coordinator-only/adapter") {
		t.Fatal("manual bootstrap copied a coordinator-only adapter path")
	}
}
