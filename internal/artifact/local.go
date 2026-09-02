package artifact

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
)

// LocalNodes runs the node-side scripts on this machine, with a workspace
// root and state directory per node name. It is what a single-host hub and
// the tests use in place of a real node: the same commands, the same blob
// directory, no wire.
type LocalNodes struct {
	Dir string
}

func (l LocalNodes) root(node string) string  { return filepath.Join(l.Dir, node, "work") }
func (l LocalNodes) state(node string) string { return filepath.Join(l.Dir, node, "state") }

func (l LocalNodes) Exec(ctx context.Context, node, dir, command string) (string, error) {
	cmd := exec.CommandContext(ctx, "/bin/sh", "-c", command)
	if dir != "" {
		cmd.Dir = dir
	}
	out, err := cmd.CombinedOutput()
	if err != nil {
		return string(out), fmt.Errorf("%w: %s", err, out)
	}
	return string(out), nil
}

func (l LocalNodes) PutBlob(_ context.Context, node, name string, content io.Reader, size int64) error {
	dir := filepath.Join(l.state(node), "blobs")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	f, err := os.Create(filepath.Join(dir, name))
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = io.CopyN(f, content, size)
	return err
}

func (l LocalNodes) GetBlob(_ context.Context, node, name string, into io.Writer) error {
	f, err := os.Open(filepath.Join(l.state(node), "blobs", name))
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = io.Copy(into, f)
	return err
}

func (l LocalNodes) Git(_ context.Context, node string) (string, string, string, error) {
	if err := os.MkdirAll(l.root(node), 0o755); err != nil {
		return "", "", "", err
	}
	if err := os.MkdirAll(l.state(node), 0o700); err != nil {
		return "", "", "", err
	}
	out, err := exec.Command("git", "--version").Output()
	if err != nil {
		return "", l.root(node), l.state(node), nil
	}
	return string(out), l.root(node), l.state(node), nil
}
