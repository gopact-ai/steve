package node

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"sync"
	"time"

	"github.com/gopact-ai/steve/internal/acphost"
	"github.com/gopact-ai/steve/internal/node/journal"
	"github.com/gopact-ai/steve/internal/nodewire"
	steveruntime "github.com/gopact-ai/steve/internal/runtime"
)

const SessionGrace = 10 * time.Minute
const liveBufferBytes = 16 << 20

// exitGrace is how long an agent's process group is given to stop once
// the agent has exited, before its attachment is told the agent is gone
// with unsettledReason: as long as a host gives a closed agent to leave
// before it kills it, and as the transport waits before it first reports
// a group that does not stop.
const exitGrace = 5 * time.Second

// unsettledReason closes the attachment of an agent that has exited while
// its process group has not stopped. It is not an exit, so a hub takes the
// agent for gone without taking it for stopped.
const unsettledReason = "agent exited; its process group has not stopped"

type agentProcess struct {
	pluginRuntimeID    string
	server             *Server
	id, harness, owner string
	proc               acphost.Process
	journal            *journal.Journal
	mu                 sync.Mutex
	// inputMu preserves stdin order across attachments, including a line
	// accepted by an old reader whose write to a busy process is pending.
	inputMu     sync.Mutex
	attached    *attachment
	haveIn, out uint64
	exit        string
	// unsettled is unsettledReason once the agent has exited and its
	// process group has not stopped within exitGrace. The exit is recorded
	// only once the group has stopped.
	unsettled  string
	exitReady  chan struct{}
	ended      time.Time
	released   bool
	grace      *time.Timer
	graceEpoch uint64
	killOnce   sync.Once
}

type outputLine struct {
	data []byte
	exit string
}
type attachment struct {
	stream    *nodewire.Stream
	replaying bool
	live      bytes.Buffer
	available chan struct{}
	exit      string
	haveIn    uint64
	failed    bool
}

func (s *Server) sessionGrace() time.Duration {
	if d := s.conf().SessionGrace; d > 0 {
		return d
	}
	return SessionGrace
}

func (s *Server) runAgent(ctx context.Context, stream *nodewire.Stream) {
	req := stream.Request()
	if req.Kind != nodewire.StreamACP {
		closeStream(stream, "unknown stream kind")
		return
	}
	if !journal.ValidID(req.Stream) {
		closeStream(stream, "invalid stream id")
		return
	}
	s.processMu.Lock()
	p := s.processes[req.Stream]
	if req.Resume {
		s.processMu.Unlock()
		if p == nil {
			closeStream(stream, "unknown process stream")
			return
		}
		p.attach(stream, req)
		return
	}
	if p != nil {
		s.processMu.Unlock()
		closeStream(stream, "process stream already exists")
		return
	}
	spec, ok := s.conf().Harnesses[req.Harness]
	if !ok {
		s.processMu.Unlock()
		closeStream(stream, "unknown harness")
		return
	}
	transport := acphost.LocalTransport{Command: spec.Command, Args: spec.Args, ProcessDir: s.processDir(spec), Env: steveruntime.ApplyEnv(spec.Env, req.Harness, s.conf().StateDir)}
	if req.Plugin != nil {
		prepared, err := s.pluginProcessConfig(ctx, req)
		if err != nil {
			s.processMu.Unlock()
			closeStream(stream, err.Error())
			return
		}
		transport = prepared
	}
	if req.Plugin != nil {
		if err := s.pluginStore().BeginRuntimeUse(ctx, *req.Plugin, "stream/"+req.Stream, "stream"); err != nil {
			s.processMu.Unlock()
			closeStream(stream, err.Error())
			return
		}
	}
	proc, err := s.startAgent(ctx, transport)
	if err != nil {
		if req.Plugin != nil {
			err = errors.Join(err, s.pluginStore().EndRuntimeUse(context.WithoutCancel(ctx), *req.Plugin, "stream/"+req.Stream))
		}
		s.processMu.Unlock()
		closeStream(stream, err.Error())
		return
	}
	s.hubMu.Lock()
	owner := s.hubName
	s.hubMu.Unlock()
	p = &agentProcess{server: s, id: req.Stream, harness: req.Harness, owner: owner, proc: proc}
	if req.Plugin != nil {
		p.pluginRuntimeID = req.Plugin.ID
	}
	p.journal, err = journal.New(s.conf().StateDir, req.Stream, journal.Options{})
	if err != nil {
		// Without a journal the process still serves this attachment; it
		// only cannot be resumed. resumeErr reports it unresumable, so a
		// lost link ends it at once and the hub's resume is refused: the
		// same end as a journal that fails mid-run, which is why the open
		// is not refused here.
		slog.Warn(fmt.Sprintf("steve-node: stream %s not resumable: %v", req.Stream, err), "stream", req.Stream)
	}
	// Publish only after the initial attachment exists, so an immediate
	// reconnect cannot be superseded later by the original open.
	a, err := p.replace(stream, req)
	s.processes[req.Stream] = p
	s.processWG.Add(1)
	s.processMu.Unlock()
	slog.Info(fmt.Sprintf("steve-node: process stream %s started on %s", req.Stream, req.Harness), "stream", req.Stream, "harness", req.Harness)
	// Start draining only after publication; an immediately exiting process
	// must still deliver its last lines and close reason to its attachment.
	if err != nil {
		p.kill()
		closeStream(stream, err.Error())
	}
	go p.run(ctx)
	if err == nil {
		p.serveAttachment(a, req)
	}
}

// startAgent starts the agent of a process stream on transport.
func (s *Server) startAgent(ctx context.Context, transport acphost.LocalTransport) (acphost.Process, error) {
	if start := s.conf().startAgent; start != nil {
		return start(ctx, transport)
	}
	return transport.Start(ctx)
}

func (p *agentProcess) kill() { p.killOnce.Do(func() { p.proc.Kill() }) }

func (p *agentProcess) replace(stream *nodewire.Stream, req nodewire.OpenRequest) (*attachment, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if req.Harness != p.harness {
		return nil, errors.New("harness does not match process stream")
	}
	if req.Resume {
		// An unresumable stream says so even after its lost link released
		// it; that is the cause the hub needs to report.
		if err := p.resumeErr(); err != nil {
			return nil, err
		}
	}
	if p.released && p.exit == "" {
		return nil, errors.New("process stream released")
	}
	if req.Resume {
		if req.AfterIn < p.haveIn {
			return nil, errors.New("input cursor behind accepted input")
		}
		// Validate the output cursor before superseding a working attachment.
		if err := p.journal.Out.ReplayUntil(req.AfterOut, req.AfterOut, io.Discard); err != nil {
			return nil, err
		}
	}
	if p.grace != nil {
		p.grace.Stop()
		p.grace = nil
	}
	p.graceEpoch++
	old := p.attached
	a := &attachment{stream: stream, replaying: req.Resume, available: make(chan struct{}, 1), haveIn: p.haveIn}
	p.attached = a
	if old != nil {
		go func() { closeStream(old.stream, "superseded") }()
	}
	return a, nil
}

// resumeErr says why no attachment can resume this process, or nil.
// The caller holds p.mu.
func (p *agentProcess) resumeErr() error {
	if p.journal == nil {
		return journal.ErrUnresumable
	}
	return p.journal.Err()
}

func (p *agentProcess) attach(stream *nodewire.Stream, req nodewire.OpenRequest) {
	a, err := p.replace(stream, req)
	if err != nil {
		closeStream(stream, err.Error())
		return
	}
	p.serveAttachment(a, req)
}

func (p *agentProcess) serveAttachment(a *attachment, req nodewire.OpenRequest) {
	defer p.detach(a)
	// Detect loss independently of the input pipe: a busy harness can stop
	// reading stdin without preventing its reconnect grace from starting.
	go func() { <-a.stream.Done(); p.detach(a) }()
	if req.Resume {
		if err := (nodewire.ResumeAck{HaveIn: a.haveIn}).Write(a.stream); err != nil {
			return
		}
	}
	go p.readInput(a)
	if req.Resume {
		after := req.AfterOut
		for {
			p.mu.Lock()
			if p.attached != a {
				p.mu.Unlock()
				return
			}
			through := p.out
			p.mu.Unlock()
			writer := replayWriter{stream: a.stream}
			if err := p.journal.Out.ReplayUntil(after, through, &writer); err != nil {
				closeStream(a.stream, err.Error())
				return
			}
			after = through
			if writer.exit != "" {
				closeStream(a.stream, writer.exit)
				return
			}
			p.mu.Lock()
			if after == p.out {
				// This lock also guards emit. Every later output enters live;
				// every earlier output was in the replay snapshot just drained.
				a.replaying = false
				exit := p.closedWith()
				p.mu.Unlock()
				if exit != "" {
					closeStream(a.stream, exit)
					return
				}
				break
			}
			p.mu.Unlock()
		}
		slog.Info(fmt.Sprintf("steve-node: reattached stream %s, replayed %d lines", p.id, after-req.AfterOut), "stream", p.id)
	}
	for {
		p.mu.Lock()
		if a.live.Len() > 0 {
			data := make([]byte, min(a.live.Len(), nodewire.MaxPayload))
			// Reading a bytes.Buffer within its length cannot fail.
			_, _ = a.live.Read(data)
			p.mu.Unlock()
			if _, err := a.stream.Write(data); err != nil {
				return
			}
			continue
		}
		exit := a.exit
		p.mu.Unlock()
		if exit != "" {
			closeStream(a.stream, exit)
			return
		}
		select {
		case <-a.available:
		case <-a.stream.Done():
			return
		}
	}
}

type replayWriter struct {
	stream *nodewire.Stream
	exit   string
}

func (w *replayWriter) Write(line []byte) (int, error) {
	if reason, ok := journal.IsExit(line); ok {
		w.exit = reason
		return len(line), nil
	}
	return w.stream.Write(line)
}

func (p *agentProcess) detach(a *attachment) {
	p.mu.Lock()
	if p.attached != a {
		p.mu.Unlock()
		return
	}
	p.attached = nil
	// A clean close ends the process, and so does any loss when no later
	// attachment could resume it: the grace period only waits for one.
	kill := errors.Is(a.stream.Err(), io.EOF) || p.resumeErr() != nil
	if p.exit == "" {
		if kill {
			p.released = true
		} else {
			p.graceEpoch++
			epoch := p.graceEpoch
			p.grace = time.AfterFunc(p.server.sessionGrace(), func() {
				p.mu.Lock()
				// Stop cannot retract an expiry callback already waiting on
				// the lock. It must belong to this detachment, not an old one.
				kill := p.graceEpoch == epoch && p.attached == nil && p.exit == ""
				if kill {
					p.released = true
				}
				p.mu.Unlock()
				if kill {
					p.kill()
				}
			})
		}
	}
	p.mu.Unlock()
	if kill {
		p.kill()
	}
}

func (p *agentProcess) readInput(a *attachment) {
	// Input ends when the attachment or the process goes; detach handles
	// the first and the process waiter the second, so the pump's own
	// error adds nothing.
	_ = readLines(a.stream, func(line []byte, complete bool) error {
		p.inputMu.Lock()
		p.mu.Lock()
		if p.attached != a || p.closedWith() != "" {
			p.mu.Unlock()
			p.inputMu.Unlock()
			return io.EOF
		}
		if p.journal != nil {
			if !complete {
				p.journal.Disable(journal.ErrLineTooLong)
			} else {
				// The journal latches its own failure and marks the
				// stream unresumable; input still reaches the process.
				_, _ = p.journal.In.Append(line)
			}
		}
		if complete {
			p.haveIn++
		}
		haveIn := p.haveIn
		p.mu.Unlock()
		_, err := p.proc.Stdin().Write(line)
		p.inputMu.Unlock()
		if err == nil && complete {
			// An ack that cannot be sent means the attachment is going,
			// which its Done channel already reports.
			_ = a.stream.AckInput(haveIn)
		}
		return err
	})
}

func (p *agentProcess) emit(line outputLine, complete bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if line.exit == "" && p.journal != nil {
		if !complete {
			p.journal.Disable(journal.ErrLineTooLong)
		} else {
			// The journal latches its own failure; live output goes on.
			_, _ = p.journal.Out.Append(line.data)
		}
	}
	if complete {
		p.out++
	}
	a := p.attached
	if a == nil || a.replaying || a.failed {
		return
	}
	if a.live.Len()+len(line.data) <= liveBufferBytes {
		// Writing to a bytes.Buffer cannot fail.
		_, _ = a.live.Write(line.data)
		if line.exit != "" {
			a.exit = line.exit
		}
		select {
		case a.available <- struct{}{}:
		default:
		}
		return
	}
	// The log remains available. Never let a slow reader stall stdout or
	// another process on the same mux.
	a.failed = true
	go func() { closeStream(a.stream, "slow consumer") }()
}

func (p *agentProcess) run(ctx context.Context) {
	defer p.server.processWG.Done()
	done := make(chan struct{})
	defer close(done)
	go func() {
		select {
		case <-ctx.Done():
			p.kill()
		case <-done:
		}
	}()
	read := make(chan struct{})
	go p.closeHeldOutput(read)
	// Output ends with the process; Wait below is where its fate is read,
	// so the pump's own error adds nothing.
	_ = readLines(p.proc.Stdout(), func(b []byte, complete bool) error { p.emit(outputLine{data: b}, complete); return nil })
	close(read)
	waited, err := p.await(ctx)
	if !waited {
		return
	}
	p.kill()
	code := 0
	if err != nil {
		code = -1
		// A local agent's end tells its exit code as exec.ExitError
		// does, whichever way it was reaped.
		var e interface{ ExitCode() int }
		if errors.As(err, &e) {
			code = e.ExitCode()
		}
	}
	if p.pluginRuntimeID != "" && p.proc.Stopped() {
		if info, err := p.server.pluginStore().RuntimeInfo(p.pluginRuntimeID); err == nil {
			if err := p.server.pluginStore().EndRuntimeUse(context.WithoutCancel(ctx), info.Ref, "stream/"+p.id); err != nil {
				slog.Error("steve-node: plugin exit receipt failed", "error", err)
			}
		}
		if err := p.server.pluginRuntimePool().Drop(p.pluginRuntimeID); err != nil {
			slog.Error("steve-node: plugin broker stop failed", "error", err)
		}
	}
	p.mu.Lock()
	p.exit, p.ended = fmt.Sprintf("exit %d", code), time.Now()
	if p.grace != nil {
		p.grace.Stop()
	}
	if p.journal != nil {
		if err := p.journal.Finish(code); err != nil {
			slog.Error(fmt.Sprintf("steve-node: stream %s: record exit: %v", p.id, err), "stream", p.id)
		}
		// The exit is recorded above. Close does the last sync, and a
		// fault there is latched by the journal itself, which marks the
		// stream unresumable.
		_ = p.journal.Close()
	}
	if p.exitReady != nil {
		close(p.exitReady)
	}
	p.mu.Unlock()
	p.emit(outputLine{exit: fmt.Sprintf("exit %d", code)}, true)
	slog.Info(fmt.Sprintf("steve-node: stream %s ended: exit %d", p.id, code), "stream", p.id)
}

// closeHeldOutput closes the agent's output if it is still open
// acphost.ExitedOutputWait after the agent has exited, held by something
// the agent left outside its process group, so the stream ends with the
// agent. read is closed once the output has been read to its end.
func (p *agentProcess) closeHeldOutput(read <-chan struct{}) {
	select {
	case <-p.proc.Exited():
	case <-read:
		return
	}
	held := time.NewTimer(acphost.ExitedOutputWait)
	defer held.Stop()
	select {
	case <-held.C:
		slog.Warn(fmt.Sprintf("steve-node: stream %s: the agent exited and its output is still open after %s; the output is closed", p.id, acphost.ExitedOutputWait), "stream", p.id)
		_ = p.proc.Stdout().Close()
	case <-read:
	}
}

// await reports true with what Wait returns, once the agent has exited
// and what it left in its process group has stopped. A group that has not
// stopped within exitGrace of the agent's exit gets the attachment told
// the agent is gone; a node that stops then waits no longer, and await
// reports false with no exit recorded, so the stop stays unconfirmed.
// Wait's goroutine lives on until the group has stopped.
func (p *agentProcess) await(ctx context.Context) (bool, error) {
	waited := make(chan error, 1)
	go func() { waited <- p.proc.Wait() }()
	select {
	case err := <-waited:
		return true, err
	case <-p.proc.Exited():
	}
	grace := time.NewTimer(exitGrace)
	defer grace.Stop()
	select {
	case err := <-waited:
		return true, err
	case <-grace.C:
	}
	p.unsettle()
	select {
	case err := <-waited:
		return true, err
	case <-ctx.Done():
	}
	p.mu.Lock()
	if p.grace != nil {
		p.grace.Stop()
	}
	if p.journal != nil {
		// No exit is recorded, so the stream stays unended; Close does the
		// last sync, and a fault there is latched by the journal itself.
		_ = p.journal.Close()
	}
	p.mu.Unlock()
	slog.Error(fmt.Sprintf("steve-node: stream %s: stopping while the agent's process group has not stopped; its stop stays unconfirmed", p.id), "stream", p.id)
	return false, nil
}

// unsettle tells the attachment the agent is gone while its process group
// has not stopped. Nothing goes to the output log: the exit is recorded,
// and a release answered, only once the group has stopped.
func (p *agentProcess) unsettle() {
	slog.Error(fmt.Sprintf("steve-node: stream %s: the agent exited and its process group has not stopped after %s; the hub is told the agent is gone, and its stop stays unconfirmed until the group stops", p.id, exitGrace), "stream", p.id)
	p.mu.Lock()
	defer p.mu.Unlock()
	p.unsettled = unsettledReason
	a := p.attached
	if a == nil || a.replaying || a.failed {
		return
	}
	a.exit = p.unsettled
	select {
	case a.available <- struct{}{}:
	default:
	}
}

// closedWith is why an attachment is closed once the agent is over: its
// exit, or, while its process group has not stopped, unsettledReason. The
// caller holds p.mu.
func (p *agentProcess) closedWith() string {
	if p.exit != "" {
		return p.exit
	}
	return p.unsettled
}

// readLines discards a trailing partial line on loss. An oversized line is
// streamed in bounded fragments, but makes the process non-resumable.
// It returns only when reading or consuming fails, so its error is never
// nil.
func readLines(src io.Reader, consume func([]byte, bool) error) error {
	r := bufio.NewReaderSize(src, 64<<10)
	var line []byte
	oversized := false
	for {
		part, err := r.ReadSlice('\n')
		if err != nil && err != bufio.ErrBufferFull {
			return err
		}
		line = append(line, part...)
		if len(line) > journal.MaxLine {
			oversized = true
		}
		complete := err == nil
		if complete || oversized {
			if err := consume(line, complete); err != nil {
				return err
			}
			line = nil
		}
		if complete {
			oversized = false
		}
	}
}

func (s *Server) stopProcesses(owner string) {
	s.processMu.Lock()
	defer s.processMu.Unlock()
	for _, p := range s.processes {
		if owner != "" && p.owner != owner {
			continue
		}
		p.mu.Lock()
		p.released = true
		if p.grace != nil {
			p.grace.Stop()
		}
		p.mu.Unlock()
		p.kill()
	}
}

func (s *Server) releaseProcess(stream *nodewire.Stream) {
	s.processMu.Lock()
	p := s.processes[stream.Request().Stream]
	s.processMu.Unlock()
	if p == nil {
		closeStream(stream, "unknown process stream; exit is unconfirmed")
		return
	}
	p.mu.Lock()
	p.released = true
	if p.exit != "" {
		p.mu.Unlock()
		closeStream(stream, "exit 0")
		return
	}
	if p.exitReady == nil {
		p.exitReady = make(chan struct{})
	}
	ended := p.exitReady
	p.mu.Unlock()
	p.kill()
	var stopping <-chan struct{}
	if s.ctx != nil {
		stopping = s.ctx.Done()
	}
	// Kill returning confirms only delivery of a termination request. The
	// successful release receipt is physical exit evidence for the hub, so
	// it follows the sole process waiter regardless of the child's exit code.
	select {
	case <-ended:
		closeStream(stream, "exit 0")
	case <-stream.Done():
		return
	case <-stopping:
		closeStream(stream, "process exit is unconfirmed: node is stopping")
	}
}

func (s *Server) pruneStreams(ctx context.Context) {
	ticker := time.NewTicker(time.Minute)
	defer ticker.Stop()
	for {
		if err := journal.Prune(s.conf().StateDir, time.Now()); err != nil {
			slog.Error(fmt.Sprintf("steve-node: prune stream journals: %v", err))
		}
		s.processMu.Lock()
		for id, p := range s.processes {
			p.mu.Lock()
			expired := !p.ended.IsZero() && time.Since(p.ended) >= journal.Retention
			p.mu.Unlock()
			if expired {
				delete(s.processes, id)
			}
		}
		s.processMu.Unlock()
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func (s *Server) injectDrop(ctx context.Context, mux *nodewire.Mux) {
	if d := s.conf().FaultDropAfter; d > 0 {
		s.faultOnce.Do(func() {
			go func() {
				timer := time.NewTimer(d)
				defer timer.Stop()
				select {
				case <-ctx.Done():
				case <-timer.C:
					slog.Warn("steve-node: fault injection: drop hub once")
					// The drop is the point; its close error is not.
					_ = mux.Close()
				}
			}()
		})
	}
}
