package admin

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/gopact-ai/steve/internal/agent"
	"github.com/gopact-ai/steve/internal/config"
	"github.com/gopact-ai/steve/internal/datalevel"
	"github.com/gopact-ai/steve/internal/project"
)

// nativeImportProject adopts only the original directory from a node-validated
// import receipt. The config commit protocol serializes concurrent imports and
// preserves every existing workspace's ownership and access policy.
func (a *Service) nativeImportProject(ctx context.Context, name string, target config.Node, selected agent.Agent, workdir string) (string, error) {
	if a.Projects == nil {
		return "", errors.New("历史会话导入需要项目服务")
	}
	if !filepath.IsAbs(workdir) || strings.ContainsRune(workdir, 0) {
		return "", errors.New("历史会话没有有效的原工作目录")
	}
	workdir = filepath.Clean(workdir)
	if err := ctx.Err(); err != nil {
		return "", err
	}
	a.ConfigStore.rlock()
	err := checkNativeImportTarget(a.cfg(), name, target, selected)
	a.ConfigStore.runlock()
	if err != nil {
		return "", err
	}
	found, err := a.inspect(ctx, name, workdir)
	if err != nil {
		return "", fmt.Errorf("检查历史会话原工作目录失败：%w", err)
	}
	if len(found) == 1 && found[0].Missing {
		return "", errors.New("历史会话的原工作目录已不存在，无法接续会话")
	}
	var id string
	reused := errors.New("existing native import workspace")
	err = a.changeProjects(ctx, func(candidate *config.Config) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		a.ConfigStore.rlock()
		err := checkNativeImportTarget(a.cfg(), name, target, selected)
		a.ConfigStore.runlock()
		if err != nil {
			return err
		}
		list, err := a.Projects.List(ctx)
		if err != nil {
			return err
		}
		for _, p := range list {
			for _, ws := range p.Workspaces() {
				if ws.Node != name || filepath.Clean(ws.Path) != workdir {
					continue
				}
				if id != "" && id != p.ID {
					return errors.New("多个项目匹配历史会话的原工作目录，请选择项目")
				}
				id = p.ID
			}
		}
		if id != "" {
			return reused
		}
		id = nativeImportProjectName(workdir)
		if _, exists := candidate.Projects[id]; exists || id == HomeProjectID {
			sum := sha256.Sum256([]byte(name + "\x00" + workdir))
			id += "-" + hex.EncodeToString(sum[:4])
		}
		if _, exists := candidate.Projects[id]; exists {
			return fmt.Errorf("自动关联项目名称 %s 已被其他目录占用", id)
		}
		candidate.Projects[id] = config.Project{Home: config.ProjectHome{Node: name, Path: workdir}, Level: string(datalevel.Internal), Repo: string(project.RepoInPlace)}
		return nil
	})
	if errors.Is(err, reused) {
		err = nil
	}
	return id, err
}

func checkNativeImportTarget(cfg *config.Config, name string, target config.Node, selected agent.Agent) error {
	if err := checkAgentNodeTarget(cfg, name, target); err != nil {
		return err
	}
	current, exists := cfg.Agents[selected.ID]
	if !exists || current.Node != name || current.Harness != selected.Harness {
		return errors.New("Agent 配置已变化，请重新选择")
	}
	return nil
}

func nativeImportProjectName(workdir string) string {
	base := strings.ToLower(filepath.Base(workdir))
	var slug strings.Builder
	for _, r := range base {
		if r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '.' || r == '_' || r == '-' {
			slug.WriteRune(r)
		} else {
			slug.WriteByte('-')
		}
		if slug.Len() >= 32 {
			break
		}
	}
	prefix := strings.Trim(slug.String(), ".-_")
	if prefix == "" {
		prefix = "project"
	}
	return prefix
}
