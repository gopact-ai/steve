package admin

import (
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/gopact-ai/steve/internal/config"
	"github.com/gopact-ai/steve/internal/consoleapi"
)

func TestRuntimeSettingsPublishOnlyAfterCommit(t *testing.T) {
	a := agentAdminFixture(t)
	a.Cfg.Gateway.OwnerID = "owner"
	if err := config.Save(a.Path, a.Cfg); err != nil {
		t.Fatal(err)
	}
	a.RuntimeSettings = config.NewRuntimeSettings(a.Cfg)
	s := NewSettings(a, a.Cfg)
	before, err := s.Settings(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	original := a.RuntimeSettings.Load()
	request := consoleapi.SettingsUpdate{BaseRevision: before.Revision, Settings: json.RawMessage(`{"gateway":{"prompt_timeout":"17m","task_max_turns":7},"policies":{"landing":{"conflicts":"manual"},"planning":{"attempts":4}}}`)}
	a.WriteConfig = func(string, *config.Config) error { return errors.New("not committed") }
	if _, err := s.UpdateSettings(t.Context(), request); err == nil {
		t.Fatal("save should fail")
	}
	if a.RuntimeSettings.Load() != original {
		t.Fatal("failed save published runtime policy")
	}
	a.WriteConfig = config.Save
	after, err := s.UpdateSettings(t.Context(), request)
	if err != nil {
		t.Fatal(err)
	}
	if after.PendingRestart || after.ApplyMode != "live" {
		t.Fatalf("safe runtime settings demand restart: %+v", after)
	}
	got := a.RuntimeSettings.Load()
	if time.Duration(got.Gateway.PromptTimeout) != 17*time.Minute || got.Gateway.TaskMaxTurns != 7 || got.Policies.Planning.Attempts != 4 || got.Policies.Landing.Conflicts != "manual" {
		t.Fatalf("runtime snapshot not published: %+v", got)
	}
	var effective config.SettingsValues
	if err := json.Unmarshal(after.Effective, &effective); err != nil || effective != got {
		t.Fatalf("effective settings do not describe runtime: %+v %v", effective, err)
	}
	for _, f := range after.Fields {
		if f.Path == "gateway.owner_id" {
			continue
		}
		if f.ApplyMode == "restart" {
			t.Fatalf("runtime field still marked restart: %s", f.Path)
		}
	}
}
