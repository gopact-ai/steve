package admin

import (
	"context"
	"slices"
	"strings"

	"github.com/gopact-ai/steve/internal/config"
	"github.com/gopact-ai/steve/internal/consoleapi"
)

func (a *Service) pluginResources(ctx context.Context, kind string) ([]consoleapi.PluginResourceView, error) {
	if a.PluginLibrary == nil {
		return nil, nil
	}
	ConfigMu.RLock()
	items := config.ClonePluginInstallations(a.Cfg.Plugins)
	ConfigMu.RUnlock()
	var resources []consoleapi.PluginResourceView
	for id, item := range items {
		if len(item.Projects) == 0 {
			continue
		}
		record, err := a.PluginLibrary.Record(ctx, item.Projects[0], item.Digest)
		if err != nil {
			return nil, err
		}
		names := []string{}
		if kind == "skill" {
			for name := range record.Manifest.Skills {
				names = append(names, name)
			}
		} else {
			for name := range record.Manifest.MCP {
				names = append(names, name)
			}
		}
		for _, name := range names {
			resources = append(resources, consoleapi.PluginResourceView{Installation: id, PackageID: item.PackageID, Version: record.Manifest.Version, Name: name, Enabled: item.Enabled, Projects: slices.Clone(item.Projects)})
		}
	}
	slices.SortFunc(resources, func(a, b consoleapi.PluginResourceView) int {
		return strings.Compare(a.Installation+"/"+a.Name, b.Installation+"/"+b.Name)
	})
	return resources, nil
}
