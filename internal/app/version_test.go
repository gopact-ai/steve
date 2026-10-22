package app

import "testing"

func TestVersionLineNamesTheReleaseOnce(t *testing.T) {
	for _, tc := range []struct{ release, build, want string }{
		{"0.1.0", "v0.1.0 (6444a70)", "0.1.0 (6444a70)"},
		{"0.1.0", "6444a70+dirty", "0.1.0 (6444a70+dirty)"},
		{"", "v0.1.0 (6444a70)", "v0.1.0 (6444a70)"},
	} {
		if got := versionLine(tc.release, tc.build); got != tc.want {
			t.Fatalf("versionLine(%q, %q) = %q, want %q", tc.release, tc.build, got, tc.want)
		}
	}
}
