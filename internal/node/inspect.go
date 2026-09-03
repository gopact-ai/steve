package node

import (
	"context"
	"encoding/json"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/gopact-ai/steve/internal/nodewire"
)

// InspectRepos looks at a project directory the way a person opening it
// would: is it a git repository, or does it hold several? For each, the
// branch, the last commit, whether there is uncommitted work, the remote,
// and whether an AGENTS.md or CLAUDE.md is there for the agents to read.
// A directory that is not there says so, once.
func InspectRepos(ctx context.Context, root string) []nodewire.Repo {
	root = expandHome(root)
	info, err := os.Stat(root)
	if err != nil || !info.IsDir() {
		return []nodewire.Repo{{Path: ".", Missing: true}}
	}
	// A scanned directory answers with a list, empty or not; only a
	// directory nobody has looked at yet is nil.
	out := []nodewire.Repo{}
	if isRepo(root) {
		out = append(out, describeRepo(ctx, root, "."))
		return out
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		return out
	}
	for _, e := range entries {
		if !e.IsDir() || strings.HasPrefix(e.Name(), ".") {
			continue
		}
		if dir := filepath.Join(root, e.Name()); isRepo(dir) {
			out = append(out, describeRepo(ctx, dir, e.Name()))
		}
		if len(out) >= 32 {
			break
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out
}

// isRepo asks git rather than looking for a .git entry: a stray or
// broken .git is not a repository anyone can work in.
func isRepo(dir string) bool {
	if _, err := os.Stat(filepath.Join(dir, ".git")); err != nil {
		return false
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", "-C", dir, "rev-parse", "--is-inside-work-tree")
	cmd.Env = append(os.Environ(), "GIT_OPTIONAL_LOCKS=0")
	out, err := cmd.Output()
	return err == nil && strings.TrimSpace(string(out)) == "true"
}

func describeRepo(ctx context.Context, dir, rel string) nodewire.Repo {
	r := nodewire.Repo{Path: rel}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	git := func(args ...string) string {
		cmd := exec.CommandContext(ctx, "git", append([]string{"-C", dir}, args...)...)
		cmd.Env = append(os.Environ(), "GIT_OPTIONAL_LOCKS=0", "LC_ALL=C")
		raw, err := cmd.Output()
		if err != nil {
			return ""
		}
		return strings.TrimSpace(string(raw))
	}
	r.Branch = git("rev-parse", "--abbrev-ref", "HEAD")
	if line := git("log", "-1", "--format=%h%x00%s%x00%ct"); line != "" {
		parts := strings.SplitN(line, "\x00", 3)
		if len(parts) == 3 {
			r.Head, r.Subject = parts[0], parts[1]
			if secs, err := strconv.ParseInt(parts[2], 10, 64); err == nil {
				r.At = time.Unix(secs, 0).UTC()
			}
		}
	}
	r.Dirty = git("status", "--porcelain", "--untracked-files=no") != ""
	r.Remote = git("remote", "get-url", "origin")
	for _, name := range []string{"AGENTS.md", "CLAUDE.md"} {
		if _, err := os.Stat(filepath.Join(dir, name)); err == nil {
			r.AgentsMD = true
			break
		}
	}
	return r
}

func expandHome(path string) string {
	if path == "~" || strings.HasPrefix(path, "~/") {
		if home, err := os.UserHomeDir(); err == nil {
			return filepath.Join(home, strings.TrimPrefix(strings.TrimPrefix(path, "~"), "/"))
		}
	}
	return path
}

// inspect serves StreamInspect.
func (s *Server) inspect(ctx context.Context, stream *nodewire.Stream) {
	defer stream.Close()
	path := strings.TrimSpace(stream.Request().Command)
	if path == "" {
		_ = json.NewEncoder(stream).Encode(nodewire.InspectReply{Error: "a path is required"})
		return
	}
	repos := InspectRepos(ctx, path)
	if repos == nil {
		repos = []nodewire.Repo{}
	}
	if err := json.NewEncoder(stream).Encode(nodewire.InspectReply{Repos: repos}); err != nil {
		log.Printf("steve-node: inspect reply: %v", err)
	}
}
