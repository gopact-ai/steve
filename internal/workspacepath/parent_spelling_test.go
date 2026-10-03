package workspacepath

import "testing"

func TestMetadataParentPreservesDriveSpellingForNodeRequests(t *testing.T) {
	for _, tc := range []struct{ path, want string }{
		{`c:\root\container\work`, `c:\root\container`},
		{`C:\root\container\work`, `C:\root\container`},
		{`c:/root/container/work`, `c:\root\container`},
		{`\\server\share\container\work`, `\\server\share\container`},
		{`/srv/container/work`, `/srv/container`},
	} {
		if got := Dir(tc.path); got != tc.want {
			t.Errorf("Dir(%q)=%q, want unchanged node volume spelling %q", tc.path, got, tc.want)
		}
	}
}
