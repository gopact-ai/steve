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
// state and configuration stay as they are; only the binary changes. A
// program that was running when it is replaced is kept as steve.previous
// and put back, and started again, when the new one does not stay up. A
// program found not running is not trusted that far: it is set aside as
// steve.rejected and an earlier steve.previous stays the fallback, so
// upgrading again after a failed upgrade cannot lose the last program
// known to work.
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
	b.WriteString(peerSwapSection)
	return b.String(), nil
}

// peerSwapSection is the second half of the upgrade: the verified program
// takes the installed program's place, the peer that was running is
// stopped and started again on it, and the previous program comes back
// when the new one does not stay up.
const peerSwapSection = `chmod 700 "$binary_tmp"
# Staged next to the installed program first: from here on every move is
# a rename within one directory, so the installed path is never half a file.
mv -f "$binary_tmp" "$state_dir/bin/steve.new"
start_peer() {
  nohup "$state_dir/bin/steve" peer --config "$state_dir/config.json" --cluster-config "$state_dir/config.json.cluster.json" >> "$state_dir/peer.log" 2>&1 < /dev/null &
  peer_pid=$!
  sleep 3
  kill -0 "$peer_pid" 2>/dev/null
}
# Only this account's peer started from this installation is stopped; the
# link session the coordinator holds open is replaced from its side. The
# peer may have been started from its own directory as ./bin/steve, so the
# process is identified by what the installation itself records — the pid
# in its gateway lock, confirmed to still be a peer — then by the program
# path, and last by a relative launch whose working directory is this
# installation. Another account's peer, and this account's peer under a
# different state directory, are none of this upgrade's business. Missing
# the process would leave it holding the gateway lock, and the new program
# would exit on it and be taken for a broken build.
pattern=$(printf '%s' "$state_dir/bin/steve peer " | sed 's#[][\.*^$+?(){}|]#\\&#g')
state_real=$(cd "$state_dir" && pwd -P)
process_dir() {
  if [ -r "/proc/$1/cwd" ]; then
    readlink "/proc/$1/cwd" 2>/dev/null
  else
    lsof -a -p "$1" -d cwd -Fn 2>/dev/null | sed -n 's/^n//p' | head -n 1
  fi
}
running=""
locked=$(head -n 1 "$state_dir/cluster/peer-process/gateway.lock" 2>/dev/null | tr -dc '0-9')
if [ -n "$locked" ] && kill -0 "$locked" 2>/dev/null && ps -o command= -p "$locked" 2>/dev/null | grep -q 'steve peer '; then
  running="$locked"
fi
if [ -z "$running" ]; then
  running=$(pgrep -u "$(id -u)" -f "^$pattern" || true)
fi
if [ -z "$running" ]; then
  for candidate in $(pgrep -u "$(id -u)" -f '^\./bin/steve peer ' || true); do
    case "$(process_dir "$candidate")" in
      "$state_real"|"$state_dir") running="${running:+$running }$candidate" ;;
    esac
  done
fi
if [ -n "$running" ] || [ ! -f "$state_dir/bin/steve.previous" ]; then
  mv -f "$state_dir/bin/steve" "$state_dir/bin/steve.previous"
else
  echo 'No peer is running; the installed program is set aside and the earlier one stays the fallback.'
  mv -f "$state_dir/bin/steve" "$state_dir/bin/steve.rejected"
fi
mv -f "$state_dir/bin/steve.new" "$state_dir/bin/steve"
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
if [ ! -f "$state_dir/bin/steve.previous" ]; then
  echo 'There is no previous program to restore; the peer is down. Inspect ~/.steve-peer/peer.log.' >&2
  exit 28
fi
mv -f "$state_dir/bin/steve.previous" "$state_dir/bin/steve"
if start_peer; then
  echo 'Previous program restarted; inspect ~/.steve-peer/peer.log for why the new one exited.' >&2
  exit 26
fi
echo 'Neither program stayed running; the peer is down. Inspect ~/.steve-peer/peer.log.' >&2
exit 28
`
