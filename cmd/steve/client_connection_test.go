package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
)

func resolveTestConnection(args ...string) (consoleConnection, error) {
	flags := flag.NewFlagSet("client-test", flag.ContinueOnError)
	options := addConsoleClientFlags(flags)
	if err := flags.Parse(args); err != nil {
		return consoleConnection{}, err
	}
	return options.resolve(flags)
}

func writeClientFixture(t *testing.T, path, contents string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}
}

func clientFixtureConfig(address, token string) string {
	data, _ := json.Marshal(map[string]any{"gateway": map[string]string{"read_model_addr": address, "read_model_token": token}})
	return string(data)
}

func TestConsoleConnectionDefaultsAreOptIn(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	writeClientFixture(t, "config.json", "broken")
	writeClientFixture(t, "config.json.cluster.json", "broken")
	t.Setenv("STEVE_CONSOLE_TOKEN", "not-an-implicit-token")
	connection, err := resolveTestConnection()
	if err != nil || connection != (consoleConnection{URL: defaultReadModelURL}) {
		t.Fatalf("default connection = %+v, %v", connection, err)
	}
	connection, err = resolveTestConnection("-url", "https://console.example/base", "-token", "explicit-token")
	if err != nil || connection != (consoleConnection{URL: "https://console.example/base", Token: "explicit-token"}) {
		t.Fatalf("legacy explicit connection = %+v, %v", connection, err)
	}
}

func TestConsoleConnectionConfigAndSidecar(t *testing.T) {
	for _, test := range []struct {
		name    string
		address string
		sidecar string
		want    string
	}{
		{"ordinary", "127.0.0.1:8800", "", "http://127.0.0.1:8800"},
		{"empty", "", "", defaultReadModelURL},
		{"ipv4 wildcard", "0.0.0.0:8800", "", "http://127.0.0.1:8800"},
		{"ipv6 wildcard", "[::]:8800", "", "http://[::1]:8800"},
		{"omitted host", ":8800", "", "http://127.0.0.1:8800"},
		{"https", "https://console.example/base/", "", "https://console.example/base"},
		{"sidecar", "127.0.0.1:8800", `{"ui_address":"127.0.0.1:9900","data_dir":false}`, "http://127.0.0.1:9900"},
		{"empty sidecar address", "127.0.0.1:8800", `{"ui_address":""}`, defaultReadModelURL},
		{"omitted sidecar address", "127.0.0.1:8800", `{}`, defaultReadModelURL},
		{"sidecar wildcard", "127.0.0.1:8800", `{"ui_address":"[::]:9900"}`, "http://[::1]:9900"},
	} {
		t.Run(test.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.json")
			writeClientFixture(t, path, clientFixtureConfig(test.address, "config-owner-token"))
			if test.sidecar != "" {
				writeClientFixture(t, path+".cluster.json", test.sidecar)
			}
			connection, err := resolveTestConnection("-config", path)
			if err != nil || connection != (consoleConnection{URL: test.want, Token: "config-owner-token"}) {
				t.Fatalf("connection = %+v, %v", connection, err)
			}
		})
	}
}

func TestConsoleConnectionOverrides(t *testing.T) {
	for _, test := range []struct {
		name       string
		configured string
		args       []string
		wantURL    string
		wantToken  string
		wantError  bool
	}{
		{"token", "http://console.example", []string{"-token", "replacement"}, "http://console.example", "replacement", false},
		{"clear token", "http://console.example", []string{"-token="}, "http://console.example", "", false},
		{"same origin path", "http://console.example", []string{"-url", "http://console.example/proxy/"}, "http://console.example/proxy", "owner-token", false},
		{"default port and case", "https://CONSOLE.example:443", []string{"-url", "https://console.example"}, "https://console.example", "owner-token", false},
		{"http default port", "http://console.example", []string{"-url", "http://CONSOLE.example:80"}, "http://CONSOLE.example:80", "owner-token", false},
		{"ipv6 canonical", "http://[0:0:0:0:0:0:0:1]:8800", []string{"-url", "http://[::1]:8800"}, "http://[::1]:8800", "owner-token", false},
		{"ipv6 zone is case sensitive", "http://[fe80::1%25ETH0]:8800", []string{"-url", "http://[fe80::1%25eth0]:8800"}, "", "", true},
		{"wildcard canonical", "0.0.0.0:8800", []string{"-url", "http://127.0.0.1:8800"}, "http://127.0.0.1:8800", "owner-token", false},
		{"changed host", "http://console.example", []string{"-url", "http://other.example"}, "", "", true},
		{"changed port", "http://console.example", []string{"-url", "http://console.example:8800"}, "", "", true},
		{"changed scheme", "https://console.example", []string{"-url", "http://console.example"}, "", "", true},
		{"subdomain", "https://console.example", []string{"-url", "https://child.console.example"}, "", "", true},
		{"loopback aliases differ", "http://127.0.0.1:8800", []string{"-url", "http://localhost:8800"}, "", "", true},
		{"explicit default URL is override", "http://console.example", []string{"-url", defaultReadModelURL}, "", "", true},
		{"new origin and token", "http://console.example", []string{"-url", "https://other.example", "-token", "other-token"}, "https://other.example", "other-token", false},
		{"new origin without auth", "http://console.example", []string{"-url", "https://other.example", "-token", ""}, "https://other.example", "", false},
	} {
		t.Run(test.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.json")
			writeClientFixture(t, path, clientFixtureConfig(test.configured, "owner-token"))
			connection, err := resolveTestConnection(append([]string{"-config", path}, test.args...)...)
			if test.wantError {
				if err == nil || !strings.Contains(err.Error(), "origin") || strings.Contains(err.Error(), "owner-token") {
					t.Fatalf("wanted safe origin error, got %v", err)
				}
				return
			}
			if err != nil || connection != (consoleConnection{URL: test.wantURL, Token: test.wantToken}) {
				t.Fatalf("connection = %+v, %v", connection, err)
			}
		})
	}
	t.Run("no configured credential", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "config.json")
		writeClientFixture(t, path, `{}`)
		connection, err := resolveTestConnection("-config", path, "-url", "https://other.example")
		if err != nil || connection != (consoleConnection{URL: "https://other.example"}) {
			t.Fatalf("connection = %+v, %v", connection, err)
		}
	})
	t.Run("sidecar defines credential origin", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "config.json")
		writeClientFixture(t, path, clientFixtureConfig("127.0.0.1:8800", "owner-token"))
		writeClientFixture(t, path+".cluster.json", `{"ui_address":"127.0.0.1:9900"}`)
		connection, err := resolveTestConnection("-config", path, "-url", "http://127.0.0.1:9900/proxy")
		if err != nil || connection.Token != "owner-token" {
			t.Fatalf("same sidecar origin = %+v, %v", connection, err)
		}
		if _, err := resolveTestConnection("-config", path, "-url", "http://127.0.0.1:8800"); err == nil {
			t.Fatal("inherited sidecar credential at original gateway origin")
		}
	})
}

func TestNormalizeConsoleURL(t *testing.T) {
	for _, test := range []struct{ address, want string }{
		{"http://0.0.0.0:8800/", "http://127.0.0.1:8800"},
		{"https://[::]:8800/base/", "https://[::1]:8800/base"},
		{"http://:8800", "http://127.0.0.1:8800"},
		{"::", "http://[::1]"},
		{"[::1]:8800", "http://[::1]:8800"},
		{"https://[fe80::1%25lo]:8800", "https://[fe80::1%25lo]:8800"},
	} {
		t.Run(test.address, func(t *testing.T) {
			got, err := normalizeConsoleURL(test.address, true)
			if err != nil || got != test.want {
				t.Fatalf("URL = %q, %v; want %q", got, err, test.want)
			}
		})
	}
	for _, address := range []string{
		"", "console.example:8800", "//console.example", "http://", "http:///state", "https://:0",
		"ftp://console.example", "http://console.example:bad", "http://console.example:",
		"http://console.example:0", "http://console.example:65536", "http://console.example:-1",
		"http://[not-ip]:8800", "http://::1:8800", "http://[::1", "http://bad host",
		"http://owner:secret-token@console.example", "http://secret-token@console.example",
		"http://console.example/?token=secret-token", "http://console.example/?", "http://console.example/#",
		"http://console.example/#secret-token", "http://console.example/%secret-token", "http://console.example/\nsecret-token",
	} {
		t.Run(address, func(t *testing.T) {
			_, err := normalizeConsoleURL(address, false)
			if err == nil || strings.Contains(err.Error(), "secret-token") {
				t.Fatalf("wanted safe URL validation error, got %v", err)
			}
		})
	}
}

func TestConsoleConnectionReadErrors(t *testing.T) {
	for _, test := range []struct {
		name    string
		config  string
		sidecar string
	}{
		{"missing", "", ""},
		{"empty config", " ", ""},
		{"syntax", `{"gateway":`, ""},
		{"root null", `null`, ""},
		{"root array", `[]`, ""},
		{"gateway type", `{"gateway":"secret-token"}`, ""},
		{"address type", `{"gateway":{"read_model_addr":123}}`, ""},
		{"token type", `{"gateway":{"read_model_token":123}}`, ""},
		{"trailing data", `{} {}`, ""},
		{"invalid URL", clientFixtureConfig("http://secret-token@console.example", "secret-token"), ""},
		{"sidecar syntax", `{}`, `{"ui_address":`},
		{"empty sidecar", `{}`, " "},
		{"sidecar array", `{}`, `[]`},
		{"sidecar null", `{}`, `null`},
		{"sidecar type", `{}`, `{"ui_address":123}`},
		{"sidecar URL", `{}`, `{"ui_address":"https://console.example/?token=secret-token"}`},
	} {
		t.Run(test.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.json")
			if test.config != "" {
				writeClientFixture(t, path, test.config)
			}
			if test.sidecar != "" {
				writeClientFixture(t, path+".cluster.json", test.sidecar)
			}
			for _, overrides := range [][]string{nil, {"-url", "http://127.0.0.1:8800", "-token", "replacement"}} {
				_, err := resolveTestConnection(append([]string{"-config", path}, overrides...)...)
				if err == nil || strings.Contains(err.Error(), "secret-token") {
					t.Fatalf("wanted safe configuration error, got %v", err)
				}
			}
		})
	}
	for _, target := range []string{"directory", "dangling symlink"} {
		t.Run(target, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.json")
			writeClientFixture(t, path, `{}`)
			if target == "directory" {
				if err := os.Mkdir(path+".cluster.json", 0o700); err != nil {
					t.Fatal(err)
				}
			} else if err := os.Symlink(path+".missing", path+".cluster.json"); err != nil {
				t.Skipf("symlinks unavailable: %v", err)
			}
			if _, err := resolveTestConnection("-config", path); err == nil {
				t.Fatal("ignored unreadable existing sidecar")
			}
		})
	}
	if _, err := resolveTestConnection("-config="); err == nil {
		t.Fatal("ignored explicitly empty config path")
	}
}

func captureClientOutput(t *testing.T, command func() error) (string, error) {
	t.Helper()
	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	original := os.Stdout
	os.Stdout = writer
	t.Cleanup(func() { os.Stdout = original })
	output := make(chan string, 1)
	go func() {
		data, _ := io.ReadAll(reader)
		output <- string(data)
	}()
	commandErr := command()
	os.Stdout = original
	writer.Close()
	result := <-output
	reader.Close()
	return result, commandErr
}

func clientTreeSnapshot(t *testing.T, root string) map[string]string {
	t.Helper()
	snapshot := make(map[string]string)
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		value := fmt.Sprintf("%s %d", info.Mode(), info.ModTime().UnixNano())
		if !entry.IsDir() {
			data, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			value += string(data)
		}
		snapshot[path] = value
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return snapshot
}

func TestConsoleClientCommandsConfigReadOnly(t *testing.T) {
	for _, command := range []string{"dash", "top", "say"} {
		for _, sidecar := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/sidecar=%v", command, sidecar), func(t *testing.T) {
				dir := t.TempDir()
				t.Chdir(dir)
				token := "fixture-owner-token&+?"
				var requests atomic.Int32
				server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
					requests.Add(1)
					if request.Header.Get("Authorization") != "Bearer "+token || request.URL.RawQuery != "" {
						t.Error("credential must appear only in Authorization")
					}
					if command == "say" {
						var body map[string]string
						if request.Method != http.MethodPost || request.URL.Path != "/console/send" || request.Header.Get("Content-Type") != "application/json" || json.NewDecoder(request.Body).Decode(&body) != nil || body["conversation"] != "console:fixture" || body["input"] != "/fleet two words" {
							t.Error("say request changed")
						}
						io.WriteString(writer, `{"reply":{"title":"Fixture","text":"fixture-reply"}}`)
						return
					}
					if request.Method != http.MethodGet || request.URL.Path != "/state" {
						t.Error("unexpected read-model request")
					}
					io.WriteString(writer, `{}`)
				}))
				defer server.Close()
				address := strings.TrimPrefix(server.URL, "http://")
				if sidecar {
					address = "127.0.0.1:1"
					writeClientFixture(t, "config.json.cluster.json", fmt.Sprintf(`{"ui_address":%q,"cert_file":"missing.pem","data_dir":"must-not-create-cluster"}`, server.URL))
				}
				config := fmt.Sprintf(`{"gateway":{"read_model_addr":%q,"read_model_token":%q,"state_path":"must-not-create-state/state.json","home_path":"must-not-create-home"},"harnesses":{"mock":{"command":"must-not-run-agent"}},"agents":false,"feishu":{"app_id":"incomplete"}}`, address, token)
				writeClientFixture(t, "config.json", config)
				before := clientTreeSnapshot(t, dir)
				args := []string{command, "-config", "config.json"}
				if command == "top" {
					args = append(args, "-once", "-refresh", "1ms")
				} else if command == "say" {
					args = append(args, "-conversation", "console:fixture", "/fleet", "two", "words")
				}
				output, err := captureClientOutput(t, func() error { return run(args) })
				if err != nil || requests.Load() != 1 {
					t.Fatalf("command error=%v requests=%d", err, requests.Load())
				}
				if command == "dash" && strings.TrimSpace(output) != server.URL+"/?token="+url.QueryEscape(token) {
					t.Fatal("dash login link changed")
				}
				if command == "say" && output != "== Fixture\nfixture-reply\n" {
					t.Fatalf("say output = %q", output)
				}
				if command == "top" && (output == "" || strings.Contains(output, token)) {
					t.Fatal("top must render without exposing the token")
				}
				if after := clientTreeSnapshot(t, dir); !reflect.DeepEqual(before, after) {
					t.Fatal("client initialized or changed local state")
				}
			})
		}
	}
}

func TestConsoleClientCommandsDoNotSendCredentialsAcrossOrigins(t *testing.T) {
	for _, command := range []string{"dash", "top", "say"} {
		t.Run(command, func(t *testing.T) {
			var destinationRequests atomic.Int32
			var authorization atomic.Value
			destination := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				destinationRequests.Add(1)
				authorization.Store(request.Header.Get("Authorization"))
				if request.URL.RawQuery != "" {
					t.Error("unexpected query")
				}
				io.WriteString(writer, `{}`)
			}))
			defer destination.Close()
			source := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				if request.Header.Get("Authorization") != "Bearer config-owner-token" {
					t.Error("source credential missing")
				}
				http.Redirect(writer, request, destination.URL+request.URL.Path, http.StatusTemporaryRedirect)
			}))
			defer source.Close()
			path := filepath.Join(t.TempDir(), "config.json")
			writeClientFixture(t, path, clientFixtureConfig(source.URL, "config-owner-token"))
			invoke := func(extra ...string) error {
				args := append([]string{command, "-config", path}, extra...)
				if command == "top" {
					args = append(args, "-once")
				} else if command == "say" {
					args = append(args, "/fleet")
				}
				_, err := captureClientOutput(t, func() error { return run(args) })
				return err
			}
			if err := invoke("-url", destination.URL); err == nil || strings.Contains(err.Error(), "config-owner-token") {
				t.Fatalf("wanted safe cross-origin error, got %v", err)
			}
			if destinationRequests.Load() != 0 {
				t.Fatal("sent a request before validating override")
			}
			if err := invoke(); err != nil && strings.Contains(err.Error(), "config-owner-token") {
				t.Fatal("redirect error leaked credential")
			}
			if destinationRequests.Load() != 0 {
				t.Fatal("followed cross-origin redirect")
			}
			for _, token := range []string{"other-token", ""} {
				if err := invoke("-url", destination.URL, "-token", token); err != nil {
					t.Fatal(err)
				}
				want := ""
				if token != "" {
					want = "Bearer " + token
				}
				if authorization.Load() != want {
					t.Fatal("explicit token was not used")
				}
			}
		})
	}
}

func TestConsoleClientCommandsRejectConfigBeforeRequest(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		requests.Add(1)
	}))
	defer server.Close()
	path := filepath.Join(t.TempDir(), "config.json")
	writeClientFixture(t, path, `{"gateway":`)
	for _, command := range []string{"dash", "top", "say"} {
		t.Run(command, func(t *testing.T) {
			args := []string{command, "-config", path, "-url", server.URL, "-token", "explicit-token"}
			if command == "say" {
				args = append(args, "/fleet")
			}
			if err := run(args); err == nil {
				t.Fatal("ignored broken explicit config")
			}
		})
	}
	if requests.Load() != 0 {
		t.Fatal("sent requests with invalid configuration")
	}
}

func TestConsoleRedirectSameOriginAndLimit(t *testing.T) {
	original, _ := http.NewRequest(http.MethodGet, "https://console.example/state", nil)
	for _, test := range []struct {
		address string
		allowed bool
	}{
		{"https://CONSOLE.example:443/next", true},
		{"https://other.example/state", false},
		{"http://console.example/state", false},
		{"https://console.example:444/state", false},
		{"https://user:secret-token@console.example/state", false},
	} {
		request, _ := http.NewRequest(http.MethodGet, test.address, nil)
		err := checkConsoleRedirect(request, []*http.Request{original})
		if (err == nil) != test.allowed {
			t.Fatalf("redirect allowed=%v, want %v", err == nil, test.allowed)
		}
	}
	if err := checkConsoleRedirect(original, make([]*http.Request, 10)); err == nil {
		t.Fatal("redirect loop was allowed")
	}
}
