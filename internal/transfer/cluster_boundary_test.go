package transfer

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/gopact-ai/steve/internal/ledger"
	"github.com/gopact-ai/steve/internal/project"
)

type offlineClusterGate struct{}

func (offlineClusterGate) Prepare(context.Context) (ledger.ReplicaPosition, error) {
	return ledger.ReplicaPosition{}, ledger.ErrReplicaUnavailable
}
func (offlineClusterGate) Propose(context.Context, ledger.ReplicatedWrite) ([]byte, error) {
	return nil, ledger.ErrReplicaUnavailable
}

func TestOfflineExporterRefusesClusterStateBeforeWritingBundleOrOwnership(t *testing.T) {
	dir, _ := transferFixture(t)
	book, err := ledger.Open(dir, ledger.Options{})
	if err != nil {
		t.Fatal(err)
	}
	projects := project.Open(book)
	projects.SetHubID("source")
	before, exists, err := projects.Ownership(t.Context(), "p")
	if err != nil {
		t.Fatal(err)
	}
	if err := book.AttachReplication(offlineClusterGate{}); err != nil {
		t.Fatal(err)
	}
	if err := book.Close(); err != nil {
		t.Fatal(err)
	}
	output := filepath.Join(t.TempDir(), "project.steve")
	_, err = Export(t.Context(), Options{StateDir: dir, HubID: "source", Project: "p", TargetHub: "target", Evidence: "node process stopped", Output: output})
	if !errors.Is(err, ErrClusterOfflineTransfer) {
		t.Fatalf("cluster exporter returned ambiguous refusal: %v", err)
	}
	if _, err := os.Stat(output); !os.IsNotExist(err) {
		t.Fatal("cluster exporter wrote an incomplete bundle")
	}
	book, err = ledger.Open(dir, ledger.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer book.Close()
	projects = project.Open(book)
	projects.SetHubID("source")
	after, afterExists, err := projects.Ownership(t.Context(), "p")
	if err != nil || exists != afterExists || before != after {
		t.Fatalf("cluster export changed project ownership: %+v %v", after, err)
	}
}

func TestOfflineImporterRefusesClusterStateBeforeExtractingFiles(t *testing.T) {
	source, _ := transferFixture(t)
	bundle := filepath.Join(t.TempDir(), "project.steve")
	if _, err := Export(t.Context(), Options{StateDir: source, HubID: "source", Project: "p", TargetHub: "target", Evidence: "all writers stopped", Output: bundle}); err != nil {
		t.Fatal(err)
	}
	target := t.TempDir()
	book, err := ledger.Open(target, ledger.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if err := book.AttachReplication(offlineClusterGate{}); err != nil {
		t.Fatal(err)
	}
	if err := book.Close(); err != nil {
		t.Fatal(err)
	}
	home := filepath.Join(t.TempDir(), "restored")
	_, err = Import(t.Context(), ImportOptions{StateDir: target, HubID: "target", ExpectedSource: "source", Home: home, Input: bundle})
	if !errors.Is(err, ErrClusterOfflineTransfer) {
		t.Fatalf("cluster import returned ambiguous refusal: %v", err)
	}
	if _, err := os.Stat(home); !os.IsNotExist(err) {
		t.Fatal("cluster importer extracted files before refusing")
	}
}
