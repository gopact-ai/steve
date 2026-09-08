package app

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"

	adminsvc "github.com/gopact-ai/steve/internal/admin"
	"github.com/gopact-ai/steve/internal/harness"
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
	probeDir := func(node string) string {
		if node == "" {
			return filepath.Join(filepath.Dir(cfg.Gateway.StatePath), "probe")
		}
		for _, s := range nodes.Statuses() {
			if s.Name == node && s.Advert.StateDir != "" {
				return filepath.Join(s.Advert.StateDir, "probe")
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
	if cfg.Gateway.IssuerAddr != "" {
		issuer := &http.Server{Addr: cfg.Gateway.IssuerAddr, Handler: ledger.IssuerHandler(book, cfg.Gateway.IssuerToken)}
		go func() {
			if err := issuer.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
				slog.Error(fmt.Sprintf("steve: lease issuer on %s: %v", cfg.Gateway.IssuerAddr, err))
			}
		}()
		life.Defer(func() { issuer.Close() })
		slog.Info(fmt.Sprintf("steve: issuing region %s leases on %s", book.Region(), cfg.Gateway.IssuerAddr))
	}
	return &modelsValues{endpoints: endpoints, probeDir: probeDir, prober: prober, seen: seen}, nil
}

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

func (v *modelsValues) Endpoints() func(ctx context.Context) []models.Endpoint { return v.endpoints }

func (v *modelsValues) ProbeDir() func(node string) string { return v.probeDir }

func (v *modelsValues) Prober() *models.Prober { return v.prober }

func (v *modelsValues) Seen() *models.Book { return v.seen }
