package nodebootstrap

import (
	"fmt"
	"strings"
)

// UpgradeSpec describes bringing an installed peer to another build: the
// verified program already uploaded under UploadID replaces the one in
// ~/.steve-peer/bin and the peer is restarted on it.
type UpgradeSpec struct {
	UploadID         string
	OS, Arch, SHA256 string
}

// BuildPeerUpgrade is the script that swaps a peer's program. The peer's
// state and configuration stay as they are; only the binary changes. The
// previous program is kept next to the new one and put back, and started
// again, when the new one does not stay up.
func BuildPeerUpgrade(spec UpgradeSpec) (string, error) {
	if !uploadShape.MatchString(spec.UploadID) {
		return "", fmt.Errorf("invalid peer upload ID")
	}
	if !shaShape.MatchString(spec.SHA256) || (spec.OS != "linux" && spec.OS != "darwin") || (spec.Arch != "amd64" && spec.Arch != "arm64") {
		return "", fmt.Errorf("peer upgrade requires verified platform and SHA-256")
	}
	var b strings.Builder
	b.WriteString(`#!/bin/bash
set -e
umask 077
bin_dir="$HOME/steve-bin"
state_dir="$HOME/.steve-peer"
if [ ! -f "$state_dir/config.json" ] || [ ! -x "$state_dir/bin/steve" ]; then
  echo 'No peer installation to upgrade under ~/.steve-peer.' >&2
  exit 30
fi
if ! mkdir "$bin_dir/.install-lock" 2>/dev/null; then
  echo 'Another node installation holds the installation lock.' >&2
  exit 21
fi
`)
	fmt.Fprintf(&b, `upload_dir="$bin_dir/.upload-%s"
binary_tmp="$upload_dir/steve-node"
cleanup() {
  rm -f "$binary_tmp"
  rmdir "$upload_dir" 2>/dev/null || true
  rmdir "$bin_dir/.install-lock" 2>/dev/null || true
}
trap cleanup EXIT
case "$(uname -s)/$(uname -m)" in
  %s) ;;
  *) echo 'The provided peer binary does not match this machine.' >&2; exit 22 ;;
esac
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
`, spec.UploadID, unamePattern(spec.OS, spec.Arch), spec.SHA256)
	b.WriteString(`chmod 700 "$binary_tmp"
start_peer() {
  nohup "$state_dir/bin/steve" peer --config "$state_dir/config.json" --cluster-config "$state_dir/config.json.cluster.json" >> "$state_dir/peer.log" 2>&1 < /dev/null &
  peer_pid=$!
  sleep 1
  kill -0 "$peer_pid" 2>/dev/null
}
# Only this account's peer started from this installation is stopped; the
# link session the coordinator holds open is replaced from its side.
running=$(pgrep -u "$(id -u)" -f "^$state_dir/bin/steve peer " || true)
mv -f "$state_dir/bin/steve" "$state_dir/bin/steve.previous"
mv -f "$binary_tmp" "$state_dir/bin/steve"
if [ -n "$running" ]; then
  echo "Stopping peer process $running."
  kill -TERM $running 2>/dev/null || true
  for _ in $(seq 1 60); do
    if ! kill -0 $running 2>/dev/null; then break; fi
    sleep 0.5
  done
  if kill -0 $running 2>/dev/null; then
    echo 'Peer did not stop within 30 s; ending it.' >&2
    kill -KILL $running 2>/dev/null || true
    sleep 0.5
  fi
fi
if start_peer; then
  echo 'Peer restarted on the new program; the coordinator still has to see it come back.'
  exit 0
fi
echo 'The new program did not stay running; restoring the previous one.' >&2
mv -f "$state_dir/bin/steve" "$state_dir/bin/steve.rejected"
mv -f "$state_dir/bin/steve.previous" "$state_dir/bin/steve"
if start_peer; then
  echo 'Previous program restarted; inspect ~/.steve-peer/peer.log for why the new one exited.' >&2
  exit 26
fi
echo 'Neither program stayed running; inspect ~/.steve-peer/peer.log.' >&2
exit 28
`)
	return b.String(), nil
}
