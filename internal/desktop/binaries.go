package desktop

import (
	"os"
	"path/filepath"
	"strings"
)

// BundledNodeBinary resolves a node package beside the running backend. Paths
// are not saved in user configuration: moving or upgrading the app moves these
// packages with it. The SSH installer validates the selected binary's platform
// and digest before sending it to a user-selected machine.
func BundledNodeBinary(platform string) (string, bool) {
	executable, err := os.Executable()
	if err != nil {
		return "", false
	}
	return bundledNodeBinary(executable, platform)
}

// BundledPeerBinary contains the coordinating and execution services for a
// peer installation. It is resolved from the current app location at use time.
func BundledPeerBinary(platform string) (string, bool) {
	backend, err := os.Executable()
	if err != nil {
		return "", false
	}
	return bundledPeerBinary(backend, platform)
}

func bundledPeerBinary(backend, platform string) (string, bool) {
	switch platform {
	case "linux/amd64", "linux/arm64", "darwin/amd64", "darwin/arm64":
	default:
		return "", false
	}
	path := filepath.Join(filepath.Dir(backend), "peer-binaries", strings.ReplaceAll(platform, "/", "-"), "steve")
	if !executable(path) {
		return "", false
	}
	return path, true
}

func bundledNodeBinary(backend, platform string) (string, bool) {
	switch platform {
	case "linux/amd64", "linux/arm64", "darwin/amd64", "darwin/arm64":
	default:
		return "", false
	}
	path := filepath.Join(filepath.Dir(backend), "node-binaries", strings.ReplaceAll(platform, "/", "-"), "steve-node")
	if !executable(path) {
		return "", false
	}
	return path, true
}
