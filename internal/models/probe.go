package models

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/gopact-ai/acp"
	"github.com/gopact-ai/steve/internal/harness"
	"github.com/gopact-ai/steve/internal/view"
)

// Endpoint is one harness on one machine, and where a probe session may
// have its working directory there.
type Endpoint struct {
	Node    string
	Harness string
	Workdir string
}

// Opener opens and closes sessions; the harness manager satisfies it.
type Opener interface {
	OpenSession(ctx context.Context, at harness.Placement, upstreamID, workdir string, servers []acp.MCPServer) (harness.Runner, error)
	CloseSession(ctx context.Context, at harness.Placement, upstreamID string) error
}

// settingsReporter is a runner that says what its agent is set to. A
// probe only reads, so it asks for the reporting method alone: a runner
// that reports selectors without offering setters is still observed.
type settingsReporter interface {
	Settings() view.Settings
}

// Bind the local session, so it losing Settings fails the build here
// rather than turning every probe into "asked, and it does not tell".
// The remote session reports the same way but is unexported by its
// package, so only this one can be bound from here.
var _ settingsReporter = (*harness.Session)(nil)

// Prepare makes a directory exist on a machine before a session opens in
// it. The registry's command channel does this for a node; the hub uses
// the filesystem.
type Prepare func(ctx context.Context, node, dir string) error

// ProbeTimeout bounds one probe: starting an agent and opening a session,
// nothing more.
const ProbeTimeout = 90 * time.Second

// Prober asks a harness what it can run by opening a session and reading
// the answer, then closing it. It is discovery, not work: no prompt is
// ever sent.
type Prober struct {
	open    Opener
	book    *Book
	prepare Prepare
}

func NewProber(open Opener, book *Book, prepare Prepare) *Prober {
	return &Prober{open: open, book: book, prepare: prepare}
}

// Probe opens one throwaway session and records what it reported.
func (p *Prober) Probe(ctx context.Context, ep Endpoint) (Observation, error) {
	ctx, cancel := context.WithTimeout(ctx, ProbeTimeout)
	defer cancel()
	if p.prepare != nil {
		if err := p.prepare(ctx, ep.Node, ep.Workdir); err != nil {
			return Observation{}, fmt.Errorf("prepare %s: %w", ep.Workdir, err)
		}
	}
	at := harness.Placement{Node: ep.Node, Harness: ep.Harness}
	// A new session, never a loaded one: the point is what the harness
	// says when it starts fresh here.
	runner, err := p.open.OpenSession(ctx, at, "", ep.Workdir, nil)
	if err != nil {
		return Observation{}, err
	}
	// An agent with no selectors reports nothing; its observation records
	// that it was asked and did not tell.
	var settings view.Settings
	if reporter, ok := runner.(settingsReporter); ok {
		settings = reporter.Settings()
	}
	if err := p.open.CloseSession(ctx, at, runner.ID()); err != nil {
		slog.Error(fmt.Sprintf("models: close probe session on %s/%s: %v", ep.Node, ep.Harness, err), "node", ep.Node, "harness", ep.Harness)
	}
	obs := Observation{
		Node: ep.Node, Harness: ep.Harness,
		Current: settings.Model, Available: settings.Models, Version: settings.Adapter,
		Source: "probe", At: time.Now().UTC(),
	}
	p.book.Observe(obs)
	return obs, nil
}

// Result is one probe's outcome, for whoever asked.
type Result struct {
	Endpoint Endpoint
	Observation
	Err error
}

// ProbeAll probes every endpoint in turn. With force false, endpoints the
// book already knows are skipped: discovery is for what nobody has seen.
func (p *Prober) ProbeAll(ctx context.Context, eps []Endpoint, force bool) []Result {
	var out []Result
	for _, ep := range eps {
		if ctx.Err() != nil {
			break
		}
		if !force {
			if _, known := p.book.Get(ep.Node, ep.Harness); known {
				continue
			}
		}
		obs, err := p.Probe(ctx, ep)
		out = append(out, Result{Endpoint: ep, Observation: obs, Err: err})
	}
	return out
}
