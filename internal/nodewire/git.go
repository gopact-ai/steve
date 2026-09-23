package nodewire

import "github.com/gopact-ai/steve/internal/artifact/ops"

const MinimumGitVersion = ops.MinimumGitVersion

func GitWarning(version string) string { return ops.GitWarning(version) }
