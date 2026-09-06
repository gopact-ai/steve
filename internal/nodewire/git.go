package nodewire

import "github.com/gopact-ai/steve/internal/artifact/ops"

const MinimumGitVersion = ops.MinimumGitVersion

func GitAtLeast(version string, major, minor int) bool {
	return ops.GitAtLeast(version, major, minor)
}

func GitWarning(version string) string { return ops.GitWarning(version) }
