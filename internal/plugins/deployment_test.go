package plugins

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func configuredDeployment(t *testing.T) (*Store, Deployment) {
	t.Helper()
	bundle := example(t, "team-tools")
	store := &Store{Dir: filepath.Join(t.TempDir(), "plugins")}
	if _, err := store.Install(t.Context(), InstallRequest{CommandID: "package", ExpectedDigest: bundle.Digest, Bundle: bundle, Source: Source{Kind: "directory", Location: t.TempDir()}}); err != nil {
		t.Fatal(err)
	}
	secret, err := store.PutSecret(t.Context(), "team-token", "private-node-token")
	if err != nil {
		t.Fatal(err)
	}
	d := Deployment{Installation: "team", PackageID: bundle.Manifest.ID, Digest: bundle.Digest, Node: "worker", Projects: []string{"p"}, Configuration: Configuration{Values: map[string]string{"endpoint": "https://team.example.invalid/mcp"}, Secrets: map[string]SecretRef{"token": secret.Reference}}}
	return store, d
}

func TestDeploymentPinsConfigurationAndSecretVersions(t *testing.T) {
	store, d := configuredDeployment(t)
	first, err := store.PrepareDeployment(t.Context(), d, Environment{})
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := store.Deployment(first.Hash)
	if err != nil {
		t.Fatal(err)
	}
	before, _ := json.Marshal(first)
	after, _ := json.Marshal(decoded)
	if !bytes.Equal(before, after) || bytes.Contains(before, []byte("private-node-token")) {
		t.Fatal("deployment leaked a secret or failed round trip")
	}
	servers, err := store.ResolveServers(d, Environment{})
	if err != nil || servers["team"].Headers["Authorization"] != "Bearer private-node-token" {
		t.Fatalf("resolved credentials: %v", err)
	}
	replacement, err := store.PutSecret(t.Context(), "team-token", "rotated-node-token")
	if err != nil {
		t.Fatal(err)
	}
	next := d
	next.Configuration = d.Configuration.Clone()
	next.Configuration.Secrets["token"] = replacement.Reference
	second, err := store.PrepareDeployment(t.Context(), next, Environment{})
	if err != nil {
		t.Fatal(err)
	}
	if second.Hash == first.Hash {
		t.Fatal("rotation reused deployment identity")
	}
	old, err := store.ResolveServers(decoded.Deployment, Environment{})
	if err != nil || old["team"].Headers["Authorization"] != "Bearer private-node-token" {
		t.Fatal("old deployment switched secrets")
	}
	infos, err := store.Secrets()
	if err != nil || len(infos) != 2 {
		t.Fatalf("secret versions: %v", err)
	}
	raw, _ := json.Marshal(infos)
	if bytes.Contains(raw, []byte("node-token")) {
		t.Fatal("metadata exposed a secret")
	}
	restarted := &Store{Dir: store.Dir}
	replay, err := restarted.PrepareDeployment(t.Context(), d, Environment{})
	if err != nil || replay.Hash != first.Hash || !replay.PreparedAt.Equal(first.PreparedAt) {
		t.Fatalf("prepare replay: %v", err)
	}
}

func TestDeploymentRefusesMissingCredentialsAndInvalidResolvedValues(t *testing.T) {
	cases := []struct {
		name string
		edit func(*Deployment)
	}{
		{"missing-reference", func(d *Deployment) { delete(d.Configuration.Secrets, "token") }},
		{"missing-version", func(d *Deployment) {
			ref := d.Configuration.Secrets["token"]
			ref.Revision = "00000000000000000000000000000000"
			d.Configuration.Secrets["token"] = ref
		}},
		{"secret-as-public", func(d *Deployment) { d.Configuration.Values["token"] = "do-not-echo" }},
		{"missing-endpoint", func(d *Deployment) { delete(d.Configuration.Values, "endpoint") }},
		{"bad-endpoint", func(d *Deployment) { d.Configuration.Values["endpoint"] = "file:///do-not-echo" }},
		{"unknown-setting", func(d *Deployment) { d.Configuration.Values["unknown"] = "do-not-echo" }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			store, d := configuredDeployment(t)
			tc.edit(&d)
			_, err := store.PrepareDeployment(t.Context(), d, Environment{})
			if err == nil {
				t.Fatal("accepted invalid configuration")
			}
			if bytes.Contains([]byte(err.Error()), []byte("do-not-echo")) {
				t.Fatal("configuration value leaked in error")
			}
		})
	}
}

func TestDeploymentScopeIsPartOfIdentity(t *testing.T) {
	_, d := configuredDeployment(t)
	first, _ := d.Hash()
	changed := d
	changed.Projects = []string{"other"}
	second, _ := changed.Hash()
	if first == second {
		t.Fatal("project not included in identity")
	}
	changed = d
	changed.Node = "other"
	second, _ = changed.Hash()
	if first == second {
		t.Fatal("node not included in identity")
	}
	d.Projects = []string{"p", "q"}
	changed = d
	changed.Projects = []string{"q", "p"}
	first, _ = d.Hash()
	second, _ = changed.Hash()
	if first != second {
		t.Fatal("project ordering changed identity")
	}
}

func TestDeploymentRechecksSecretAvailabilityOnReplay(t *testing.T) {
	store, d := configuredDeployment(t)
	if _, err := store.PrepareDeployment(t.Context(), d, Environment{}); err != nil {
		t.Fatal(err)
	}
	ref := d.Configuration.Secrets["token"]
	if err := os.Remove(filepath.Join(store.Dir, "secrets", ref.Name+"-"+ref.Revision+".json")); err != nil {
		t.Fatal(err)
	}
	if _, err := store.PrepareDeployment(t.Context(), d, Environment{}); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("missing secret on replay: %v", err)
	}
}
