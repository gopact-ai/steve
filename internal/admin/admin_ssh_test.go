package admin

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"maps"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gopact-ai/steve/internal/config"
	"github.com/gopact-ai/steve/internal/consoleapi"
	"github.com/gopact-ai/steve/internal/httpapi"
	"github.com/gopact-ai/steve/internal/i18n"
	"github.com/gopact-ai/steve/internal/readmodel"
	"github.com/gopact-ai/steve/internal/sshconnect"
)

func SshCheckFixture() sshconnect.CheckResult {
	check := sshconnect.CheckResult{OS: "linux", Arch: "amd64", Reachable: true}
	for _, name := range []string{"curl", "sha256sum", "bash", "nohup", "node", "npm"} {
		check.Tools = append(check.Tools, sshconnect.Tool{Name: name, Available: true})
	}
	return check
}

func InstallBinaryFixture(t *testing.T) string {
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
	admin.cfg().Gateway.NodeBinary = InstallBinaryFixture(t)
	admin.cfg().Harnesses = map[string]config.Harness{"codex": {Adapter: "codex-acp", Command: "/coordinator-only/adapter"}}
	if err := config.Save(admin.Path, admin.cfg()); err != nil {
		t.Fatal(err)
	}
	before, _ := os.ReadFile(admin.Path)
	backend := sshNodeBackend{admin: admin}
	req := sshconnect.InstallRequest{Alias: "example", Name: "remote", Addr: "127.0.0.1:1", HubURL: "https://coordinator.example"}
	plan, err := backend.Preview(t.Context(), req, SshCheckFixture())
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
	after, _ := os.ReadFile(admin.Path)
	if string(before) != string(after) {
		t.Fatal("preview changed persistent configuration")
	}
	id := strings.Repeat("a", 48)
	registration, err := backend.Register(t.Context(), req, SshCheckFixture(), id)
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
	plan, err := backend.Preview(t.Context(), req, SshCheckFixture())
	if err != nil || !hasSSHBlocker(plan, "binary") {
		t.Fatalf("missing package blocker: %#v, %v", plan, err)
	}
	admin.cfg().Gateway.NodeBinary = InstallBinaryFixture(t)
	check := SshCheckFixture()
	check.Arch = "arm64"
	plan, err = backend.Preview(t.Context(), req, check)
	if err != nil || !hasSSHBlocker(plan, "binary") || hasSSHBlocker(plan, "download_address") {
		t.Fatalf("missing platform blocker or unwanted HTTP dependency: %#v, %v", plan, err)
	}
}

func TestSSHEmptyNodeNeedsNoAgentOrNPMLocallyOrRemotely(t *testing.T) {
	admin := nodeAdminFixture(t)
	admin.cfg().Harnesses = map[string]config.Harness{}
	admin.cfg().Gateway.NodeBinary = InstallBinaryFixture(t)
	check := SshCheckFixture()
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
	admin.cfg().Gateway.NodeBinary = filepath.Join(t.TempDir(), "does-not-exist")
	if _, ok := admin.Bootstrap("remote", registration.Token); ok {
		t.Fatal("returned download script with unverified package")
	}
	if _, ok := admin.Bootstrap("remote", "incorrect-token"); ok {
		t.Fatal("bootstrap accepted another token")
	}
}

func TestManualBootstrapKeepsExplicitAdapterPortable(t *testing.T) {
	admin := nodeAdminFixture(t)
	admin.cfg().Harnesses = map[string]config.Harness{"codex": {Adapter: "codex-acp", Command: "/coordinator-only/adapter"}}
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

// A server coordination node upgrades no machine. Its console refuses an
// upgrade and the status of one alike, whichever node ID is asked for: a
// machine it enrolled, itself or an ID no machine has. The refusal says
// that upgrades are unsupported here, not that the node is unknown or has
// no upgrade record, in the API's usual error body and in the language
// the request names.
func TestServerCoordinatorRefusesAnUpgradeAndItsStatusAsUnsupported(t *testing.T) {
	refusesAsUnsupported(t, "/console/ssh/upgrades/", func(text i18n.Catalog) *sshconnect.StepError {
		return sshconnect.Fail(text, "preflight", "upgrade_unsupported", text.T(i18n.SSHUpgradeUnsupported), text.T(i18n.SSHUpgradeUnsupportedFix))
	})
}

// A server coordination node restarts no machine either, and refuses a
// restart and its status the way it refuses an upgrade.
func TestServerCoordinatorRefusesARestartAndItsStatusAsUnsupported(t *testing.T) {
	refusesAsUnsupported(t, "/console/ssh/restarts/", func(text i18n.Catalog) *sshconnect.StepError {
		return sshconnect.Fail(text, "preflight", "restart_unsupported", text.T(i18n.SSHRestartUnsupported), text.T(i18n.SSHRestartUnsupportedFix))
	})
}

// refusesAsUnsupported asks a server coordination node's console to start
// and to read an operation at path for several node IDs in both languages,
// and expects each refused with 400 and what refusal says in that language.
func refusesAsUnsupported(t *testing.T, path string, refusal func(i18n.Catalog) *sshconnect.StepError) {
	t.Helper()
	admin := nodeAdminFixture(t)
	t.Cleanup(admin.CloseSSH)
	token := strings.Repeat("t", 40)
	server, err := httpapi.NewServer(readmodel.New(readmodel.Sources{}), httpapi.ServerConfig{Token: token})
	if err != nil {
		t.Fatal(err)
	}
	server.SetSSH(admin)
	go func() { _ = server.Serve() }()
	t.Cleanup(func() { _ = server.Close() })
	for _, tc := range []struct {
		language string
		locale   i18n.Locale
	}{{"zh-CN", i18n.LocaleZH}, {"en", i18n.LocaleEN}} {
		text := i18n.New(tc.locale)
		want := refusal(text)
		for _, node := range []string{"node-test", admin.NodeName, "Mac mini"} {
			for _, method := range []string{http.MethodPost, http.MethodGet} {
				request, err := http.NewRequestWithContext(t.Context(), method, server.URL()+path+url.PathEscape(node), nil)
				if err != nil {
					t.Fatal(err)
				}
				request.Header.Set("Authorization", "Bearer "+token)
				request.Header.Set("Accept-Language", tc.language)
				response, err := http.DefaultClient.Do(request)
				if err != nil {
					t.Fatal(err)
				}
				raw, err := io.ReadAll(response.Body)
				response.Body.Close()
				if err != nil {
					t.Fatal(err)
				}
				var body struct {
					Error string               `json:"error"`
					Step  sshconnect.StepError `json:"step"`
				}
				decoder := json.NewDecoder(bytes.NewReader(raw))
				decoder.DisallowUnknownFields()
				if response.StatusCode != http.StatusBadRequest || decoder.Decode(&body) != nil || body.Error != want.Error() ||
					body.Step.Stage != want.Stage || body.Step.Code != want.Code || body.Step.Message != want.Message || body.Step.Suggestion != want.Suggestion {
					t.Errorf("%s %s in %s = %d %s, want 400 refusing it as %s: %q", method, node, tc.language, response.StatusCode, bytes.TrimSpace(raw), want.Code, want.Message)
				}
			}
		}
	}
}

// A restart is kept among the fleet's events under the machine it
// restarted, with who ran it, whether by hand, what it did and why it
// failed, dated when it happened.
func TestARestartIsKeptAmongTheFleetEvents(t *testing.T) {
	admin := &Service{View: readmodel.New(readmodel.Sources{})}
	at := time.Date(2026, 9, 29, 8, 30, 0, 0, time.UTC)
	admin.RecordNodeRestart(sshconnect.RestartRecord{NodeID: "node-dev", By: "node-hub", Outcome: sshconnect.RestartFailed, Reason: "The peer did not stay running", At: at})
	history, _, err := admin.View.History(t.Context(), "", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(history) != 1 {
		t.Fatalf("history = %#v", history)
	}
	got := history[0]
	want := map[string]string{"by": "node-hub", "trigger": "manual", "outcome": sshconnect.RestartFailed, "reason": "The peer did not stay running"}
	if got.Kind != "observe.node.restart" || got.Subject != "node-dev" || !got.At.Equal(at) || !maps.Equal(got.Data, want) || !strings.Contains(got.Text, "node-dev") {
		t.Fatalf("recorded restart = %#v", got)
	}
}
