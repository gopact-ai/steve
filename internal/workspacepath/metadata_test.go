package workspacepath

import "testing"

// Windows rows run on Linux too: this is metadata syntax, never host filepath.
func TestQualifiedMetadataSyntaxAndRoots(t *testing.T) {
	for _, tc := range []struct {
		value, parent, base string
		abs                 bool
	}{
		{`C:\root\container\work`, `C:\root\container`, "work", true},
		{`C:/root\container//work`, `C:\root\container`, "work", true},
		{`c:/root\container//work`, `c:\root\container`, "work", true},
		{`C:\root\temp\..\work`, `C:\root`, "work", true},
		{`C:\..\work`, `C:\`, "work", true},
		{`C:\`, `C:\`, "/", true},
		{`\\server\share\container\work`, `\\server\share\container`, "work", true},
		{`\\server/share\temp/../work`, `\\server\share\`, "work", true},
		{`\\server\share\..\work`, `\\server\share\`, "work", true},
		{`\\server\share`, `\\server\share\`, "/", true},
		{`/srv/container/work`, `/srv/container`, "work", true},
		{`/srv/a\b/work`, `/srv/a\b`, "work", true},
		{`/srv/a\..\b/work`, `/srv/a\..\b`, "work", true},
		{`//server/share/work`, `/server/share`, "work", true},
		{`C:\root\work\.`, `C:\root\work`, ".", true},
		{`\\server\share\work\..`, `\\server\share\work`, "..", true},
		{`/srv/work/.`, `/srv/work`, ".", true},
	} {
		if got := IsAbs(tc.value); got != tc.abs {
			t.Errorf("IsAbs(%q)=%v, want %v", tc.value, got, tc.abs)
		}
		if got := Dir(tc.value); got != tc.parent {
			t.Errorf("Dir(%q)=%q, want %q", tc.value, got, tc.parent)
		}
		if got := Base(tc.value); got != tc.base {
			t.Errorf("Base(%q)=%q, want %q", tc.value, got, tc.base)
		}
	}
	for _, invalid := range []string{
		"", ".", "relative/work", `C:work`, `C:`, `\root\work`,
		`\\server`, `\\server\`, `\\server\\share`, `\\server\.\work`,
		`\\server\..\work`, `\\?\C:\work`, `\\.\pipe\work`, `é:/work`,
	} {
		if IsAbs(invalid) {
			t.Errorf("unqualified/unsupported Windows location became absolute: %q", invalid)
		}
	}
}

func TestMetadataEqualityDoesNotExpandAcrossSyntaxOrVolumes(t *testing.T) {
	for _, tc := range []struct {
		a, b string
		same bool
	}{
		{`C:\root\work`, `C:/root/temp/../work/`, true},
		{`C:\`, `C:/../`, true},
		{`\\server\share`, `\\server\share\..\`, true},
		{`\\server\share\work`, `\\server/share/temp/../work`, true},
		{`C:\root\work`, `c:\root\work`, true},
		{`C:\root\work`, `c:\Root\work`, false},
		{`\\server\share\work`, `\\SERVER\share\work`, false},
		{`\\server\share\work`, `\\server\SHARE\work`, false},
		{`C:\root\work`, `D:\root\work`, false},
		{`C:\root\work`, `C:root\work`, false},
		{`\\server\share\work`, `//server/share/work`, false},
		{`\\server\share\work`, `\\server\other\work`, false},
		{`\\server\share\work`, `\\other\share\work`, false},
		{`\\?\C:\work`, `C:\work`, false},
		{`/srv/a\b`, `/srv/a/b`, false},
		{`/srv/a\..\b`, `/srv/b`, false},
		{`/srv/./a\b`, `/srv/a\b`, true},
		{`C:root/../work`, `work`, false},
		{"", ".", false},
		{"", "", false},
	} {
		if got := Same(tc.a, tc.b); got != tc.same {
			t.Errorf("Same(%q, %q)=%v, want %v", tc.a, tc.b, got, tc.same)
		}
	}
}

func TestMetadataOverlapUsesComponentBoundaries(t *testing.T) {
	for _, tc := range []struct {
		a, b    string
		overlap bool
	}{
		{`C:\root\container`, `C:/root/container/work`, true},
		{`C:\root\container`, `c:/root/container/work`, true},
		{`C:\root\container`, `c:\Root\container\work`, false},
		{`C:\`, `C:\root\work`, true},
		{`C:\root\container`, `C:\root\container-other`, false},
		{`C:\root\container`, `D:\root\container\work`, false},
		{`\\server\share`, `\\server\share\work`, true},
		{`\\server\share\container`, `\\server\share\container\work`, true},
		{`\\server\share`, `\\server\share-other\work`, false},
		{`\\server\share`, `/server/share/work`, false},
		{`/srv/a\b`, `/srv/a\b/work`, true},
		{`/srv/a\b`, `/srv/a/b/work`, false},
		{`/`, `/srv/work`, true},
		{`/srv/work`, `/srv/work-other`, false},
		{`C:root`, `C:root/work`, false},
		{"", ".", false},
	} {
		for _, pair := range [][2]string{{tc.a, tc.b}, {tc.b, tc.a}} {
			if got := Overlap(pair[0], pair[1]); got != tc.overlap {
				t.Errorf("Overlap(%q, %q)=%v, want %v", pair[0], pair[1], got, tc.overlap)
			}
		}
	}
}
