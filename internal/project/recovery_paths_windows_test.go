//go:build windows

package project

import "testing"

func TestNativeWindowsProjectOverlapBoundaries(t *testing.T) {
	for _, tc := range []struct {
		a, b    string
		overlap bool
	}{
		{`C:\root\container`, `C:\root\container\work`, true},
		{`C:\`, `C:\root\work`, true},
		{`\\server\share`, `\\server\share\container\work`, true},
		{`C:\root\container`, `C:\root\container-other`, false},
		{`C:\root\container`, `D:\root\container\work`, false},
	} {
		if got := pathsOverlap(tc.a, tc.b); got != tc.overlap {
			t.Errorf("native overlap(%q, %q)=%v, want %v", tc.a, tc.b, got, tc.overlap)
		}
		if got := pathsOverlap(tc.b, tc.a); got != tc.overlap {
			t.Errorf("reverse native overlap(%q, %q)=%v, want %v", tc.b, tc.a, got, tc.overlap)
		}
	}
}
