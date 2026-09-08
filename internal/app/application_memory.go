package app

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/gopact-ai/steve/internal/agentmcp"
	"github.com/gopact-ai/steve/internal/config"
	"github.com/gopact-ai/steve/internal/home"
	"github.com/gopact-ai/steve/internal/ledger"
	"github.com/gopact-ai/steve/internal/memory"
)

type applicationMemory struct {
	Service *memory.Service
	Home    home.Loader
	Shared  *memory.LedgerStore
}

// prepareApplicationMemory chooses one authority per deployment. Cluster
// activation uses only the generation-scoped ledger after its initial import.
func prepareApplicationMemory(ctx context.Context, cfg *config.Config, book *ledger.Ledger) (applicationMemory, error) {
	locale := home.LocaleZH
	if cfg.EffectiveLocale() == "en" {
		locale = home.LocaleEN
	}
	memoryDir := filepath.Join(filepath.Dir(cfg.Gateway.StatePath), "memory")
	if book == nil {
		return applicationMemory{Service: memory.NewService(memory.NewMarkdown(cfg.Gateway.HomePath, memoryDir), filepath.Join(memoryDir, "audit.jsonl")), Home: home.Dir{Path: cfg.Gateway.HomePath, Locale: locale}}, nil
	}
	shared := memory.NewLedgerStore(book)
	shared.SetWriteGuard(func(ctx context.Context, tx *ledger.Tx) error {
		return agentmcp.AuthorizeContext(ctx, applicationMCPTx{tx})
	})
	bootstrapped, err := shared.HomeBootstrapped(ctx)
	if err != nil {
		return applicationMemory{}, err
	}
	if !bootstrapped {
		files := home.DefaultFiles(locale)
		if cfg.Gateway.HomePath != "" {
			local, err := home.Files(cfg.Gateway.HomePath)
			if err != nil && !errors.Is(err, home.ErrMissing) {
				return applicationMemory{}, err
			}
			for _, file := range local {
				if !file.Missing {
					files[file.Name] = file.Text
				}
			}
		}
		// Only explicitly configured project scopes participate in the initial
		// import. After the shared home marker exists, local files are ignored.
		source := memory.NewMarkdown(cfg.Gateway.HomePath, memoryDir)
		for id := range cfg.Projects {
			if id == config.ReservedHomeProject {
				continue
			}
			scope := memory.ProjectScope(id)
			path, err := source.Path(scope)
			if err != nil {
				return applicationMemory{}, err
			}
			info, err := os.Lstat(path)
			if os.IsNotExist(err) {
				continue
			}
			if err != nil {
				return applicationMemory{}, err
			}
			if !info.Mode().IsRegular() {
				return applicationMemory{}, fmt.Errorf("project memory %q must be a regular file for import", id)
			}
			raw, err := os.ReadFile(path)
			if os.IsNotExist(err) {
				continue
			}
			if err != nil {
				return applicationMemory{}, err
			}
			if _, err := shared.Bootstrap(ctx, scope, string(raw), memory.Actor{By: "onboard"}); err != nil {
				return applicationMemory{}, err
			}
		}
		if _, err := shared.BootstrapHome(ctx, files, memory.Actor{By: "onboard"}); err != nil {
			return applicationMemory{}, err
		}
	}
	label := "共享档案"
	if locale == home.LocaleEN {
		label = "Shared profile"
	}
	reader := home.Reader{Locale: locale, Label: label, ReadFiles: func() (map[string]string, error) { return shared.HomeFiles(ctx) }}
	editable := home.EditableReader{Reader: reader, SaveIdentity: func(ctx context.Context, soul, user string) error {
		return shared.WriteIdentity(ctx, soul, user, memory.Actor{By: "onboard"})
	}}
	return applicationMemory{Service: memory.NewService(shared, ""), Home: editable, Shared: shared}, nil
}
