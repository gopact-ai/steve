package hubid

import (
	"os"
	"path/filepath"
	"sync"
	"testing"
)

func TestConcurrentStartupRetainsOneIdentity(t *testing.T) {
	dir := t.TempDir()
	var wg sync.WaitGroup
	ids := make(chan string, 8)
	for n := 0; n < 8; n++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			id, err := Resolve(dir, "")
			if err != nil {
				t.Error(err)
				return
			}
			ids <- id
		}()
	}
	wg.Wait()
	close(ids)
	var first string
	for id := range ids {
		if first == "" {
			first = id
		}
		if id != first {
			t.Fatal("split identity")
		}
	}
	if _, err := Resolve(dir, "other"); err == nil {
		t.Fatal("implicit identity replacement")
	}
	if id, err := Resolve(dir, first); err != nil || id != first {
		t.Fatal(id, err)
	}
	if info, err := os.Stat(filepath.Join(dir, FileName)); err != nil || info.Mode().Perm() != 0o600 {
		t.Fatal("identity permissions", err)
	}
}
func TestCorruptIdentityIsNotSilentlyReplaced(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, FileName), []byte("partial"), 0o600)
	if _, err := Resolve(dir, ""); err == nil {
		t.Fatal("corrupt identity replaced")
	}
}
