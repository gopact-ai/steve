package admin

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/gopact-ai/steve/internal/config"
	"github.com/gopact-ai/steve/internal/consoleapi"
)

func ChannelValue[T any](v T) *T { return &v }

func ChannelsAdminFixture(t *testing.T) (*Service, *hubChannelsService) {
	t.Helper()
	a := agentAdminFixture(t)
	a.Cfg.Gateway.OwnerID, a.Cfg.Gateway.DefaultChannel = "console-owner", "feishu"
	a.Cfg.Gateway.StatePath = filepath.Join(t.TempDir(), "state.json")
	a.Cfg.Projects = map[string]config.Project{"work": {Home: config.ProjectHome{Path: t.TempDir()}}}
	a.Cfg.Feishu = config.Feishu{AppID: "app", AppSecret: "original-private-secret", OwnerOpenID: "im-owner"}
	if err := config.Save(a.Path, a.Cfg); err != nil {
		t.Fatal(err)
	}
	return a, NewChannels(a, a.Cfg)
}

func AssertNoChannelSecrets(t *testing.T, view consoleapi.ChannelsView) {
	t.Helper()
	raw, err := json.Marshal(view)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "private-secret") || strings.Contains(string(raw), `"app_secret":`) {
		t.Fatal("channels response exposed a secret")
	}
}

func TestChannelsRotationCASAndEffectiveSettings(t *testing.T) {
	a, s := ChannelsAdminFixture(t)
	before, err := s.Channels(t.Context())
	if err != nil || before.Revision == "" || before.PendingRestart || before.ApplyMode != "restart" {
		t.Fatalf("initial view=%+v err=%v", before, err)
	}
	AssertNoChannelSecrets(t, before)
	update := consoleapi.ChannelsUpdate{BaseRevision: before.Revision, Channels: config.ChannelPatch{Feishu: &config.FeishuChannelPatch{AppSecret: &config.ChannelSecret{Action: "replace", Value: ChannelValue("rotated-private-secret")}}}}
	var wg sync.WaitGroup
	results := make(chan error, 2)
	for range 2 {
		wg.Go(func() { _, err := s.UpdateChannels(t.Context(), update); results <- err })
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
		t.Fatalf("winners=%d conflicts=%d", wins, conflicts)
	}
	after, _ := s.Channels(t.Context())
	if !after.PendingRestart || !reflect.DeepEqual(before.Effective, after.Effective) || !reflect.DeepEqual(after.Desired, after.Effective) {
		t.Fatal("secret rotation did not remain pending behind redacted settings")
	}
	AssertNoChannelSecrets(t, after)
	reloaded, err := config.Load(a.Path)
	if err != nil || reloaded.Feishu.AppSecret != "rotated-private-secret" {
		t.Fatal("secret rotation not persisted", err)
	}
	if _, err := s.UpdateChannels(t.Context(), consoleapi.ChannelsUpdate{Channels: config.ChannelPatch{}}); !errors.Is(err, consoleapi.ErrSettingsConflict) {
		t.Fatal("missing revision accepted", err)
	}
	policies := NewSettings(a, a.Cfg)
	settings, _ := policies.Settings(t.Context())
	if _, err := policies.UpdateSettings(t.Context(), consoleapi.SettingsUpdate{BaseRevision: settings.Revision, Settings: json.RawMessage(`{"gateway":{"task_max_turns":3}}`)}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.UpdateChannels(t.Context(), consoleapi.ChannelsUpdate{BaseRevision: after.Revision}); !errors.Is(err, consoleapi.ErrSettingsConflict) {
		t.Fatal("unrelated settings save did not invalidate channel revision", err)
	}
}

func TestChannelsDisablePreservesConsoleIdentityAndSecrets(t *testing.T) {
	a, _ := ChannelsAdminFixture(t)
	a.Cfg.Gateway.OwnerID = ""
	if err := config.Save(a.Path, a.Cfg); err != nil {
		t.Fatal(err)
	}
	s := NewChannels(a, a.Cfg)
	before, _ := s.Channels(t.Context())
	update := consoleapi.ChannelsUpdate{BaseRevision: before.Revision, Channels: config.ChannelPatch{DefaultChannel: ChannelValue("console"), Feishu: &config.FeishuChannelPatch{Enabled: ChannelValue(false), OwnerOpenID: ChannelValue("different-im-owner")}}}
	after, err := s.UpdateChannels(t.Context(), update)
	if err != nil {
		t.Fatal(err)
	}
	if after.Desired.Console.OwnerID != "im-owner" || after.Desired.Feishu.OwnerOpenID != "different-im-owner" || after.Desired.Feishu.Enabled || !after.Desired.Feishu.AppSecretConfigured || !after.PendingRestart || !after.Effective.Feishu.Enabled {
		t.Fatal("disable changed identity or falsely updated effective channel")
	}
	AssertNoChannelSecrets(t, after)
	if a.Cfg.Feishu.AppSecret != "original-private-secret" {
		t.Fatal("disabled channel lost omitted secret")
	}
	if _, err := s.UpdateChannels(t.Context(), consoleapi.ChannelsUpdate{BaseRevision: after.Revision, Channels: config.ChannelPatch{Feishu: &config.FeishuChannelPatch{AppSecret: &config.ChannelSecret{Action: "clear"}}}}); err != nil {
		t.Fatal(err)
	}
	after, _ = s.Channels(t.Context())
	if after.Desired.Feishu.AppSecretConfigured || a.Cfg.Feishu.AppSecret != "" || !after.Effective.Feishu.AppSecretConfigured {
		t.Fatal("explicit clear did not preserve effective snapshot")
	}
}

func TestChannelsSaveFailuresAndExternalEditRespectCommitBoundary(t *testing.T) {
	a, s := ChannelsAdminFixture(t)
	before, _ := s.Channels(t.Context())
	raw, err := os.ReadFile(a.Path)
	if err != nil {
		t.Fatal(err)
	}
	update := consoleapi.ChannelsUpdate{BaseRevision: before.Revision, Channels: config.ChannelPatch{Feishu: &config.FeishuChannelPatch{AppSecret: &config.ChannelSecret{Action: "replace", Value: ChannelValue("rotated-private-secret")}}}}
	a.WriteConfig = func(string, *config.Config) error {
		return errors.New("write failed original-private-secret rotated-private-secret")
	}
	if _, err := s.UpdateChannels(t.Context(), update); err == nil || strings.Contains(err.Error(), "private-secret") {
		t.Fatal("write failure leaked a secret or reported success")
	}
	after, _ := s.Channels(t.Context())
	stored, _ := os.ReadFile(a.Path)
	if !reflect.DeepEqual(before, after) || string(stored) != string(raw) || a.Cfg.Feishu.AppSecret != "original-private-secret" {
		t.Fatal("failed save published candidate")
	}
	a.WriteConfig = func(path string, c *config.Config) error {
		if err := config.Save(path, c); err != nil {
			return err
		}
		return &config.CommittedError{Err: fmt.Errorf("directory sync failed for %s", c.Feishu.AppSecret)}
	}
	applied, err := s.UpdateChannels(t.Context(), update)
	if err != nil || applied.Warning == "" || !applied.PendingRestart || a.Cfg.Feishu.AppSecret != "rotated-private-secret" {
		t.Fatal("committed warning rolled back candidate", err)
	}
	AssertNoChannelSecrets(t, applied)
	if err := os.WriteFile(a.Path, []byte("external edit"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := s.UpdateChannels(t.Context(), consoleapi.ChannelsUpdate{BaseRevision: applied.Revision}); !errors.Is(err, consoleapi.ErrSettingsConflict) {
		t.Fatal("external file edit was overwritten", err)
	}
	stored, _ = os.ReadFile(a.Path)
	if string(stored) != "external edit" {
		t.Fatal("external edit was lost")
	}
}

func TestChannelsViewsDoNotAliasAppliedConfiguration(t *testing.T) {
	a, _ := ChannelsAdminFixture(t)
	a.Cfg.Feishu.AllowedSenders = []string{"allowed"}
	s := NewChannels(a, a.Cfg)
	view, _ := s.Channels(t.Context())
	view.Desired.Feishu.AllowedSenders[0] = "changed"
	view.Effective.Feishu.AllowedSenders[0] = "changed"
	next, _ := s.Channels(t.Context())
	if next.Desired.Feishu.AllowedSenders[0] != "allowed" || next.Effective.Feishu.AllowedSenders[0] != "allowed" || next.PendingRestart {
		t.Fatal("public response mutated runtime snapshot or declaration")
	}
}
