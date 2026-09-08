package admin

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/gopact-ai/steve/internal/consoleapi"
	"github.com/gopact-ai/steve/internal/node"
	"github.com/gopact-ai/steve/internal/nodewire"
	"github.com/gopact-ai/steve/internal/skills"
	"github.com/gopact-ai/steve/internal/text"
)

// Skills is the skills page: what is found where, what is on, who pins
// what by path, and which machines hold the current bundle.
func (a *Service) Skills(ctx context.Context) (consoleapi.SkillsView, error) {
	if a.LiveSkills == nil || a.LiveSkills.Map == nil {
		return consoleapi.SkillsView{}, errors.New("技能没有配置")
	}
	view := consoleapi.SkillsView{Fingerprint: a.LiveSkills.Map.Fingerprint(), SearchPaths: a.LiveSkills.Map.SearchPaths(), BuiltinRoot: a.LiveSkills.Map.BuiltinRootPath(), Skills: []consoleapi.SkillView{}, Nodes: []consoleapi.SkillNode{}, Sources: []consoleapi.SkillSource{}}
	// A skill from a source resolves into its clone; that, not the name,
	// says which source it came from — a skill of the same name from
	// the user's directory or the shipped set is not the source's.
	type clone struct{ slug, root, dir string }
	var clones []clone
	for _, src := range a.LiveSkills.Map.Sources() {
		view.Sources = append(view.Sources, skillSource(src))
		dir := src.Dir
		if real, err := filepath.EvalSymlinks(dir); err == nil {
			dir = real
		}
		clones = append(clones, clone{src.Slug, src.Root, dir})
	}
	if view.SearchPaths == nil {
		view.SearchPaths = []string{}
	}
	enabled, err := a.LiveSkills.Map.Enabled()
	if err != nil {
		return view, err
	}
	on := map[string]bool{}
	for _, ref := range enabled {
		on[ref.Name] = true
	}
	available, err := a.LiveSkills.Map.Available()
	if err != nil {
		return view, err
	}
	// Who asks for a skill by path: an agent's or a project's pinned
	// skill sources are directories, matched against the skill's.
	byPath := map[string]*consoleapi.SkillView{}
	for _, ref := range available {
		d := skills.Describe(ref.Path)
		item := consoleapi.SkillView{Name: ref.Name, Path: ref.Path, Root: filepath.Dir(ref.Path), Title: d.Title, Description: d.Description, Enabled: on[ref.Name], Builtin: view.BuiltinRoot != "" && filepath.Dir(ref.Path) == view.BuiltinRoot, Agents: []string{}, Projects: []string{}}
		for _, c := range clones {
			if strings.HasPrefix(ref.Path, c.dir+string(filepath.Separator)) {
				item.Source, item.Root = c.slug, c.root
			}
		}
		view.Skills = append(view.Skills, item)
		byPath[filepath.Clean(ref.Path)] = &view.Skills[len(view.Skills)-1]
	}
	if a.Catalog != nil {
		for _, ag := range a.Catalog.List() {
			for _, src := range ag.Skills {
				if item, ok := byPath[filepath.Clean(src)]; ok {
					item.Agents = append(item.Agents, ag.ID)
				}
			}
		}
	}
	if a.Projects != nil {
		if list, err := a.Projects.List(ctx); err == nil {
			for _, p := range list {
				for _, src := range p.Skills {
					if item, ok := byPath[filepath.Clean(src)]; ok {
						item.Projects = append(item.Projects, p.ID)
					}
				}
			}
		}
	}
	want := a.Shipper.hash()
	for _, name := range a.Nodes.Names() {
		item := consoleapi.SkillNode{Name: name}
		if adv, err := a.Nodes.Advert(ctx, name); err == nil {
			item.Up = true
			item.Takes = slices.Contains(adv.Features, nodewire.FeatureSkills)
			item.Synced = want != "" && adv.Skills == want
		}
		view.Nodes = append(view.Nodes, item)
	}
	return view, nil
}

// SkillContent is one skill's SKILL.md.
func (a *Service) SkillContent(_ context.Context, name string) (consoleapi.SkillDoc, error) {
	if a.LiveSkills == nil || a.LiveSkills.Map == nil {
		return consoleapi.SkillDoc{}, errors.New("技能没有配置")
	}
	available, err := a.LiveSkills.Map.Available()
	if err != nil {
		return consoleapi.SkillDoc{}, err
	}
	for _, ref := range available {
		if ref.Name == name {
			content, err := skills.Content(ref.Path)
			if err != nil {
				return consoleapi.SkillDoc{}, err
			}
			return consoleapi.SkillDoc{Name: ref.Name, Path: ref.Path, Content: content}, nil
		}
	}
	return consoleapi.SkillDoc{}, fmt.Errorf("没有叫 %q 的技能", name)
}

// MachineSkills is what every machine last said its AI tools have of
// their own: the hub's own scan, and each node's advert as the registry
// holds it. Nothing is asked here; the refresh loop asks every minute,
// and RefreshMachineSkills asks now.
func (a *Service) MachineSkills(ctx context.Context) []consoleapi.MachineSkills {
	have := map[string]bool{}
	if a.LiveSkills != nil && a.LiveSkills.Map != nil {
		if avail, err := a.LiveSkills.Map.Available(); err == nil {
			for _, ref := range avail {
				have[ref.Name] = true
			}
		}
	}
	found := func(name string, hub bool, own []nodewire.OwnSkill, err error) consoleapi.MachineSkills {
		item := consoleapi.MachineSkills{Name: nodewire.Place(name), Hub: hub, Skills: []consoleapi.FoundSkill{}}
		if err != nil {
			item.Error = text.Clip(strings.TrimSpace(err.Error()), 200)
			return item
		}
		item.Up = true
		for _, f := range own {
			item.Skills = append(item.Skills, consoleapi.FoundSkill{Name: f.Name, Path: f.Path, Title: f.Title, Description: f.Description, Loaded: have[f.Name]})
		}
		return item
	}
	out := []consoleapi.MachineSkills{found("", true, node.OwnSkills(5*time.Minute), nil)}
	for _, name := range a.Nodes.Names() {
		adv, err := a.Nodes.Advert(ctx, name)
		out = append(out, found(name, false, adv.OwnSkills, err))
	}
	return out
}

// RefreshMachineSkills asks every machine to look again, at once, and
// returns what they said; one that is down or slow says so.
func (a *Service) RefreshMachineSkills(ctx context.Context) []consoleapi.MachineSkills {
	ownSkillsRescan()
	names := a.Nodes.Names()
	var wg sync.WaitGroup
	for _, name := range names {
		wg.Add(1)
		go func(name string) {
			defer wg.Done()
			rctx, cancel := context.WithTimeout(ctx, 20*time.Second)
			defer cancel()
			_, _ = a.Nodes.Refresh(rctx, name)
		}(name)
	}
	wg.Wait()
	return a.MachineSkills(ctx)
}

func ownSkillsRescan() { node.OwnSkills(0) }

// ImportSkill loads a machine's skill onto the hub, into the owner's own
// skills directory, where it is a hub skill like any other — not enabled
// until the owner says so.
func (a *Service) ImportSkill(ctx context.Context, nodeName, path string) (string, error) {
	if a.LiveSkills == nil || a.LiveSkills.Map == nil {
		return "", errors.New("技能没有配置")
	}
	path = strings.TrimSpace(path)
	if path == "" || !strings.HasPrefix(path, "/") {
		return "", fmt.Errorf("目录 %q 不是绝对路径", path)
	}
	name := filepath.Base(path)
	dest := filepath.Join(a.LiveSkills.Map.UserDir(), name)
	if _, err := os.Lstat(dest); err == nil {
		return "", fmt.Errorf("hub 上已经有叫 %s 的技能（%s）", name, dest)
	}
	sctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	encoded, err := a.Nodes.Files(sctx, a.nodeKey(nodeName), nodewire.FileRequest{Op: nodewire.FileImportSkill, Path: path})
	if err != nil {
		return "", fmt.Errorf("从 %s 取 %s：%s", nodewire.Place(a.nodeKey(nodeName)), path, text.Clip(strings.TrimSpace(err.Error()), 200))
	}
	if err := skills.UnpackImport(encoded, dest); err != nil {
		return "", err
	}
	return name, nil
}

func skillSource(s skills.Source) consoleapi.SkillSource {
	out := consoleapi.SkillSource{Slug: s.Slug, URL: s.URL, Ref: s.Ref, Subdir: s.Subdir, Root: s.Root, Head: s.Head, FetchedAt: s.FetchedAt, Skills: s.Skills, Error: s.Error}
	if out.Skills == nil {
		out.Skills = []string{}
	}
	return out
}

// AddSkillSource installs a git repository of skills; nothing is
// enabled by it, so nothing restarts.
func (a *Service) AddSkillSource(ctx context.Context, spec string) (consoleapi.SkillSource, error) {
	if a.LiveSkills == nil || a.LiveSkills.Map == nil {
		return consoleapi.SkillSource{}, errors.New("技能没有配置")
	}
	src, err := a.LiveSkills.AddSource(ctx, spec)
	if err != nil {
		return consoleapi.SkillSource{}, err
	}
	return skillSource(src), nil
}

// UpdateSkillSources fetches every source again; the text of enabled
// skills may change, so it takes the lock and restarts the AI tools.
func (a *Service) UpdateSkillSources(ctx context.Context) ([]consoleapi.SkillSource, error) {
	var out []consoleapi.SkillSource
	err := a.withSkillsLock(func() error {
		updated, err := a.LiveSkills.UpdateSources(ctx)
		for _, s := range updated {
			out = append(out, skillSource(s))
		}
		return err
	})
	return out, err
}

// RemoveSkillSource forgets a source; skills enabled from it go with it.
func (a *Service) RemoveSkillSource(_ context.Context, slug string) error {
	return a.withSkillsLock(func() error { return a.LiveSkills.RemoveSource(slug) })
}

// withSkillsLock runs a change to the skills the way the chat verb does:
// not while a turn runs, since the change restarts the AI tools.
func (a *Service) withSkillsLock(op func() error) error {
	if a.LiveSkills == nil || a.LiveSkills.Map == nil {
		return errors.New("技能没有配置")
	}
	if a.Coordinator != nil {
		release, ok := a.Coordinator.SkillsLock()
		if !ok {
			return fmt.Errorf("%w：有回合在跑，改技能会重启 AI 工具，等它结束再改", consoleapi.ErrBusy)
		}
		defer release()
	}
	return op()
}

// SetSkill turns a skill on or off for every agent. Agents in flight
// keep their session; the next session opens with the new set, and the
// changed fingerprint tells the owner to /new.
func (a *Service) SetSkill(_ context.Context, name string, enabled bool) error {
	return a.withSkillsLock(func() error {
		if enabled {
			return a.LiveSkills.Enable(name)
		}
		return a.LiveSkills.Disable(name)
	})
}

// AddSkillPath adds a directory to look for skills in.
func (a *Service) AddSkillPath(_ context.Context, path string) error {
	if a.LiveSkills == nil || a.LiveSkills.Map == nil {
		return errors.New("技能没有配置")
	}
	path = strings.TrimSpace(path)
	if path == "" {
		return errors.New("目录不能为空")
	}
	if strings.HasPrefix(path, "~") {
		if home, err := os.UserHomeDir(); err == nil {
			path = filepath.Join(home, strings.TrimPrefix(strings.TrimPrefix(path, "~"), "/"))
		}
	}
	if info, err := os.Stat(path); err != nil || !info.IsDir() {
		return fmt.Errorf("hub 上没有目录 %s", path)
	}
	return a.LiveSkills.AddPath(path)
}

// RemoveSkillPath stops looking in a directory; skills enabled from it
// go with it, so it takes the lock.
func (a *Service) RemoveSkillPath(_ context.Context, path string) error {
	path = strings.TrimSpace(path)
	if path == "" {
		return errors.New("目录不能为空")
	}
	return a.withSkillsLock(func() error { return a.LiveSkills.RemovePath(path) })
}
