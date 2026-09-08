package transfer

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gopact-ai/steve/internal/ledger"
	"github.com/gopact-ai/steve/internal/project"
)

// The export and import refusals below each come from one stage of the
// transfer. Every one is checked through the public entry point so the
// stage order, and the words an operator sees, stay what they were.

func TestExportRefusesIncompleteOptionsExistingOutputAndReleasedProject(t *testing.T) {
	source, _ := transferFixture(t)
	options := Options{StateDir: source, HubID: "source", Project: "p", TargetHub: "target", Evidence: "stopped"}
	incomplete := options
	incomplete.Output = filepath.Join(t.TempDir(), "bundle")
	incomplete.Evidence = ""
	if _, err := Export(t.Context(), incomplete); err == nil || !strings.Contains(err.Error(), "explicit stop evidence") {
		t.Fatal("export without stop evidence was not refused", err)
	}
	occupied := options
	occupied.Output = filepath.Join(t.TempDir(), "bundle")
	if err := os.WriteFile(occupied.Output, []byte("someone else's file"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := Export(t.Context(), occupied); err == nil || !strings.Contains(err.Error(), "output already exists; refusing to overwrite") {
		t.Fatal("export over an existing output was not refused", err)
	}
	assertOwner(t, source, "source", "p", true)
	first := options
	first.Output = filepath.Join(t.TempDir(), "bundle")
	b, err := Export(t.Context(), first)
	if err != nil {
		t.Fatal(err)
	}
	second := options
	second.Output = filepath.Join(t.TempDir(), "bundle")
	_, err = Export(t.Context(), second)
	if err == nil || !strings.Contains(err.Error(), "project already released as "+b.Owner.TransferID) {
		t.Fatal("second export of a released project was not refused", err)
	}
	if _, err := os.Stat(second.Output); !os.IsNotExist(err) {
		t.Fatal("refused export wrote a bundle", err)
	}
}

func TestImportRefusalsKeepTheirStageAndWords(t *testing.T) {
	source, _ := transferFixture(t)
	if err := os.MkdirAll(filepath.Join(source, "memory", "projects"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(source, "memory", "projects", "p.md"), []byte("# p\n"), 0600); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "bundle")
	b, err := Export(t.Context(), Options{StateDir: source, HubID: "source", Project: "p", TargetHub: "target", Output: path, Evidence: "stopped"})
	if err != nil {
		t.Fatal(err)
	}
	input, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	options := ImportOptions{StateDir: t.TempDir(), HubID: "target", ExpectedSource: "source", Input: path, Home: filepath.Join(t.TempDir(), "home")}
	refusals := []struct {
		name    string
		arrange func(t *testing.T, o *ImportOptions)
		want    string
	}{
		{"no home", func(t *testing.T, o *ImportOptions) { o.Home = "" }, "import requires a new local home"},
		{"wrong target", func(t *testing.T, o *ImportOptions) { o.HubID = "elsewhere" }, "import requires matching target/source identities and a new local home"},
		{"occupied home", func(t *testing.T, o *ImportOptions) {
			if err := os.MkdirAll(o.Home, 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(o.Home, "resident"), []byte("here first"), 0600); err != nil {
				t.Fatal(err)
			}
		}, "target home must be empty"},
		{"occupied memory", func(t *testing.T, o *ImportOptions) {
			writeTargetMemory(t, o.StateDir, "p.md", "# someone else's p\n")
		}, "target memory already exists"},
		{"foreign marker", func(t *testing.T, o *ImportOptions) {
			writeInstallMarker(t, o.StateDir, b.Owner.TransferID, []byte(`{"Digest":"other","Home":"/elsewhere","Project":"p"}`))
		}, "transfer install marker mismatch"},
		{"retry over changed memory", func(t *testing.T, o *ImportOptions) {
			writeInstallMarker(t, o.StateDir, b.Owner.TransferID, installMarkerBytes(t, *o, input))
			writeTargetMemory(t, o.StateDir, "p.md", "# rewritten since\n")
		}, "target project memory changed"},
	}
	for _, refusal := range refusals {
		t.Run(refusal.name, func(t *testing.T) {
			o := options
			o.StateDir = t.TempDir()
			o.Home = filepath.Join(t.TempDir(), "home")
			refusal.arrange(t, &o)
			_, err := Import(t.Context(), o)
			if err == nil || !strings.Contains(err.Error(), refusal.want) {
				t.Fatalf("got %v, want %q", err, refusal.want)
			}
			assertOwner(t, o.StateDir, "target", "p", false)
		})
	}
	t.Run("retry over identical memory", func(t *testing.T) {
		o := options
		o.StateDir = t.TempDir()
		o.Home = filepath.Join(t.TempDir(), "home")
		writeInstallMarker(t, o.StateDir, b.Owner.TransferID, installMarkerBytes(t, o, input))
		writeTargetMemory(t, o.StateDir, "p.md", "# p\n")
		if _, err := Import(t.Context(), o); err != nil {
			t.Fatal("retry with the memory already in place failed", err)
		}
		moved := o
		moved.Home = filepath.Join(t.TempDir(), "elsewhere")
		if _, err := Import(t.Context(), moved); err == nil || !strings.Contains(err.Error(), "same transfer cannot move its accepted home") {
			t.Fatal("accepted transfer moved its home", err)
		}
		other, _ := transferFixture(t)
		otherBundle := filepath.Join(t.TempDir(), "bundle")
		if _, err := Export(t.Context(), Options{StateDir: other, HubID: "source", Project: "p", TargetHub: "target", Output: otherBundle, Evidence: "stopped"}); err != nil {
			t.Fatal(err)
		}
		collision := o
		collision.Input = otherBundle
		collision.Home = filepath.Join(t.TempDir(), "another")
		if _, err := Import(t.Context(), collision); err == nil || !strings.Contains(err.Error(), "ownership collision or stale transfer") {
			t.Fatal("a second transfer of the same project id was accepted", err)
		}
	})
}

// installMarkerBytes is what claimInstall records for this import: the
// digest of the bundle file and the home the project will occupy.
func installMarkerBytes(t *testing.T, o ImportOptions, input []byte) []byte {
	t.Helper()
	home, err := canonicalTransferPath(o.Home)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(struct{ Digest, Home, Project string }{digest(input), home, "p"})
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func writeInstallMarker(t *testing.T, stateDir, transferID string, raw []byte) {
	t.Helper()
	if err := (&ledger.FileDocument{Path: filepath.Join(stateDir, "transfers", transferID+".json")}).Save(raw); err != nil {
		t.Fatal(err)
	}
}

func writeTargetMemory(t *testing.T, stateDir, name, content string) {
	t.Helper()
	if err := (&ledger.FileDocument{Path: filepath.Join(stateDir, "memory", "projects", name)}).Save([]byte(content)); err != nil {
		t.Fatal(err)
	}
}

// assertOwner checks whether hub still (or already) executes project id.
func assertOwner(t *testing.T, stateDir, hub, id string, want bool) {
	t.Helper()
	book, err := ledger.Open(stateDir, ledger.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer book.Close()
	projects := project.Open(book)
	projects.SetHubID(hub)
	_, found, err := projects.Get(t.Context(), id)
	if want && (err != nil || !found) {
		t.Fatalf("%s no longer owns %s: found=%v err=%v", hub, id, found, err)
	}
	if !want && err == nil && found {
		t.Fatalf("%s owns %s after a refused transfer", hub, id)
	}
}
