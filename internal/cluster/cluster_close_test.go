package cluster

import (
	"context"
	"net/http"
	"sync"
	"testing"
	"time"
)

// Close stops what console requests reach, the SSH service among them, and
// the owner lets the process go once it returns, so it may not return while
// a console request is still being served.
func TestPeerCloseReturnsOnlyOnceConsoleRequestsHaveFinished(t *testing.T) {
	options, _ := testPeerOptions(t, ClusterPeerTestDir(t), nil)
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	let := func() { once.Do(func() { close(release) }) }
	options.SSHHandler = func(SSHControl, string, string) (http.Handler, error) {
		return http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
			close(entered)
			<-release
		}), nil
	}
	peer, err := OpenPeer(context.Background(), options)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { let(); _ = peer.Close() })
	go func() {
		request, _ := http.NewRequest(http.MethodGet, peer.UiURL+"/console/ssh/hosts", nil)
		request.Header.Set("Authorization", "Bearer "+peer.UIToken)
		if response, err := http.DefaultClient.Do(request); err == nil {
			response.Body.Close()
		}
	}()
	<-entered
	closed := make(chan error, 1)
	go func() { closed <- peer.Close() }()
	select {
	case err := <-closed:
		t.Fatalf("Close returned (%v) while a console request was still being served", err)
	case <-time.After(300 * time.Millisecond):
	}
	let()
	<-closed
}
