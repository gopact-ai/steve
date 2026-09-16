package sshconnect

import (
	"context"
	"fmt"
	"io"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"
)

// Installation phases, in the order an installation runs them. The plan
// decides whether an upload happens; everything else always does.
const (
	PhasePreflight    = "preflight"
	PhaseRegistration = "registration"
	PhaseLink         = "link"
	PhaseUpload       = "upload"
	PhaseInstallation = "installation"
	PhaseConnectivity = "connectivity"
)

// LogLine is one line of the installation record: Steve narrating a phase
// ("steve"), or what the remote wrote on stdout or stderr.
type LogLine struct {
	At     time.Time `json:"at"`
	Stream string    `json:"stream"`
	Text   string    `json:"text"`
}

const (
	maxLogLines     = 300
	maxLogLineBytes = 512
	redactedMarker  = "[redacted]"
)

func (s *Service) phasesFor(plan InstallPlan) []string {
	phases := []string{PhasePreflight, PhaseRegistration}
	if _, ok := s.backend.(Linker); ok {
		phases = append(phases, PhaseLink)
	}
	if plan.Binary != nil {
		phases = append(phases, PhaseUpload)
	}
	return append(phases, PhaseInstallation, PhaseConnectivity)
}

// Status reports how an installation is going without touching it: the
// phase it is in, the steps it has settled, and what the remote has said
// so far. A plan that was never committed is "planned".
func (s *Service) Status(id string) (InstallResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	stored, ok := s.plans[id]
	if !ok {
		return InstallResult{}, fail("planning", "unknown_plan", "安装计划不存在或已过期", "重新检查机器并生成计划")
	}
	if !stored.running && !stored.done {
		return InstallResult{PlanID: id, Name: stored.plan.Request.Name, Status: "planned", Steps: []Step{}, Phases: s.phasesFor(stored.plan)}, nil
	}
	return cloneResult(stored.result), nil
}

// enter marks the phase the installation is now in and narrates it.
func (s *Service) enter(result *InstallResult, phase, message string) {
	result.Phase = phase
	result.appendLog(s.now(), "steve", message)
	s.progress(*result)
}

// note narrates something inside the current phase.
func (s *Service) note(result *InstallResult, message string) {
	result.appendLog(s.now(), "steve", message)
	s.progress(*result)
}

// output records what the remote wrote, with the credential the script
// carried replaced wherever it shows up. Output the runner cut at its
// limit loses its last line too: a credential split at the cut would not
// match, and a partial line says nothing a person needs.
func (s *Service) output(result *InstallResult, out Output, secret string) {
	at := s.now()
	for _, stream := range []struct{ name, text string }{{"stdout", out.Stdout}, {"stderr", out.Stderr}} {
		lines := strings.Split(strings.TrimRight(stream.text, "\n"), "\n")
		truncated := len(stream.text) >= outputLimit
		if truncated {
			lines = lines[:len(lines)-1]
		}
		for _, line := range lines {
			line = strings.TrimRight(line, "\r")
			if line == "" {
				continue
			}
			if secret != "" {
				line = strings.ReplaceAll(line, secret, redactedMarker)
			}
			result.appendLog(at, stream.name, line)
		}
		if truncated {
			result.appendLog(at, "steve", stream.name+" 输出超过上限，其余已省略")
		}
	}
	s.progress(*result)
}

// noteStored narrates into a stored installation by id, for the paths
// that have no result of their own on the stack (resuming a registration).
func (s *Service) noteStored(id, message string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if stored := s.plans[id]; stored != nil && stored.running {
		stored.result.appendLog(s.now(), "steve", message)
	}
}

// phaseReporter is the Reporter handed to a backend during verification.
// It writes into the installation's result only until the verification
// returns; a backend that kept the context and reports late is ignored,
// so nothing writes to the result after commit has moved on.
type phaseReporter struct {
	mu     sync.Mutex
	s      *Service
	result *InstallResult
	closed bool
}

func (r *phaseReporter) report(message string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return
	}
	r.s.note(r.result, message)
}

func (r *phaseReporter) close() {
	r.mu.Lock()
	r.closed = true
	r.mu.Unlock()
}

func (r *InstallResult) appendLog(at time.Time, stream, text string) {
	if len(text) > maxLogLineBytes {
		cut := maxLogLineBytes - len("…")
		for cut > 0 && !utf8.RuneStart(text[cut]) {
			cut--
		}
		text = text[:cut] + "…"
	}
	r.Log = append(r.Log, LogLine{At: at, Stream: stream, Text: text})
	if len(r.Log) > maxLogLines {
		r.Log = append(r.Log[:0], r.Log[len(r.Log)-maxLogLines:]...)
	}
}

// Reporter lets a backend narrate what it is waiting on during
// verification, where the SSH session is already over and only the
// backend knows how far the node has come.
type reporterKey struct{}

func WithReporter(ctx context.Context, report func(string)) context.Context {
	return context.WithValue(ctx, reporterKey{}, report)
}

// Report narrates into the installation that owns ctx; a no-op elsewhere.
func Report(ctx context.Context, message string) {
	if report, ok := ctx.Value(reporterKey{}).(func(string)); ok && report != nil {
		report(message)
	}
}

// Upload pacing. A thin VPN link delivers a few hundred KiB/s at best, so
// the upload is bounded by movement, with a generous ceiling behind it.
// Movement is measured where the SSH client reads; once it has read the
// last byte, it still pushes its buffered window to the remote and waits
// for the remote to finish, and nothing is left to count during that tail,
// so only the ceiling bounds it.
const (
	uploadStallLimit  = 90 * time.Second
	uploadReportEvery = 10 * time.Second
	uploadLimit       = 90 * time.Minute
)

// meteredReader counts what the SSH client has taken so far and notes when
// it has taken everything.
type meteredReader struct {
	io.Reader
	read    atomic.Int64
	drained atomic.Bool
}

func (m *meteredReader) Read(p []byte) (int, error) {
	n, err := m.Reader.Read(p)
	m.read.Add(int64(n))
	if err == io.EOF {
		m.drained.Store(true)
	}
	return n, err
}

func mib(n int64) float64 { return float64(n) / (1 << 20) }

// watchUpload narrates the upload's progress into the install log and ends
// it when bytes stop moving. The returned settle stops the watch and says
// whether a stall, rather than anything else, ended the upload. Until
// settle returns, the watch is the only writer of result.
func (s *Service) watchUpload(ctx context.Context, cancel context.CancelFunc, result *InstallResult, metered *meteredReader, size int64) (settle func() bool) {
	stop, done := make(chan struct{}), make(chan struct{})
	var stalled atomic.Bool
	go func() {
		defer close(done)
		ticker := time.NewTicker(s.uploadTick)
		defer ticker.Stop()
		started := time.Now()
		last, movedAt, reportedAt, reported := int64(0), started, started, int64(0)
		for {
			select {
			case <-ctx.Done():
				return
			case <-stop:
				return
			case now := <-ticker.C:
				read := metered.read.Load()
				if read != last {
					last, movedAt = read, now
				} else if !metered.drained.Load() && now.Sub(movedAt) >= s.uploadStall {
					stalled.Store(true)
					cancel()
					return
				}
				if now.Sub(reportedAt) < s.uploadReport {
					continue
				}
				s.note(result, uploadProgress(read, size, read-reported, now.Sub(reportedAt)))
				reportedAt, reported = now, read
			}
		}
	}()
	return func() bool {
		close(stop)
		<-done
		return stalled.Load()
	}
}

func uploadProgress(read, size, delta int64, window time.Duration) string {
	if window <= 0 || delta <= 0 {
		return fmt.Sprintf("已上传 %.1f / %.1f MiB，等待远端接收…", mib(read), mib(size))
	}
	rate := float64(delta) / window.Seconds()
	remaining := time.Duration(float64(size-read) / rate * float64(time.Second)).Round(time.Second)
	if remaining < 0 {
		remaining = 0
	}
	return fmt.Sprintf("已上传 %.1f / %.1f MiB（%.2f MiB/s，预计还需 %s）", mib(read), mib(size), rate/(1<<20), remaining)
}
