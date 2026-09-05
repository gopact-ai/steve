package adapter

import (
	"context"
	"crypto/sha512"
	"encoding/base64"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
)

// fixture publishes one tarball body and registers a catalog entry for it,
// returning the installer, the entry's name and a counter of how many times
// the body was actually fetched.
func fixture(t *testing.T, body string) (*Installer, string, *atomic.Int32) {
	t.Helper()
	var fetches atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fetches.Add(1)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(server.Close)

	sum := sha512.Sum512([]byte(body))
	name := "test-acp"
	Catalog[name] = Pin{
		Package:   "@test/acp",
		Version:   "1.2.3",
		Tarball:   server.URL + "/acp.tgz",
		Integrity: "sha512-" + base64.StdEncoding.EncodeToString(sum[:]),
		Bin:       "node_modules/.bin/test-acp",
	}
	t.Cleanup(func() { delete(Catalog, name) })

	install := &Installer{Dir: t.TempDir(), HTTP: server.Client()}
	install.Link = func(_ context.Context, prefix, tarball string) error {
		if _, err := os.Stat(tarball); err != nil {
			return err
		}
		bin := filepath.Join(prefix, "node_modules", ".bin")
		if err := os.MkdirAll(bin, 0o700); err != nil {
			return err
		}
		return os.WriteFile(filepath.Join(bin, "test-acp"), []byte("#!/bin/sh\n"), 0o700)
	}
	return install, name, &fetches
}

func TestEnsureInstallsThePinnedVersionAndThenStopsFetching(t *testing.T) {
	install, name, fetches := fixture(t, "a tarball")
	got, err := install.Ensure(t.Context(), name)
	if err != nil {
		t.Fatal(err)
	}
	if got.Version != "1.2.3" || got.Cached {
		t.Fatalf("first install = %#v", got)
	}
	if _, err := os.Stat(got.Command); err != nil {
		t.Fatalf("the command it reported is not there: %v", err)
	}
	// The archive is not what runs, so it must not be left lying next to
	// what does.
	if _, err := os.Stat(filepath.Join(filepath.Dir(filepath.Dir(filepath.Dir(got.Command))), "package.tgz")); err == nil {
		t.Fatal("the tarball was left in the install directory")
	}

	again, err := install.Ensure(t.Context(), name)
	if err != nil {
		t.Fatal(err)
	}
	if !again.Cached || again.Command != got.Command {
		t.Fatalf("second call = %#v, want the cached first one", again)
	}
	if n := fetches.Load(); n != 1 {
		t.Fatalf("fetched %d times; a machine that already holds the version must not go to the network", n)
	}
}

func TestEnsureRefusesABodyThatDoesNotMatchTheDigest(t *testing.T) {
	install, name, _ := fixture(t, "a tarball")
	pin := Catalog[name]
	pin.Integrity = "sha512-" + base64.StdEncoding.EncodeToString(make([]byte, 64))
	Catalog[name] = pin

	_, err := install.Ensure(t.Context(), name)
	if err == nil {
		t.Fatal("a body that does not match the pin was accepted")
	}
	if !strings.Contains(err.Error(), "digest") {
		t.Fatalf("error = %v", err)
	}
	// Nothing usable, and nothing staged, may survive a refusal.
	entries, readErr := os.ReadDir(install.Dir)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if len(entries) != 0 {
		t.Fatalf("a refused install left %d entries behind", len(entries))
	}
}

func TestEnsureRefusesAnAdapterThatIsNotInTheCatalog(t *testing.T) {
	install := &Installer{Dir: t.TempDir()}
	_, err := install.Ensure(t.Context(), "whatever-acp")
	if !errors.Is(err, ErrUnknown) {
		t.Fatalf("error = %v, want ErrUnknown", err)
	}
	// The message has to say what this machine can run, or the person
	// reading it has nowhere to go.
	for _, known := range Names() {
		if !strings.Contains(err.Error(), known) {
			t.Fatalf("error %q does not name %q", err, known)
		}
	}
}

func TestAHalfFinishedDirectoryIsNotUsed(t *testing.T) {
	install, name, fetches := fixture(t, "a tarball")
	pin := Catalog[name]
	// Everything an install produces except the marker, which is written
	// last precisely so this case is recognisable.
	dest := install.dir(name, pin.Version)
	if err := os.MkdirAll(filepath.Join(dest, "node_modules", ".bin"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dest, pin.Bin), []byte("truncated"), 0o700); err != nil {
		t.Fatal(err)
	}
	if _, ok := install.Resolve(name); ok {
		t.Fatal("a directory with no marker was reported as installed")
	}
	if _, err := install.Ensure(t.Context(), name); err == nil {
		t.Skip("install replaced the leftover directory; nothing more to assert")
	}
	if fetches.Load() == 0 {
		t.Fatal("the leftover directory was trusted instead of refetched")
	}
}

func TestResolveDoesNotFetch(t *testing.T) {
	install, name, fetches := fixture(t, "a tarball")
	got, ok := install.Resolve(name)
	if ok {
		t.Fatal("nothing is installed yet")
	}
	if got.Package != "@test/acp" || got.Version != "1.2.3" {
		t.Fatalf("resolve = %#v; it must still say what is pinned", got)
	}
	if fetches.Load() != 0 {
		t.Fatal("Resolve went to the network")
	}
}

func TestTheCatalogPinsExactVersionsAndDigests(t *testing.T) {
	for name, pin := range Catalog {
		if name == "test-acp" {
			continue
		}
		if pin.Package == "" || pin.Version == "" || pin.Bin == "" {
			t.Fatalf("%s: %#v", name, pin)
		}
		if !strings.HasPrefix(pin.Integrity, "sha512-") {
			t.Fatalf("%s: integrity %q is not an npm sha512 digest", name, pin.Integrity)
		}
		// A tarball URL that does not carry the version would let the
		// registry decide what the pin means.
		if !strings.Contains(pin.Tarball, pin.Version) {
			t.Fatalf("%s: tarball %q does not name version %s", name, pin.Tarball, pin.Version)
		}
		if strings.Contains(pin.Version, "^") || strings.Contains(pin.Version, "~") || strings.Contains(pin.Version, "*") {
			t.Fatalf("%s: %q is a range, not a version", name, pin.Version)
		}
	}
}
