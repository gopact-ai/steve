//go:build unix

package runtime

import (
	"strings"
	"testing"
)

func TestAcquireLockRefusesASecondGateway(t *testing.T) {
	dir := t.TempDir()
	release, err := AcquireLock(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := AcquireLock(dir); err == nil || !strings.Contains(err.Error(), "another gateway") {
		t.Fatalf("second acquire = %v, want a refusal", err)
	}
	release()
	again, err := AcquireLock(dir)
	if err != nil {
		t.Fatalf("acquire after release: %v", err)
	}
	again()
}
