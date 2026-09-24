// Resolving a merge conflict by hand: the half-merged tree git kept is
// checked out, the person's own text replaces the marked files, and what
// comes out lands the ordinary way. It is the same shape as handing the
// conflict to an agent, with a person writing the resolution instead.

package artifact

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/gopact-ai/steve/internal/project"
)

// Edit is one file of a conflict as a person resolved it.
type Edit struct {
	Path string `json:"path"`
	Text string `json:"text"`
}

// stillMarked says a file is the half-merged text sent back rather than a
// resolution of it. git writes its markers at the start of a line, so the
// check is by line: a resolution may legitimately mention "=======" in the
// middle of one.
func stillMarked(text string) bool {
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSuffix(line, "\r")
		if strings.HasPrefix(line, "<<<<<<< ") || strings.HasPrefix(line, ">>>>>>> ") || line == "=======" || strings.HasPrefix(line, "||||||| ") {
			return true
		}
	}
	return false
}

// ErrSealedByHand says the project's data never leaves its home machine,
// so the hub cannot check the conflict out to be edited here.
var ErrSealedByHand = errors.New("this project's data stays on its home machine, so its conflicts cannot be edited from the console")

// ResolveByHand writes a person's resolution over the half-merged tree and
// lands it. The result moves the canonical name, which is what lets the
// original queued result merge cleanly on the next pass — exactly what an
// agent's resolution does, so nothing downstream has to tell them apart.
func (s *Store) ResolveByHand(ctx context.Context, p project.Project, stuck Stuck, edits []Edit, by string) (Landing, error) {
	if stuck.Marked == "" {
		return Landing{}, errors.New("this conflict left no half-merged tree to edit")
	}
	if metadataOnly(p) {
		return Landing{}, ErrSealedByHand
	}
	if len(edits) == 0 {
		return Landing{}, errors.New("no file was resolved")
	}
	wanted, err := resolvedFiles(stuck, edits)
	if err != nil {
		return Landing{}, err
	}
	if err := s.BringHome(ctx, p, stuck.Marked); err != nil {
		return Landing{}, err
	}
	ws, err := s.Materialize(ctx, project.Request{Project: p.ID, Isolated: true, Base: stuck.Marked, Owner: "resolve-" + short(stuck.Artifact)})
	if err != nil {
		return Landing{}, err
	}
	defer func() {
		if err := s.Discard(ctx, ws); err != nil {
			// A worktree left behind costs disk, not correctness.
			_ = err
		}
	}()
	if err := writeResolution(ws.Path, wanted); err != nil {
		return Landing{}, err
	}
	m, _, err := s.Publish(ctx, ws, stuck.Marked, by, "resolved "+short(stuck.Artifact)+" by hand")
	if err != nil {
		return Landing{}, err
	}
	return s.Land(ctx, p, m.ID, by)
}

// writeResolution writes each resolved file into the workspace at dir. The
// half-merged tree is content other machines committed, so a link in it is
// never followed out of the workspace, and a link where a resolved file goes
// is replaced by the text rather than written through.
func writeResolution(dir string, files map[string]string) error {
	root, err := os.OpenRoot(dir)
	if err != nil {
		return err
	}
	defer root.Close()
	for path, text := range files {
		name := filepath.FromSlash(path)
		if err := root.MkdirAll(filepath.Dir(name), 0o755); err != nil {
			return fmt.Errorf("%s: %w", path, err)
		}
		if info, err := root.Lstat(name); err == nil && !info.Mode().IsRegular() {
			if err := root.Remove(name); err != nil {
				return fmt.Errorf("%s: %w", path, err)
			}
		}
		if err := root.WriteFile(name, []byte(text), 0o644); err != nil {
			return fmt.Errorf("%s: %w", path, err)
		}
	}
	return nil
}

// resolvedFiles checks a resolution before any of it is written: only the
// files the conflict is actually on may be rewritten, the paths have to
// stay inside the tree, and text that still carries markers is the
// half-merged file sent back rather than a resolution of it.
func resolvedFiles(stuck Stuck, edits []Edit) (map[string]string, error) {
	conflicted := map[string]bool{}
	for _, path := range stuck.Paths {
		conflicted[path] = true
	}
	out := make(map[string]string, len(edits))
	for _, edit := range edits {
		path := strings.TrimPrefix(filepath.ToSlash(filepath.Clean("/"+edit.Path)), "/")
		if path == "" || path == "." {
			return nil, fmt.Errorf("%q is not a path in this project", edit.Path)
		}
		if !conflicted[path] {
			return nil, fmt.Errorf("%s is not one of this conflict's files", path)
		}
		if stillMarked(edit.Text) {
			return nil, fmt.Errorf("%s still has conflict markers in it", path)
		}
		out[path] = edit.Text
	}
	for path := range conflicted {
		if _, ok := out[path]; !ok {
			return nil, fmt.Errorf("%s has not been resolved", path)
		}
	}
	return out, nil
}

// BringHome makes sure the hub has the objects for a commit that was made
// on a node: a conflict of an in-place project is merged where the project
// lives, and the hub only learns the tree when it asks for it.
func (s *Store) BringHome(ctx context.Context, p project.Project, sha string) error {
	hub, err := s.Repo(ctx, p.ID)
	if err != nil {
		return err
	}
	if hub.Has(ctx, sha) {
		return nil
	}
	if p.Home.Node == "" {
		return fmt.Errorf("artifact %s is not on the hub", short(sha))
	}
	_, _, state, err := s.nodes.Git(ctx, p.Home.Node)
	if err != nil {
		return err
	}
	return s.pull(ctx, p.Home.Node, nodeBare(state, p.ID), hub, sha, nil)
}
