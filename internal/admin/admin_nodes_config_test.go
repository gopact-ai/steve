package admin

import (
	"errors"
	"reflect"
	"slices"
	"testing"

	"github.com/gopact-ai/steve/internal/config"
	"github.com/gopact-ai/steve/internal/consoleapi"
	"github.com/gopact-ai/steve/internal/node"
)

// A machine change whose configuration cannot be saved leaves the
// configuration exactly as it was.
func TestFailedMachineConfigurationSaveChangesNothing(t *testing.T) {
	for _, tc := range []struct {
		name    string
		fixture func(*testing.T) *Service
		change  func(*testing.T, *Service) error
	}{
		{"add", workerAdmissionFixture, func(t *testing.T, a *Service) error {
			_, err := a.AddNode(t.Context(), consoleapi.AddNodeRequest{Name: "node-new", Addr: "127.0.0.1:1"})
			return err
		}},
		{"admit", workerAdmissionFixture, func(t *testing.T, a *Service) error {
			return a.AdmitWorker(t.Context(), "node-new", node.Config{Addr: "127.0.0.1:1", Token: "token"})
		}},
		{"remove", nodeAdminFixture, func(t *testing.T, a *Service) error {
			return a.RemoveNode(t.Context(), "node-test")
		}},
		{"hub settings", hubNodeSettingsFixture, func(t *testing.T, a *Service) error {
			set, err := a.NodeSettings(t.Context(), NodeName())
			if err != nil {
				t.Fatal(err)
			}
			set.Tools = []string{"git"}
			_, err = a.SetNodeSettings(t.Context(), NodeName(), set)
			return err
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a := tc.fixture(t)
			before := a.cfg().Clone()
			a.WriteConfig = func(string, *config.Config) error { return errors.New("disk full") }
			if err := tc.change(t, a); err == nil {
				t.Fatal("a failed save was not reported")
			}
			if !reflect.DeepEqual(a.cfg(), before) {
				t.Fatalf("a failed save changed the configuration:\n%+v\nwas\n%+v", a.cfg(), before)
			}
		})
	}
}

// A worker whose configuration reached the file stays recorded and is
// dialed even though the directory could not be synced.
func TestAdmitWorkerKeepsAWorkerWhoseConfigurationIsInPlace(t *testing.T) {
	admin := workerAdmissionFixture(t)
	server := startAgentAdminNode(t, nil)
	admin.WriteConfig = func(path string, cfg *config.Config) error {
		if err := config.Save(path, cfg); err != nil {
			return err
		}
		return &config.CommittedError{Err: errors.New("sync directory")}
	}
	err := admin.AdmitWorker(t.Context(), "node-test", node.Config{Addr: server.Addr(), Token: "test-node-token"})
	if !config.Committed(err) {
		t.Fatalf("AdmitWorker = %v, want the committed save error", err)
	}
	if _, ok := admin.cfg().Nodes["node-test"]; !ok {
		t.Fatal("the worker in the saved file was dropped from the configuration")
	}
	if !slices.Contains(admin.Nodes.Names(), "node-test") {
		t.Fatal("the worker in the saved file was not dialed")
	}
}
