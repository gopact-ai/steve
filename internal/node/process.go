package node

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os/exec"
	"sync"
	"time"

	"github.com/gopact-ai/steve/internal/acphost"
	"github.com/gopact-ai/steve/internal/node/journal"
	"github.com/gopact-ai/steve/internal/nodewire"
	steveruntime "github.com/gopact-ai/steve/internal/runtime"
)

const SessionGrace = 10 * time.Minute
const liveBufferBytes = 16 << 20

type agentProcess struct {
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
	exitReady   chan struct{}
	ended       time.Time
	released    bool
	grace       *time.Timer
	graceEpoch  uint64
	killOnce    sync.Once
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
	if req.Stream != "" && !journal.ValidID(req.Stream) {
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
	if p != nil && req.Stream != "" {
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
	proc, err := (acphost.LocalTransport{
		Command: spec.Command, Args: spec.Args,
		ProcessDir: s.processDir(spec), Env: steveruntime.ApplyEnv(spec.Env, req.Harness, s.conf().StateDir),
	}).Start(ctx)
	if err != nil {
		s.processMu.Unlock()
		closeStream(stream, err.Error())
		return
	}
	s.hubMu.Lock()
	owner := s.hubName
	s.hubMu.Unlock()
	p = &agentProcess{server: s, id: req.Stream, harness: req.Harness, owner: owner, proc: proc}
	if req.Stream != "" {
		p.journal, err = journal.New(s.conf().StateDir, req.Stream, journal.Options{})
		if err != nil {
			slog.Warn(fmt.Sprintf("steve-node: stream %s not resumable: %v", req.Stream, err), "stream", req.Stream)
		}
	}
	// Publish only after the initial attachment exists, so an immediate
	// reconnect cannot be superseded later by the original open.
	a, err := p.replace(stream, req)
	// Legacy streams still need shutdown bookkeeping, but can never resume.
	key := req.Stream
	if key == "" {
		key = fmt.Sprintf("legacy-%p", p)
	}
	s.processes[key] = p
	s.processWG.Add(1)
	s.processMu.Unlock()
	slog.Info(fmt.Sprintf("steve-node: process stream %s started on %s", key, req.Harness), "stream", key, "harness", req.Harness)
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

func (p *agentProcess) kill() { p.killOnce.Do(func() { p.proc.Kill() }) }

func (p *agentProcess) replace(stream *nodewire.Stream, req nodewire.OpenRequest) (*attachment, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if req.Harness != p.harness {
		return nil, errors.New("harness does not match process stream")
	}
	if p.released && p.exit == "" {
		return nil, errors.New("process stream released")
	}
	if req.Resume {
		if p.journal == nil {
			return nil, journal.ErrUnresumable
		}
		if err := p.journal.Err(); err != nil {
			return nil, err
		}
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
				exit := p.exit
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
	kill := p.id == "" || errors.Is(a.stream.Err(), io.EOF)
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
	if p.id == "" {
		_, _ = io.Copy(p.proc.Stdin(), a.stream)
		return
	}
	_ = readLines(a.stream, func(line []byte, complete bool) error {
		p.inputMu.Lock()
		p.mu.Lock()
		if p.attached != a || p.exit != "" {
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
		if err == nil && complete && p.id != "" {
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
	// Output ends with the process; Wait below is where its fate is read,
	// so the pump's own error adds nothing.
	if p.id == "" {
		// Old hubs do not promise newline framing or reconnect support.
		_, _ = io.Copy(writerFunc(func(b []byte) (int, error) {
			p.emit(outputLine{data: append([]byte(nil), b...)}, false)
			return len(b), nil
		}), p.proc.Stdout())
	} else {
		_ = readLines(p.proc.Stdout(), func(b []byte, complete bool) error { p.emit(outputLine{data: b}, complete); return nil })
	}
	err := p.proc.Wait()
	p.kill()
	code := 0
	if err != nil {
		code = -1
		if e, ok := err.(*exec.ExitError); ok {
			code = e.ExitCode()
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

type writerFunc func([]byte) (int, error)

func (f writerFunc) Write(b []byte) (int, error) { return f(b) }

// readLines discards a trailing partial line on loss. An oversized line is
// streamed in bounded fragments, but makes the process non-resumable.
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
