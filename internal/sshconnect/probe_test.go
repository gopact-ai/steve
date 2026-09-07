package sshconnect

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// This remote shell substitute runs the production probe against only a fresh
// fixture HOME. Installation and uploads are rejected rather than simulated.
type probeShellRunner struct{ home string }

func (r probeShellRunner) Bind(_ context.Context, _ string, args []string) (Connection, error) {
	return &fixtureConnection{runner: r, args: args}, nil
}
func (r probeShellRunner) Run(ctx context.Context, args []string, input string) (Output, error) {
	if args[len(args)-1] != "sh -s" || input != probeScript {
		return Output{}, context.Canceled
	}
	cmd := exec.CommandContext(ctx, "/bin/sh", "-s")
	cmd.Env = []string{"HOME=" + r.home, "PATH=" + filepath.Join(r.home, "commands") + ":/usr/bin:/bin", "SSH_CONNECTION=127.0.0.1 10000 127.0.0.1 22"}
	cmd.Stdin = strings.NewReader(input)
	out, err := cmd.Output()
	return Output{Stdout: string(out)}, err
}
func (r probeShellRunner) Upload(context.Context, []string, io.Reader) (Output, error) {
	return Output{}, context.Canceled
}

func TestCheckExplainsObservedExistingPathsWithoutChangingRemoteState(t *testing.T) {
	for _, tc := range []struct{ name, marker, kind string }{
		{"fresh", "", ""},
		{"regular config", "steve-bin/node.json", "file"},
		{"node directory", ".steve-node", "directory"},
		{"peer directory", ".steve-peer", "directory"},
		{"broken config link", "steve-bin/node.json", "symlink"},
		{"broken state link", ".steve-peer", "symlink"},
		{"existing config link", "steve-bin/node.json", "valid symlink"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			home := filepath.Join(t.TempDir(), "remote home with spaces")
			if err := os.MkdirAll(filepath.Join(home, "commands"), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(home, "commands", "codex"), []byte("#!/bin/sh\ntouch \"$HOME/unexpected-agent-run\"\n"), 0o700); err != nil {
				t.Fatal(err)
			}
			marker := filepath.Join(home, tc.marker)
			if tc.marker != "" {
				if err := os.MkdirAll(filepath.Dir(marker), 0o700); err != nil {
					t.Fatal(err)
				}
				switch tc.kind {
				case "file":
					if err := os.WriteFile(marker, []byte("private-test-config-must-not-appear"), 0o600); err != nil {
						t.Fatal(err)
					}
				case "directory":
					if err := os.Mkdir(marker, 0o700); err != nil {
						t.Fatal(err)
					}
				case "symlink":
					if err := os.Symlink("missing-target", marker); err != nil {
						t.Fatal(err)
					}
				case "valid symlink":
					target := filepath.Join(home, "private-config")
					if err := os.WriteFile(target, []byte("private-test-config-must-not-appear"), 0o600); err != nil {
						t.Fatal(err)
					}
					if err := os.Symlink(target, marker); err != nil {
						t.Fatal(err)
					}
				}
			}
			before := fixtureFiles(t, home)
			config := configFixture(t, map[string]string{"config": "Host dev\nHostName fixture.example\n"})
			backend := &fakeBackend{}
			svc := New(Options{ConfigPath: config, Runner: probeShellRunner{home: home}, Backend: backend})
			t.Cleanup(func() { _ = svc.Close() })
			check, err := svc.Check(t.Context(), "dev")
			if err != nil {
				t.Fatal(err)
			}
			raw, err := json.Marshal(check)
			if err != nil {
				t.Fatal(err)
			}
			var wire struct {
				Mode  string   `json:"installation_mode"`
				Paths []string `json:"existing_paths"`
			}
			if err := json.Unmarshal(raw, &wire); err != nil {
				t.Fatal(err)
			}
			want := []string{}
			if tc.marker != "" {
				want = append(want, "~/"+tc.marker)
			}
			if wire.Mode != "executor" || !reflect.DeepEqual(wire.Paths, want) || check.ExistingInstallation != (tc.marker != "") {
				t.Fatalf("missing or misleading installation evidence: mode=%q paths=%v existing=%v", wire.Mode, wire.Paths, check.ExistingInstallation)
			}
			for _, tool := range check.Tools {
				if tool.Name == "codex" && (!tool.Available || tool.Executable != filepath.Join(home, "commands", "codex")) {
					t.Fatal("probe lost executable path containing spaces")
				}
			}
			if strings.Contains(string(raw), "private-test-config") || strings.Contains(string(raw), "管理入口") {
				t.Fatal("probe disclosed config or invented a management action")
			}
			if tc.marker != "" {
				plan, err := svc.Plan(t.Context(), installRequest())
				if err != nil || plan.Ready {
					t.Fatalf("existing state allowed fresh installation: %#v %v", plan, err)
				}
				if _, err := svc.Commit(t.Context(), plan.ID); err == nil {
					t.Fatal("existing state allowed commit")
				}
			}
			if backend.registrations != 0 || !reflect.DeepEqual(before, fixtureFiles(t, home)) {
				t.Fatal("read-only check/blocked plan modified remote state")
			}
		})
	}
}

func fixtureFiles(t *testing.T, root string) map[string]string {
	t.Helper()
	files := map[string]string{}
	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		value := info.Mode().String() + info.ModTime().String()
		if info.Mode()&os.ModeSymlink != 0 {
			target, err := os.Readlink(path)
			if err != nil {
				return err
			}
			value += target
		} else if info.Mode().IsRegular() {
			raw, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			value += string(raw)
		}
		files[path] = value
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return files
}

func TestProbeRejectsUntrustedExistingPathEvidence(t *testing.T) {
	for _, extra := range []string{
		"STEVE_CHECK\texisting_path_node_config\t/etc/passwd\n",
		"STEVE_CHECK\texisting_path_node_config\t~/steve-bin/node.json\nSTEVE_CHECK\texisting_path_node_config\t~/steve-bin/node.json\n",
		"STEVE_CHECK\texisting_path_node_config\t~/steve-bin/node.json\n",
		"STEVE_CHECK\texisting_path_unknown\t~/untrusted\n",
		"STEVE_CHECK\texisting_path_peer_state\t~/.steve-peer\textra\n",
		"STEVE_CHECK\texisting_path_peer_state\n",
	} {
		if _, err := parseProbe(completeProbe + extra); err == nil {
			t.Fatalf("accepted untrusted path evidence %q", extra)
		}
	}
}

func TestCheckInstallationModeComesFromServiceOptions(t *testing.T) {
	for _, mode := range []InstallationMode{"", InstallExecutor, InstallPeer, "invalid"} {
		t.Run(string(mode), func(t *testing.T) {
			config := configFixture(t, map[string]string{"config": "Host dev\nHostName fixture.example\n"})
			runner := &recordingRunner{}
			svc := New(Options{ConfigPath: config, Runner: runner, InstallationMode: mode})
			t.Cleanup(func() { _ = svc.Close() })
			check, err := svc.Check(t.Context(), "dev")
			if mode == "invalid" {
				if err == nil || len(runner.calls) != 0 {
					t.Fatal("invalid installation mode reached remote probe")
				}
				return
			}
			want := mode
			if want == "" {
				want = InstallExecutor
			}
			if err != nil || check.InstallationMode != want {
				t.Fatalf("check mode = %q %v; want %q", check.InstallationMode, err, want)
			}
		})
	}
}

func TestExistingStateAppearingAfterReviewBlocksBeforeAnyRegistration(t *testing.T) {
	svc, runner, backend, _ := serviceFixture(t)
	plan, err := svc.Plan(t.Context(), installRequest())
	if err != nil || !plan.Ready {
		t.Fatalf("plan = %#v %v", plan, err)
	}
	runner.checkOutput = strings.ReplaceAll(completeProbe, "existing\t0", "existing\t1") + "STEVE_CHECK\texisting_path_peer_state\t~/.steve-peer\n"
	if _, err := svc.Commit(t.Context(), plan.ID); err == nil {
		t.Fatal("existing state appeared after review but commit continued")
	}
	if backend.registrations != 0 {
		t.Fatal("existing state detected after registration")
	}
	for _, call := range runner.calls {
		if call.upload || call.stdin != probeScript {
			t.Fatal("existing state was modified after stale approval")
		}
	}
}

func TestExistingNodeMetadataIsOptionalBoundedAndNeverDisclosesCredentials(t *testing.T) {
	for _, tc := range []struct{ name, node, owner, kind, wantName, wantOwner string }{
		{"recorded identity", `{"name":"node-a","token":"private-node-token","state_dir":"/elsewhere"}`, `{"hub":"old-workspace","token":"private-owner-token"}`, "", "node-a", "old-workspace"},
		{"malformed JSON", `{`, `{"hub":9}`, "", "", ""},
		{"unexpected shapes", `["node-a"]`, `{"hub":{"name":"owner"}}`, "", "", ""},
		{"unsafe identifier", `{"name":"node-a\nSTEVE_CHECK"}`, `{"hub":"<script>"}`, "", "", ""},
		{"duplicate name", `{"name":"first","name":"second"}`, `{}`, "", "", ""},
		{"oversized config", `{"name":"node-a","padding":"` + strings.Repeat("x", 65536) + `"}`, `{}`, "", "", ""},
		{"config symlink", `{"name":"node-a"}`, `{}`, "file link", "", ""},
		{"parent symlink", `{"name":"node-a"}`, `{"hub":"old-workspace"}`, "parent link", "", "old-workspace"},
		{"missing Python", `{"name":"node-a"}`, `{"hub":"old-workspace"}`, "no Python", "", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			home := filepath.Join(t.TempDir(), "home with spaces")
			for _, dir := range []string{"commands", "steve-bin", ".steve-node"} {
				if err := os.MkdirAll(filepath.Join(home, dir), 0o700); err != nil {
					t.Fatal(err)
				}
			}
			if err := os.WriteFile(filepath.Join(home, "steve-bin", "node.json"), []byte(tc.node), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(home, ".steve-node", "hub.json"), []byte(tc.owner), 0o600); err != nil {
				t.Fatal(err)
			}
			if tc.kind == "file link" {
				if err := os.Rename(filepath.Join(home, "steve-bin", "node.json"), filepath.Join(home, "private-target")); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(filepath.Join(home, "private-target"), filepath.Join(home, "steve-bin", "node.json")); err != nil {
					t.Fatal(err)
				}
			}
			if tc.kind == "parent link" {
				if err := os.Rename(filepath.Join(home, "steve-bin"), filepath.Join(home, "private-target")); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(filepath.Join(home, "private-target"), filepath.Join(home, "steve-bin")); err != nil {
					t.Fatal(err)
				}
			}
			if tc.kind == "no Python" {
				if err := os.WriteFile(filepath.Join(home, "commands", "python3"), []byte("#!/bin/sh\nexit 127\n"), 0o700); err != nil {
					t.Fatal(err)
				}
			} else {
				python, err := exec.LookPath("python3")
				if err != nil {
					t.Skip("Python 3 required for metadata fixture")
				}
				if err := os.Symlink(python, filepath.Join(home, "commands", "python3")); err != nil {
					t.Fatal(err)
				}
			}
			for _, program := range []string{"commands/codex", "steve-bin/steve-node"} {
				if err := os.WriteFile(filepath.Join(home, program), []byte("#!/bin/sh\ntouch \"$HOME/unexpected-program-run\"\n"), 0o700); err != nil {
					t.Fatal(err)
				}
			}
			before := fixtureFiles(t, home)
			runner := probeShellRunner{home: home}
			out, err := runner.Run(t.Context(), []string{"sh -s"}, probeScript)
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(out.Stdout, "private-node-token") || strings.Contains(out.Stdout, "private-owner-token") || strings.Contains(out.Stderr, "private-") {
				t.Fatal("probe disclosed credential material")
			}
			values, err := parseProbe(out.Stdout)
			if err != nil || values["existing"] != "1" || values["existing_node_name"] != tc.wantName || values["existing_node_owner"] != tc.wantOwner {
				t.Fatalf("optional metadata = name:%q owner:%q existing:%q err:%v", values["existing_node_name"], values["existing_node_owner"], values["existing"], err)
			}
			if !reflect.DeepEqual(before, fixtureFiles(t, home)) {
				t.Fatal("metadata probe modified fixture or executed existing program")
			}
		})
	}
}

func TestCheckExposesOnlyValidatedRecordedIdentity(t *testing.T) {
	for _, tc := range []struct{ name, extra, wantName string }{
		{"valid", "STEVE_CHECK\texisting_node_name\tnode-a\n", "node-a"},
		{"malformed", "STEVE_CHECK\texisting_node_name\tnode-a\textra\n", ""},
		{"unsafe", "STEVE_CHECK\texisting_node_name\t../private\n", ""},
		{"overlong", "STEVE_CHECK\texisting_node_name\t" + strings.Repeat("a", 65) + "\n", ""},
		{"duplicate", "STEVE_CHECK\texisting_node_name\tnode-a\nSTEVE_CHECK\texisting_node_name\tnode-b\n", ""},
		{"unknown", "STEVE_CHECK\texisting_node_token\tprivate-test-token\n", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			svc, runner, _, _ := serviceFixture(t)
			runner.checkOutput = strings.ReplaceAll(completeProbe, "existing\t0", "existing\t1") + "STEVE_CHECK\texisting_node_owner\told-workspace\n" + tc.extra
			check, err := svc.Check(t.Context(), "dev")
			if err != nil || check.ExistingNode == nil || check.ExistingNode.Name != tc.wantName || check.ExistingNode.Owner != "old-workspace" {
				t.Fatalf("recorded identity = %#v %v", check.ExistingNode, err)
			}
			raw, err := json.Marshal(check)
			if err != nil || strings.Contains(string(raw), "private-test-token") {
				t.Fatal("check response disclosed unknown metadata")
			}
		})
	}
	svc, runner, _, _ := serviceFixture(t)
	runner.checkOutput = completeProbe + "STEVE_CHECK\texisting_node_name\tnode-a\n"
	check, err := svc.Check(t.Context(), "dev")
	if err != nil || check.ExistingNode != nil {
		t.Fatal("identity without existing-installation evidence was displayed")
	}
}
