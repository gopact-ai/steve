//go:build !unix

package acphost

import (
	"errors"
	"os"
)

const workspaceFilesSupported = false

func openWorkspaceText(root *os.Root, rel string) (*os.File, error) {
	info, err := root.Stat(rel)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, errors.New("text-file request requires a regular file")
	}
	return root.Open(rel)
}

func workspaceSingleLink(os.FileInfo) bool { return false }

func checkWorkspaceTextWritable(*os.Root, string) error {
	return errors.New("workspace text-file callbacks are unavailable")
}
