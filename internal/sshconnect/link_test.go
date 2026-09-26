package sshconnect_test

import (
	"bufio"
	"context"
	"errors"
	"io"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gopact-ai/steve/internal/sshconnect"
	"github.com/gopact-ai/steve/internal/sshconnect/linktest"
	"github.com/hashicorp/yamux"
)

func waitLink(t *testing.T, link *sshconnect.Link, want func(sshconnect.LinkStatus) bool) sshconnect.LinkStatus {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		status := link.Status()
		if want(status) {
			return status
		}
		if time.Now().After(deadline) {
			t.Fatalf("link did not reach the wanted state: %+v", status)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// echoServer answers on a free loopback port by writing back what it reads.
func echoServer(t *testing.T) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { listener.Close() })
	go func() {
		for {
			connection, err := listener.Accept()
			if err != nil {
				return
			}
			go func() {
				_, _ = io.Copy(connection, connection)
				connection.Close()
			}()
		}
	}()
	return listener.Addr().String()
}

// echoThrough dials address, sends a line and expects it back.
func echoThrough(t *testing.T, address, line string) error {
	t.Helper()
	connection, err := net.DialTimeout("tcp", address, 2*time.Second)
	if err != nil {
		return err
	}
	defer connection.Close()
	_ = connection.SetDeadline(time.Now().Add(3 * time.Second))
	if _, err := io.WriteString(connection, line+"\n"); err != nil {
		return err
	}
	got, err := bufio.NewReader(connection).ReadString('\n')
	if err != nil {
		return err
	}
	if strings.TrimSpace(got) != line {
		t.Fatalf("echo through %s returned %q", address, got)
	}
	return nil
}

// A link is up once the far end has announced itself over the session and
// answers through the multiplexer. The loopback ports this machine chose
// are what the route table points at, so they are bound before the
// session is even attempted, and they survive the session going down: the
// same listeners carry the next session's streams.
func TestLinkComesUpReconnectsAndKeepsItsLocalPorts(t *testing.T) {
	sessions := &linktest.Launcher{Junk: "Welcome to box\nLast login: never\n"}
	var mu sync.Mutex
	var seen []sshconnect.LinkStatus
	record := func(status sshconnect.LinkStatus) {
		mu.Lock()
		defer mu.Unlock()
		seen = append(seen, status)
	}
	inbound := sessions.Reserve(t)
	link := sshconnect.OpenLink(t.Context(), sshconnect.LinkSpec{Alias: "dev", Inbound: []sshconnect.PortForward{{Listen: inbound, Target: "127.0.0.1:7712"}}, Outbound: []sshconnect.PortForward{{Target: "127.0.0.1:7702"}, {Target: "127.0.0.1:7701"}}}, sshconnect.LinkOptions{Launch: sessions, Backoff: func(int) time.Duration { return 10 * time.Millisecond }, OnChange: record})
	t.Cleanup(link.Close)
	first := link.Status()
	if len(first.Outbound) != 2 || first.Outbound[0].Listen == "" || first.Outbound[0].Listen == first.Outbound[1].Listen {
		t.Fatalf("local ports were not chosen up front: %+v", first)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	if err := link.WaitConnected(ctx); err != nil {
		t.Fatalf("link did not come up: %v", err)
	}
	args := strings.Join(sessions.Launches()[0], " ")
	for _, want := range []string{"-T", "-o BatchMode=yes", "-o ClearAllForwardings=yes", `-- dev exec "$HOME/.steve-peer/bin/steve" link --listen '` + inbound + `=127.0.0.1:7712' --allow '127.0.0.1:7702' --allow '127.0.0.1:7701'`} {
		if !strings.Contains(args, want) {
			t.Fatalf("session arguments lack %q: %s", want, args)
		}
	}
	if strings.Contains(args, "-N") || strings.Contains(args, "-R") || strings.Contains(args, "-L") {
		t.Fatalf("the session still asks sshd for port forwards: %s", args)
	}
	sessions.Drop(errors.New("connection closed by remote host"))
	again := waitLink(t, link, func(s sshconnect.LinkStatus) bool { return s.Connected && s.Attempts >= 2 })
	mu.Lock()
	var down *sshconnect.LinkStatus
	for i := range seen {
		if !seen[i].Connected && seen[i].LastError != "" {
			down = &seen[i]
		}
	}
	mu.Unlock()
	if down == nil || !strings.Contains(down.LastError, "connection closed") {
		t.Fatalf("the reason the session ended was not reported: %+v", down)
	}
	if again.Outbound[0].Listen != first.Outbound[0].Listen || sessions.Count() < 2 {
		t.Fatalf("the second session does not use the same local ports: %+v vs %+v", again, first)
	}
	link.Close()
	if status := link.Status(); status.Connected {
		t.Fatalf("closed link still reports connected: %+v", status)
	}
	if _, err := net.DialTimeout("tcp", first.Outbound[0].Listen, time.Second); err == nil {
		t.Fatal("a closed link still holds its local port")
	}
}

// Traffic crosses the link in both directions: a connection to one of
// this machine's local ports reaches the machine's target, and a
// connection to the machine's listener reaches this machine's target.
// While the session is down, a dial at a local port is refused at once.
func TestLinkCarriesTrafficBothWaysAndRefusesWhileDown(t *testing.T) {
	hubEcho, machineEcho := echoServer(t), echoServer(t)
	sessions := &linktest.Launcher{}
	inbound := sessions.Reserve(t)
	link := sshconnect.OpenLink(t.Context(), sshconnect.LinkSpec{Alias: "dev", Inbound: []sshconnect.PortForward{{Listen: inbound, Target: hubEcho}}, Outbound: []sshconnect.PortForward{{Target: machineEcho}}}, sshconnect.LinkOptions{Launch: sessions, Backoff: func(int) time.Duration { return time.Hour }})
	t.Cleanup(link.Close)
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	if err := link.WaitConnected(ctx); err != nil {
		t.Fatal(err)
	}
	outbound := link.Status().Outbound[0].Listen
	if err := echoThrough(t, outbound, "to the machine"); err != nil {
		t.Fatalf("outbound traffic did not cross the link: %v", err)
	}
	if err := echoThrough(t, inbound, "to the hub"); err != nil {
		t.Fatalf("inbound traffic did not cross the link: %v", err)
	}
	sessions.Drop(errors.New("connection reset"))
	waitLink(t, link, func(s sshconnect.LinkStatus) bool { return !s.Connected })
	began := time.Now()
	err := echoThrough(t, outbound, "nobody home")
	if err == nil || time.Since(began) > time.Second {
		t.Fatalf("a dial while the session is down did not fail fast: err=%v after %s", err, time.Since(began))
	}
}

// A session the client cannot even start (the alias is gone, ssh refuses)
// leaves the link down with the reason, and waiting on it ends with that
// reason rather than the deadline alone.
func TestLinkReportsWhyItCannotComeUp(t *testing.T) {
	sessions := &linktest.Launcher{Refuse: errors.New("ssh: Could not resolve hostname dev")}
	link := sshconnect.OpenLink(t.Context(), sshconnect.LinkSpec{Alias: "dev", Outbound: []sshconnect.PortForward{{Target: "127.0.0.1:7702"}}}, sshconnect.LinkOptions{Launch: sessions, Backoff: func(int) time.Duration { return 10 * time.Millisecond }})
	t.Cleanup(link.Close)
	ctx, cancel := context.WithTimeout(t.Context(), 200*time.Millisecond)
	defer cancel()
	err := link.WaitConnected(ctx)
	if err == nil || !strings.Contains(err.Error(), "Could not resolve hostname") {
		t.Fatalf("the wait did not say why: %v", err)
	}
	if status := link.Status(); status.Connected || status.Attempts < 2 {
		t.Fatalf("link did not keep trying: %+v", status)
	}
}

// The far end only opens the targets it was told to allow: a stream that
// names anything else is closed unanswered, and one that names an allowed
// target is connected to it.
func TestFarEndOpensOnlyAllowedTargets(t *testing.T) {
	allowed, forbidden := echoServer(t), echoServer(t)
	farIn, hubOut := io.Pipe()
	hubIn, farOut := io.Pipe()
	ctx, cancel := context.WithCancel(t.Context())
	served := make(chan error, 1)
	go func() {
		served <- sshconnect.ServeLink(ctx, farIn, farOut, sshconnect.ServeLinkOptions{Logs: io.Discard, Allowed: []string{allowed}})
	}()
	reader := bufio.NewReader(hubIn)
	if banner, err := reader.ReadString('\n'); err != nil || strings.TrimSpace(banner) != "STEVE-LINK/1" {
		t.Fatalf("the far end did not announce itself: %q %v", banner, err)
	}
	config := yamux.DefaultConfig()
	config.LogOutput = io.Discard
	client, err := yamux.Client(struct {
		io.Reader
		io.WriteCloser
	}{reader, hubOut}, config)
	if err != nil {
		t.Fatal(err)
	}
	open := func(target string) *yamux.Stream {
		stream, err := client.OpenStream()
		if err != nil {
			t.Fatal(err)
		}
		if _, err := io.WriteString(stream, target+"\nping\n"); err != nil {
			t.Fatal(err)
		}
		_ = stream.SetReadDeadline(time.Now().Add(3 * time.Second))
		return stream
	}
	if got, err := bufio.NewReader(open(allowed)).ReadString('\n'); err != nil || got != "ping\n" {
		t.Fatalf("an allowed target was not reached: %q %v", got, err)
	}
	if got, err := bufio.NewReader(open(forbidden)).ReadString('\n'); err != io.EOF || got != "" {
		t.Fatalf("a target outside the allowed list was answered: %q %v", got, err)
	}
	cancel()
	select {
	case err := <-served:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("the far end did not stop with its context")
	}
}

// A far end that cannot bind a listen address (something on the machine
// holds the port) ends the session at once, and the link keeps the reason.
func TestLinkReportsAFarEndThatCannotBindItsPort(t *testing.T) {
	taken, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer taken.Close()
	sessions := &linktest.Launcher{}
	link := sshconnect.OpenLink(t.Context(), sshconnect.LinkSpec{Alias: "dev", Inbound: []sshconnect.PortForward{{Listen: taken.Addr().String(), Target: "127.0.0.1:7712"}}}, sshconnect.LinkOptions{Launch: sessions, Backoff: func(int) time.Duration { return 10 * time.Millisecond }})
	t.Cleanup(link.Close)
	ctx, cancel := context.WithTimeout(t.Context(), 300*time.Millisecond)
	defer cancel()
	err = link.WaitConnected(ctx)
	if err == nil || !strings.Contains(err.Error(), "监听 "+taken.Addr().String()+" 失败") {
		t.Fatalf("the far end's reason was not kept: %v", err)
	}
	if status := link.Status(); status.Connected || status.Attempts < 2 {
		t.Fatalf("link did not keep trying: %+v", status)
	}
}

// Shell output ahead of the banner is skipped only up to the reader's
// buffer: a login that floods stdout ends the attempt with a reason
// instead of being read without bound.
func TestLinkGivesUpOnAFarEndThatFloodsStdout(t *testing.T) {
	sessions := &linktest.Launcher{Junk: strings.Repeat("x", 65<<10)}
	link := sshconnect.OpenLink(t.Context(), sshconnect.LinkSpec{Alias: "dev"}, sshconnect.LinkOptions{Launch: sessions, Backoff: func(int) time.Duration { return time.Hour }})
	t.Cleanup(link.Close)
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()
	err := link.WaitConnected(ctx)
	if err == nil || !strings.Contains(err.Error(), "输出过多") {
		t.Fatalf("the flood was not refused: %v", err)
	}
}

// Told to stop, the far end returns without waiting for the hub to do
// anything: on the machine, sshd may never close its stdin.
func TestFarEndStopsWithItsContextWhileTheHubIsSilent(t *testing.T) {
	farIn, _ := io.Pipe()
	hubIn, farOut := io.Pipe()
	ctx, cancel := context.WithCancel(t.Context())
	served := make(chan error, 1)
	go func() {
		served <- sshconnect.ServeLink(ctx, farIn, farOut, sshconnect.ServeLinkOptions{Logs: io.Discard})
	}()
	if _, err := bufio.NewReader(hubIn).ReadString('\n'); err != nil {
		t.Fatal(err)
	}
	cancel()
	select {
	case err := <-served:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("the far end waited for the hub before stopping")
	}
}

// noticing is a listener that closes closed once the far end closes a
// connection it handed over.
type noticing struct {
	net.Listener
	once   sync.Once
	closed chan struct{}
}

func (l *noticing) Accept() (net.Conn, error) {
	connection, err := l.Listener.Accept()
	if err != nil {
		return nil, err
	}
	return &noticed{Conn: connection, owner: l}, nil
}

type noticed struct {
	net.Conn
	owner *noticing
}

func (c *noticed) Close() error {
	c.owner.once.Do(func() { close(c.owner.closed) })
	return c.Conn.Close()
}

// The hub counts a link as up once the far end's multiplexer answers a
// ping, which it does as soon as it runs. A connection the far end takes
// from a listen address before then must still reach the hub rather than
// be dropped: here one is already waiting when the far end starts, and
// the hub reads the far end's announcement, which the far end cannot get
// past until then, only once the far end has closed the connection or
// has had ample time to.
func TestFarEndCarriesAConnectionThatArrivedBeforeItsSession(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	early, err := net.DialTimeout("tcp", listener.Addr().String(), 2*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer early.Close()
	if _, err := io.WriteString(early, "early\n"); err != nil {
		t.Fatal(err)
	}
	watched := &noticing{Listener: listener, closed: make(chan struct{})}
	farIn, hubOut := io.Pipe()
	hubIn, farOut := io.Pipe()
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	options := sshconnect.ServeLinkOptions{
		Logs:    io.Discard,
		Listens: []sshconnect.PortForward{{Listen: "127.0.0.1:7", Target: "127.0.0.1:8"}},
		Listen:  func(string, string) (net.Listener, error) { return watched, nil },
	}
	go func() { _ = sshconnect.ServeLink(ctx, farIn, farOut, options) }()
	select {
	case <-watched.closed:
	case <-time.After(200 * time.Millisecond):
	}
	reader := bufio.NewReader(hubIn)
	if banner, err := reader.ReadString('\n'); err != nil || strings.TrimSpace(banner) != "STEVE-LINK/1" {
		t.Fatalf("the far end did not announce itself: %q %v", banner, err)
	}
	config := yamux.DefaultConfig()
	config.LogOutput = io.Discard
	hub, err := yamux.Client(struct {
		io.Reader
		io.WriteCloser
	}{reader, hubOut}, config)
	if err != nil {
		t.Fatal(err)
	}
	defer hub.Close()
	if _, err := hub.Ping(); err != nil {
		t.Fatal(err)
	}
	waiting, stop := context.WithTimeout(t.Context(), 3*time.Second)
	defer stop()
	stream, err := hub.AcceptStreamWithContext(waiting)
	if err != nil {
		_ = early.SetReadDeadline(time.Now().Add(time.Second))
		_, dropped := early.Read(make([]byte, 1))
		t.Fatalf("the connection that arrived before the session never reached the hub (%v); the far end ended it with %v", err, dropped)
	}
	_ = stream.SetDeadline(time.Now().Add(3 * time.Second))
	if got, err := bufio.NewReader(stream).ReadString('\n'); err != nil || got != "127.0.0.1:8\n" {
		t.Fatalf("the stream named %q: %v", got, err)
	}
	if got, err := bufio.NewReader(stream).ReadString('\n'); err != nil || got != "early\n" {
		t.Fatalf("the stream carried %q: %v", got, err)
	}
	if _, err := io.WriteString(stream, "carried\n"); err != nil {
		t.Fatal(err)
	}
	_ = early.SetReadDeadline(time.Now().Add(3 * time.Second))
	if got, err := bufio.NewReader(early).ReadString('\n'); err != nil || got != "carried\n" {
		t.Fatalf("the early connection got %q back: %v", got, err)
	}
}

// The far end binds each listen address through Listen, and the listener
// it is given is the one it serves and closes when the session ends.
func TestFarEndBindsItsListenAddressesThroughListen(t *testing.T) {
	farIn, _ := io.Pipe()
	hubIn, farOut := io.Pipe()
	ctx, cancel := context.WithCancel(t.Context())
	var asked []string
	var given net.Listener
	options := sshconnect.ServeLinkOptions{
		Logs:    io.Discard,
		Listens: []sshconnect.PortForward{{Listen: "127.0.0.1:7", Target: "127.0.0.1:8"}},
		Listen: func(network, address string) (net.Listener, error) {
			asked = append(asked, network+" "+address)
			listener, err := net.Listen("tcp", "127.0.0.1:0")
			given = listener
			return listener, err
		},
	}
	served := make(chan error, 1)
	go func() { served <- sshconnect.ServeLink(ctx, farIn, farOut, options) }()
	if _, err := bufio.NewReader(hubIn).ReadString('\n'); err != nil {
		t.Fatal(err)
	}
	if len(asked) != 1 || asked[0] != "tcp 127.0.0.1:7" {
		t.Fatalf("the far end bound %v", asked)
	}
	cancel()
	if err := <-served; err != nil {
		t.Fatal(err)
	}
	if _, err := given.Accept(); !errors.Is(err, net.ErrClosed) {
		t.Fatalf("the listener given to the far end is still open: %v", err)
	}
}
