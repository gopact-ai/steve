package app

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"time"

	adminsvc "github.com/gopact-ai/steve/internal/admin"
	"github.com/gopact-ai/steve/internal/harness"
	"github.com/gopact-ai/steve/internal/httpdrain"
	"github.com/gopact-ai/steve/internal/ledger"
	"github.com/gopact-ai/steve/internal/models"
	"github.com/gopact-ai/steve/internal/nodewire"
	steveview "github.com/gopact-ai/steve/internal/view"
)

func assembleModels(life lifetime, boot runtimeAssembly, machines fleetAssembly) (modelsAssembly, error) {
	book := boot.Book()
	cfg := boot.Config()
	manager := boot.Manager()
	fleet := machines.Fleet()
	nodes := machines.Nodes()
	// What every harness was seen running, per machine: sessions report
	// it as they open, and a probe asks on purpose for the ones nobody
	// has used yet.
	seen := models.New()
	if err := seen.Persist(book.Document("models")); err != nil {
		return nil, err
	}
	fleet.SetModels(seen)
	manager.SetObserver(func(at harness.Placement, s steveview.Settings) {
		seen.Observe(models.Observation{Node: at.Node, Harness: at.Harness, Current: s.Model, Available: s.Models, Version: s.Adapter, Source: "session", Selectors: adminsvc.SelectorsOf(s.Options)})
	})
	prober := models.NewProber(manager, seen, func(ctx context.Context, node, dir string) error {
		if node == "" {
			return os.MkdirAll(dir, 0o700)
		}
		_, err := nodes.Files(ctx, node, nodewire.FileRequest{Op: nodewire.FileMkdir, Path: dir})
		return err
	})
	// The closures below outlive assembly, while the administration
	// rewrites cfg; they keep only what they need from it.
	hubProbeDir := probeWorkdir(filepath.Dir(cfg.Gateway.StatePath))
	probeDir := func(node string) string {
		if node == "" {
			return hubProbeDir
		}
		for _, s := range nodes.Statuses() {
			if s.Name == node && s.Advert.StateDir != "" {
				return probeWorkdir(s.Advert.StateDir)
			}
		}
		return ""
	}
	endpoints := func(ctx context.Context) []models.Endpoint {
		var eps []models.Endpoint
		known := map[string]bool{}
		for _, c := range fleet.All(ctx) {
			key := c.Node + "/" + c.Harness
			if !c.Eligible || known[key] {
				continue
			}
			dir := probeDir(c.Node)
			if dir == "" {
				continue
			}
			known[key] = true
			eps = append(eps, models.Endpoint{Node: c.Node, Harness: c.Harness, Workdir: dir})
		}
		return eps
	}
	fleet.SetHubSlots(cfg.HubSlots())
	fleet.SetNodeLevels(cfg.NodeLevels())
	fleet.SetNodeRegions(cfg.NodeRegions())
	nodes.SetHubLevel(string(cfg.HubLevel()))
	// Lease authorities were registered before execution recovery.
	if addr := cfg.Gateway.IssuerAddr; addr != "" {
		issuer := httpdrain.New(&http.Server{Handler: ledger.IssuerHandler(book, cfg.Gateway.IssuerToken), ReadHeaderTimeout: 10 * time.Second})
		served := make(chan struct{})
		go func() {
			defer close(served)
			listener, err := net.Listen("tcp", addr)
			if err == nil {
				err = issuer.Serve(listener)
			}
			if err != nil {
				slog.Error(fmt.Sprintf("steve: lease issuer on %s: %v", addr, err))
			}
		}()
		// The ledger closes after this step: a lease request already
		// inside it is answered, or cancelled once the grace is over,
		// before the step ends.
		life.Defer(func() {
			grace, cancel := context.WithTimeout(context.Background(), issuerShutdownGrace)
			defer cancel()
			_ = issuer.Shutdown(grace)
			<-served
		})
		slog.Info(fmt.Sprintf("steve: issuing region %s leases on %s", book.Region(), addr))
	}
	return &modelsValues{endpoints: endpoints, probeDir: probeDir, prober: prober, seen: seen}, nil
}

// issuerShutdownGrace is how long lease requests in flight may run on once
// the application stops.
const issuerShutdownGrace = 5 * time.Second

type modelsAssembly interface {
	Endpoints() func(ctx context.Context) []models.Endpoint
	ProbeDir() func(node string) string
	Prober() *models.Prober
	Seen() *models.Book
}

type modelsValues struct {
	endpoints func(ctx context.Context) []models.Endpoint
	probeDir  func(node string) string
	prober    *models.Prober
	seen      *models.Book
}

// probeWorkdir is the working directory in which model probes open their
// throwaway sessions on a hub or node whose state lives in stateDir. Both the
// background discovery after startup and explicitly requested probes use it.
func probeWorkdir(stateDir string) string { return filepath.Join(stateDir, "probe") }

func (v *modelsValues) Endpoints() func(ctx context.Context) []models.Endpoint { return v.endpoints }

func (v *modelsValues) ProbeDir() func(node string) string { return v.probeDir }

func (v *modelsValues) Prober() *models.Prober { return v.prober }

func (v *modelsValues) Seen() *models.Book { return v.seen }
