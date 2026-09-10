package fleetlab

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
)

func (h *Hub) prepare(ctx context.Context) error {
	root, err := moduleRootContext(ctx)
	if err != nil {
		return err
	}
	for name, pkg := range map[string]string{"steve": "./cmd/steve", "fleet": "./e2e/fleet", "labagent": "./e2e/fleetlab/agent"} {
		cmd := exec.CommandContext(ctx, "go", "build", "-o", filepath.Join(h.Dir, name), pkg)
		cmd.Dir = root
		cmd.Env = append(os.Environ(), "CGO_ENABLED=0", "GOOS=linux", "GOARCH="+runtime.GOARCH)
		if out, err := cmd.CombinedOutput(); err != nil {
			return fmt.Errorf("build %s: %w\n%s", name, err, out)
		}
	}
	for name, content := range map[string]string{
		"scratch/kvtool/go.mod":    "module example.invalid/kvtool\n\ngo 1.24\n",
		"scratch/kvtool/main.go":   "package main\nimport \"fmt\"\nfunc main() { fmt.Println(\"kvtool\") }\n",
		"scratch/kvtool/README.md": "# kvtool\n\nA tiny acceptance fixture.\n",
		"account/.gitconfig":       "[user]\n name = Steve Lab\n email = lab@steve.invalid\n",
	} {
		path := filepath.Join(h.Dir, name)
		if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
			return err
		}
		if err := os.WriteFile(path, []byte(content), 0600); err != nil {
			return err
		}
	}
	for _, args := range [][]string{{"init", "-b", "main"}, {"add", "."}, {"-c", "user.name=Steve Lab", "-c", "user.email=lab@steve.invalid", "commit", "-m", "Seed the acceptance project"}} {
		cmd := exec.CommandContext(ctx, "git", args...)
		cmd.Dir = h.ProjectDir
		cmd.Env = h.baseEnvironment()
		if out, err := cmd.CombinedOutput(); err != nil {
			return fmt.Errorf("seed project: %w\n%s", err, out)
		}
	}
	return nil
}

func (h *Hub) config() (string, error) {
	nodes := map[string]any{}
	for name, n := range h.lab.nodes {
		nodes[name] = map[string]any{"addr": n.Addr, "token": n.Token}
	}
	conf := map[string]any{
		"gateway": map[string]any{"hub_id": "lab-" + token(12), "owner_id": "lab-owner",
			"state_path": filepath.Join(h.Dir, "state", "state.json"), "home_path": filepath.Join(h.Dir, "home"),
			"read_model_addr": "127.0.0.1:0", "read_model_token": h.Token, "default_project": "scratch",
			"prompt_timeout": "2m", "task_max_turns": 40, "task_max_elapsed": "15m", "direct_transfer": true},
		"harnesses": map[string]any{"mock": map[string]any{"command": filepath.Join(h.Dir, "labagent"), "permission": "auto"}},
		"agents": map[string]any{
			"coordinator": map[string]any{"harness": "mock", "default": true},
			"builder":     map[string]any{"harness": "mock", "node": "node-b", "requires": []string{"build"}},
			"writer":      map[string]any{"harness": "mock", "node": "node-a", "requires": []string{"docs"}},
		}, "nodes": nodes, "projects": map[string]any{"scratch": map[string]any{"home": map[string]any{"path": h.ProjectDir}}},
	}
	raw, err := json.MarshalIndent(conf, "", "  ")
	if err != nil {
		return "", err
	}
	path := filepath.Join(h.Dir, "hub.json")
	return path, os.WriteFile(path, raw, 0600)
}
