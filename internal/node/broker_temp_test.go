package node

import (
	"os"
	"testing"
)

// Unix socket paths include the directory name in their small kernel limit.
// t.TempDir includes the full test/subtest name, which is too long under the
// default Darwin TMPDIR. Keep endpoints in an owned, private short directory.
func brokerTempDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "steve-broker-test-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.RemoveAll(dir); err != nil {
			t.Error(err)
		}
	})
	return dir
}
