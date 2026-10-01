package nodebootstrap

import "strings"

// RestartSpec describes bringing an installed peer up again on the
// program it already has. IfStopped leaves a peer that is still running
// alone, however unreachable it is: only a machine whose peer is down gets
// one started.
type RestartSpec struct {
	ExpectedCluster string
	ExpectedNode    string
	IfStopped       bool
}

// BuildPeerRestart is the script that restarts a peer on its installed
// program. It finds and stops the running peer with the same sections an
// upgrade uses, and uploads, verifies and moves nothing. On success its
// last line is STEVE_RESTART with what it did: restarted a peer that was
// running, started one that was not, or, with IfStopped, found one running
// and left it. A peer that does not stay up exits 28 with the last lines
// of its log, and a machine where the peer cannot be looked for exits 29.
// A program that exits because the gateway lock is held was started beside
// a peer the script did not find: with IfStopped that peer runs and is
// left alone, and a restart exits 31, since it did not stop it.
func BuildPeerRestart(spec RestartSpec) string {
	var b strings.Builder
	b.WriteString(`#!/bin/bash
set -e
umask 077
bin_dir="$HOME/steve-bin"
state_dir="$HOME/.steve-peer"
if [ ! -f "$state_dir/config.json" ] || [ ! -x "$state_dir/bin/steve" ]; then
  echo 'No peer installation to restart under ~/.steve-peer.' >&2
  exit 30
fi
mkdir -p "$bin_dir"
if ! mkdir "$bin_dir/.install-lock" 2>/dev/null; then
  echo 'Another node installation holds the installation lock.' >&2
  exit 21
fi
trap 'rmdir "$bin_dir/.install-lock" 2>/dev/null || true' EXIT
`)
	b.WriteString(peerStartSection)
	if spec.ExpectedCluster != "" || spec.ExpectedNode != "" {
		b.WriteString(identifiedPeerStop(spec))
	} else {
		b.WriteString(peerLocateSection)
	}
	if spec.IfStopped {
		b.WriteString(`if [ -n "$running" ]; then
  echo "Peer process $running is still running; it is left alone."
  printf 'STEVE_RESTART\trunning\n'
  exit 0
fi
`)
	}
	if spec.ExpectedCluster == "" && spec.ExpectedNode == "" {
		b.WriteString(peerStopSection)
	}
	b.WriteString(`if start_peer; then
  if [ -n "$running" ]; then
    echo 'Peer restarted; the cluster still has to see it come back.'
    printf 'STEVE_RESTART\trestarted\n'
  else
    echo 'No peer was running; peer started. The cluster still has to see it come back.'
    printf 'STEVE_RESTART\tstarted\n'
  fi
  exit 0
fi
if exited_on_lock; then
`)
	if spec.IfStopped {
		b.WriteString(`  echo 'A peer process this script did not find holds the gateway lock; it is taken for running and left alone.'
  printf 'STEVE_RESTART\trunning\n'
  exit 0
`)
	} else {
		b.WriteString(`  echo 'The peer process that was running was not stopped: it still holds the gateway lock, and the one started beside it exited. Last lines of ~/.steve-peer/peer.log:' >&2
  tail -n 20 "$state_dir/peer.log" >&2 || true
  exit 31
`)
	}
	b.WriteString(`fi
echo 'The peer did not stay running; the peer is down. Last lines of ~/.steve-peer/peer.log:' >&2
tail -n 20 "$state_dir/peer.log" >&2 || true
exit 28
`)
	return b.String()
}

// Automatic restarts require an exact installation and a stable process
// handle. Old binaries reject the capability flag before serving anything.
func identifiedPeerStop(spec RestartSpec) string {
	if spec.ExpectedCluster == "" || spec.ExpectedNode == "" {
		return "echo 'Incomplete expected peer identity; nothing was stopped or started.' >&2\nexit 32\n"
	}
	return `if ! protocol=$("$state_dir/bin/steve" --peer-stop-protocol 2>/dev/null) || [ "$protocol" != STEVE_PEER_STOP_V1 ]; then
  echo 'The installed program does not support identified peer stopping; upgrade it before retrying.' >&2
  exit 33
fi
result=""
if result=$("$state_dir/bin/steve" peer-stop --cluster-config "$state_dir/config.json.cluster.json" --expect-cluster ` + shellQuote(spec.ExpectedCluster) + ` --expect-node ` + shellQuote(spec.ExpectedNode) + `); then
  case "$result" in
    $'STEVE_PEER_STOP\tstopped') running=identified ;;
    $'STEVE_PEER_STOP\tabsent') running="" ;;
    *) echo 'The original installation stop was not proved; nothing will be started.' >&2; exit 32 ;;
  esac
else
  case "$result" in
    $'STEVE_PEER_STOP\tunsupported') echo 'Stable process handles are unavailable; nothing was stopped or started.' >&2; exit 34 ;;
    *) echo 'The original installation stop was not proved; nothing will be started.' >&2; exit 32 ;;
  esac
fi
`
}
