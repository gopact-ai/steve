//go:build linux

package peerstop

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"syscall"

	"golang.org/x/sys/unix"
)

type installation struct {
	root, sidecar, cluster, node string
	binary                       os.FileInfo
}
type installedIdentity struct {
	Version   int    `json:"version"`
	ClusterID string `json:"cluster_id"`
	NodeID    string `json:"node_id"`
	DataDir   string `json:"data_dir"`
}

func privateFile(path string) (*os.File, error) {
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW|unix.O_NONBLOCK, 0)
	if err != nil {
		return nil, fmt.Errorf("%w: open private installation file: %v", ErrUnproven, err)
	}
	f := os.NewFile(uintptr(fd), path)
	info, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, err
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 || stat.Nlink != 1 || stat.Uid != uint32(os.Getuid()) || info.Size() > 8<<20 {
		f.Close()
		return nil, fmt.Errorf("%w: installation file is not private and singly owned", ErrUnproven)
	}
	return f, nil
}

func loadInstallation(sidecar, cluster, node string) (installation, error) {
	var result installation
	if !filepath.IsAbs(sidecar) || filepath.Base(sidecar) != "config.json.cluster.json" || cluster == "" || node == "" {
		return result, fmt.Errorf("%w: exact installation identity is required", ErrUnproven)
	}
	root, err := filepath.EvalSymlinks(filepath.Dir(sidecar))
	if err != nil {
		return result, fmt.Errorf("%w: installation path: %v", ErrUnproven, err)
	}
	result = installation{root: root, sidecar: filepath.Join(root, "config.json.cluster.json"), cluster: cluster, node: node}
	if err := result.verify(); err != nil {
		return result, err
	}
	result.binary, err = os.Lstat(filepath.Join(root, "bin", "steve"))
	if err != nil || !result.binary.Mode().IsRegular() {
		return result, fmt.Errorf("%w: installed program is unavailable", ErrUnproven)
	}
	stat, ok := result.binary.Sys().(*syscall.Stat_t)
	if !ok || stat.Uid != uint32(os.Getuid()) || result.binary.Mode().Perm()&0022 != 0 {
		return result, fmt.Errorf("%w: installed program ownership is unsafe", ErrUnproven)
	}
	return result, nil
}

func (i installation) verify() error {
	application, err := privateFile(filepath.Join(i.root, "config.json"))
	if err != nil {
		return err
	}
	application.Close()

	file, err := privateFile(i.sidecar)
	if err != nil {
		return err
	}
	defer file.Close()
	raw, err := io.ReadAll(io.LimitReader(file, 8<<20))
	if err != nil {
		return err
	}
	var value installedIdentity
	if err := json.Unmarshal(raw, &value); err != nil {
		return fmt.Errorf("%w: installation identity is unreadable", ErrUnproven)
	}
	data, err := filepath.EvalSymlinks(value.DataDir)
	if err != nil || !filepath.IsAbs(value.DataDir) || value.Version != 1 || value.ClusterID != i.cluster || value.NodeID != i.node || data != filepath.Join(i.root, "cluster") {
		return fmt.Errorf("%w: installation cluster, node or state directory differs", ErrUnproven)
	}
	return nil
}

func sameLock(file *os.File) error {
	before, err := file.Stat()
	if err != nil {
		return err
	}
	after, err := os.Lstat(file.Name())
	if err != nil || !os.SameFile(before, after) {
		return fmt.Errorf("%w: installation lock was replaced", ErrUnproven)
	}
	return nil
}
