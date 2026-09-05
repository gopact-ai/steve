package node

import (
	"bufio"
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gopact-ai/steve/internal/node/journal"
	"github.com/gopact-ai/steve/internal/nodewire"
)

type memoryNode struct {
	s                    *Server
	ctx                  context.Context
	muxes                []*nodewire.Mux
	wg                   sync.WaitGroup
	dropAcks             bool
	dropData             int
	pauseData            int
	paused, continueData chan struct{}
}

func newMemoryNode(t *testing.T, command string, args ...string) *memoryNode {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	m := &memoryNode{ctx: ctx, s: NewServer(ServerConfig{StateDir: t.TempDir(), WorkspaceRoot: t.TempDir(), SessionGrace: time.Second,
		Harnesses: map[string]HarnessSpec{"cat": {Command: command, Args: args}}})}
	t.Cleanup(func() {
		cancel()
		for _, mux := range m.muxes {
			_ = mux.Close()
		}
		m.s.stopProcesses("")
		m.wg.Wait()
		m.s.processWG.Wait()
	})
	return m
}

func (m *memoryNode) connection(t *testing.T) *nodewire.Mux {
	t.Helper()
	a, b := net.Pipe()
	var nodeSocket io.ReadWriteCloser = b
	if m.dropAcks || m.dropData > 0 || m.pauseData > 0 {
		nodeSocket = &ackDroppingConn{Conn: b, dropAcks: m.dropAcks, dropData: m.dropData, pauseData: m.pauseData, paused: m.paused, continueData: m.continueData, ctx: m.ctx}
	}
	hub, node := nodewire.NewMux(a, true), nodewire.NewMux(nodeSocket, false)
	m.muxes = append(m.muxes, hub, node)
	m.wg.Add(1)
	go func() {
		defer m.wg.Done()
		for {
			s, err := node.Accept(m.ctx)
			if err != nil {
				return
			}
			m.wg.Add(1)
			go func() {
				defer m.wg.Done()
				if s.Request().Kind == nodewire.StreamRelease {
					m.s.releaseAttempt(s)
				} else {
					m.s.runAgent(m.ctx, s)
				}
			}()
		}
	}()
	return hub
}

func openProcess(t *testing.T, mux *nodewire.Mux, req nodewire.OpenRequest) (*nodewire.Stream, *bufio.Reader) {
	t.Helper()
	req.Kind, req.Harness = nodewire.StreamACP, "cat"
	s, err := mux.Open(req)
	if err != nil {
		t.Fatal(err)
	}
	return s, bufio.NewReader(s)
}

func expectLine(t *testing.T, r *bufio.Reader, want string) {
	t.Helper()
	line, err := r.ReadString('\n')
	if err != nil || line != want {
		t.Fatalf("read %q, %v; want %q", line, err, want)
	}
}

func waitFor(t *testing.T, test func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if test() {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("condition did not become true")
}

func (m *memoryNode) process(t *testing.T, id string) *agentProcess {
	t.Helper()
	var p *agentProcess
	waitFor(t, func() bool { m.s.processMu.Lock(); p = m.s.processes[id]; m.s.processMu.Unlock(); return p != nil })
	return p
}

func waitOutput(t *testing.T, p *agentProcess, n uint64) {
	t.Helper()
	waitFor(t, func() bool { p.mu.Lock(); defer p.mu.Unlock(); return p.out >= n })
}

func TestProcessReplayThenLiveAfterSocketDrop(t *testing.T) {
	m := newMemoryNode(t, "/bin/cat")
	mux := m.connection(t)
	s, r := openProcess(t, mux, nodewire.OpenRequest{Stream: "replay"})
	for i := 1; i <= 12; i++ {
		_, _ = fmt.Fprintf(s, "%d\n", i)
	}
	for i := 1; i <= 4; i++ {
		expectLine(t, r, fmt.Sprintf("%d\n", i))
	}
	p := m.process(t, "replay")
	waitOutput(t, p, 12)
	_ = mux.Close()
	s, r = openProcess(t, m.connection(t), nodewire.OpenRequest{Stream: "replay", Resume: true, AfterOut: 4, AfterIn: 12})
	ack, err := nodewire.ReadResumeAck(r)
	if err != nil || ack.HaveIn != 12 {
		t.Fatalf("ack = %+v, %v", ack, err)
	}
	for i := 5; i <= 12; i++ {
		expectLine(t, r, fmt.Sprintf("%d\n", i))
	}
	_, _ = io.WriteString(s, "live\n")
	expectLine(t, r, "live\n")
	_ = s.Close()
}

func TestAcceptedInputSurvivesLostAckAndPartialInputIsDiscarded(t *testing.T) {
	m := newMemoryNode(t, "/bin/cat")
	mux := m.connection(t)
	s, _ := openProcess(t, mux, nodewire.OpenRequest{Stream: "inputs"})
	_, _ = io.WriteString(s, "executed\nhalf")
	p := m.process(t, "inputs")
	waitOutput(t, p, 1)
	_ = mux.Close() // The hub did not consume either the output or the ack.
	s, r := openProcess(t, m.connection(t), nodewire.OpenRequest{Stream: "inputs", Resume: true, AfterIn: 2})
	ack, err := nodewire.ReadResumeAck(r)
	if err != nil || ack.HaveIn != 1 {
		t.Fatalf("ack = %+v, %v", ack, err)
	}
	_, _ = io.WriteString(s, "half-complete\n")
	expectLine(t, r, "executed\n")
	expectLine(t, r, "half-complete\n")
	waitOutput(t, p, 2)
	var in bytes.Buffer
	if err := p.journal.In.Replay(0, &in); err != nil {
		t.Fatal(err)
	}
	if in.String() != "executed\nhalf-complete\n" {
		t.Fatalf("executed inputs = %q", in.String())
	}
	_ = s.Close()
}

func TestDropDuringReplayAndSupersedingAttachment(t *testing.T) {
	m := newMemoryNode(t, "/bin/cat")
	mux := m.connection(t)
	s, r := openProcess(t, mux, nodewire.OpenRequest{Stream: "again"})
	line := strings.Repeat("x", 256<<10) + "\n"
	for range 20 {
		_, _ = io.WriteString(s, line)
		expectLine(t, r, line)
	}
	_ = mux.Close()
	mux = m.connection(t)
	s, r = openProcess(t, mux, nodewire.OpenRequest{Stream: "again", Resume: true, AfterIn: 20})
	if _, err := nodewire.ReadResumeAck(r); err != nil {
		t.Fatal(err)
	}
	expectLine(t, r, line)
	_ = mux.Close()
	mux = m.connection(t)
	s, r = openProcess(t, mux, nodewire.OpenRequest{Stream: "again", Resume: true, AfterIn: 20, AfterOut: 1})
	if _, err := nodewire.ReadResumeAck(r); err != nil {
		t.Fatal(err)
	}
	for range 19 {
		expectLine(t, r, line)
	}
	newer, nr := openProcess(t, mux, nodewire.OpenRequest{Stream: "again", Resume: true, AfterIn: 20, AfterOut: 20})
	if _, err := nodewire.ReadResumeAck(nr); err != nil {
		t.Fatal(err)
	}
	_, err := io.ReadAll(r)
	if err == nil || err.Error() != "superseded" {
		t.Fatalf("old attachment: %v", err)
	}
	_, _ = io.WriteString(newer, "new owner\n")
	expectLine(t, nr, "new owner\n")
	_ = newer.Close()
}

func TestReplayExitAfterProcessEndedOffline(t *testing.T) {
	m := newMemoryNode(t, "/bin/sh", "-c", "read line; printf '%s\\n' \"$line\"; exit 7")
	mux := m.connection(t)
	s, _ := openProcess(t, mux, nodewire.OpenRequest{Stream: "exit"})
	_, _ = io.WriteString(s, "last\n")
	p := m.process(t, "exit")
	waitOutput(t, p, 2)
	_ = mux.Close()
	_, r := openProcess(t, m.connection(t), nodewire.OpenRequest{Stream: "exit", Resume: true, AfterIn: 1})
	ack, err := nodewire.ReadResumeAck(r)
	if err != nil || ack.HaveIn != 1 {
		t.Fatal(ack, err)
	}
	expectLine(t, r, "last\n")
	_, err = io.ReadAll(r)
	if err == nil || err.Error() != "exit 7" {
		t.Fatalf("exit = %v", err)
	}
}

func TestNewOutputWhileReplayIsBlockedEntersJournalThenLive(t *testing.T) {
	m := newMemoryNode(t, "/bin/cat")
	mux := m.connection(t)
	s, r := openProcess(t, mux, nodewire.OpenRequest{Stream: "seam"})
	for _, line := range []string{"one\n", "two\n", "three\n"} {
		_, _ = io.WriteString(s, line)
		expectLine(t, r, line)
	}
	p := m.process(t, "seam")
	_ = mux.Close()
	m.pauseData = 2 // ResumeAck is the first payload; pause the first replay row.
	m.paused, m.continueData = make(chan struct{}), make(chan struct{})
	s, r = openProcess(t, m.connection(t), nodewire.OpenRequest{Stream: "seam", Resume: true, AfterIn: 3})
	if _, err := nodewire.ReadResumeAck(r); err != nil {
		t.Fatal(err)
	}
	<-m.paused
	_, _ = io.WriteString(s, "during replay\n")
	waitOutput(t, p, 4)
	var journaled bytes.Buffer
	if err := p.journal.Out.Replay(3, &journaled); err != nil || journaled.String() != "during replay\n" {
		t.Fatal(journaled.String(), err)
	}
	close(m.continueData)
	for _, line := range []string{"one\n", "two\n", "three\n", "during replay\n"} {
		expectLine(t, r, line)
	}
	_, _ = io.WriteString(s, "live\n")
	expectLine(t, r, "live\n")
}

func TestUnresumableProcessKeepsRunningAndGraceKillsDetachedProcess(t *testing.T) {
	m := newMemoryNode(t, "/bin/cat")
	cfg := m.s.conf()
	cfg.SessionGrace = 40 * time.Millisecond
	m.s.cfg.Store(&cfg)
	mux := m.connection(t)
	s, r := openProcess(t, mux, nodewire.OpenRequest{Stream: "broken"})
	p := m.process(t, "broken")
	p.journal.Disable(errors.New("disk full"))
	_, _ = io.WriteString(s, "still running\n")
	expectLine(t, r, "still running\n")
	_ = mux.Close()
	_, r = openProcess(t, m.connection(t), nodewire.OpenRequest{Stream: "broken", Resume: true, AfterIn: 1})
	_, err := nodewire.ReadResumeAck(r)
	if err == nil || !strings.Contains(err.Error(), journal.ErrUnresumable.Error()) {
		t.Fatal(err)
	}
	waitFor(t, func() bool { p.mu.Lock(); defer p.mu.Unlock(); return p.exit != "" })
}

func TestReadLinesDropsPartialButBoundsOversizedLines(t *testing.T) {
	var got bytes.Buffer
	complete := 0
	maxChunk := 0
	err := readLines(strings.NewReader("whole\n"+strings.Repeat("x", journal.MaxLine+100)+"\nhalf"), func(b []byte, done bool) error {
		maxChunk = max(maxChunk, len(b))
		if done {
			complete++
		}
		_, _ = got.Write(b)
		return nil
	})
	if !errors.Is(err, io.EOF) || complete != 2 || strings.HasSuffix(got.String(), "half") || maxChunk > journal.MaxLine+64<<10 {
		t.Fatalf("lines=%d max=%d err=%v", complete, maxChunk, err)
	}
}

// Lose only the input confirmation, after the node has executed the line.
// WriteFrame writes its nine-byte header and payload separately under a lock.
type ackDroppingConn struct {
	net.Conn
	skip                 int
	dropAcks             bool
	dropData, dataFrames int
	pauseData            int
	paused, continueData chan struct{}
	ctx                  context.Context
}

func (c *ackDroppingConn) Write(b []byte) (int, error) {
	if c.skip > 0 {
		c.skip -= len(b)
		return len(b), nil
	}
	if len(b) == 9 && c.dropAcks && nodewire.Kind(b[4]) == nodewire.KindInputAck {
		c.skip = int(binary.BigEndian.Uint32(b[5:9]))
		return len(b), nil
	}
	if len(b) == 9 && nodewire.Kind(b[4]) == nodewire.KindData {
		c.dataFrames++
		if c.dataFrames == c.dropData {
			_ = c.Conn.Close()
			return 0, io.ErrUnexpectedEOF
		}
		if c.dataFrames == c.pauseData {
			close(c.paused)
			select {
			case <-c.continueData:
			case <-c.ctx.Done():
				return 0, c.ctx.Err()
			}
		}
	}
	return c.Conn.Write(b)
}
