// Package nodebootstrap builds installation scripts without registering a node
// or performing network or process operations.
package nodebootstrap

import (
	"encoding/json"
	"fmt"
	"net/url"
	"regexp"
	"strconv"
	"strings"
)

type Harness struct {
	Adapter string   `json:"adapter,omitempty"`
	Command string   `json:"command,omitempty"`
	Args    []string `json:"args,omitempty"`
}

type Spec struct {
	Name, Port, Token string
	Harnesses         map[string]Harness
	DownloadURL       string
	UploadID          string
	OS, Arch, SHA256  string
}

var nameShape = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,63}$`)
var shaShape = regexp.MustCompile(`^[a-f0-9]{64}$`)

// Build refuses an existing installation instead of implicitly stopping or
// replacing it. Credentials are input data for bash/curl, never process args.
func Build(spec Spec) (string, error) {
	if err := validateSpec(spec); err != nil {
		return "", err
	}

	config, err := json.MarshalIndent(struct {
		Name          string             `json:"name"`
		Listen        string             `json:"listen"`
		Token         string             `json:"token"`
		WorkspaceRoot string             `json:"workspace_root"`
		StateDir      string             `json:"state_dir"`
		Harnesses     map[string]Harness `json:"harnesses"`
	}{spec.Name, "0.0.0.0:" + spec.Port, spec.Token, "~/steve-work", "~/.steve-node", spec.Harnesses}, "", "  ")
	if err != nil {
		return "", fmt.Errorf("encode node configuration: %w", err)
	}
	var b strings.Builder
	b.WriteString(`#!/bin/bash
set -e
umask 077
bin_dir="$HOME/steve-bin"
config_path="$bin_dir/node.json"
if [ -e "$config_path" ] || [ -L "$config_path" ] || [ -e "$HOME/.steve-node" ] || [ -L "$HOME/.steve-node" ]; then
  echo 'An existing node configuration or state already exists; use its management controls to make changes.' >&2
  exit 20
fi
mkdir -p "$bin_dir"
if ! mkdir "$bin_dir/.install-lock" 2>/dev/null; then
  echo 'Another node installation holds the installation lock.' >&2
  exit 21
fi
binary_tmp=''
cleanup() {
  if [ -n "$binary_tmp" ]; then rm -f "$binary_tmp"; fi
  rmdir "$bin_dir/.install-lock" 2>/dev/null || true
}
trap cleanup EXIT
if [ -e "$config_path" ] || [ -L "$config_path" ] || [ -e "$HOME/.steve-node" ] || [ -L "$HOME/.steve-node" ]; then
  echo 'An existing node configuration or state already exists.' >&2
  exit 20
fi
`)
	if spec.DownloadURL != "" || spec.UploadID != "" {
		fmt.Fprintf(&b, `case "$(uname -s)/$(uname -m)" in
  %s) ;;
  *) echo 'The provided node binary does not match this machine.' >&2; exit 22 ;;
esac
if ! command -v sha256sum >/dev/null 2>&1 && ! command -v shasum >/dev/null 2>&1; then
  echo 'SHA-256 verification requires sha256sum or shasum.' >&2
  exit 23
fi
`, unamePattern(spec.OS, spec.Arch))
		if spec.UploadID != "" {
			fmt.Fprintf(&b, `upload_dir="$bin_dir/.upload-%s"
if [ ! -f "$upload_dir/steve-node" ] || [ -L "$upload_dir" ] || [ -L "$upload_dir/steve-node" ]; then
  echo 'The uploaded node binary is missing or not a regular file.' >&2
  exit 27
fi
binary_tmp="$upload_dir/steve-node"
`, spec.UploadID)
		} else {
			fmt.Fprintf(&b, `
binary_tmp=$(mktemp "$bin_dir/.steve-node.XXXXXXXX")
curl -q --config - --output "$binary_tmp" <<'STEVE_DOWNLOAD'
url = %s
fail
silent
show-error
connect-timeout = 10
max-time = 120
STEVE_DOWNLOAD
`, strconv.Quote(spec.DownloadURL))
		}
		fmt.Fprintf(&b, `
if command -v sha256sum >/dev/null 2>&1; then
  actual=$(sha256sum "$binary_tmp")
else
  actual=$(shasum -a 256 "$binary_tmp")
fi
if [ "${actual%%%% *}" != '%s' ]; then
  echo 'Node binary SHA-256 verification failed.' >&2
  exit 24
fi
chmod 700 "$binary_tmp"
mv -f "$binary_tmp" "$bin_dir/steve-node"
binary_tmp=''
`, spec.SHA256)
		if spec.UploadID != "" {
			b.WriteString("rmdir \"$upload_dir\" 2>/dev/null || true\n")
		}
	}
	b.WriteString(`if [ ! -x "$bin_dir/steve-node" ]; then
  echo 'steve-node is not installed; provide a binary for this platform first.' >&2
  exit 25
fi
mkdir -p "$HOME/steve-work"
(
  set -C
  cat > "$config_path" <<'STEVE_CONFIG'
`)
	b.Write(config)
	b.WriteString(`
STEVE_CONFIG
)
nohup bash -lc 'exec "$HOME/steve-bin/steve-node" -config "$HOME/steve-bin/node.json"' >> "$HOME/steve-node.log" 2>&1 < /dev/null &
node_pid=$!
sleep 0.2
if ! kill -0 "$node_pid" 2>/dev/null; then
  echo 'Node startup did not remain running; inspect ~/steve-node.log.' >&2
  exit 26
fi
echo 'Node process started; coordinator connectivity must still be verified.'
`)
	return b.String(), nil
}

func validateSpec(spec Spec) error {
	port, err := strconv.Atoi(spec.Port)
	if !nameShape.MatchString(spec.Name) || err != nil || port < 1 || port > 65535 || spec.Token == "" {
		return fmt.Errorf("invalid node bootstrap identity or port")
	}
	if spec.DownloadURL != "" {
		u, err := url.Parse(spec.DownloadURL)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.User != nil || u.Fragment != "" || strings.ContainsAny(spec.DownloadURL, "\r\n\x00") {
			return fmt.Errorf("invalid node download URL")
		}
	}
	if spec.DownloadURL != "" && spec.UploadID != "" {
		return fmt.Errorf("choose one node binary source")
	}
	if spec.UploadID != "" && spec.UploadID != PreviewUploadID && !uploadShape.MatchString(spec.UploadID) {
		return fmt.Errorf("invalid node upload ID")
	}
	if spec.DownloadURL != "" || spec.UploadID != "" {
		if !shaShape.MatchString(spec.SHA256) || (spec.OS != "linux" && spec.OS != "darwin") || (spec.Arch != "amd64" && spec.Arch != "arm64") {
			return fmt.Errorf("node install requires verified platform and SHA-256")
		}
	}
	return nil
}

func unamePattern(goos, arch string) string {
	osname := "Linux"
	if goos == "darwin" {
		osname = "Darwin"
	}
	if arch == "arm64" {
		return osname + "/arm64|" + osname + "/aarch64"
	}
	return osname + "/x86_64|" + osname + "/amd64"
}
