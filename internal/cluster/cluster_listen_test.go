package cluster

import (
	"context"
	"errors"
	"net"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
)

type countedListener struct {
	net.Listener
	closes atomic.Int32
}

func (l *countedListener) Close() error {
	l.closes.Add(1)
	return l.Listener.Close()
}

// OpenPeer binds its Raft, peer and UI listeners through Listen, at the
// configured addresses (a configured port 0 at a port it picks on the same
// host), and a peer that fails to open closes each listener it was given
// exactly once.
func TestOpenPeerBindsThroughListenAndClosesWhatItTookOnce(t *testing.T) {
	options, _ := testPeerOptions(t, filepath.Join(ClusterPeerTestDir(t), "peer"), nil)
	settings, err := LoadClusterPeerConfig(options.ClusterPath)
	if err != nil {
		t.Fatal(err)
	}
	refused := errors.New("no UI port for this peer")
	var mu sync.Mutex
	var asked []string
	var taken []*countedListener
	options.Listen = func(network, address string) (net.Listener, error) {
		mu.Lock()
		defer mu.Unlock()
		if len(asked) == 2 {
			asked = append(asked, address)
			return nil, refused
		}
		listener, err := net.Listen(network, address)
		if err != nil {
			// A picked port in use is retried with another; only the
			// binds OpenPeer keeps are recorded.
			return nil, err
		}
		asked = append(asked, address)
		counted := &countedListener{Listener: listener}
		taken = append(taken, counted)
		return counted, nil
	}
	if _, err := OpenPeer(context.Background(), options); !errors.Is(err, refused) {
		t.Fatalf("the refused UI listener did not fail the peer: %v", err)
	}
	want := []string{settings.RaftBindAddress, settings.PeerBindAddress, settings.UIAddress}
	if len(asked) != len(want) {
		t.Fatalf("asked for %v, want %v", asked, want)
	}
	for i := range want {
		if !sameListenAddress(asked[i], want[i]) {
			t.Fatalf("asked for %v, want %v", asked, want)
		}
	}
	for _, listener := range taken {
		if closes := listener.closes.Load(); closes != 1 {
			t.Fatalf("%s was closed %d times", listener.Addr(), closes)
		}
	}
}

// sameListenAddress reports whether asked binds configured: the same
// address, or, for a configured port 0, a nonzero port on the same host.
func sameListenAddress(asked, configured string) bool {
	if asked == configured {
		return true
	}
	askedHost, askedPort, err := net.SplitHostPort(asked)
	if err != nil {
		return false
	}
	host, port, err := net.SplitHostPort(configured)
	return err == nil && port == "0" && askedHost == host && askedPort != "0"
}
