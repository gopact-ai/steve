package app

import (
	"errors"
	"net"
	"net/http"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	adminsvc "github.com/gopact-ai/steve/internal/admin"
	"github.com/gopact-ai/steve/internal/agent"
	"github.com/gopact-ai/steve/internal/config"
	"github.com/gopact-ai/steve/internal/desktop"
	"github.com/gopact-ai/steve/internal/harness"
	"github.com/gopact-ai/steve/internal/ledger"
	"github.com/gopact-ai/steve/internal/node"
	"github.com/gopact-ai/steve/internal/roster"
	"github.com/gopact-ai/steve/internal/runtime"
)

// A hub configured to issue leases to other regions does not start without
// its issuer: an address it cannot bind fails the startup with the address
// and the reason, and everything built before is released.
func TestHubDoesNotStartWhenItsLeaseIssuerCannotBind(t *testing.T) {
	taken, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer taken.Close()
	addr := taken.Addr().String()
	installation, err := desktop.Bootstrap(desktop.Options{StateDir: filepath.Join(t.TempDir(), "desktop")})
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(installation.Paths.Config)
	if err != nil {
		t.Fatal(err)
	}
	cfg.Gateway.IssuerAddr = addr
	cfg.Gateway.IssuerToken = "issuer-token"
	if err := config.Save(installation.Paths.Config, cfg); err != nil {
		t.Fatal(err)
	}

	application, err := Build(t.Context(), Config{Path: installation.Paths.Config})
	if application != nil {
		_ = application.Close()
	}
	if err == nil {
		t.Fatalf("the hub started although its lease issuer could not bind %s", addr)
	}
	if want := "lease issuer on " + addr + ":"; !strings.Contains(err.Error(), want) || !errors.Is(err, syscall.EADDRINUSE) {
		t.Fatalf("startup error %q does not name the issuer address (%q) and why it could not bind", err, want)
	}
	release, err := runtime.AcquireLock(filepath.Dir(cfg.Gateway.StatePath))
	if err != nil {
		t.Fatalf("the failed startup left its state directory locked: %v", err)
	}
	release()
}

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
	life := assembleLeaseIssuer(t, book, cfg)
	url := "http://" + addr + "/leases/acquire"

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

// The startup log names the address the issuer actually listens on, so a
// configured port 0 or host name still tells other regions where to reach it.
func TestLeaseIssuerLogsTheAddressItBound(t *testing.T) {
	output := captureLog(t)
	state := t.TempDir()
	book, err := ledger.Open(state, ledger.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { book.Close() })
	assembleLeaseIssuer(t, book, &config.Config{Gateway: config.Gateway{StatePath: filepath.Join(state, "state.json"), IssuerAddr: "127.0.0.1:0"}})
	const prefix = " leases on "
	logged := output.String()
	at := strings.Index(logged, prefix)
	if at < 0 {
		t.Fatalf("the issuer did not log where it listens: %q", logged)
	}
	addr, _, _ := strings.Cut(logged[at+len(prefix):], "\n")
	addr = strings.Fields(addr)[0]
	resp, err := http.Post("http://"+addr+"/leases/acquire", "application/json", strings.NewReader(`{}`))
	if err != nil {
		t.Fatalf("the logged issuer address %q does not answer: %v", addr, err)
	}
	resp.Body.Close()
}

// assembleLeaseIssuer assembles the models stage, which starts the lease
// issuer cfg names, and returns the lifetime that stops it.
func assembleLeaseIssuer(t *testing.T, book *ledger.Ledger, cfg *config.Config) *applicationLifetime {
	t.Helper()
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
	return life
}
