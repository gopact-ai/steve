package nodewire

import "testing"

func TestGitMinimumVersion(t *testing.T) {
	for _, tc := range []struct {
		version string
		good    bool
	}{
		{"", false}, {"unknown", false}, {"1.99.0", false}, {"2.37.9", false},
		{"git version 2.38.0", true}, {"2.38", true}, {"2.39.5", true},
		{"2.39.3 (Apple Git-145)", true}, {"2.47.1.windows.2", true}, {"3.0.0", true},
	} {
		t.Run(tc.version, func(t *testing.T) {
			if got := GitAtLeast(tc.version, 2, 38); got != tc.good {
				t.Fatalf("at least = %v", got)
			}
			if warning := GitWarning(tc.version); (warning == "") != tc.good {
				t.Fatalf("warning = %q", warning)
			}
		})
	}
}
