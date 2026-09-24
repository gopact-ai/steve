package sshconnect

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"sync"
	"time"

	"github.com/hashicorp/yamux"
)

// bridge is one end of a link. It holds the listeners whose connections
// go out through the multiplexer to a target on the other end, and it
// answers the other end's streams by dialing the target each names,
// provided that target is one it was told to allow.
type bridge struct {
	allowed   map[string]bool
	bind      ListenFunc
	mu        sync.Mutex
	listeners map[int]net.Listener
	session   *yamux.Session
}

const (
	streamHeaderTimeout = 10 * time.Second
	targetDialTimeout   = 5 * time.Second
)

func newBridge(allowed []string) *bridge {
	b := &bridge{allowed: make(map[string]bool, len(allowed)), bind: listenTCP, listeners: map[int]net.Listener{}}
	for _, target := range allowed {
		b.allowed[target] = true
	}
	return b
}

func (b *bridge) listening(i int) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.listeners[i] != nil
}

// listen opens forward i's listener, at a free loopback port when the
// forward names none, and returns where it answers.
func (b *bridge) listen(i int, forward PortForward) (string, error) {
	address := forward.Listen
	if address == "" {
		address = "127.0.0.1:0"
	}
	listener, err := b.bind(address)
	if err != nil {
		return "", err
	}
	b.mu.Lock()
	b.listeners[i] = listener
	b.mu.Unlock()
	go b.accept(listener, forward.Target)
	return listener.Addr().String(), nil
}

func (b *bridge) accept(listener net.Listener, target string) {
	for {
		connection, err := listener.Accept()
		if err != nil {
			return
		}
		go b.carry(connection, target)
	}
}

// carry sends an accepted connection to target on the other end. With no
// session up the connection is closed at once, which the dialer sees as
// a refused connection rather than a stall.
func (b *bridge) carry(connection net.Conn, target string) {
	b.mu.Lock()
	session := b.session
	b.mu.Unlock()
	if session == nil {
		connection.Close()
		return
	}
	stream, err := session.OpenStream()
	if err != nil {
		connection.Close()
		return
	}
	if _, err := io.WriteString(stream, target+"\n"); err != nil {
		stream.Close()
		connection.Close()
		return
	}
	splice(connection, stream, stream)
}

// attach makes session the one new connections are carried on and starts
// answering its streams.
func (b *bridge) attach(session *yamux.Session) {
	b.mu.Lock()
	b.session = session
	b.mu.Unlock()
	go b.serve(session)
}

func (b *bridge) detach() {
	b.mu.Lock()
	b.session = nil
	b.mu.Unlock()
}

func (b *bridge) serve(session *yamux.Session) {
	for {
		stream, err := session.AcceptStream()
		if err != nil {
			return
		}
		go b.answer(stream)
	}
}

// answer connects a stream from the other end to the target it names.
// A target not on the allowed list, or one that does not answer, closes
// the stream unanswered.
func (b *bridge) answer(stream *yamux.Stream) {
	_ = stream.SetReadDeadline(time.Now().Add(streamHeaderTimeout))
	reader := bufio.NewReader(stream)
	line, err := reader.ReadString('\n')
	target := strings.TrimSpace(line)
	if err != nil || !b.allowed[target] {
		stream.Close()
		return
	}
	_ = stream.SetReadDeadline(time.Time{})
	connection, err := net.DialTimeout("tcp", target, targetDialTimeout)
	if err != nil {
		stream.Close()
		return
	}
	splice(connection, stream, reader)
}

// splice copies both ways between a connection and a stream, half-closing
// each side when the other stops sending, and closes both when done.
// streamReader is where the stream's bytes are read from; it may hold
// bytes read ahead of the stream itself.
func splice(connection net.Conn, stream *yamux.Stream, streamReader io.Reader) {
	var wait sync.WaitGroup
	wait.Add(2)
	go func() {
		defer wait.Done()
		_, _ = io.Copy(stream, connection)
		// A yamux stream's Close only ends this side's sending.
		stream.Close()
	}()
	go func() {
		defer wait.Done()
		_, err := io.Copy(connection, streamReader)
		// The stream ending in error is the link going away, which the
		// connection should see as a broken peer rather than a clean end.
		if tcp, ok := connection.(*net.TCPConn); ok && err == nil {
			_ = tcp.CloseWrite()
		} else {
			connection.Close()
		}
	}()
	wait.Wait()
	connection.Close()
	stream.Close()
}

// close ends every listener; connections in flight end with their
// streams.
func (b *bridge) close() {
	b.mu.Lock()
	defer b.mu.Unlock()
	for i, listener := range b.listeners {
		listener.Close()
		delete(b.listeners, i)
	}
}

// ServeLink is the far end of a link, run on the machine by `steve link`
// over the SSH session the hub opened. It binds every listen address for
// the hub, announces itself on stdout, and then multiplexes the session:
// connections accepted here go to the hub, and the hub's streams go to
// the allowed targets on this machine. It returns when the session ends,
// when the hub stops answering keepalives, or when ctx ends; the
// multiplexer's own messages go to logs.
func ServeLink(ctx context.Context, stdin io.Reader, stdout io.WriteCloser, logs io.Writer, listens []PortForward, allowed []string) error {
	return ServeLinkWith(ctx, stdin, stdout, logs, listens, allowed, listenTCP)
}

// ListenFunc binds a far end's listen address. The listener it returns
// belongs to the far end, which closes it when the session ends.
type ListenFunc func(address string) (net.Listener, error)

func listenTCP(address string) (net.Listener, error) { return net.Listen("tcp", address) }

// ServeLinkWith is ServeLink with bind opening each listen address, for a
// caller that already holds the ports the far end serves on.
func ServeLinkWith(ctx context.Context, stdin io.Reader, stdout io.WriteCloser, logs io.Writer, listens []PortForward, allowed []string, bind ListenFunc) error {
	b := newBridge(allowed)
	b.bind = bind
	defer b.close()
	for i, forward := range listens {
		if forward.Listen == "" {
			return errors.New("每个 --listen 都需要指定监听地址")
		}
		if _, err := b.listen(i, forward); err != nil {
			return fmt.Errorf("监听 %s 失败：%w", forward.Listen, err)
		}
	}
	if _, err := io.WriteString(stdout, linkBanner+"\n"); err != nil {
		return err
	}
	session, err := yamux.Server(stdio{stdin, stdout}, muxConfig(logs))
	if err != nil {
		return err
	}
	b.attach(session)
	select {
	case <-session.CloseChan():
		return nil
	case <-ctx.Done():
		// Closing the session would wait for a read of stdin that only
		// the hub can end; closing stdout tells the hub instead, and the
		// process exiting releases everything else.
		_ = stdout.Close()
		return nil
	}
}
