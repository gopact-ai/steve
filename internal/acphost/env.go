package acphost

import "strings"

// mergeEnv returns base with every variable present in overrides replaced,
// so harness env can override inherited variables (os.Environ alone would
// leave duplicates whose first occurrence wins in libc but not in Node).
func mergeEnv(base, overrides []string) []string {
	if len(overrides) == 0 {
		return base
	}
	replace := make(map[string]string, len(overrides))
	for _, kv := range overrides {
		if key, value, ok := strings.Cut(kv, "="); ok {
			replace[key] = value
		}
	}
	merged := make([]string, 0, len(base)+len(overrides))
	for _, kv := range base {
		if key, _, ok := strings.Cut(kv, "="); ok {
			if _, overridden := replace[key]; overridden {
				continue
			}
		}
		merged = append(merged, kv)
	}
	return append(merged, overrides...)
}
