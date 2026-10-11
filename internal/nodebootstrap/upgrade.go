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
// known to work. A new program that exits on the gateway lock of a peer
// the script did not stop is not at fault: the replaced program goes back
// in place of it, and the script exits 31.
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
	b.WriteString(peerStartSection)
	b.WriteString(peerLocateSection)
	b.WriteString(peerStageSection)
	b.WriteString(peerRotateSection)
	b.WriteString(peerStopSection)
	b.WriteString(peerUpgradeStartSection)
	return b.String(), nil
}

// peerStageSection opens the second half of an upgrade, once the peer
// has been looked for: the verified program takes the installed program's
// place, the peer that was running is stopped and started again on it,
// and the previous program comes back when the new one does not stay up.
// Until then the verified program is still the upload, which the upgrade
// removes however it exits, so an upgrade that cannot look for the peer
// leaves no program staged. Starting, finding and stopping the peer are
// sections of their own, shared with a restart, which runs them without
// touching the programs.
const peerStageSection = `chmod 700 "$binary_tmp"
# Staged next to the installed program first: from here on every move is
# a rename within one directory, so the installed path is never half a file.
mv -f "$binary_tmp" "$state_dir/bin/steve.new"
`

// peerStartSection defines start_peer, which starts the installed program
// in the background and succeeds when it is still running three seconds
// later, and exited_on_lock, which succeeds when the program start_peer
// started last exited because another process holds the gateway lock.
// Only what the log gained since that start tells: an earlier run's lines
// say nothing about this one, and the peer holding the lock can write on
// after the line the program exited with. A log cut shorter meanwhile is
// read from its start.
const peerStartSection = `start_peer() {
  log_start=0
  if [ -f "$state_dir/peer.log" ]; then
    log_start=$(wc -c < "$state_dir/peer.log")
  fi
  nohup "$state_dir/bin/steve" peer --config "$state_dir/config.json" --cluster-config "$state_dir/config.json.cluster.json" >> "$state_dir/peer.log" 2>&1 < /dev/null &
  peer_pid=$!
  sleep 3
  kill -0 "$peer_pid" 2>/dev/null
}
exited_on_lock() {
  [ -f "$state_dir/peer.log" ] || return 1
  if [ "$(wc -c < "$state_dir/peer.log")" -lt "$log_start" ]; then
    log_start=0
  fi
  tail -c "+$((log_start + 1))" "$state_dir/peer.log" | grep -q 'another gateway already serves'
}
`

// peerLocateSection sets running to the pids of the peer this
// installation runs, or to nothing when it runs none, and exits 29 when
// it cannot look.
const peerLocateSection = `# A recorded lock PID is only a candidate, never proof of ownership.
# Canonical executable paths handle /var -> /private/var and home aliases.
# Darwin lsof's lock field does not reliably report flock ownership; use
# the executable plus peer invocation instead. Identified automatic restarts
# use peer-stop's stable handle protocol, not this manual compatibility path.
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
state_real=$(cd "$state_dir" && pwd -P)
process_dir() {
  if [ -r "/proc/$1/cwd" ]; then
    readlink "/proc/$1/cwd" 2>/dev/null
  else
    lsof -a -p "$1" -d cwd -Fn 2>/dev/null | sed -n 's/^n//p' | head -n 1
  fi
}
canonical_program() {
  [ -f "$1" ] || return 1
  directory=$(dirname "$1")
  base=$(basename "$1")
  physical=$(cd "$directory" && pwd -P) || return 1
  printf '%s/%s\n' "$physical" "$base"
}
installed_program="$state_real/bin/steve"
installation_peer() {
  owner=$(ps -o uid= -p "$1" 2>/dev/null | tr -d ' ') || return 1
  [ "$owner" = "$(id -u)" ] || return 1
  line=$(ps -o command= -p "$1" 2>/dev/null) || return 1
  if [ ! -d /proc/self ]; then
    executable=$(lsof -a -p "$1" -d txt -Fn 2>/dev/null | sed -n 's/^n//p' | head -n 1)
    [ "$(canonical_program "$executable")" = "$installed_program" ] || return 1
  fi
  case "$line" in
    *' --config '*) config=${line#* --config }; config=${config%% --*} ;;
    *) return 1 ;;
  esac
  case "$config" in
    /*) ;;
    *) config="$(process_dir "$1")/$config" ;;
  esac
  [ "$(canonical_program "$config")" = "$state_real/config.json" ] || return 1
  case "$line" in
    /*' peer '*)
      program=${line%% peer *}
      [ "$(canonical_program "$program")" = "$installed_program" ] && return 0
      ;;
    './bin/steve peer '*)
      [ "$(process_dir "$1")" = "$state_real" ] && return 0
      ;;
    './steve peer '*|'steve peer '*)
      # This fallback is used only for the PID recorded by the installation.
      # Opening gateway.lock alone does not establish a peer's identity.
      if [ ! -d /proc/self ]; then
        return 0
      else
        [ "$(readlink "/proc/$1/exe" 2>/dev/null)" = "$installed_program" ] && return 0
      fi
      ;;
  esac
  return 1
}
running=""
locked=$(head -n 1 "$lock_file" 2>/dev/null | tr -dc '0-9')
if [ -n "$locked" ] && kill -0 "$locked" 2>/dev/null && installation_peer "$locked"; then
  running="$locked"
fi
if [ -z "$running" ]; then
  candidates=$(peer_search '^/.*steve peer |^\./bin/steve peer ') || unlocatable 'Looking for the peer process with pgrep failed'
  for candidate in $candidates; do
    if installation_peer "$candidate"; then running="${running:+$running }$candidate"; fi
  done
fi
`

// peerRotateSection puts the verified program in place. The program it
// replaces becomes the fallback when it was running or there is none yet,
// and is set aside otherwise; set_aside names where it went.
const peerRotateSection = `if [ -n "$running" ] || [ ! -f "$state_dir/bin/steve.previous" ]; then
  set_aside=steve.previous
else
  echo 'No peer is running; the installed program is set aside and the earlier one stays the fallback.'
  set_aside=steve.rejected
fi
mv -f "$state_dir/bin/steve" "$state_dir/bin/$set_aside"
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
// back to steve.previous when it does not stay up. The rotation always
// leaves one: the program it replaced, or the fallback it kept when that
// program was set aside. A new program that exits because the gateway
// lock is held was started beside a peer the script did not stop: the
// program it replaced is put back in its place, nothing more is started
// beside that peer, and the script exits 31.
const peerUpgradeStartSection = `if start_peer; then
  echo 'Peer restarted on the new program; the coordinator still has to see it come back.'
  exit 0
fi
if exited_on_lock; then
  mv -f "$state_dir/bin/$set_aside" "$state_dir/bin/steve"
  echo 'The peer process that was running was not stopped: it still holds the gateway lock, and the new program started beside it exited. The program that was installed is back in place; the machine was not upgraded. Last lines of ~/.steve-peer/peer.log:' >&2
  tail -n 20 "$state_dir/peer.log" >&2 || true
  exit 31
fi
echo 'The new program did not stay running; restoring the previous one.' >&2
mv -f "$state_dir/bin/steve" "$state_dir/bin/steve.rejected"
mv -f "$state_dir/bin/steve.previous" "$state_dir/bin/steve"
if start_peer; then
  echo 'Previous program restarted; inspect ~/.steve-peer/peer.log for why the new one exited.' >&2
  exit 26
fi
echo 'Neither program stayed running; the peer is down. Inspect ~/.steve-peer/peer.log.' >&2
exit 28
`
