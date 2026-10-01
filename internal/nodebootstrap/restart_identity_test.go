package nodebootstrap

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestAutomaticPeerRestartCannotStartAfterUnprovedInstallation(t *testing.T) {
	requirePeerPlatform(t)
	for _, verdict := range []string{"unsupported", "unproven", "missing-helper"} {
		t.Run(verdict, func(t *testing.T) {
			home := t.TempDir()
			if err := os.MkdirAll(filepath.Join(home, ".steve-peer/bin"), 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(home, ".steve-peer/config.json"), []byte("{}"), 0600); err != nil {
				t.Fatal(err)
			}
			program := `#!/bin/bash
case "$1" in
 --peer-stop-protocol) [ "$FIXTURE_STOP_RESULT" != missing-helper ] || exit 2; printf 'STEVE_PEER_STOP_V1\n'; exit 0 ;;
 peer-stop) printf 'STEVE_PEER_STOP\t%s\n' "$FIXTURE_STOP_RESULT"; exit 1 ;;
 peer) printf 'started\n' >> "$HOME/started"; exit 0 ;;
esac
exit 2
`
			if err := os.WriteFile(filepath.Join(home, ".steve-peer/bin/steve"), []byte(program), 0700); err != nil {
				t.Fatal(err)
			}
			output, err := runRestart(t, home, RestartSpec{ExpectedCluster: "cluster", ExpectedNode: "node"}, "FIXTURE_STOP_RESULT="+verdict)
			if err == nil {
				t.Fatalf("unproved restart succeeded: %s", output)
			}
			if _, err := os.Stat(filepath.Join(home, "started")); !os.IsNotExist(err) {
				t.Fatalf("failed helper still started a peer: %v %s", err, output)
			}
			if strings.Contains(output, "STEVE_RESTART\trestarted") || strings.Contains(output, "STEVE_RESTART\tstarted") {
				t.Fatal("failure claimed restart success")
			}
		})
	}
}

func TestAutomaticRestartScriptUsesOnlyTheIdentifiedHelper(t *testing.T) {
	script := BuildPeerRestart(RestartSpec{ExpectedCluster: "cluster", ExpectedNode: "node"})
	if !strings.Contains(script, "--peer-stop-protocol") || !strings.Contains(script, "--expect-cluster 'cluster'") || !strings.Contains(script, "--expect-node 'node'") {
		t.Fatal("automatic restart lacks exact identity helper")
	}
	if strings.Contains(script, "kill -TERM") || strings.Contains(script, "kill -KILL") || strings.Contains(script, "peer_search()") {
		t.Fatal("automatic restart still uses numeric PID stopping")
	}
	plain := BuildPeerRestart(RestartSpec{})
	if strings.Contains(plain, "--peer-stop-protocol") || !strings.Contains(plain, "kill -TERM") {
		t.Fatal("manual restart behavior changed")
	}
}
