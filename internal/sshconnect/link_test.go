package sshconnect

import (
	"context"
	"errors"
	"net"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeSessions stands in for the ssh client: each launch opens the -L
// listeners the arguments ask for and stays up until the test drops it.
type fakeSessions struct {
	mu       sync.Mutex
	launches [][]string
	current  *fakeSession
	refuse   error
}

type fakeSession struct {
	listeners []net.Listener
	done      chan struct{}
	once      sync.Once
	err       error
}

func (f *fakeSessions) Start(_ context.Context, args []string) (Process, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.launches = append(f.launches, args)
	if f.refuse != nil {
		return nil, f.refuse
	}
	session := &fakeSession{done: make(chan struct{})}
	for i := 0; i+1 < len(args); i++ {
		if args[i] != "-L" {
			continue
		}
		listen, _, _ := strings.Cut(args[i+1], ":127.0.0.1:")
		listener, err := net.Listen("tcp", listen)
		if err != nil {
			for _, l := range session.listeners {
				l.Close()
			}
			return nil, err
		}
		session.listeners = append(session.listeners, listener)
	}
	f.current = session
	return session, nil
}

func (f *fakeSessions) drop(err error) {
	f.mu.Lock()
	session := f.current
	f.mu.Unlock()
	session.end(err)
}

func (f *fakeSessions) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.launches)
}

func (s *fakeSession) end(err error) {
	s.once.Do(func() {
		s.err = err
		for _, l := range s.listeners {
			l.Close()
		}
		close(s.done)
	})
}

func (s *fakeSession) Wait() (string, error) {
	<-s.done
	if s.err != nil {
		return s.err.Error(), s.err
	}
	return "", nil
}

func (s *fakeSession) Kill() { s.end(errors.New("killed")) }

func waitLink(t *testing.T, link *Link, want func(LinkStatus) bool) LinkStatus {
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

// A link is up once this machine's side of the tunnel accepts connections.
// The loopback ports it chose are what the route table points at, so they
// are known before the session is even attempted, and they survive the
// session going down: the next session binds the same ports.
func TestLinkComesUpReconnectsAndKeepsItsLocalPorts(t *testing.T) {
	sessions := &fakeSessions{}
	link := OpenLink(t.Context(), LinkSpec{Alias: "dev", Inbound: []PortForward{{Listen: "127.0.0.1:59407", Target: "127.0.0.1:7712"}}, Outbound: []PortForward{{Target: "127.0.0.1:7702"}, {Target: "127.0.0.1:7701"}}}, LinkOptions{Launch: sessions, Backoff: func(int) time.Duration { return 10 * time.Millisecond }})
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
	args := strings.Join(sessions.launches[0], " ")
	for _, want := range []string{"-N", "-o BatchMode=yes", "-o ExitOnForwardFailure=yes", "-R 127.0.0.1:59407:127.0.0.1:7712", "-L " + first.Outbound[0].Listen + ":127.0.0.1:7702", "-L " + first.Outbound[1].Listen + ":127.0.0.1:7701", "-- dev"} {
		if !strings.Contains(args, want) {
			t.Fatalf("session arguments lack %q: %s", want, args)
		}
	}
	sessions.drop(errors.New("connection closed by remote host"))
	down := waitLink(t, link, func(s LinkStatus) bool { return !s.Connected })
	if !strings.Contains(down.LastError, "connection closed") {
		t.Fatalf("the reason the session ended was not kept: %+v", down)
	}
	again := waitLink(t, link, func(s LinkStatus) bool { return s.Connected && s.Attempts >= 2 })
	if again.Outbound[0].Listen != first.Outbound[0].Listen || sessions.count() < 2 {
		t.Fatalf("the second session did not bind the same local ports: %+v vs %+v", again, first)
	}
	link.Close()
	if status := link.Status(); status.Connected {
		t.Fatalf("closed link still reports connected: %+v", status)
	}
}

// A session the client cannot even start (the alias is gone, the remote
// port is taken) leaves the link down with the reason, and waiting on it
// ends with that reason rather than the deadline alone.
func TestLinkReportsWhyItCannotComeUp(t *testing.T) {
	sessions := &fakeSessions{refuse: errors.New("remote port forwarding failed for listen port 59407")}
	link := OpenLink(t.Context(), LinkSpec{Alias: "dev", Outbound: []PortForward{{Target: "127.0.0.1:7702"}}}, LinkOptions{Launch: sessions, Backoff: func(int) time.Duration { return 10 * time.Millisecond }})
	t.Cleanup(link.Close)
	ctx, cancel := context.WithTimeout(t.Context(), 200*time.Millisecond)
	defer cancel()
	err := link.WaitConnected(ctx)
	if err == nil || !strings.Contains(err.Error(), "remote port forwarding failed") {
		t.Fatalf("the wait did not say why: %v", err)
	}
	if status := link.Status(); status.Connected || status.Attempts < 2 {
		t.Fatalf("link did not keep trying: %+v", status)
	}
}
