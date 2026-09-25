//go:build unix

package cluster

import (
	"context"
	"encoding/json"
	"net"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/gopact-ai/steve/internal/stableport"
)

// Every port a peer persists from ":0" comes from the stable range, which
// the kernel does not hand out on its own, so no socket opened while the
// peer is down can take it.
func TestZeroPortsPersistInTheStableRange(t *testing.T) {
	options, _ := testPeerOptions(t, filepath.Join(ClusterPeerTestDir(t), "peer"), nil)
	peer, err := OpenPeer(context.Background(), options)
	if err != nil {
		t.Fatal(err)
	}
	defer peer.Close()
	saved, err := LoadClusterPeerConfig(options.ClusterPath)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := ReadClusterPrivate(saved.WorkerConfigFile)
	if err != nil {
		t.Fatal(err)
	}
	var worker struct{ Listen string }
	if err := json.Unmarshal(raw, &worker); err != nil {
		t.Fatal(err)
	}
	for name, address := range map[string]string{"raft": saved.RaftBindAddress, "peer": saved.PeerBindAddress, "ui": saved.UIAddress, "worker": worker.Listen} {
		_, text, err := net.SplitHostPort(address)
		if err != nil {
			t.Fatal(err)
		}
		port, _ := strconv.Atoi(text)
		if !stableport.InRange(port) {
			t.Errorf("%s persisted %s, outside the stable range", name, address)
		}
	}
}
