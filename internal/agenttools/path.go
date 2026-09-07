package agenttools

import (
	"cmp"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
)

// ExecutablePath supplements a GUI or service environment with known installer
// directories. It only inspects directory names; shell startup files and tools
// are never executed. The caller's absolute PATH entries retain precedence.
func ExecutablePath(home string) string {
	if home == "" {
		home, _ = os.UserHomeDir()
	}
	parts := filepath.SplitList(os.Getenv("PATH"))
	parts = append(parts, "/opt/homebrew/bin", "/usr/local/bin", "/usr/bin", "/bin")
	if filepath.IsAbs(home) {
		for _, dir := range []string{".local/bin", "bin", ".npm-global/bin", ".bun/bin", ".kimi-code/bin"} {
			parts = append(parts, filepath.Join(home, dir))
		}
	}
	if bin := os.Getenv("NVM_BIN"); filepath.IsAbs(bin) {
		parts = append(parts, bin)
	}
	nvmRoot := os.Getenv("NVM_DIR")
	if !filepath.IsAbs(nvmRoot) {
		nvmRoot = ""
		if filepath.IsAbs(home) {
			nvmRoot = filepath.Join(home, ".nvm")
		}
	}
	if nvmRoot != "" {
		parts = append(parts, nvmVersionBins(nvmRoot)...)
	}
	seen := make(map[string]bool, len(parts))
	search := make([]string, 0, len(parts))
	for _, dir := range parts {
		if !filepath.IsAbs(dir) {
			continue
		}
		dir = filepath.Clean(dir)
		if !seen[dir] {
			seen[dir] = true
			search = append(search, dir)
		}
	}
	return strings.Join(search, string(os.PathListSeparator))
}

func nvmVersionBins(root string) []string {
	type versionDir struct {
		name    string
		version [3]uint64
	}
	root = filepath.Join(root, "versions", "node")
	entries, _ := os.ReadDir(root)
	versions := make([]versionDir, 0, len(entries))
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		if version, ok := nodeVersion(entry.Name()); ok {
			versions = append(versions, versionDir{entry.Name(), version})
		}
	}
	slices.SortFunc(versions, func(a, b versionDir) int {
		for i := range a.version {
			if order := cmp.Compare(b.version[i], a.version[i]); order != 0 {
				return order
			}
		}
		return 0
	})
	bins := make([]string, 0, len(versions))
	for _, version := range versions {
		bins = append(bins, filepath.Join(root, version.name, "bin"))
	}
	return bins
}

func nodeVersion(name string) ([3]uint64, bool) {
	var version [3]uint64
	if !strings.HasPrefix(name, "v") {
		return version, false
	}
	parts := strings.Split(strings.TrimPrefix(name, "v"), ".")
	if len(parts) != len(version) {
		return version, false
	}
	for i, part := range parts {
		if part == "" || len(part) > 1 && part[0] == '0' {
			return version, false
		}
		for _, char := range part {
			if char < '0' || char > '9' {
				return version, false
			}
		}
		value, err := strconv.ParseUint(part, 10, 64)
		if err != nil {
			return version, false
		}
		version[i] = value
	}
	return version, true
}

// InitializePath gives discovery, adapter installation and agent subprocesses
// the same executable search roots. Call it once before starting services.
func InitializePath() error {
	return os.Setenv("PATH", ExecutablePath(""))
}
