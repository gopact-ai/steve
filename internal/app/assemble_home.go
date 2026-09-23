package app

import (
	"fmt"

	"github.com/gopact-ai/steve/internal/capability"
	"github.com/gopact-ai/steve/internal/configbuild"
	"github.com/gopact-ai/steve/internal/home"
	"github.com/gopact-ai/steve/internal/ledger"
)

func assembleHome(input inputAssembly, boot runtimeAssembly) (homeAssembly, error) {
	environment := input.Environment()
	book := boot.Book()
	catalog := boot.Catalog()
	cfg := boot.Config()
	ctx := boot.Context()
	live := boot.Live()
	var sharedMemoryBook *ledger.Ledger
	if environment != nil {
		sharedMemoryBook = book
	}
	profile, err := prepareApplicationMemoryWithSettings(ctx, cfg, sharedMemoryBook, boot.Settings())
	if err != nil {
		return nil, err
	}
	var assembler *capability.Assembler
	if environment != nil {
		assembler = configbuild.CapabilityAssembler(cfg).SetHome(profile.Home).SetLocale(profile.Locale)
		if live != nil && live.Map != nil {
			assembler.SetSkills(live.Map)
		}
	} else {
		assembler, err = wireHome(cfg, live)
		if err != nil {
			return nil, err
		}
		assembler.SetHome(profile.Home)
	}
	if settings := boot.Settings(); settings != nil {
		assembler.SetLocaleSource(func() home.Locale { return home.Locale(settings.Load().Gateway.Locale) })
	}
	warnHome(cfg)
	for _, selected := range catalog.List() {
		if _, err := assembler.AssembleMode(selected, home.ModeGuest); err != nil {
			return nil, fmt.Errorf("agent %q guest home: %w", selected.ID, err)
		}
		if cfg.EffectiveOwnerID() != "" {
			if _, err := assembler.AssembleMode(selected, home.ModeOwner); err != nil {
				return nil, fmt.Errorf("agent %q owner home: %w", selected.ID, err)
			}
		}
	}
	return &homeValues{assembler: assembler, profile: profile}, nil
}

type homeAssembly interface {
	Assembler() *capability.Assembler
	Profile() applicationMemory
}

type homeValues struct {
	assembler *capability.Assembler
	profile   applicationMemory
}

func (v *homeValues) Assembler() *capability.Assembler { return v.assembler }

func (v *homeValues) Profile() applicationMemory { return v.profile }
