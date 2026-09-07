package main

import (
	"context"
	"errors"
	"fmt"

	"github.com/gopact-ai/steve/internal/consoleapi"
	"github.com/gopact-ai/steve/internal/home"
	"github.com/gopact-ai/steve/internal/memory"
)

// Home is Steve's own directory as the page shows it.
func (a *fleetAdmin) Home(_ context.Context) (consoleapi.HomeView, error) {
	if a.homePath == "" {
		return consoleapi.HomeView{}, errors.New("没有配置档案目录（gateway.home_path）")
	}
	files, err := home.Files(a.homePath)
	if err != nil {
		return consoleapi.HomeView{}, err
	}
	view := consoleapi.HomeView{Path: a.homePath, TotalBudget: home.BudgetTotal, Files: []consoleapi.HomeFile{}, Warnings: []string{}}
	for _, f := range files {
		if f.Name == home.FileMemory && a.memory != nil && !f.Missing {
			if text, err := a.memory.Text(context.Background(), memory.Global); err == nil {
				f.Text = text
			}
		}
		view.Files = append(view.Files, consoleapi.HomeFile{Name: f.Name, Text: f.Text, Bytes: len([]byte(f.Text)), Budget: f.Budget, Template: f.Template, Missing: f.Missing})
	}
	dir := home.Dir{Path: a.homePath}
	if snap, err := dir.Load(home.ModeOwner); err == nil {
		view.OwnerBytes = len([]byte(snap.Identity))
		view.Warnings = append(view.Warnings, snap.Warnings...)
	} else {
		view.Warnings = append(view.Warnings, err.Error())
	}
	if snap, err := dir.Load(home.ModeGuest); err == nil {
		view.GuestBytes = len([]byte(snap.Identity))
	}
	view.Projects = []consoleapi.ProjectMemory{}
	if a.memory != nil && a.projects != nil {
		view.Audit = a.memory.AuditPath()
		list, err := a.projects.List(context.Background())
		if err != nil {
			view.Warnings = append(view.Warnings, err.Error())
		}
		for _, p := range list {
			if p.ID == homeProjectID {
				continue
			}
			scope := memory.ProjectScope(p.ID)
			text, err := a.memory.Text(context.Background(), scope)
			if err != nil {
				view.Warnings = append(view.Warnings, p.ID+": "+err.Error())
				continue
			}
			items, _ := a.memory.List(context.Background(), scope)
			view.Projects = append(view.Projects, consoleapi.ProjectMemory{ID: p.ID, Path: a.memory.Where(scope), Text: text, Bytes: len([]byte(text)), Budget: memory.Budget(scope), Facts: len(items)})
		}
	}
	return view, nil
}

// SetProjectMemory rewrites one project's memory whole, through the
// same lock the agents' writes take.
func (a *fleetAdmin) SetProjectMemory(ctx context.Context, id, text string) error {
	if a.memory == nil {
		return errors.New("memory is not wired")
	}
	if _, ok, err := a.projects.Get(ctx, id); err != nil {
		return err
	} else if !ok {
		return fmt.Errorf("no project %q", id)
	}
	return a.memory.Replace(ctx, memory.ProjectScope(id), text, memory.Actor{By: "console"})
}

// SetHomeFile rewrites one of the three. The next turn reads it; a
// session already open is told its instructions changed.
func (a *fleetAdmin) SetHomeFile(ctx context.Context, name, text string) error {
	if a.homePath == "" {
		return errors.New("没有配置档案目录（gateway.home_path）")
	}
	if name == home.FileMemory && a.memory != nil {
		// The global memory is a scope: same lock, ids kept, audited.
		return a.memory.Replace(ctx, memory.Global, text, memory.Actor{By: "console"})
	}
	return home.Write(a.homePath, name, text)
}
