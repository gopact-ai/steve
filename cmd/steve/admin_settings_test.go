package main

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
	a.cfg.Gateway.OwnerID = "owner"
	a.cfg.Feishu.AppID = "test-app"
	a.cfg.Feishu.AppSecret = "private-secret"
	if err := config.Save(a.path, a.cfg); err != nil {
		t.Fatal(err)
	}
	s := newHubSettingsService(a, a.cfg)
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
	if a.cfg.Gateway.TaskMaxTurns != 0 {
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
	a.cfg.Gateway.OwnerID = "owner"
	if err := config.Save(a.path, a.cfg); err != nil {
		t.Fatal(err)
	}
	s := newHubSettingsService(a, a.cfg)
	before, _ := s.Settings(t.Context())
	raw, _ := os.ReadFile(a.path)
	a.writeConfig = func(string, *config.Config) error { return errors.New("disk unavailable") }
	request := consoleapi.SettingsUpdate{BaseRevision: before.Revision, Settings: json.RawMessage(`{"gateway":{"task_max_turns":9}}`)}
	if _, err := s.UpdateSettings(t.Context(), request); err == nil {
		t.Fatal("save failure reported success")
	}
	after, _ := s.Settings(t.Context())
	disk, _ := os.ReadFile(a.path)
	if after.Revision != before.Revision || !reflect.DeepEqual(after.Desired, before.Desired) || string(disk) != string(raw) {
		t.Fatal("failed save published candidate")
	}
	a.writeConfig = func(path string, c *config.Config) error {
		if err := config.Save(path, c); err != nil {
			return err
		}
		return &config.CommittedError{Err: errors.New("directory sync unavailable")}
	}
	applied, err := s.UpdateSettings(t.Context(), request)
	if err != nil || applied.Warning == "" || !applied.PendingRestart || a.cfg.Gateway.TaskMaxTurns != 9 {
		t.Fatalf("committed warning rolled back desired: %+v %v", applied, err)
	}
}
