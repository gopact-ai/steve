package node

import (
	"context"
	"errors"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/gopact-ai/steve/internal/ability"
)

// LaunchResult is what starting a binary once told us: whether it ran at
// all — the right architecture, its libraries present — what it said, and
// the version it named, if it named one.
type LaunchResult struct {
	OK      bool
	Result  string
	Version *ability.Version
	At      time.Time
}

// LaunchTimeout bounds one launch check. A binary that has not exited in
// this long has still launched; the check is about starting, not finishing.
const LaunchTimeout = 3 * time.Second

// ProbeEvery is how often launch checks are repeated. Binaries do not
// change often, and evidence this old is still well inside its TTL.
var ProbeEvery = 10 * time.Minute

// LaunchProbe checks, in the background, that the binaries this machine
// offers actually start. Existence on PATH is checked at every snapshot;
// launching costs a process per binary, so it is done on its own clock and
// the snapshot reads the last answer.
type LaunchProbe struct {
	mu      sync.Mutex
	results map[string]LaunchResult
	wake    chan struct{}
}

func NewLaunchProbe() *LaunchProbe {
	return &LaunchProbe{results: map[string]LaunchResult{}, wake: make(chan struct{}, 1)}
}

// Lookup is the last launch result for a resolved path, if there is one.
func (p *LaunchProbe) Lookup(path string) (LaunchResult, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	r, ok := p.results[path]
	return r, ok
}

// Run checks every command now, then every ProbeEvery, until ctx ends.
// commands is asked each round, so a configuration reloaded in between is
// probed on the next pass. Wake forces a pass early.
func (p *LaunchProbe) Run(ctx context.Context, commands func() []string) {
	for {
		p.pass(ctx, commands())
		select {
		case <-ctx.Done():
			return
		case <-time.After(ProbeEvery):
		case <-p.wake:
		}
	}
}

// Wake asks for a pass now rather than at the next tick.
func (p *LaunchProbe) Wake() {
	select {
	case p.wake <- struct{}{}:
	default:
	}
}

func (p *LaunchProbe) pass(ctx context.Context, commands []string) {
	seen := map[string]bool{}
	checked, started := 0, 0
	defer func() { log.Printf("steve-node: launch probe: %d binaries checked, %d start", checked, started) }()
	for _, command := range commands {
		if command == "" {
			continue
		}
		path, err := exec.LookPath(command)
		if err != nil || seen[path] {
			continue
		}
		seen[path] = true
		if ctx.Err() != nil {
			return
		}
		r := Launch(ctx, path)
		checked++
		if r.OK {
			started++
		}
		p.mu.Lock()
		p.results[path] = r
		p.mu.Unlock()
		if !r.OK {
			log.Printf("steve-node: %s does not launch: %s", path, r.Result)
		}
	}
}

// Launch starts one binary with --version and reports whether it started.
// Exit status is not the point: many tools answer --version with a usage
// error, and that is a running program. What fails the check is a process
// that could not begin — a wrong architecture, a missing library, no
// permission — or the shell's 126/127 that means the same.
func Launch(ctx context.Context, path string) LaunchResult {
	ctx, cancel := context.WithTimeout(ctx, LaunchTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, path, "--version")
	setProcessGroup(cmd)
	cmd.Cancel = func() error { return killProcessGroup(cmd) }
	cmd.WaitDelay = time.Second
	cmd.Env = append(os.Environ(), "NO_COLOR=1", "TERM=dumb")
	var out limitedBuffer
	cmd.Stdout, cmd.Stderr = &out, &out
	err := cmd.Run()
	r := LaunchResult{At: time.Now().UTC(), Result: firstLine(out.String())}
	var exit *exec.ExitError
	switch {
	case err == nil:
		r.OK = true
	case ctx.Err() == context.DeadlineExceeded:
		r.OK = true
		if r.Result == "" {
			r.Result = "started; no answer to --version within " + LaunchTimeout.String()
		}
	case errors.As(err, &exit):
		code := exit.ExitCode()
		if code == 126 || code == 127 || unrunnable(out.String()) {
			r.OK = false
			if r.Result == "" {
				r.Result = err.Error()
			}
		} else {
			r.OK = true
		}
	default:
		r.OK = false
		r.Result = err.Error()
	}
	if r.OK {
		r.Version = versionIn(out.String())
	}
	// A generic launcher — npx, python, sh — starts, but the version it
	// prints is its own, and the thing it will run has not been checked.
	if base := filepath.Base(path); r.OK && genericLauncher(base) {
		r.Version = nil
		r.Result = base + " starts; what it runs is not checked"
	}
	return r
}

var launchers = map[string]bool{
	"npx": true, "npm": true, "node": true, "bun": true, "bunx": true, "deno": true,
	"python": true, "python3": true, "uv": true, "uvx": true, "pipx": true,
	"sh": true, "bash": true, "zsh": true, "env": true,
}

func genericLauncher(base string) bool { return launchers[base] }

func unrunnable(out string) bool {
	for _, sign := range []string{"error while loading shared libraries", "cannot execute binary file", "Exec format error", "No such file or directory", "Permission denied"} {
		if strings.Contains(out, sign) {
			return true
		}
	}
	return false
}

var versionPattern = regexp.MustCompile(`\b(\d+)\.(\d+)(?:\.(\d+))?(?:[-+][0-9A-Za-z.-]+)?\b`)

// versionIn finds the first thing that looks like a version in what the
// binary said. Three numbers is semver enough to compare; two is kept as
// opaque text.
func versionIn(out string) *ability.Version {
	for _, line := range strings.SplitN(out, "\n", 4) {
		m := versionPattern.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		if m[3] != "" {
			return &ability.Version{Scheme: "semver", Value: m[0]}
		}
		return &ability.Version{Scheme: "opaque", Value: m[0]}
	}
	return nil
}

func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	if len(s) > 160 {
		s = s[:160] + "…"
	}
	return s
}

// limitedBuffer keeps the first few KB of a chatty --version and drops the
// rest: the check reads one line.
type limitedBuffer struct {
	b []byte
}

const limitedBufferMax = 4 << 10

func (l *limitedBuffer) Write(p []byte) (int, error) {
	if room := limitedBufferMax - len(l.b); room > 0 {
		if len(p) > room {
			p = p[:room]
		}
		l.b = append(l.b, p...)
	}
	return len(p), nil
}

func (l *limitedBuffer) String() string { return string(l.b) }
