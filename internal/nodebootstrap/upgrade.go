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
	b.WriteString(peerStageSection)
	b.WriteString(peerStartSection)
	b.WriteString(peerLocateSection)
	b.WriteString(peerRotateSection)
	b.WriteString(peerStopSection)
	b.WriteString(peerUpgradeStartSection)
	return b.String(), nil
}

// peerStageSection opens the second half of an upgrade: the verified
// program takes the installed program's place, the peer that was running
// is stopped and started again on it, and the previous program comes back
// when the new one does not stay up. Starting, finding and stopping the
// peer are sections of their own, shared with a restart, which runs them
// without touching the programs.
const peerStageSection = `chmod 700 "$binary_tmp"
# Staged next to the installed program first: from here on every move is
# a rename within one directory, so the installed path is never half a file.
mv -f "$binary_tmp" "$state_dir/bin/steve.new"
`

// peerStartSection defines start_peer, which starts the installed program
// in the background and succeeds when it is still running three seconds
// later.
const peerStartSection = `start_peer() {
  nohup "$state_dir/bin/steve" peer --config "$state_dir/config.json" --cluster-config "$state_dir/config.json.cluster.json" >> "$state_dir/peer.log" 2>&1 < /dev/null &
  peer_pid=$!
  sleep 3
  kill -0 "$peer_pid" 2>/dev/null
}
`

// peerLocateSection sets running to the pids of the peer this
// installation runs, or to nothing when it runs none, and exits 29 when
// it cannot look.
const peerLocateSection = `# Only this account's peer started from this installation is stopped; the
# link session the coordinator holds open is replaced from its side. The
# peer may have been started from its own directory as ./bin/steve, so a
# process is this installation's peer when its command line starts with
# the installed program's path, or with ./bin/steve and its working
# directory is this installation. The pid in the installation's gateway
# lock is tried first and taken only when it passes that test: the lock
# keeps the pid after its peer exits, and the number may since belong to
# another installation's peer. Then the program path is searched for, and
# last a relative launch. Another account's peer, and this account's peer
# under a different state directory, are left alone. Missing the process
# would leave it holding the gateway lock, and the program started next
# would exit on it and be taken for one that cannot run.
#
# A search that cannot be made is not a search that found nothing: without
# pgrep, ps, or lsof where there is no /proc, with a pgrep that fails, or
# with a gateway lock that cannot be read, the script exits 29 before it
# stops or starts anything. Taking the peer for stopped would start a
# second one beside it, or stop one that cannot start again over a lock it
# cannot open.
unlocatable() {
  echo "$1; nothing was stopped or started." >&2
  exit 29
}
for tool in pgrep ps; do
  command -v "$tool" >/dev/null 2>&1 || unlocatable "Finding the peer process needs $tool, which this machine does not have"
done
if [ ! -d /proc/self ] && ! command -v lsof >/dev/null 2>&1; then
  unlocatable 'Finding the peer process needs lsof on a machine without /proc'
fi
lock_file="$state_dir/cluster/peer-process/gateway.lock"
if [ -e "$lock_file" ] && [ ! -r "$lock_file" ]; then
  unlocatable 'The gateway lock ~/.steve-peer/cluster/peer-process/gateway.lock cannot be read'
fi
peer_search() {
  pgrep -u "$(id -u)" -f "$1" || [ $? -eq 1 ]
}
pattern=$(printf '%s' "$state_dir/bin/steve peer " | sed 's#[][\.*^$+?(){}|]#\\&#g')
state_real=$(cd "$state_dir" && pwd -P)
process_dir() {
  if [ -r "/proc/$1/cwd" ]; then
    readlink "/proc/$1/cwd" 2>/dev/null
  else
    lsof -a -p "$1" -d cwd -Fn 2>/dev/null | sed -n 's/^n//p' | head -n 1
  fi
}
installation_peer() {
  line=$(ps -o command= -p "$1" 2>/dev/null) || return 1
  if printf '%s\n' "$line" | grep -q "^$pattern"; then return 0; fi
  printf '%s\n' "$line" | grep -q '^\./bin/steve peer ' || return 1
  case "$(process_dir "$1")" in
    "$state_real"|"$state_dir") return 0 ;;
  esac
  return 1
}
running=""
locked=$(head -n 1 "$lock_file" 2>/dev/null | tr -dc '0-9')
if [ -n "$locked" ] && kill -0 "$locked" 2>/dev/null && installation_peer "$locked"; then
  running="$locked"
fi
if [ -z "$running" ]; then
  running=$(peer_search "^$pattern") || unlocatable 'Looking for the peer process with pgrep failed'
fi
if [ -z "$running" ]; then
  relative=$(peer_search '^\./bin/steve peer ') || unlocatable 'Looking for the peer process with pgrep failed'
  for candidate in $relative; do
    if installation_peer "$candidate"; then running="${running:+$running }$candidate"; fi
  done
fi
`

// peerRotateSection puts the verified program in place. The program it
// replaces becomes the fallback when it was running or there is none yet,
// and is set aside otherwise.
const peerRotateSection = `if [ -n "$running" ] || [ ! -f "$state_dir/bin/steve.previous" ]; then
  mv -f "$state_dir/bin/steve" "$state_dir/bin/steve.previous"
else
  echo 'No peer is running; the installed program is set aside and the earlier one stays the fallback.'
  mv -f "$state_dir/bin/steve" "$state_dir/bin/steve.rejected"
fi
mv -f "$state_dir/bin/steve.new" "$state_dir/bin/steve"
`

// peerStopSection stops the peer found running: asked first, ended after
// 30 seconds.
const peerStopSection = `if [ -n "$running" ]; then
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
`

// peerUpgradeStartSection starts the peer on the new program and falls
// back to the previous one when it does not stay up.
const peerUpgradeStartSection = `if start_peer; then
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
