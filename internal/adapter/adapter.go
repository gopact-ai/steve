// Package adapter pins the ACP adapters Steve starts.
//
// An adapter is an npm package, and `npx -y @scope/name` resolves whatever
// is newest at the moment it runs: the version an agent speaks is decided
// by the network, after the deployment was tested, on each machine
// separately. This package names a version instead, fetches it once,
// checks it against a digest compiled into the binary, and runs that.
//
// What is pinned is the adapter itself. Its dependencies resolve through
// npm as the package declares them — the packages do not ship a lockfile,
// and inventing one here would be a fork of someone else's release. So the
// promise is exact: the adapter is the version named here, verified; what
// it depends on is what it says it depends on.
package adapter

import (
	"context"
	"crypto/sha512"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// Pin is one adapter at one version. Integrity is the npm registry's own
// `dist.integrity` for that version, so the value can be read straight off
// `npm view <pkg>@<version> dist.integrity` when the pin is bumped.
type Pin struct {
	Package   string
	Version   string
	Tarball   string
	Integrity string
	// Bin is where the executable lands under the install prefix.
	Bin string
}

// Catalog is the trusted list. It is code, not configuration: a machine
// cannot be talked into a different adapter by editing a file on it.
var Catalog = map[string]Pin{
	"codex-acp": {
		Package:   "@agentclientprotocol/codex-acp",
		Version:   "1.8.0",
		Tarball:   "https://registry.npmjs.org/@agentclientprotocol/codex-acp/-/codex-acp-1.8.0.tgz",
		Integrity: "sha512-F/wgzhlPOmLN9H6iEUNeV4W0RC1aL9YG2tDBZT3QuiNM7vj2ku00BDb08oVkxO2+FOsmE71XHcMmVWA2rKx8ug==",
		Bin:       "node_modules/.bin/codex-acp",
	},
	"claude-agent-acp": {
		Package:   "@agentclientprotocol/claude-agent-acp",
		Version:   "0.75.1",
		Tarball:   "https://registry.npmjs.org/@agentclientprotocol/claude-agent-acp/-/claude-agent-acp-0.75.1.tgz",
		Integrity: "sha512-Un6I4BRkhpCFS3I7kr5C/lkAm8Nc3VuGZU2YQ3xIpJAIxV94iWO0Q2CH2QABxMERpONRu4Le6XC9V+5PImQZ2A==",
		Bin:       "node_modules/.bin/claude-agent-acp",
	},
}

// Names lists the catalog, sorted, for error messages and reporting.
func Names() []string {
	names := make([]string, 0, len(Catalog))
	for name := range Catalog {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// ErrUnknown is returned for a name the catalog does not carry. A machine
// asking for an adapter Steve does not know about is a configuration
// mistake, not something to resolve off the network.
var ErrUnknown = errors.New("adapter: not in the catalog")

// Installed describes an adapter that is ready to run.
type Installed struct {
	Name    string `json:"name"`
	Package string `json:"package"`
	Version string `json:"version"`
	// Command is the executable to start.
	Command string `json:"command"`
	// Cached reports that this call did no network work.
	Cached bool `json:"cached"`
}

// Installer fetches and verifies adapters under Dir.
type Installer struct {
	// Dir is the cache root; one directory per adapter and version, so two
	// versions can sit side by side and a rollback is a pin change.
	Dir string
	// HTTP is the client used for the tarball. Nil means a default with a
	// timeout: a hung registry must not hold a node's startup open.
	HTTP *http.Client
	// Link installs the verified tarball into a prefix. Nil means npm,
	// which is what resolves the adapter's dependencies. Tests replace it.
	Link func(ctx context.Context, prefix, tarball string) error
}

// Ensure returns the adapter named by name, fetching it if this machine
// does not have that exact version yet. It never falls back: a digest that
// does not match, a name that is not in the catalog, or a failed install
// is an error, because the alternative is running something nobody chose.
func (i *Installer) Ensure(ctx context.Context, name string) (Installed, error) {
	pin, ok := Catalog[name]
	if !ok {
		return Installed{}, fmt.Errorf("%w: %q is not one of %s", ErrUnknown, name, strings.Join(Names(), ", "))
	}
	dest := i.dir(name, pin.Version)
	if command, err := verified(dest, pin); err == nil {
		return Installed{Name: name, Package: pin.Package, Version: pin.Version, Command: command, Cached: true}, nil
	}
	command, err := i.install(ctx, name, pin, dest)
	if err != nil {
		return Installed{}, err
	}
	return Installed{Name: name, Package: pin.Package, Version: pin.Version, Command: command}, nil
}

// Resolve reports what Ensure would return without fetching anything. It is
// what doctor and the node's advert use: whether this machine already holds
// the pinned version is worth showing before anyone waits for a download.
func (i *Installer) Resolve(name string) (Installed, bool) {
	pin, ok := Catalog[name]
	if !ok {
		return Installed{}, false
	}
	command, err := verified(i.dir(name, pin.Version), pin)
	if err != nil {
		return Installed{Name: name, Package: pin.Package, Version: pin.Version}, false
	}
	return Installed{Name: name, Package: pin.Package, Version: pin.Version, Command: command, Cached: true}, true
}

func (i *Installer) dir(name, version string) string {
	return filepath.Join(i.Dir, name+"@"+version)
}

// marker records what was installed. Its presence is the only thing that
// makes an install directory usable, and it is written last, so a directory
// left behind by a crash is never mistaken for a finished one.
const marker = ".steve-adapter.json"

type record struct {
	Package     string    `json:"package"`
	Version     string    `json:"version"`
	Integrity   string    `json:"integrity"`
	InstalledAt time.Time `json:"installed_at"`
}

func verified(dest string, pin Pin) (string, error) {
	raw, err := os.ReadFile(filepath.Join(dest, marker))
	if err != nil {
		return "", err
	}
	var rec record
	if err := json.Unmarshal(raw, &rec); err != nil {
		return "", err
	}
	if rec.Package != pin.Package || rec.Version != pin.Version || rec.Integrity != pin.Integrity {
		return "", fmt.Errorf("adapter: %s holds %s@%s, not the pinned %s@%s", dest, rec.Package, rec.Version, pin.Package, pin.Version)
	}
	command := filepath.Join(dest, pin.Bin)
	if _, err := os.Stat(command); err != nil {
		return "", err
	}
	return command, nil
}

func (i *Installer) install(ctx context.Context, name string, pin Pin, dest string) (string, error) {
	if err := os.MkdirAll(i.Dir, 0o700); err != nil {
		return "", err
	}
	// Staged beside the destination so the rename below is a rename and
	// not a copy across filesystems.
	tmp, err := os.MkdirTemp(i.Dir, ".tmp-"+name+"-")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(tmp)

	tarball := filepath.Join(tmp, "package.tgz")
	if err := i.fetch(ctx, pin, tarball); err != nil {
		return "", err
	}
	link := i.Link
	if link == nil {
		link = npmInstall
	}
	if err := link(ctx, tmp, tarball); err != nil {
		return "", fmt.Errorf("adapter %s: install %s@%s: %w", name, pin.Package, pin.Version, err)
	}
	if _, err := os.Stat(filepath.Join(tmp, pin.Bin)); err != nil {
		return "", fmt.Errorf("adapter %s: %s@%s installed without %s: %w", name, pin.Package, pin.Version, pin.Bin, err)
	}
	// Remove the tarball before the marker: what is published is the
	// installed tree, not the archive it came from.
	_ = os.Remove(tarball)
	rec, err := json.Marshal(record{Package: pin.Package, Version: pin.Version, Integrity: pin.Integrity, InstalledAt: time.Now().UTC()})
	if err != nil {
		return "", err
	}
	if err := os.WriteFile(filepath.Join(tmp, marker), rec, 0o600); err != nil {
		return "", err
	}
	if err := os.Rename(tmp, dest); err != nil {
		// Another process finished the same version first. Its tree is as
		// good as this one — both were verified against the same digest —
		// so use it rather than fighting over the directory.
		if command, verr := verified(dest, pin); verr == nil {
			return command, nil
		}
		return "", err
	}
	return filepath.Join(dest, pin.Bin), nil
}

// fetch downloads the tarball and checks it against the pin as it streams,
// so a body that does not match is never written anywhere it could be run.
func (i *Installer) fetch(ctx context.Context, pin Pin, path string) error {
	client := i.HTTP
	if client == nil {
		client = &http.Client{Timeout: 5 * time.Minute}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, pin.Tarball, nil)
	if err != nil {
		return err
	}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("adapter: fetch %s: %w", pin.Package, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("adapter: fetch %s: %s", pin.Package, resp.Status)
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	sum := sha512.New()
	_, copyErr := io.Copy(io.MultiWriter(file, sum), resp.Body)
	closeErr := file.Close()
	if copyErr != nil {
		return copyErr
	}
	if closeErr != nil {
		return closeErr
	}
	got := "sha512-" + base64.StdEncoding.EncodeToString(sum.Sum(nil))
	if got != pin.Integrity {
		_ = os.Remove(path)
		return fmt.Errorf("adapter: %s@%s digest is %s, pinned %s", pin.Package, pin.Version, got, pin.Integrity)
	}
	return nil
}

// npmInstall resolves the adapter's dependencies. The tarball is already
// verified, so npm is here for the dependency tree, not for trust.
func npmInstall(ctx context.Context, prefix, tarball string) error {
	cmd := exec.CommandContext(ctx, "npm", "install",
		"--prefix", prefix, "--omit=dev", "--no-audit", "--no-fund", "--loglevel=error", tarball)
	cmd.Dir = prefix
	out, err := cmd.CombinedOutput()
	if err != nil {
		detail := strings.TrimSpace(string(out))
		if detail == "" {
			detail = err.Error()
		}
		if errors.Is(err, exec.ErrNotFound) {
			return fmt.Errorf("npm is required to install an adapter and was not found: %s", detail)
		}
		return fmt.Errorf("npm install: %s", detail)
	}
	return nil
}
