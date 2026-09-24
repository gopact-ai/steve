package admin

import (
	"encoding/json"
	"errors"
	"os"
	"reflect"
	"sync"
	"testing"

	"github.com/gopact-ai/steve/internal/config"
	"github.com/gopact-ai/steve/internal/consoleapi"
)

func TestSettingsCASPreservesEffectiveAndDoesNotExposeSecrets(t *testing.T) {
	a := agentAdminFixture(t)
	a.Cfg.Gateway.OwnerID = "owner"
	a.Cfg.Feishu.AppID = "test-app"
	a.Cfg.Feishu.AppSecret = "private-secret"
	if err := config.Save(a.Path, a.Cfg); err != nil {
		t.Fatal(err)
	}
	s := NewSettings(a, a.Cfg)
	before, err := s.Settings(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if string(before.Desired) == "" || containsSecret(before, "private-secret") {
		t.Fatal("invalid redacted settings view")
	}
	update := consoleapi.SettingsUpdate{BaseRevision: before.Revision, Settings: json.RawMessage(`{"gateway":{"task_max_turns":7},"policies":{"execution":{"step_timeout":"22m"}}}`)}
	var wg sync.WaitGroup
	results := make(chan error, 2)
	for range 2 {
		wg.Go(func() { _, err := s.UpdateSettings(t.Context(), update); results <- err })
	}
	wg.Wait()
	close(results)
	wins, conflicts := 0, 0
	for err := range results {
		if err == nil {
			wins++
		} else if errors.Is(err, consoleapi.ErrSettingsConflict) {
			conflicts++
		} else {
			t.Fatal(err)
		}
	}
	if wins != 1 || conflicts != 1 {
		t.Fatalf("CAS winners=%d conflicts=%d", wins, conflicts)
	}
	after, err := s.Settings(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if !after.PendingRestart || after.Revision == before.Revision || !reflect.DeepEqual(after.Effective, before.Effective) {
		t.Fatal("desired update falsely changed effective runtime")
	}
	if _, err := s.UpdateSettings(t.Context(), consoleapi.SettingsUpdate{BaseRevision: after.Revision, Settings: json.RawMessage(`{"gateway":{"task_max_turns":0}}`)}); err != nil {
		t.Fatal(err)
	}
	if a.Cfg.Gateway.TaskMaxTurns != 0 {
		t.Fatal("explicit zero did not restore unlimited default")
	}
}

func containsSecret(v consoleapi.SettingsView, secret string) bool {
	raw, _ := json.Marshal(v)
	for i := 0; i+len(secret) <= len(raw); i++ {
		if string(raw[i:i+len(secret)]) == secret {
			return true
		}
	}
	return false
}

func TestSettingsSaveFailuresRespectTheCommitBoundary(t *testing.T) {
	a := agentAdminFixture(t)
	a.Cfg.Gateway.OwnerID = "owner"
	if err := config.Save(a.Path, a.Cfg); err != nil {
		t.Fatal(err)
	}
	s := NewSettings(a, a.Cfg)
	before, _ := s.Settings(t.Context())
	raw, _ := os.ReadFile(a.Path)
	a.WriteConfig = func(string, *config.Config) error { return errors.New("disk unavailable") }
	request := consoleapi.SettingsUpdate{BaseRevision: before.Revision, Settings: json.RawMessage(`{"gateway":{"task_max_turns":9}}`)}
	if _, err := s.UpdateSettings(t.Context(), request); err == nil {
		t.Fatal("save failure reported success")
	}
	after, _ := s.Settings(t.Context())
	disk, _ := os.ReadFile(a.Path)
	if after.Revision != before.Revision || !reflect.DeepEqual(after.Desired, before.Desired) || string(disk) != string(raw) {
		t.Fatal("failed save published candidate")
	}
	a.WriteConfig = func(path string, c *config.Config) error {
		if err := config.Save(path, c); err != nil {
			return err
		}
		return &config.CommittedError{Err: errors.New("directory sync unavailable")}
	}
	applied, err := s.UpdateSettings(t.Context(), request)
	if err != nil || applied.Warning == "" || !applied.PendingRestart || a.Cfg.Gateway.TaskMaxTurns != 9 {
		t.Fatalf("committed warning rolled back desired: %+v %v", applied, err)
	}
}

// The approval stance takes hold without a restart, so the console has to
// read it back as what is running now; leaving it out of the effective
// snapshot would show the owner a stance that has already been replaced.
func TestDefaultApprovalAppliesWithoutARestart(t *testing.T) {
	a := approvalAdminFixture(t, "ask")
	a.Cfg.Gateway.OwnerID = "owner"
	if err := config.Save(a.Path, a.Cfg); err != nil {
		t.Fatal(err)
	}
	s := NewSettings(a, a.Cfg)
	before, err := s.Settings(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	update := consoleapi.SettingsUpdate{BaseRevision: before.Revision, Settings: json.RawMessage(`{"gateway":{"default_approval":"full"}}`)}
	after, err := s.UpdateSettings(t.Context(), update)
	if err != nil {
		t.Fatal(err)
	}
	if after.PendingRestart {
		t.Fatal("a live approval change asked for a restart")
	}
	var effective config.SettingsValues
	if err := json.Unmarshal(after.Effective, &effective); err != nil {
		t.Fatal(err)
	}
	if effective.Gateway.DefaultApproval != "full" {
		t.Fatalf("effective stance reads back as %q", effective.Gateway.DefaultApproval)
	}
	if got := a.Catalog.Default().Approval; got != "full" {
		t.Fatalf("the running catalog carries %q", got)
	}
	if got := a.Catalog.Default().Options["mode"]; got != "read-only" {
		t.Fatalf("saving a global default cleared the Agent override: %q", got)
	}
	if got := a.Cfg.Agents["pinned"].Options["effort"]; got != "high" {
		t.Fatalf("saving a global default changed unrelated options: %q", got)
	}
}

// Reading the settings does not wait for a settings change to be saved.
func TestSettingsReadDuringASettingsSave(t *testing.T) {
	a := agentAdminFixture(t)
	a.Cfg.Gateway.OwnerID = "owner"
	if err := config.Save(a.Path, a.Cfg); err != nil {
		t.Fatal(err)
	}
	s := NewSettings(a, a.Cfg)
	before, err := s.Settings(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	var during consoleapi.SettingsView
	finished := readsDuringSave(t, a, func() error {
		_, err := s.UpdateSettings(t.Context(), consoleapi.SettingsUpdate{BaseRevision: before.Revision, Settings: json.RawMessage(`{"gateway":{"task_max_turns":9}}`)})
		return err
	}, func() error {
		during, err = s.Settings(t.Context())
		return err
	})
	if !finished {
		t.Fatal("reading the settings waited for a settings save")
	}
	if !reflect.DeepEqual(during.Desired, before.Desired) {
		t.Fatal("a reader saw settings that were not saved yet")
	}
	if a.Cfg.Gateway.TaskMaxTurns != 9 {
		t.Fatal("the saved settings were not published")
	}
}
