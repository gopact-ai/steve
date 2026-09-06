package ops

import (
	"strconv"
	"strings"
)

const MinimumGitVersion = "2.38"

// GitAtLeast accepts git --version output, including Apple and Windows suffixes.
func GitAtLeast(version string, major, minor int) bool {
	version = strings.TrimPrefix(strings.TrimSpace(version), "git version ")
	parts := strings.Split(version, ".")
	if len(parts) < 2 {
		return false
	}
	a, errA := strconv.Atoi(parts[0])
	b, errB := strconv.Atoi(parts[1])
	return errA == nil && errB == nil && (a > major || a == major && b >= minor)
}

// GitWarning is advisory: a missing or old git never refuses startup.
func GitWarning(version string) string {
	if version == "" {
		return "git is not available; artifact operations require git >= " + MinimumGitVersion
	}
	if !GitAtLeast(version, 2, 38) {
		return "git " + version + " is below the minimum " + MinimumGitVersion
	}
	return ""
}
