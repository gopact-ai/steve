// Package linktest runs the far end of an SSH link in the test's own
// process: what `ssh alias 'steve link …'` would start on the machine is
// served here over a pair of pipes, so a link can be exercised end to end
// without a network or an ssh client.
package linktest

import (
	"context"
	"fmt"
	"io"
	"net"
	"strings"
	"sync"
	"syscall"
	"testing"

	"github.com/gopact-ai/steve/internal/sshconnect"
)

// Launcher answers each Start by parsing the far end's command line out
// of the ssh arguments and serving it in-process.
type Launcher struct {
	// Refuse, when set, fails every Start with this error.
	Refuse error
	// Junk is written to the session's stdout before the far end starts,
	// as a login shell that prints would.
	Junk     string
	mu       sync.Mutex
	onEnd    func()
	launches [][]string
	sessions []*farEnd
	reserved map[string]*heldPort
}

type farEnd struct {
	cancel context.CancelFunc
	stdout *io.PipeWriter
	stdin  *io.PipeReader
	done   chan struct{}
	once   sync.Once
	reason error
	owner  *Launcher
}

func (l *Launcher) Start(_ context.Context, args []string) (*sshconnect.Session, error) {
	l.mu.Lock()
	l.launches = append(l.launches, args)
	refuse, junk := l.Refuse, l.Junk
	l.mu.Unlock()
	if refuse != nil {
		return nil, refuse
	}
	listens, allowed, err := parseCommand(args[len(args)-1])
	if err != nil {
		return nil, err
	}
	farIn, hubOut := io.Pipe()
	hubIn, farOut := io.Pipe()
	ctx, cancel := context.WithCancel(context.Background())
	far := &farEnd{cancel: cancel, stdout: farOut, stdin: farIn, done: make(chan struct{}), owner: l}
	go func() {
		if junk != "" {
			_, _ = io.WriteString(farOut, junk)
		}
		err := sshconnect.ServeLink(ctx, farIn, farOut, sshconnect.ServeLinkOptions{Logs: io.Discard, Listens: listens, Allowed: allowed, Listen: l.bind})
		far.end(err)
	}()
	l.mu.Lock()
	l.sessions = append(l.sessions, far)
	l.mu.Unlock()
	return &sshconnect.Session{Stdin: hubOut, Stdout: hubIn,
		Wait: func() (string, error) {
			<-far.done
			if far.reason != nil {
				return far.reason.Error(), far.reason
			}
			return "", nil
		},
		// A killed ssh client writes nothing more on stderr.
		Kill: func() { far.end(nil) }}, nil
}

// end stops the far end and closes its pipes, so the hub's reads fail.
func (f *farEnd) end(reason error) {
	f.once.Do(func() {
		f.reason = reason
		f.cancel()
		if reason != nil {
			_ = f.stdout.CloseWithError(reason)
		} else {
			_ = f.stdout.Close()
		}
		_ = f.stdin.Close()
		f.owner.mu.Lock()
		onEnd := f.owner.onEnd
		f.owner.mu.Unlock()
		if onEnd != nil {
			onEnd()
		}
		close(f.done)
	})
}

// SetOnEnd installs the function called as each session ends.
func (l *Launcher) SetOnEnd(onEnd func()) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.onEnd = onEnd
}

// Drop ends the newest session as a lost connection would, with reason
// as what the ssh client wrote on stderr.
func (l *Launcher) Drop(reason error) {
	l.mu.Lock()
	far := l.sessions[len(l.sessions)-1]
	l.mu.Unlock()
	far.end(reason)
}

// Ended returns a channel closed once session i has ended.
func (l *Launcher) Ended(i int) <-chan struct{} {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.sessions[i].done
}

// Launches lists the ssh arguments of every Start so far.
func (l *Launcher) Launches() [][]string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([][]string(nil), l.launches...)
}

// Count is how many sessions were started.
func (l *Launcher) Count() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.sessions)
}

// parseCommand reads the far end's --listen and --allow values out of the
// shell command the hub would hand to ssh.
func parseCommand(command string) ([]sshconnect.PortForward, []string, error) {
	words := strings.Fields(command)
	var listens []sshconnect.PortForward
	var allowed []string
	for i := 0; i < len(words); i++ {
		if i+1 >= len(words) {
			continue
		}
		value := strings.Trim(words[i+1], "'")
		switch words[i] {
		case "--listen":
			forward, err := sshconnect.ParseForward(value)
			if err != nil {
				return nil, nil, err
			}
			listens = append(listens, forward)
			i++
		case "--allow":
			allowed = append(allowed, value)
			i++
		}
	}
	return listens, allowed, nil
}

// Reserve binds a free loopback port for a far end's --listen and returns
// its address. The port stays bound until the test ends: each session
// that listens there is handed the same socket rather than binding the
// address again, so no other process can take the port in between.
func (l *Launcher) Reserve(t testing.TB) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := &heldPort{listener: listener}
	go port.dispatch()
	t.Cleanup(func() { listener.Close() })
	address := listener.Addr().String()
	l.mu.Lock()
	if l.reserved == nil {
		l.reserved = map[string]*heldPort{}
	}
	l.reserved[address] = port
	l.mu.Unlock()
	return address
}

// bind opens a far end's listen address: a reserved one through its held
// socket, any other on the network.
func (l *Launcher) bind(network, address string) (net.Listener, error) {
	l.mu.Lock()
	port := l.reserved[address]
	l.mu.Unlock()
	if port == nil {
		return net.Listen(network, address)
	}
	return port.take()
}

// heldPort is a reserved socket shared by the far ends that listen on it
// in turn. Its connections go to the far end holding it at the moment;
// with none, they are closed at once, as a dial with nothing listening
// would be refused.
type heldPort struct {
	listener net.Listener
	mu       sync.Mutex
	current  *portView
}

func (p *heldPort) take() (net.Listener, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.current != nil {
		return nil, &net.OpError{Op: "listen", Net: "tcp", Addr: p.listener.Addr(), Err: fmt.Errorf("bind: %w", syscall.EADDRINUSE)}
	}
	p.current = &portView{port: p, connections: make(chan net.Conn), done: make(chan struct{})}
	return p.current, nil
}

func (p *heldPort) dispatch() {
	for {
		connection, err := p.listener.Accept()
		if err != nil {
			return
		}
		p.mu.Lock()
		view := p.current
		p.mu.Unlock()
		if view == nil {
			connection.Close()
			continue
		}
		select {
		case view.connections <- connection:
		case <-view.done:
			connection.Close()
		}
	}
}

// portView is one far end's hold on a reserved port. Closing it ends that
// far end's accepts and leaves the socket bound for the next.
type portView struct {
	port        *heldPort
	connections chan net.Conn
	done        chan struct{}
	once        sync.Once
}

func (v *portView) Accept() (net.Conn, error) {
	select {
	case connection := <-v.connections:
		return connection, nil
	case <-v.done:
		return nil, net.ErrClosed
	}
}

func (v *portView) Close() error {
	v.once.Do(func() {
		close(v.done)
		v.port.mu.Lock()
		if v.port.current == v {
			v.port.current = nil
		}
		v.port.mu.Unlock()
	})
	return nil
}

func (v *portView) Addr() net.Addr { return v.port.listener.Addr() }
