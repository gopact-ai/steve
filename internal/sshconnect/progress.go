package sshconnect

import (
	"context"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

// Installation phases, in the order an installation runs them. The plan
// decides whether an upload happens; everything else always does.
const (
	PhasePreflight    = "preflight"
	PhaseRegistration = "registration"
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

func phasesFor(plan InstallPlan) []string {
	phases := []string{PhasePreflight, PhaseRegistration}
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
		return InstallResult{PlanID: id, Name: stored.plan.Request.Name, Status: "planned", Steps: []Step{}, Phases: phasesFor(stored.plan)}, nil
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
