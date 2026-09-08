package admin

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"time"

	"github.com/gopact-ai/steve/internal/attempt"
	"github.com/gopact-ai/steve/internal/config"
	"github.com/gopact-ai/steve/internal/consoleapi"
	"github.com/gopact-ai/steve/internal/node"
	"github.com/gopact-ai/steve/internal/nodewire"
	"github.com/gopact-ai/steve/internal/project"
)

// changeProjects is the single management commit protocol. Configuration owns
// declarations; the ledger is reconciled from the complete committed candidate.
func (a *Service) changeProjects(ctx context.Context, mutate func(*config.Config) error) error {
	a.Mu.Lock()
	defer a.Mu.Unlock()
	ConfigMu.RLock()
	candidate := config.CloneProjects(a.Cfg)
	ConfigMu.RUnlock()
	if err := candidate.CheckFileRevision(a.Path); err != nil {
		return err
	}
	controller := config.ProjectController{Store: a.Projects}
	if err := controller.Ensure(ctx, candidate); err != nil {
		return err
	}
	if err := mutate(candidate); err != nil {
		return err
	}
	desired, _, err := config.ProjectDeclarations(candidate)
	if err != nil {
		return err
	}
	release, err := a.holdChangedWorkspaces(ctx, desired)
	if err != nil {
		return err
	}
	defer release()
	err = controller.Commit(ctx, candidate, func() error {
		ConfigMu.Lock()
		defer ConfigMu.Unlock()
		saveErr := a.PersistConfig(candidate)
		if saveErr == nil || config.Committed(saveErr) {
			a.Cfg.Projects = candidate.Projects
		}
		return saveErr
	})
	var pending *config.ProjectionPendingError
	if !errors.As(err, &pending) && (err == nil || config.Committed(err)) {
		if a.Repos != nil {
			a.Repos.wake()
		}
		a.startProjectClonesLocked(ctx)
	}
	return err
}

func (a *Service) holdChangedWorkspaces(ctx context.Context, desired []project.Project) (func(), error) {
	if a.Attempts == nil {
		return func() {}, nil
	}
	wanted := map[string]project.Workspace{}
	for _, p := range desired {
		for _, ws := range p.Workspaces() {
			wanted[ws.ID] = ws
		}
	}
	current, err := a.Projects.List(ctx)
	if err != nil {
		return nil, err
	}
	var releases []func()
	release := func() {
		for i := len(releases) - 1; i >= 0; i-- {
			releases[i]()
		}
	}
	for _, p := range current {
		for _, ws := range p.Workspaces() {
			if next, ok := wanted[ws.ID]; ok && next.Node == ws.Node && next.Path == ws.Path {
				continue
			}
			unlock, err := a.Attempts.Hold(ctx, a.Fleet.RegionOf(ws.Node), ws.ID, "console")
			if err != nil {
				release()
				var busy attempt.Busy
				if errors.As(err, &busy) {
					return nil, fmt.Errorf("%w：工作区有回合在运行（%s）", consoleapi.ErrBusy, busy.Holder)
				}
				return nil, err
			}
			releases = append(releases, unlock)
		}
	}
	return release, nil
}

func (a *Service) AddProject(ctx context.Context, req consoleapi.AddProjectRequest) error {
	id := strings.TrimSpace(req.ID)
	if !NameShape.MatchString(id) {
		return fmt.Errorf("项目名只能是小写字母、数字、点、下划线、连字符")
	}
	if id == HomeProjectID {
		return fmt.Errorf("%s 是 Steve 自己的家，不能再声明", id)
	}
	path := strings.TrimSpace(req.Path)
	if path == "" || !(strings.HasPrefix(path, "/") || strings.HasPrefix(path, "~")) {
		return errors.New("目录要写绝对路径")
	}
	nodeKey := a.nodeKey(req.Node)
	if (nodeKey == "" || a.ClusterMode && nodeKey == NodeName()) && strings.HasPrefix(path, "~") {
		if home, err := os.UserHomeDir(); err == nil {
			path = filepath.Join(home, strings.TrimPrefix(strings.TrimPrefix(path, "~"), "/"))
		}
	}
	level := project.Level(req.Level).OrDefault()
	repo := project.RepoMode(req.Repo)
	if repo == "" {
		repo = project.RepoInPlace
	}
	item := config.Project{Home: config.ProjectHome{Node: nodeKey, Path: path}, Level: string(level), Repo: string(repo)}
	return a.changeProjects(ctx, func(candidate *config.Config) error {
		if old, exists := candidate.Projects[id]; exists {
			if reflect.DeepEqual(old, item) {
				return nil
			}
			return fmt.Errorf("项目 %s 已经存在", id)
		}
		candidate.Projects[id] = item
		return nil
	})
}

// inspect refuses an unreachable machine instead of treating unknown contents
// as an existing directory that is safe to adopt.
func (a *Service) inspect(ctx context.Context, nodeKey, path string) ([]nodewire.Repo, error) {
	if nodeKey == "" {
		return node.InspectRepos(ctx, path), nil
	}
	ictx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	return a.Nodes.Inspect(ictx, nodeKey, path)
}

func (a *Service) AddWorkspace(ctx context.Context, projectID string, req consoleapi.AddWorkspaceRequest) error {
	nodeKey, path := a.nodeKey(req.Node), strings.TrimSpace(req.Path)
	if path == "" {
		return errors.New("目录不能为空")
	}
	if nodeKey == "" && strings.HasPrefix(path, "~") {
		if home, err := os.UserHomeDir(); err == nil {
			path = filepath.Join(home, strings.TrimPrefix(strings.TrimPrefix(path, "~"), "/"))
		}
	}
	origin := req.Origin
	if origin == "" || origin == "adopt" {
		origin = string(project.OriginAdopted)
	} else if origin == "clone" {
		origin = string(project.OriginCloned)
	} else {
		return fmt.Errorf("未知工作区来源 %q", origin)
	}
	return a.changeProjects(ctx, func(candidate *config.Config) error {
		item, exists := candidate.Projects[projectID]
		if !exists {
			return fmt.Errorf("没有叫 %q 的项目", projectID)
		}
		for _, ws := range item.Workspaces {
			if ws.Node == nodeKey {
				if ws.Path == path && (ws.Origin == origin || (ws.Origin == "" && origin == string(project.OriginAdopted))) {
					return nil
				}
				return fmt.Errorf("项目 %s 在 %s 已有副本", projectID, nodeKey)
			}
		}
		found, err := a.inspect(ctx, nodeKey, path)
		if err != nil {
			return fmt.Errorf("检查工作区失败：%w", err)
		}
		missing := len(found) == 1 && found[0].Missing
		ws := config.ProjectWorkspace{Node: nodeKey, Path: path, Origin: origin}
		if origin == string(project.OriginCloned) {
			p, ok, err := a.Projects.Get(ctx, projectID)
			if err != nil {
				return err
			}
			if !ok {
				return fmt.Errorf("没有叫 %q 的项目", projectID)
			}
			ws.Source = a.cloneSource(p)
			if ws.Source == "" {
				return fmt.Errorf("项目 %s 没有可以克隆的来源", projectID)
			}
			if !missing {
				return fmt.Errorf("%s 上已经有 %s；克隆需要尚不存在的目录", nodewire.Place(nodeKey), path)
			}
		} else if missing {
			return fmt.Errorf("%s 上没有目录 %s，无法认领", nodewire.Place(nodeKey), path)
		}
		item.Workspaces = append(item.Workspaces, ws)
		candidate.Projects[projectID] = item
		return nil
	})
}

func (a *Service) RemoveWorkspace(ctx context.Context, projectID, nodeName string) error {
	nodeKey := a.nodeKey(nodeName)
	return a.changeProjects(ctx, func(candidate *config.Config) error {
		item, exists := candidate.Projects[projectID]
		if !exists {
			return fmt.Errorf("没有叫 %q 的项目", projectID)
		}
		for i, ws := range item.Workspaces {
			if ws.Node == nodeKey {
				item.Workspaces = append(item.Workspaces[:i:i], item.Workspaces[i+1:]...)
				candidate.Projects[projectID] = item
				return nil
			}
		}
		return nil
	})
}

func (a *Service) RemoveProject(ctx context.Context, id string) error {
	if id == HomeProjectID {
		return fmt.Errorf("%s 是 Steve 自己的家，不能移除", id)
	}
	return a.changeProjects(ctx, func(candidate *config.Config) error {
		if id == candidate.Gateway.DefaultProject {
			return fmt.Errorf("%s 是默认项目，不能移除", id)
		}
		delete(candidate.Projects, id)
		return nil
	})
}

// cloneSource is what a copy of the project is cloned from: the external
// remote it declares, else the remote of the repository its home is.
func (a *Service) cloneSource(p project.Project) string {
	if p.ExternalRemote != "" {
		return p.ExternalRemote
	}
	if a.Repos == nil {
		return ""
	}
	for _, r := range a.Repos.Get(p.Canonical().ID) {
		if r.Path == "." && r.Remote != "" {
			return r.Remote
		}
	}
	return ""
}

// ResumeProjectCopies is called after startup reconciliation and dependency wiring.
func (a *Service) ResumeProjectCopies(ctx context.Context) error {
	a.Mu.Lock()
	defer a.Mu.Unlock()
	ops, err := a.Projects.CloneOperations(ctx)
	if err != nil {
		return err
	}
	for _, op := range ops {
		if op.State == project.CloneRunning {
			cleanup, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			err := a.Projects.FinishClone(cleanup, op, false, "hub restarted without a confirmed clone completion", errors.New("clone completion was not recorded before restart"))
			cancel()
			if err != nil {
				return err
			}
		}
	}
	if err := (config.ProjectController{Store: a.Projects}).Ensure(ctx, a.Cfg); err != nil {
		return err
	}
	a.startProjectClonesLocked(ctx)
	return nil
}

func (a *Service) startProjectClonesLocked(ctx context.Context) {
	if a.Nodes == nil && a.cloneFiles == nil {
		return
	}
	list, err := a.Projects.List(ctx)
	if err != nil {
		return
	}
	if a.cloning == nil {
		a.cloning = map[string]bool{}
	}
	for _, p := range list {
		for nodeKey, c := range p.Copies {
			if c.Origin != project.OriginCloned || c.State != project.CopyProvisioning {
				continue
			}
			key := p.ID + "\x00" + nodeKey + "\x00" + c.Path + "\x00" + c.Source
			if a.cloning[key] {
				continue
			}
			ttl := a.cloneLeaseTTL
			if ttl <= 0 {
				ttl = 30 * time.Second
			}
			region := ""
			if a.Fleet != nil {
				region = a.Fleet.RegionOf(nodeKey)
			}
			op, err := a.Projects.BeginClone(ctx, p.ID, c, region, "operator-config", ttl)
			if err != nil {
				slog.Warn(fmt.Sprintf("steve: clone %s not started; ownership unavailable: %v", p.ID, err), "project", p.ID)
				continue
			}
			a.cloning[key] = true
			go a.clone(op, key, ttl)
		}
	}
}

func (a *Service) clone(op project.CloneOperation, key string, ttl time.Duration) {
	timeout := a.cloneTimeout
	if timeout <= 0 {
		timeout = 10 * time.Minute
	}
	lifetime := a.Lifetime
	if lifetime == nil {
		lifetime = context.Background()
	}
	ctx, cancel := context.WithTimeout(lifetime, timeout)
	defer cancel()
	type renewal struct {
		operation project.CloneOperation
		err       error
	}
	done := make(chan struct{})
	renewed := make(chan renewal, 1)
	go func() {
		current := op
		ticker := time.NewTicker(ttl / 3)
		defer ticker.Stop()
		for {
			select {
			case <-done:
				renewed <- renewal{operation: current}
				return
			case <-ticker.C:
				renewCtx, finish := context.WithTimeout(context.Background(), ttl/3)
				err := a.Projects.RenewClone(renewCtx, &current, ttl)
				finish()
				if err != nil {
					cancel()
					renewed <- renewal{operation: current, err: err}
					return
				}
			}
		}
	}()
	files := a.cloneFiles
	if files == nil {
		files = a.Nodes.Files
	}
	_, err := files(ctx, op.Copy.Node, nodewire.FileRequest{Op: nodewire.FileClone, Path: op.Copy.Path, Source: op.Copy.Source})
	close(done)
	latest := <-renewed
	confirmed := latest.err == nil && (op.Copy.Node == "" || err == nil)
	evidence := "file operation returned successfully"
	if err != nil && op.Copy.Node == "" {
		evidence = "local synchronous file operation returned after its process ended"
	}
	if !confirmed {
		evidence = "remote completion or lease ownership could not be confirmed; physical path remains isolated"
	}
	cleanup, finish := context.WithTimeout(context.Background(), 10*time.Second)
	recordErr := a.Projects.FinishClone(cleanup, latest.operation, confirmed, evidence, errors.Join(err, latest.err))
	finish()
	a.Mu.Lock()
	defer a.Mu.Unlock()
	delete(a.cloning, key)
	if recordErr != nil {
		slog.Error(fmt.Sprintf("steve: clone %s cleanup pending; workspace remains isolated: %v", op.ID, recordErr), "clone", op.ID)
	}
	if a.Repos != nil {
		a.Repos.wake()
	}
}
