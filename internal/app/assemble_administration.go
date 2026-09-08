package app

import (
	"fmt"
	"path/filepath"

	adminsvc "github.com/gopact-ai/steve/internal/admin"
	"github.com/gopact-ai/steve/internal/ledger"
)

func assembleAdministration(life lifetime, input inputAssembly, boot runtimeAssembly, work executionAssembly, planning plansAssembly, projection readModelAssembly, page consoleAssembly) (administrationAssembly, error) {
	environment := input.Environment()
	book := boot.Book()
	cfg := boot.Config()
	ctx := boot.Context()
	stop := boot.Stop()
	executions := work.Executions()
	tasks := work.Tasks()
	supervisor := planning.Supervisor()
	view := projection.View()
	admin := page.Admin()
	cons := page.Console()
	dashboard := page.Dashboard()
	materials := page.Materials()
	admin.Materials, admin.Console, admin.Owner = materials, cons, cfg.EffectiveOwnerID()
	admin.MaterialLevel = cfg.HubLevel()
	cons.SetMaterials(materials, admin.AuthorizeMaterials)
	cons.SetDefaultLocale(cfg.EffectiveLocale())
	dashboard.SetSettings(adminsvc.NewSettings(admin, cfg))
	channelSettings := adminsvc.NewChannels(admin, cfg)
	dashboard.SetChannels(channelSettings)
	var restartDocument ledger.Doc = &ledger.FileDocument{Path: filepath.Join(filepath.Dir(cfg.Gateway.StatePath), "service-restarts.json")}
	if environment != nil {
		restartDocument = book.Document("service-restarts")
	}
	services, err := adminsvc.NewServices(admin, executions, dashboard.SealWrites, stop, restartDocument)
	life.SetServices(services)
	if err != nil {
		return nil, fmt.Errorf("initialize service restart control: %w", err)
	}
	dashboard.SetServices(services)
	if err := admin.ResumeProjectCopies(ctx); err != nil {
		return nil, fmt.Errorf("resume configured copies: %w", err)
	}
	tasks.SetObserver(func(id string) { view.TaskChanged(id) })
	supervisor.Runs().Observe(view)
	return &administrationValues{channelSettings: channelSettings, services: services}, nil
}

type administrationAssembly interface {
	ChannelSettings() channelRuntime
	Services() *adminsvc.Services
}

type administrationValues struct {
	channelSettings channelRuntime
	services        *adminsvc.Services
}

func (v *administrationValues) ChannelSettings() channelRuntime { return v.channelSettings }

func (v *administrationValues) Services() *adminsvc.Services { return v.services }
