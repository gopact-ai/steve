package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"path/filepath"

	"github.com/gopact-ai/steve/internal/config"
	"github.com/gopact-ai/steve/internal/hubid"
	"github.com/gopact-ai/steve/internal/project"
	"github.com/gopact-ai/steve/internal/transfer"
)

// migrateCmd is deliberately offline: both source and target hub services
// must be stopped. Bundles contain one project's data, never config secrets.
func migrateCmd(args []string) error {
	if len(args) == 0 {
		return errors.New("usage: steve migrate export|import --config config.json [options]")
	}
	verb := args[0]
	flags := flag.NewFlagSet("migrate "+verb, flag.ContinueOnError)
	path := flags.String("config", "config.json", "hub config")
	id := flags.String("project", "", "project id")
	target := flags.String("target-hub", "", "stable destination hub id")
	out := flags.String("out", "", "new output bundle")
	input := flags.String("in", "", "project bundle")
	source := flags.String("expected-source", "", "expected stable source hub id")
	home := flags.String("home", "", "empty destination local directory")
	evidence := flags.String("evidence", "", "operator verification that source project writers have stopped")
	snapshot := flags.String("workspace-snapshot", "", "complete local offline copy of remote canonical contents")
	if err := flags.Parse(args[1:]); err != nil {
		return err
	}
	cfg, err := config.Load(*path)
	if err != nil {
		return err
	}
	cfg.Gateway.HubID, err = hubid.Resolve(filepath.Dir(cfg.Gateway.StatePath), cfg.Gateway.HubID)
	if err != nil {
		return err
	}
	switch verb {
	case "export":
		b, err := transfer.Export(context.Background(), transfer.Options{StateDir: filepath.Dir(cfg.Gateway.StatePath), HubID: cfg.Gateway.HubID, Project: *id, TargetHub: *target, Output: *out, Evidence: *evidence, WorkspaceSnapshot: *snapshot})
		if err != nil {
			return err
		}
		fmt.Printf("project %s released to %s at epoch %d; bundle %s\n", b.Owner.Project, b.Owner.TargetHub, b.Owner.Epoch, *out)
		return nil
	case "import":
		p, err := transfer.Import(context.Background(), transfer.ImportOptions{StateDir: filepath.Dir(cfg.Gateway.StatePath), HubID: cfg.Gateway.HubID, ExpectedSource: *source, TargetLevel: cfg.HubLevel(), Home: *home, Input: *input, Finalize: func(p project.Project) error { return recordMigratedProject(*path, cfg, p) }})
		if err != nil {
			return err
		}

		fmt.Printf("project %s active on %s at %s; prior model sessions require a fresh session\n", p.ID, cfg.Gateway.HubID, p.Home.Path)
		return nil
	default:
		return fmt.Errorf("unknown migration action %s", verb)
	}
}
func recordMigratedProject(path string, cfg *config.Config, p project.Project) error {
	cfg = config.CloneProjects(cfg)
	if cfg.Projects == nil {
		cfg.Projects = map[string]config.Project{}
	}
	desired := config.Project{Home: config.ProjectHome{Path: p.Home.Path}, Level: string(p.Level), Repo: string(p.Repo), Skills: p.Skills, ExternalRemote: p.ExternalRemote, DefaultRole: string(p.DefaultRole)}
	if len(p.ConfigGrants) > 0 {
		desired.Grants = map[string]string{}
		for principal, role := range p.ConfigGrants {
			desired.Grants[principal] = string(role)
		}
	}
	if old, exists := cfg.Projects[p.ID]; exists {
		a, _ := json.Marshal(old)
		b, _ := json.Marshal(desired)
		if string(a) != string(b) {
			return errors.New("target config already declares a conflicting project")
		}
		return nil
	}
	cfg.Projects[p.ID] = desired
	return config.Save(path, cfg)
}
