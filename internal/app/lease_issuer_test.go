package app

import (
	"net"
	"net/http"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	adminsvc "github.com/gopact-ai/steve/internal/admin"
	"github.com/gopact-ai/steve/internal/agent"
	"github.com/gopact-ai/steve/internal/config"
	"github.com/gopact-ai/steve/internal/harness"
	"github.com/gopact-ai/steve/internal/ledger"
	"github.com/gopact-ai/steve/internal/node"
	"github.com/gopact-ai/steve/internal/roster"
)

// The ledger closes after the lease issuer's shutdown step, so that step may
// not end while a lease request is still inside the ledger, and the request
// is answered rather than cut off.
func TestLeaseIssuerStopsOnlyOnceTheRequestInsideTheLedgerIsAnswered(t *testing.T) {
	var hold atomic.Bool
	entered, release := make(chan struct{}), make(chan struct{})
	var released sync.Once
	now := func() time.Time {
		if hold.CompareAndSwap(true, false) {
			close(entered)
			<-release
		}
		return time.Now()
	}
	state := t.TempDir()
	book, err := ledger.Open(state, ledger.Options{Now: now})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { book.Close() })
	t.Cleanup(func() { released.Do(func() { close(release) }) })
	free, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := free.Addr().String()
	free.Close()
	const token = "issuer-token"
	cfg := &config.Config{Gateway: config.Gateway{StatePath: filepath.Join(state, "state.json"), IssuerAddr: addr, IssuerToken: token}}
	manager, err := harness.NewManager(nil)
	if err != nil {
		t.Fatal(err)
	}
	catalog, err := agent.NewCatalog(nil)
	if err != nil {
		t.Fatal(err)
	}
	nodes := node.NewRegistry("hub", nil)
	t.Cleanup(nodes.Close)
	life := &applicationLifetime{}
	t.Cleanup(func() { life.Close() })
	boot := &runtimeValues{book: book, cfg: cfg, configStore: adminsvc.NewConfigStore(cfg), manager: manager, catalog: catalog}
	if _, err := assembleModels(life, boot, &fleetValues{fleet: roster.New(catalog), nodes: nodes}); err != nil {
		t.Fatal(err)
	}
	url := "http://" + addr + "/leases/acquire"
	for deadline := time.Now().Add(5 * time.Second); ; {
		resp, err := http.Post(url, "application/json", strings.NewReader(`{}`))
		if err == nil {
			resp.Body.Close()
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("lease issuer never answered: %v", err)
		}
		time.Sleep(10 * time.Millisecond)
	}

	hold.Store(true)
	answered := make(chan int, 1)
	go func() {
		request, _ := http.NewRequest(http.MethodPost, url, strings.NewReader(`{"key":"lease/k","holder":"h","ttl":60000000000}`))
		request.Header.Set("Authorization", "Bearer "+token)
		resp, err := http.DefaultClient.Do(request)
		if err != nil {
			answered <- 0
			return
		}
		resp.Body.Close()
		answered <- resp.StatusCode
	}()
	<-entered
	closed := make(chan struct{})
	go func() { life.Close(); close(closed) }()
	select {
	case <-closed:
		t.Fatal("the lease issuer's shutdown step ended while a lease request was still inside the ledger")
	case <-time.After(200 * time.Millisecond):
	}
	released.Do(func() { close(release) })
	<-closed
	if code := <-answered; code != http.StatusOK {
		t.Fatalf("the lease request in flight was answered with %d", code)
	}
}
