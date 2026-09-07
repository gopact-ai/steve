package nodebootstrap

import (
	"encoding/base64"
	"fmt"
	"strings"
)

// PeerSpec describes a full peer installation. JoinPackage is an opaque
// base64-encoded private import bundle, supplied only during explicit commit.
// A preview uses PreviewJoinPackage and contains no key or token material.
type PeerSpec struct {
	UploadID         string
	OS, Arch, SHA256 string
	JoinPackage      string
}

const PreviewJoinPackage = "pending-node-token"

func BuildPeer(spec PeerSpec) (string, error) {
	if spec.UploadID != PreviewUploadID && !uploadShape.MatchString(spec.UploadID) {
		return "", fmt.Errorf("invalid peer upload ID")
	}
	if !shaShape.MatchString(spec.SHA256) || (spec.OS != "linux" && spec.OS != "darwin") || (spec.Arch != "amd64" && spec.Arch != "arm64") {
		return "", fmt.Errorf("peer install requires verified platform and SHA-256")
	}
	if spec.JoinPackage != PreviewJoinPackage {
		if len(spec.JoinPackage) > 2<<20 {
			return "", fmt.Errorf("peer join package exceeds size limit")
		}
		raw, err := base64.StdEncoding.DecodeString(spec.JoinPackage)
		if err != nil || len(raw) == 0 || base64.StdEncoding.EncodeToString(raw) != spec.JoinPackage {
			return "", fmt.Errorf("invalid peer join package encoding")
		}
	}
	var b strings.Builder
	b.WriteString(`#!/bin/bash
set -e
umask 077
bin_dir="$HOME/steve-bin"
state_dir="$HOME/.steve-peer"
if [ -e "$bin_dir/node.json" ] || [ -L "$bin_dir/node.json" ] || [ -e "$HOME/.steve-node" ] || [ -L "$HOME/.steve-node" ]; then
  echo 'An existing node configuration or state already exists; use its management controls.' >&2
  exit 20
fi
if ! mkdir "$bin_dir/.install-lock" 2>/dev/null; then
  echo 'Another node installation holds the installation lock.' >&2
  exit 21
fi
package_file=''
cleanup() {
  if [ -n "$package_file" ]; then rm -f "$package_file"; fi
  rmdir "$bin_dir/.install-lock" 2>/dev/null || true
}
trap cleanup EXIT
`)
	fmt.Fprintf(&b, `case "$(uname -s)/$(uname -m)" in
  %s) ;;
  *) echo 'The provided peer binary does not match this machine.' >&2; exit 22 ;;
esac
upload_dir="$bin_dir/.upload-%s"
binary_tmp="$upload_dir/steve-node"
if [ ! -f "$binary_tmp" ] || [ -L "$upload_dir" ] || [ -L "$binary_tmp" ]; then
  echo 'The uploaded peer binary is missing or not a regular file.' >&2
  exit 27
fi
if command -v sha256sum >/dev/null 2>&1; then
  actual=$(sha256sum "$binary_tmp")
elif command -v shasum >/dev/null 2>&1; then
  actual=$(shasum -a 256 "$binary_tmp")
else
  echo 'SHA-256 verification requires sha256sum or shasum.' >&2
  exit 23
fi
if [ "${actual%%%% *}" != '%s' ]; then
  echo 'Peer binary SHA-256 verification failed.' >&2
  exit 24
fi
package_file=$(mktemp "$upload_dir/.join.XXXXXXXX")
`, unamePattern(spec.OS, spec.Arch), spec.UploadID, spec.SHA256)
	decode := "base64 -d"
	if spec.OS == "darwin" {
		decode = "base64 -D"
	}
	fmt.Fprintf(&b, "%s > \"$package_file\" <<'STEVE_PEER_JOIN'\n%s\nSTEVE_PEER_JOIN\n", decode, spec.JoinPackage)
	b.WriteString(`chmod 700 "$binary_tmp"
# Import validates the operation and existing state before replacing any
# installed executable. The bundle contains no authority signing key.
"$binary_tmp" peer-import --package "$package_file" --state-dir "$state_dir"
mkdir -p "$state_dir/bin"
mv -f "$binary_tmp" "$state_dir/bin/steve"
rm -f "$package_file"
package_file=''
rmdir "$upload_dir" 2>/dev/null || true
nohup "$state_dir/bin/steve" peer --config "$state_dir/config.json" --cluster-config "$state_dir/config.json.cluster.json" >> "$state_dir/peer.log" 2>&1 < /dev/null &
peer_pid=$!
sleep 0.2
if ! kill -0 "$peer_pid" 2>/dev/null; then
  echo 'Peer startup did not remain running; inspect ~/.steve-peer/peer.log.' >&2
  exit 26
fi
echo 'Peer process started; membership and independent connectivity must still be verified.'
`)
	return b.String(), nil
}
