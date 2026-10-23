package node

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gopact-ai/steve/internal/nodewire"
)

func restartFixture(t *testing.T) *Server {
	t.Helper()
	s := NewServer(ServerConfig{Name: "n", StateDir: t.TempDir()})
	s.EnableRestart()
	s.ctx = t.Context()
	if err := s.startRestartControl(func() {}); err != nil {
		t.Fatal(err)
	}
	if err := s.claim("hub"); err != nil {
		t.Fatal(err)
	}
	return s
}

func TestRestartAtomicallySealsAdmissionAndReplaysAcrossIncarnations(t *testing.T) {
	s := restartFixture(t)
	done, err := s.beginWork()
	if err != nil {
		t.Fatal(err)
	}
	if reply, accepted := s.restartCommand("hub", nodewire.RestartRequest{Action: "restart", CommandID: "command-1"}); accepted || reply.ErrorCode != "busy" || reply.Status.ActiveStreams != 1 {
		t.Fatal(reply, accepted)
	}
	done()
	if reply, accepted := s.restartCommand("foreign", nodewire.RestartRequest{Action: "restart", CommandID: "command-1"}); accepted || reply.ErrorCode != "ownership" {
		t.Fatal(reply, accepted)
	}
	var wg sync.WaitGroup
	accepted := make(chan bool, 2)
	for range 2 {
		wg.Go(func() {
			_, ok := s.restartCommand("hub", nodewire.RestartRequest{Action: "restart", CommandID: "command-1"})
			accepted <- ok
		})
	}
	wg.Wait()
	close(accepted)
	wins := 0
	for ok := range accepted {
		if ok {
			wins++
		}
	}
	if wins != 1 {
		t.Fatalf("restart scheduled %d times", wins)
	}
	if _, err := s.beginWork(); !errors.Is(err, nodewire.ErrRestartBusy) {
		t.Fatal("work admitted after accepted restart", err)
	}
	if reply, ok := s.restartCommand("hub", nodewire.RestartRequest{Action: "restart", CommandID: "command-2"}); ok || reply.ErrorCode != "busy" {
		t.Fatal("second command accepted while draining", reply)
	}
	next := NewServer(s.conf())
	next.EnableRestart()
	next.ctx = t.Context()
	if err := next.startRestartControl(func() {}); err != nil {
		t.Fatal(err)
	}
	if err := next.claim("hub"); err != nil {
		t.Fatal(err)
	}
	reply, repeated := next.restartCommand("hub", nodewire.RestartRequest{Action: "restart", CommandID: "command-1"})
	if repeated || reply.Status.State != "restarted" || reply.Status.PreviousIncarnation != s.generation || reply.Status.Incarnation != next.generation || reply.Status.CompletedAt.IsZero() {
		t.Fatal("restart receipt did not survive process change", reply, repeated)
	}
	latest, _ := next.restartCommand("hub", nodewire.RestartRequest{Action: "get"})
	if latest.Status.CommandID != "command-1" || latest.Status.Incarnation != next.generation {
		t.Fatal(latest)
	}
}

func TestRestartRefusesLiveAndDetachedProcessesAndStorageFailure(t *testing.T) {
	s := restartFixture(t)
	s.processes["detached"] = &agentProcess{}
	reply, accepted := s.restartCommand("hub", nodewire.RestartRequest{Action: "restart", CommandID: "busy-process"})
	if accepted || reply.ErrorCode != "busy" || reply.Status.Processes != 1 {
		t.Fatal(reply, accepted)
	}
	s.processes["detached"].exit = "exit 0"
	if err := os.MkdirAll(s.restartPath(), 0700); err != nil {
		t.Fatal(err)
	}
	reply, accepted = s.restartCommand("hub", nodewire.RestartRequest{Action: "restart", CommandID: "storage-failure"})
	if accepted || reply.ErrorCode != "storage" {
		t.Fatal(reply, accepted)
	}
	done, err := s.beginWork()
	if err != nil {
		t.Fatal("failed receipt unexpectedly sealed admission", err)
	}
	done()
}

func serveRestartNode(t *testing.T) (*Server, *Registry, <-chan error) {
	t.Helper()
	s := NewServer(ServerConfig{Name: "restart-node", Listen: "127.0.0.1:0", Token: "token", StateDir: t.TempDir(), Harnesses: map[string]HarnessSpec{"cat": {Command: "/bin/cat"}}})
	s.EnableRestart()
	ctx, cancel := context.WithCancel(t.Context())
	exit := make(chan error, 1)
	finished := make(chan struct{})
	go func() { defer close(finished); exit <- s.Serve(ctx) }()
	t.Cleanup(func() {
		cancel()
		select {
		case <-finished:
		case <-time.After(5 * time.Second):
			t.Error("node cleanup did not finish")
		}
	})
	deadline := time.Now().Add(5 * time.Second)
	for strings.HasSuffix(s.Addr(), ":0") && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	r := NewRegistry("hub", map[string]Config{"n": {Addr: s.Addr(), Token: "token"}})
	t.Cleanup(r.Close)
	if _, err := r.RestartStatus(t.Context(), "n", ""); err != nil {
		t.Fatal(err)
	}
	return s, r, exit
}

func TestRestartWireReturnsDurableReplyBeforeServiceExit(t *testing.T) {
	s, r, exit := serveRestartNode(t)
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	status, err := r.Restart(ctx, "n", "reply-before-stop")
	if err != nil || status.State != "accepted" {
		t.Fatal("service stopped without delivering restart receipt", status, err)
	}
	raw, err := os.ReadFile(s.restartPath())
	if err != nil || !strings.Contains(string(raw), "reply-before-stop") {
		t.Fatal("reply preceded its durable receipt", err)
	}
	select {
	case err := <-exit:
		if !errors.Is(err, ErrRestartRequested) {
			t.Fatal(err)
		}
	case <-ctx.Done():
		t.Fatal("accepted restart never drained the service")
	}
}

func TestRestartDoesNotKillLiveACPAndWaitsForVerifiedExit(t *testing.T) {
	_, r, exit := serveRestartNode(t)
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	proc, err := r.Transport("n", "cat").Start(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer proc.Kill()
	reader := bufio.NewReader(proc.Stdout())
	for i, line := range []string{"before rejected restart\n", "still alive after rejection\n"} {
		if _, err := io.WriteString(proc.Stdin(), line); err != nil {
			t.Fatal(err)
		}
		if got, err := reader.ReadString('\n'); err != nil || got != line {
			t.Fatal(got, err)
		}
		if i == 0 {
			status, err := r.Restart(ctx, "n", "after-exit")
			if !errors.Is(err, nodewire.ErrRestartBusy) || status.Processes != 1 {
				t.Fatal("active process did not prevent restart", status, err)
			}
		}
	}
	proc.Kill()
	for {
		status, err := r.RestartStatus(ctx, "n", "")
		if err == nil && status.Processes == 0 && status.ActiveStreams == 0 {
			break
		}
		select {
		case <-ctx.Done():
			t.Fatal("released process has no exit evidence", status, err)
		case <-time.After(time.Millisecond):
		}
	}
	status, err := r.Restart(ctx, "n", "after-exit")
	if err != nil || status.State != "accepted" {
		t.Fatal(status, err)
	}
	if err := <-exit; !errors.Is(err, ErrRestartRequested) {
		t.Fatal(err)
	}
}

func TestRestartStatusDistinguishesUnknownCommandAndCorruptReceipts(t *testing.T) {
	s, r, exit := serveRestartNode(t)
	defer func() { s.restart.mu.Lock(); cancel := s.restart.cancel; s.restart.mu.Unlock(); cancel(); <-exit }()
	if _, err := r.RestartStatus(t.Context(), "n", "never-accepted"); !errors.Is(err, nodewire.ErrRestartNotFound) {
		t.Fatal(err)
	}
	if err := os.WriteFile(s.restartPath(), []byte("null"), 0600); err != nil {
		t.Fatal(err)
	}
	next := NewServer(s.conf())
	if err := next.startRestartControl(func() {}); err == nil {
		t.Fatal("corrupt receipt document was silently discarded")
	}
}

func TestRestartPreflightFailurePreservesServiceAndCommandCanRetry(t *testing.T) {
	s := restartFixture(t)
	failure := errors.New("invalid disk configuration")
	checked := false
	s.SetRestartCheck(func() error {
		checked = true
		if !s.restart.draining || len(s.restart.records) != 0 {
			t.Fatal("validation did not run between seal and receipt")
		}
		return failure
	})
	reply, accepted := s.restartCommand("hub", nodewire.RestartRequest{Action: "restart", CommandID: "preflight"})
	if accepted || !checked || reply.ErrorCode != "preflight" {
		t.Fatal(reply, accepted, checked)
	}
	done, err := s.beginWork()
	if err != nil {
		t.Fatal("preflight failure left the service draining", err)
	}
	done()
	if reply, _ := s.restartCommand("hub", nodewire.RestartRequest{Action: "get", CommandID: "preflight"}); reply.ErrorCode != "not_found" {
		t.Fatal("rejected preflight published a receipt", reply)
	}
	s.SetRestartCheck(func() error { return nil })
	if reply, accepted := s.restartCommand("hub", nodewire.RestartRequest{Action: "restart", CommandID: "preflight"}); !accepted || reply.Status.State != "accepted" {
		t.Fatal("fixed preflight cannot retry same command", reply)
	}
}

func TestRestartGateRejectsEveryTypedWorkStreamWhileReadsRemainAvailable(t *testing.T) {
	s, r, exit := serveRestartNode(t)
	defer func() { s.restart.mu.Lock(); cancel := s.restart.cancel; s.restart.mu.Unlock(); cancel(); <-exit }()
	reply, accepted := s.restartCommand("hub", nodewire.RestartRequest{Action: "restart", CommandID: "held-restart"})
	if !accepted {
		t.Fatal(reply)
	}
	c, err := r.connect(t.Context(), "n")
	if err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(t.TempDir(), "must-not-write")
	for _, kind := range []string{nodewire.StreamACP, nodewire.StreamExec, nodewire.StreamArtifact, nodewire.StreamFiles, nodewire.StreamBlob, nodewire.StreamGrant, nodewire.StreamFetch, nodewire.StreamAdmit, nodewire.StreamSkills, nodewire.StreamRelease, nodewire.StreamConfig, nodewire.StreamMCPProbe} {
		stream, err := c.mux.Open(nodewire.OpenRequest{Kind: kind, Command: "touch " + marker})
		if err != nil {
			t.Fatal(err)
		}
		_, err = io.ReadAll(stream)
		stream.Close()
		if err == nil || !strings.Contains(err.Error(), "busy") {
			t.Fatalf("%s entered draining node: %v", kind, err)
		}
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatal("rejected Exec performed a write")
	}
	if _, err := r.Settings(t.Context(), "n"); err != nil {
		t.Fatal("config read was unavailable while draining", err)
	}
	status, err := r.RestartStatus(t.Context(), "n", "held-restart")
	if err != nil || status.State != "accepted" {
		t.Fatal(status, err)
	}
	stream, err := c.mux.Open(nodewire.OpenRequest{Kind: nodewire.StreamAdvert})
	if err != nil {
		t.Fatal(err)
	}
	var advert nodewire.Advert
	if err := json.NewDecoder(stream).Decode(&advert); err != nil {
		t.Fatal(err)
	}
	stream.Close()
}
