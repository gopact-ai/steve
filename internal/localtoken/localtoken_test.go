package localtoken

import (
	"os"
	"path/filepath"
	"sync"
	"testing"
)

func TestResolveCreatesOnePrivateTokenAndKeepsIt(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "state")
	var wg sync.WaitGroup
	tokens := make(chan string, 8)
	for range 8 {
		wg.Go(func() {
			token, err := Resolve(dir)
			if err != nil {
				t.Error(err)
				return
			}
			tokens <- token
		})
	}
	wg.Wait()
	close(tokens)
	first := <-tokens
	if len(first) < MinLength {
		t.Fatalf("token %q is shorter than %d", first, MinLength)
	}
	for token := range tokens {
		if token != first {
			t.Fatal("concurrent first startups disagreed on the token")
		}
	}
	info, err := os.Stat(filepath.Join(dir, FileName))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("token file mode = %v", info.Mode().Perm())
	}
	again, err := Resolve(dir)
	if err != nil || again != first {
		t.Fatalf("restart changed the token: %q %v", again, err)
	}
	read, err := Read(dir)
	if err != nil || read != first {
		t.Fatalf("Read = %q %v", read, err)
	}
}

func TestReadRefusesAnExposedOrMalformedToken(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, FileName)
	if _, err := Read(dir); !os.IsNotExist(err) {
		t.Fatalf("missing token: %v", err)
	}
	if err := os.WriteFile(path, []byte("0123456789012345678901234567890123456789abcd"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Read(dir); err == nil {
		t.Fatal("a token other users can read was accepted")
	}
	if _, err := Resolve(dir); err == nil {
		t.Fatal("Resolve adopted a token other users can read")
	}
	if err := os.Chmod(path, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("short"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Read(dir); err == nil {
		t.Fatal("a short token was accepted")
	}
}
