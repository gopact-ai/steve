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
// configured addresses, and a peer that fails to open closes each
// listener it was given exactly once.
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
		asked = append(asked, address)
		if len(asked) == 3 {
			return nil, refused
		}
		listener, err := net.Listen(network, address)
		if err != nil {
			return nil, err
		}
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
		if asked[i] != want[i] {
			t.Fatalf("asked for %v, want %v", asked, want)
		}
	}
	for _, listener := range taken {
		if closes := listener.closes.Load(); closes != 1 {
			t.Fatalf("%s was closed %d times", listener.Addr(), closes)
		}
	}
}
