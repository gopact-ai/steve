package app

import (
	"context"
	"log"
	"os"
	"path/filepath"
	"strings"

	adminsvc "github.com/gopact-ai/steve/internal/admin"
	"github.com/gopact-ai/steve/internal/agent"
	"github.com/gopact-ai/steve/internal/capability"
	"github.com/gopact-ai/steve/internal/config"
	"github.com/gopact-ai/steve/internal/harness"
	"github.com/gopact-ai/steve/internal/home"
	"github.com/gopact-ai/steve/internal/hubid"
	"github.com/gopact-ai/steve/internal/i18n"
	"github.com/gopact-ai/steve/internal/runtime"
	"github.com/gopact-ai/steve/internal/skills"
)

func load(path string) (*config.Config, *agent.Catalog, *harness.Manager, *skills.Live, error) {
	return loadConfigured(path, nil)
}

func loadConfigured(path string, configure func(*config.Config) error) (*config.Config, *agent.Catalog, *harness.Manager, *skills.Live, error) {
	cfg, err := config.Load(path)
	if err != nil {
		return nil, nil, nil, nil, err
	}
	persistedHubID := cfg.Gateway.HubID
	if configure != nil {
		if err := configure(cfg); err != nil {
			return nil, nil, nil, nil, err
		}
	}
	if err := cfg.ValidateChannels(); err != nil {
		return nil, nil, nil, nil, err
	}
	catalog, err := cfg.AgentCatalog()
	if err != nil {
		return nil, nil, nil, nil, err
	}
	stateDir := filepath.Dir(cfg.Gateway.StatePath)
	identity, err := hubid.Resolve(stateDir, persistedHubID)
	if err != nil {
		return nil, nil, nil, nil, err
	}
	if configure == nil || cfg.Gateway.HubID == "" {
		cfg.Gateway.HubID = identity
	}
	selectedRuntimes := make([]string, 0, len(cfg.Harnesses))
	for name := range cfg.Harnesses {
		selectedRuntimes = append(selectedRuntimes, name)
	}
	if err := runtime.PrepareSelected(stateDir, selectedRuntimes); err != nil {
		return nil, nil, nil, nil, err
	}
	skillMap, err := skills.Setup(stateDir)
	if err != nil {
		return nil, nil, nil, nil, err
	}
	live := &skills.Live{Map: skillMap, Dests: runtime.SelectedSkillDests(stateDir, selectedRuntimes)}
	if err := live.Apply(); err != nil {
		return nil, nil, nil, nil, err
	}
	if err := cfg.PrepareAdapters(context.Background()); err != nil {
		return nil, nil, nil, nil, err
	}
	manager, err := adminsvc.HarnessRuntimeConfig(cfg).HarnessManager()
	if err != nil {
		return nil, nil, nil, nil, err
	}
	live.After = manager.Restart
	return cfg, catalog, manager, live, nil
}

func wireHome(cfg *config.Config, live *skills.Live) (*capability.Assembler, error) {
	locale := home.LocaleZH
	if i18n.FromLang(cfg.EffectiveLocale()) == i18n.LocaleEN {
		locale = home.LocaleEN
	}
	if err := home.BootstrapLocale(cfg.Gateway.HomePath, cfg.EffectiveOwnerID(), locale); err != nil {
		return nil, err
	}
	assembler := cfg.CapabilityAssembler().SetHome(home.Dir{Path: cfg.Gateway.HomePath, Locale: locale})
	if live != nil && live.Map != nil {
		assembler.SetSkills(live.Map)
	}
	return assembler, nil
}

func warnHome(cfg *config.Config) {
	if cfg.FeishuEnabled() && cfg.Feishu.OwnerOpenID == "" {
		log.Printf("steve: feishu.owner_open_id is unset; DMs use guest home")
	}
}

func checkHome(cfg *config.Config) error {
	info, err := os.Stat(cfg.Gateway.HomePath)
	if err != nil {
		return err
	}
	if info.Mode().Perm() != 0o700 {
		log.Printf("steve: home directory mode is %o, want 700", info.Mode().Perm())
	}
	for _, name := range []string{home.FileSoul, home.FileUser, home.FileMemory} {
		path := filepath.Join(cfg.Gateway.HomePath, name)
		st, err := os.Stat(path)
		if err != nil {
			return err
		}
		if st.Mode().Perm() != 0o600 {
			log.Printf("steve: %s mode is %o, want 600", name, st.Mode().Perm())
		}
	}
	snap, err := home.Load(cfg.Gateway.HomePath, home.ModeOwner)
	if err != nil {
		return err
	}
	for _, warning := range snap.Warnings {
		log.Printf("steve: %s", warning)
	}
	user, err := os.ReadFile(filepath.Join(cfg.Gateway.HomePath, home.FileUser))
	if err != nil {
		return err
	}
	if strings.Contains(string(user), home.TemplateMarker) {
		log.Printf("steve: edit USER.md")
	}
	return nil
}
