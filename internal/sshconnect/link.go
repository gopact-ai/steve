package sshconnect

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

// PortForward is one forwarded port: a connection accepted at Listen is
// carried through the SSH session to Target on the other side.
type PortForward struct {
	Listen string `json:"listen"`
	Target string `json:"target"`
}

// LinkSpec describes the session a Link keeps open to one machine. The
// cluster protocol rides on it in both directions, so neither machine
// needs a route to the other: only the SSH alias has to work.
type LinkSpec struct {
	Alias string
	// Inbound listeners open on the machine's loopback and lead back to
	// this machine (ssh -R). Their ports are chosen before the machine's
	// node is configured, since that node is told where to find them.
	Inbound []PortForward
	// Outbound listeners open on this machine's loopback and lead to the
	// machine (ssh -L). An empty Listen picks a free port when the link
	// opens; the port then stays the same for the life of the link.
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

// Process is one running SSH session.
type Process interface {
	// Wait returns when the session ends, with what the client wrote on
	// stderr and its exit error.
	Wait() (string, error)
	Kill()
}

// Launcher starts SSH sessions. OpenSSH is the real one; tests supply
// sessions that open the local listeners without a network.
type Launcher interface {
	Start(context.Context, []string) (Process, error)
}

// A Reaper ends sessions a previous process of this program left behind:
// they hold the remote ports a new session needs, so it could never come
// up. Launchers that leave nothing behind need not implement it.
type Reaper interface {
	Reap(inbound []PortForward)
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
// its side.
type Link struct {
	spec    LinkSpec
	options LinkOptions
	cancel  context.CancelFunc
	done    chan struct{}
	mu      sync.Mutex
	status  LinkStatus
	changed chan struct{}
}

const linkReadyTimeout = 30 * time.Second

func defaultLinkBackoff(attempt int) time.Duration {
	wait := time.Second << uint(min(attempt-1, 5))
	return min(wait, 30*time.Second)
}

// OpenLink chooses the local ports at once and starts keeping the session
// up. The status's Outbound is complete before OpenLink returns; a port
// taken by something else while the session is down is replaced, and the
// change reported, before the next attempt.
func OpenLink(ctx context.Context, spec LinkSpec, options LinkOptions) *Link {
	if options.Launch == nil {
		options.Launch = OpenSSH{}
	}
	if options.Backoff == nil {
		options.Backoff = defaultLinkBackoff
	}
	ctx, cancel := context.WithCancel(ctx)
	link := &Link{spec: spec, options: options, cancel: cancel, done: make(chan struct{}), changed: make(chan struct{})}
	link.status.Outbound = chooseListeners(spec.Outbound)
	go link.run(ctx)
	return link
}

// chooseListeners fills in outbound listen addresses: a free loopback port
// for each that names none. The port is released again at once; the ssh
// client binds it when the session comes up.
func chooseListeners(outbound []PortForward) []PortForward {
	chosen := make([]PortForward, len(outbound))
	for i, forward := range outbound {
		chosen[i] = forward
		if forward.Listen == "" {
			chosen[i].Listen = freeLoopbackPort()
		}
	}
	return chosen
}

func freeLoopbackPort() string {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return "127.0.0.1:0"
	}
	defer listener.Close()
	return listener.Addr().String()
}

// refreshListeners replaces every chosen outbound port that something else
// has bound meanwhile: the ssh client could not bind it either, and with
// ExitOnForwardFailure the session would end at once, every time. Ports
// the spec names are kept as they are.
func (l *Link) refreshListeners() {
	l.mu.Lock()
	outbound := append([]PortForward(nil), l.status.Outbound...)
	l.mu.Unlock()
	changed := false
	for i, forward := range outbound {
		if l.spec.Outbound[i].Listen != "" {
			continue
		}
		listener, err := net.Listen("tcp", forward.Listen)
		if err == nil {
			listener.Close()
			continue
		}
		outbound[i].Listen = freeLoopbackPort()
		changed = true
	}
	if changed {
		l.update(func(s *LinkStatus) { s.Outbound = outbound })
	}
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

// Close ends the session and stops reopening it.
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
	if reaper, ok := l.options.Launch.(Reaper); ok {
		reaper.Reap(l.spec.Inbound)
	}
	for attempt := 1; ; attempt++ {
		l.refreshListeners()
		l.update(func(s *LinkStatus) { s.Attempts = attempt })
		began := time.Now()
		err := l.session(ctx)
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
// session counts as up once this machine's first outbound listener
// accepts, which the ssh client only does after it has authenticated. The
// remote forwards are confirmed shortly after; one that fails ends the
// session (ExitOnForwardFailure), so the status can go up and straight
// down again, and the reason is the session's last stderr line.
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
	ready := l.readiness(ctx, ended)
	if ready {
		l.update(func(s *LinkStatus) { s.Connected, s.Since, s.LastError = true, time.Now(), "" })
	} else {
		process.Kill()
	}
	<-ended
	reason := lastLine(stderr)
	switch {
	case ctx.Err() != nil:
		return ctx.Err()
	case !ready && reason == "":
		return fmt.Errorf("SSH 会话在 %s 内没有就绪", linkReadyTimeout)
	case reason != "":
		return errors.New(reason)
	case exitErr != nil:
		return exitErr
	}
	return errors.New("SSH 会话已结束")
}

func (l *Link) readiness(ctx context.Context, ended <-chan struct{}) bool {
	if len(l.status.Outbound) == 0 {
		// Nothing local to observe; the session is up while the process is.
		select {
		case <-ended:
			return false
		case <-time.After(2 * time.Second):
			return true
		case <-ctx.Done():
			return false
		}
	}
	probe := l.Status().Outbound[0].Listen
	deadline := time.Now().Add(linkReadyTimeout)
	for time.Now().Before(deadline) {
		select {
		case <-ended:
			return false
		case <-ctx.Done():
			return false
		case <-time.After(100 * time.Millisecond):
		}
		connection, err := net.DialTimeout("tcp", probe, time.Second)
		if err == nil {
			connection.Close()
			return true
		}
	}
	return false
}

// arguments builds the ssh command line: the same locked-down session
// options the enrollment connection uses, no command, and the forwards.
// Forwardings from the user's configuration are not cleared, since that
// option would clear these too; a configured forward that cannot bind ends
// the session, and the reason is reported.
func (l *Link) arguments() []string {
	args := []string{"-N", "-T", "-o", "BatchMode=yes", "-o", "StrictHostKeyChecking=yes", "-o", "ConnectTimeout=8", "-o", "ConnectionAttempts=1", "-o", "PermitLocalCommand=no", "-o", "ForwardAgent=no", "-o", "ForwardX11=no", "-o", "RemoteCommand=none", "-o", "ControlMaster=no", "-o", "ControlPath=none", "-o", "UpdateHostKeys=no", "-o", "ExitOnForwardFailure=yes", "-o", "ServerAliveInterval=15", "-o", "ServerAliveCountMax=3"}
	for _, forward := range l.spec.Inbound {
		args = append(args, "-R", inboundArgument(forward))
	}
	for _, forward := range l.Status().Outbound {
		args = append(args, "-L", forward.Listen+":"+forward.Target)
	}
	return append(args, "--", l.spec.Alias)
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

func inboundArgument(forward PortForward) string { return forward.Listen + ":" + forward.Target }

// Start runs the ssh client as a long-lived session. A proxy helper the
// alias starts inherits stderr; WaitDelay keeps Wait from following it
// after the client itself was killed.
func (s OpenSSH) Start(_ context.Context, args []string) (Process, error) {
	command := exec.Command(s.binary(), args...)
	stderr := &boundedOutput{}
	command.Stderr = stderr
	command.Stdin = nil
	command.WaitDelay = time.Second
	if err := command.Start(); err != nil {
		return nil, err
	}
	return &sshProcess{command: command, stderr: stderr}, nil
}

func (s OpenSSH) binary() string {
	if s.Binary != "" {
		return s.Binary
	}
	return "ssh"
}

// Reap ends ssh clients of a previous process of this program that still
// hold this link's remote forwards: a crash leaves them running, and the
// machine's sshd would refuse the same ports to the new session. A process
// is one of ours when its command line carries every one of the link's -R
// arguments, which no other use of ssh shares.
func (s OpenSSH) Reap(inbound []PortForward) {
	if len(inbound) == 0 {
		return
	}
	for _, pid := range strayProcesses(s.binary(), inbound) {
		if process, err := os.FindProcess(pid); err == nil {
			_ = process.Signal(syscall.SIGTERM)
		}
	}
}

// strayProcesses lists other processes running binary with every forward
// on their command line, from ps, which macOS and Linux both have.
func strayProcesses(binary string, inbound []PortForward) []int {
	output, err := exec.Command("ps", "-axo", "pid=,args=").Output()
	if err != nil {
		return nil
	}
	var pids []int
	for _, line := range strings.Split(string(output), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		pid, err := strconv.Atoi(fields[0])
		if err != nil || pid == os.Getpid() {
			continue
		}
		command := strings.Join(fields[1:], " ")
		if !strings.HasSuffix(fields[1], binary) {
			continue
		}
		stray := true
		for _, forward := range inbound {
			if !strings.Contains(command, " -R "+inboundArgument(forward)+" ") {
				stray = false
				break
			}
		}
		if stray {
			pids = append(pids, pid)
		}
	}
	return pids
}

type sshProcess struct {
	command *exec.Cmd
	stderr  *boundedOutput
}

func (p *sshProcess) Wait() (string, error) {
	err := p.command.Wait()
	return p.stderr.String(), err
}

func (p *sshProcess) Kill() {
	if p.command.Process != nil {
		_ = p.command.Process.Kill()
	}
}
