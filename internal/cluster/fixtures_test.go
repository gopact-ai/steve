package cluster

import (
	"net"
	"os"
	"path/filepath"
	"testing"

	"github.com/gopact-ai/steve/internal/sshconnect"
)

func InstallBinaryFixture(t *testing.T) string {
	t.Helper()
	raw := make([]byte, 64)
	copy(raw, []byte{0x7f, 'E', 'L', 'F', 2, 1, 1})
	raw[16], raw[18], raw[20], raw[52] = 2, 62, 1, 64
	path := filepath.Join(t.TempDir(), "node")
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func SshCheckFixture() sshconnect.CheckResult {
	check := sshconnect.CheckResult{OS: "linux", Arch: "amd64", Reachable: true}
	for _, name := range []string{"curl", "sha256sum", "bash", "nohup", "node", "npm"} {
		check.Tools = append(check.Tools, sshconnect.Tool{Name: name, Available: true})
	}
	return check
}

// HoldEnrollmentPorts binds a machine's peer and Raft ports for an
// enrollment and keeps them bound until the test ends. A node that serves
// on them takes the listeners through PeerOptions.Listeners, so the ports
// are never released and bound again.
func HoldEnrollmentPorts(t *testing.T) *PeerListeners {
	t.Helper()
	held := &PeerListeners{}
	for _, listener := range []*net.Listener{&held.Peer, &held.Raft} {
		bound, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { bound.Close() })
		*listener = bound
	}
	return held
}

// Addresses are the held peer and Raft addresses, in that order.
func (l *PeerListeners) Addresses() (string, string) {
	return l.Peer.Addr().String(), l.Raft.Addr().String()
}
