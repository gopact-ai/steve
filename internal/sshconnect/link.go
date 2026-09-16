package sshconnect

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/hashicorp/yamux"
)

// PortForward is one forwarded port: a connection accepted at Listen is
// carried through the SSH session to Target on the other side.
type PortForward struct {
	Listen string `json:"listen"`
	Target string `json:"target"`
}

// PeerProgram is where a machine's installation leaves the program that
// answers the far end of a link, as the machine's login shell sees it.
const PeerProgram = "$HOME/.steve-peer/bin/steve"

// LinkSpec describes the session a Link keeps open to one machine. The
// cluster protocol rides on it in both directions, so neither machine
// needs a route to the other: only the SSH alias has to work, and the
// only thing asked of the machine's sshd is to run a command.
type LinkSpec struct {
	Alias string
	// Inbound listeners open on the machine's loopback and lead back to
	// this machine. Their ports are chosen before the machine's node is
	// configured, since that node is told where to find them.
	Inbound []PortForward
	// Outbound listeners open on this machine's loopback and lead to the
	// machine. An empty Listen picks a free port when the link opens; the
	// port then stays the same for the life of the link.
	Outbound []PortForward
}

// LinkStatus is what the link knows about its session right now.
type LinkStatus struct {
	Connected bool      `json:"connected"`
	Since     time.Time `json:"since"`
	Attempts  int       `json:"attempts"`
	LastError string    `json:"last_error,omitempty"`
	// Outbound is the spec's outbound list with every Listen filled in.
	Outbound []PortForward `json:"outbound"`
}

// Session is one running SSH session with the far end's program on it:
// its stdin and stdout carry the link.
type Session struct {
	Stdin  io.WriteCloser
	Stdout io.Reader
	// Wait returns when the session ends, with what was written on stderr
	// and its exit error.
	Wait func() (string, error)
	Kill func()
}

// Launcher starts SSH sessions. OpenSSH is the real one; tests supply
// sessions that run the far end in this process.
type Launcher interface {
	Start(context.Context, []string) (*Session, error)
}

type LinkOptions struct {
	Launch Launcher
	// Backoff is how long to wait before attempt n (n >= 1) after the
	// previous session ended; nil means 1 s doubling to 30 s.
	Backoff func(attempt int) time.Duration
	// OnChange is told about every change of status, on the link's own
	// goroutine.
	OnChange func(LinkStatus)
}

// Link keeps one SSH session to a machine open and reopens it whenever it
// ends. It never gives up: the alias may start working again after the
// laptop changes networks, and the machine's node keeps reconnecting on
// its side. The local listeners are this link's own for its whole life,
// so while the session is down a dial fails fast at a closed stream
// rather than at a port something else may have taken.
type Link struct {
	spec    LinkSpec
	options LinkOptions
	bridge  *bridge
	cancel  context.CancelFunc
	done    chan struct{}
	mu      sync.Mutex
	status  LinkStatus
	changed chan struct{}
}

const (
	linkReadyTimeout = 30 * time.Second
	linkBanner       = "STEVE-LINK/1"
)

func defaultLinkBackoff(attempt int) time.Duration {
	wait := time.Second << uint(min(attempt-1, 5))
	return min(wait, 30*time.Second)
}

// OpenLink binds the local ports at once and starts keeping the session
// up. The status's Outbound is complete before OpenLink returns.
func OpenLink(ctx context.Context, spec LinkSpec, options LinkOptions) *Link {
	if options.Launch == nil {
		options.Launch = OpenSSH{}
	}
	if options.Backoff == nil {
		options.Backoff = defaultLinkBackoff
	}
	ctx, cancel := context.WithCancel(ctx)
	link := &Link{spec: spec, options: options, cancel: cancel, done: make(chan struct{}), changed: make(chan struct{})}
	link.bridge = newBridge(targetsOf(spec.Inbound))
	link.status.Outbound = append([]PortForward(nil), spec.Outbound...)
	if err := link.bind(); err != nil {
		link.status.LastError = err.Error()
	}
	go link.run(ctx)
	return link
}

func targetsOf(forwards []PortForward) []string {
	targets := make([]string, len(forwards))
	for i, forward := range forwards {
		targets[i] = forward.Target
	}
	return targets
}

// bind opens every outbound listener not yet open and records where it
// answers. Listeners already open are kept; the caller holds no lock.
func (l *Link) bind() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	for i := range l.status.Outbound {
		if l.bridge.listening(i) {
			continue
		}
		address, err := l.bridge.listen(i, l.status.Outbound[i])
		if err != nil {
			return fmt.Errorf("本机监听 %s 失败：%w", l.status.Outbound[i].Listen, err)
		}
		l.status.Outbound[i].Listen = address
	}
	return nil
}

func (l *Link) Status() LinkStatus {
	l.mu.Lock()
	defer l.mu.Unlock()
	status := l.status
	status.Outbound = append([]PortForward(nil), l.status.Outbound...)
	return status
}

// WaitConnected returns once the session is up, or with why it is not when
// ctx ends first.
func (l *Link) WaitConnected(ctx context.Context) error {
	for {
		l.mu.Lock()
		status, changed := l.status, l.changed
		l.mu.Unlock()
		if status.Connected {
			return nil
		}
		select {
		case <-changed:
		case <-ctx.Done():
			if status.LastError != "" {
				return fmt.Errorf("SSH 隧道未建立：%s", status.LastError)
			}
			return errors.New("SSH 隧道未建立")
		case <-l.done:
			return errors.New("SSH 隧道已关闭")
		}
	}
}

// Close ends the session, releases the local ports and stops reopening.
func (l *Link) Close() {
	l.cancel()
	<-l.done
}

func (l *Link) update(change func(*LinkStatus)) {
	l.mu.Lock()
	change(&l.status)
	status := l.status
	status.Outbound = append([]PortForward(nil), l.status.Outbound...)
	close(l.changed)
	l.changed = make(chan struct{})
	l.mu.Unlock()
	if l.options.OnChange != nil {
		l.options.OnChange(status)
	}
}

func (l *Link) run(ctx context.Context) {
	defer close(l.done)
	defer l.update(func(s *LinkStatus) { s.Connected = false; s.Since = time.Now() })
	defer l.bridge.close()
	for attempt := 1; ; attempt++ {
		l.update(func(s *LinkStatus) { s.Attempts = attempt })
		began := time.Now()
		err := l.bind()
		if err == nil {
			err = l.session(ctx)
		}
		if ctx.Err() != nil {
			return
		}
		l.update(func(s *LinkStatus) {
			s.Connected, s.Since = false, time.Now()
			if err != nil {
				s.LastError = err.Error()
			}
		})
		// A session that held for a while starts the backoff over: this
		// is a fresh outage, not the same one still going on.
		if time.Since(began) > time.Minute {
			attempt = 0
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(l.options.Backoff(max(attempt, 1))):
		}
	}
}

// session runs one SSH session to its end and says why it ended. The
// session is up once the far end has announced itself and answered a
// ping through the multiplexer; from then on it carries streams both
// ways until either side's multiplexer closes or the process ends.
func (l *Link) session(ctx context.Context) error {
	process, err := l.options.Launch.Start(ctx, l.arguments())
	if err != nil {
		return err
	}
	ended := make(chan struct{})
	var stderr string
	var exitErr error
	go func() {
		stderr, exitErr = process.Wait()
		close(ended)
	}()
	stop := context.AfterFunc(ctx, process.Kill)
	defer stop()
	mux, readyErr := l.ready(ctx, process, ended)
	if readyErr == nil {
		l.bridge.attach(mux)
		l.update(func(s *LinkStatus) { s.Connected, s.Since, s.LastError = true, time.Now(), "" })
		select {
		case <-mux.CloseChan():
		case <-ended:
		case <-ctx.Done():
		}
		l.bridge.detach()
		_ = mux.Close()
	}
	process.Kill()
	<-ended
	reason := lastLine(stderr)
	switch {
	case ctx.Err() != nil:
		return ctx.Err()
	case reason != "":
		return errors.New(reason)
	case readyErr != nil:
		return readyErr
	case exitErr != nil:
		return exitErr
	}
	return errors.New("SSH 会话已结束")
}

// ready waits for the far end's banner on the session's stdout, then
// opens the multiplexer over the session and pings through it. Anything
// the machine's shell prints before the banner is skipped.
func (l *Link) ready(ctx context.Context, process *Session, ended <-chan struct{}) (*yamux.Session, error) {
	reader := bufio.NewReaderSize(process.Stdout, 64<<10)
	result := make(chan readyOutcome, 1)
	go func() {
		if err := awaitBanner(reader); err != nil {
			result <- readyOutcome{err: err}
			return
		}
		mux, err := yamux.Client(stdio{reader, process.Stdin}, muxConfig())
		if err != nil {
			result <- readyOutcome{err: err}
			return
		}
		if _, err := mux.Ping(); err != nil {
			_ = mux.Close()
			result <- readyOutcome{err: fmt.Errorf("远端链路程序没有应答：%w", err)}
			return
		}
		result <- readyOutcome{mux: mux}
	}()
	select {
	case got := <-result:
		return got.mux, got.err
	case <-ended:
		go discardLate(result)
		return nil, errors.New("SSH 会话在就绪前已结束")
	case <-ctx.Done():
		go discardLate(result)
		return nil, ctx.Err()
	case <-time.After(linkReadyTimeout):
		process.Kill()
		go discardLate(result)
		return nil, fmt.Errorf("SSH 会话在 %s 内没有就绪", linkReadyTimeout)
	}
}

// discardLate closes a multiplexer that came up after its session was
// given up on.
func discardLate(result <-chan readyOutcome) {
	if got := <-result; got.mux != nil {
		_ = got.mux.Close()
	}
}

type readyOutcome struct {
	mux *yamux.Session
	err error
}

// awaitBanner reads lines until the far end announces itself. The
// reader's buffer bounds how much shell chatter is tolerated before it.
func awaitBanner(reader *bufio.Reader) error {
	read := 0
	for {
		line, err := reader.ReadString('\n')
		if strings.TrimSpace(line) == linkBanner {
			return nil
		}
		if err != nil {
			return fmt.Errorf("远端链路程序没有启动：%w", err)
		}
		if read += len(line); read > reader.Size() {
			return errors.New("远端在链路程序启动前输出过多")
		}
	}
}

func muxConfig() *yamux.Config {
	config := yamux.DefaultConfig()
	config.KeepAliveInterval = 15 * time.Second
	config.ConnectionWriteTimeout = 30 * time.Second
	config.LogOutput = io.Discard
	return config
}

// stdio is a session's two pipes as the one connection yamux wants.
type stdio struct {
	io.Reader
	io.WriteCloser
}

func (s stdio) Close() error { return s.WriteCloser.Close() }

// arguments builds the ssh command line: the same locked-down session
// options the enrollment connection uses, every forwarding cleared, and
// the far end's program as the command.
func (l *Link) arguments() []string {
	return []string{"-T", "-o", "BatchMode=yes", "-o", "StrictHostKeyChecking=yes", "-o", "ConnectTimeout=8", "-o", "ConnectionAttempts=1", "-o", "PermitLocalCommand=no", "-o", "ForwardAgent=no", "-o", "ForwardX11=no", "-o", "RemoteCommand=none", "-o", "ControlMaster=no", "-o", "ControlPath=none", "-o", "UpdateHostKeys=no", "-o", "ClearAllForwardings=yes", "-o", "ServerAliveInterval=15", "-o", "ServerAliveCountMax=3", "--", l.spec.Alias, l.remoteCommand()}
}

// remoteCommand is what the machine's login shell runs: the far end of
// the link, told which ports to listen at for this machine and which of
// its own ports this machine may open.
func (l *Link) remoteCommand() string {
	words := []string{"exec", `"` + PeerProgram + `"`, "link"}
	for _, forward := range l.spec.Inbound {
		words = append(words, "--listen", shellQuote(forward.Listen+"="+forward.Target))
	}
	for _, forward := range l.spec.Outbound {
		words = append(words, "--allow", shellQuote(forward.Target))
	}
	return strings.Join(words, " ")
}

func shellQuote(text string) string { return "'" + strings.ReplaceAll(text, "'", "'\"'\"'") + "'" }

// ParseForward reads a "listen=target" pair of TCP addresses.
func ParseForward(text string) (PortForward, error) {
	listen, target, ok := strings.Cut(text, "=")
	if !ok {
		return PortForward{}, fmt.Errorf("%q 需要写成 listen=target", text)
	}
	forward := PortForward{Listen: listen, Target: target}
	for _, address := range []string{listen, target} {
		if err := checkAddress(address); err != nil {
			return PortForward{}, err
		}
	}
	return forward, nil
}

func checkAddress(address string) error {
	host, port, err := net.SplitHostPort(address)
	if err != nil {
		return fmt.Errorf("%q 需要写成 host:port", address)
	}
	if n, err := strconv.Atoi(port); err != nil || n < 1 || n > 65535 || host == "" {
		return fmt.Errorf("%q 不是可用的 host:port", address)
	}
	return nil
}

func lastLine(output string) string {
	lines := strings.Split(strings.TrimSpace(output), "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		if line := strings.TrimSpace(lines[i]); line != "" {
			return line
		}
	}
	return ""
}

// Start runs the ssh client with the far end's program as its command.
// A proxy helper the alias starts inherits stderr; WaitDelay keeps Wait
// from following it after the client itself was killed.
func (s OpenSSH) Start(_ context.Context, args []string) (*Session, error) {
	command := exec.Command(s.binary(), args...)
	stdin, err := command.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := command.StdoutPipe()
	if err != nil {
		return nil, err
	}
	stderr := &boundedOutput{}
	command.Stderr = stderr
	command.WaitDelay = time.Second
	if err := command.Start(); err != nil {
		return nil, err
	}
	return &Session{Stdin: stdin, Stdout: stdout,
		Wait: func() (string, error) { err := command.Wait(); return stderr.String(), err },
		Kill: func() {
			if command.Process != nil {
				_ = command.Process.Kill()
			}
		}}, nil
}

func (s OpenSSH) binary() string {
	if s.Binary != "" {
		return s.Binary
	}
	return "ssh"
}
